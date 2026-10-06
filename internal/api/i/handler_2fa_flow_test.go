package i

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/pquerna/otp/totp"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TOTP フルフロー: register → done → unregister。それぞれの happy path と
// 各種エラー (NoProfile / WrongPassword / NoTempSecret / InvalidToken) を
// カバーする。

func TestTwoFARegister_Success(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	rec := postExtra(h.TwoFARegister, `{"password":"pass"}`, user)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["secret"])
	assert.Equal(t, "Elythia", resp["issuer"])
	// `url` は otpauth:// 形式 (authenticator アプリの「アプリで開く」用)、
	// `qr` は PNG data URL (frontend が <img src=...> で読む用) — 両者は
	// 別形式でなければならない (#697)。
	assert.Contains(t, resp["url"], "otpauth://totp/")
	assert.Contains(t, resp["qr"], "data:image/png;base64,",
		"qr field must be a base64 PNG data URL so the frontend <img src=...> renders")

	// tempSecret がプロファイルに書き込まれている
	profile := repo.Profiles["u1"]
	require.NotNil(t, profile.TwoFactorTempSecret)
}

// #1555 issuer は instance host (config.host) を使う (upstream register.ts)。
// serverURL 配線時は host を返し、otpauth URL にも埋め込まれる。
func TestTwoFARegister_IssuerFromHost(t *testing.T) {
	h, repo := newExtraHandler(t)
	h.SetServerURL("https://misskey.example")
	user := setupUserWithPassword(repo, "u1", "pass")
	rec := postExtra(h.TwoFARegister, `{"password":"pass"}`, user)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "misskey.example", resp["issuer"])
	// otpauth URL の issuer param にも host が埋まる。
	assert.Contains(t, resp["url"], "misskey.example")
}

func TestTwoFARegister_WrongPassword(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "correct")
	rec := postExtra(h.TwoFARegister, `{"password":"wrong"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTwoFARegister_NoProfile(t *testing.T) {
	h, repo := newExtraHandler(t)
	repo.Users["u1"] = &model.User{ID: "u1"}
	rec := postExtra(h.TwoFARegister, `{"password":"pass"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TOTP gate (upstream drop-in 互換): 既に 2FA 有効な user が token 無しで
// register (= secret 上書き再登録) を呼ぶと 403 INVALID_TOKEN で refuse される。
func TestTwoFARegister_With2FA_RequiresToken(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	rec := postExtra(h.TwoFARegister, `{"password":"pass"}`, user)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_TOKEN")
}

// 2FA 有効でも valid token (backup code) を渡せば成功し新しい secret が発行される。
func TestTwoFARegister_With2FA_AcceptsBackupCode(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	rec := postExtra(h.TwoFARegister, `{"password":"pass","token":"backup1"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.NotNil(t, repo.Profiles["u1"].TwoFactorTempSecret)
}

// upstream order regression guard: wrong-password + wrong-token を同時送信
// したとき、upstream Misskey TS は TOTP gate を先に評価して INVALID_TOKEN
// (authentication failed) を返す。mk-go も同じ shape にしないと frontend の
// error UI 分岐 (TOTP 再入力 vs password 再入力) が崩れる。
func TestTwoFARegister_With2FA_WrongPasswordAndToken_ReturnsTokenError(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	rec := postExtra(h.TwoFARegister, `{"password":"wrong","token":"wrong"}`, user)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_TOKEN",
		"TOTP gate must fire before password check (upstream Misskey TS order)")
	assert.NotContains(t, rec.Body.String(), "INCORRECT_PASSWORD")
}

func TestTwoFADone_Success(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")

	// register で tempSecret を生成
	rec := postExtra(h.TwoFARegister, `{"password":"pass"}`, user)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	secret := resp["secret"].(string)

	// 現在時刻で有効な TOTP を生成
	token, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)

	// done で有効化。新しい挙動: 200 + body にバックアップコードを含む。
	rec2 := postExtra(h.TwoFADone, `{"token":"`+token+`"}`, user)
	require.Equal(t, http.StatusOK, rec2.Code)
	var doneResp map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &doneResp))
	codes, _ := doneResp["backupCodes"].([]any)
	require.Len(t, codes, 5, "TwoFADone should return 5 backup codes")

	profile := repo.Profiles["u1"]
	assert.True(t, profile.TwoFactorEnabled)
	require.NotNil(t, profile.TwoFactorSecret)
	assert.Equal(t, secret, *profile.TwoFactorSecret)
	assert.Nil(t, profile.TwoFactorTempSecret)
	assert.Len(t, profile.TwoFactorBackupSecret, 5)
}

// #1767: upstream done.ts は token から空白を除去してから検証する
// (authenticator が "123 456" と表示する code を受け付ける)。
func TestTwoFADone_StripsWhitespaceFromToken(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	rec := postExtra(h.TwoFARegister, `{"password":"pass"}`, user)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	secret := resp["secret"].(string)

	token, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	// 6 桁 code の中央に空白を挿入 ("123 456" 形式)。
	spaced := token[:3] + " " + token[3:]
	rec2 := postExtra(h.TwoFADone, `{"token":"`+spaced+`"}`, user)
	require.Equal(t, http.StatusOK, rec2.Code, "空白入り TOTP code も受理される")
	assert.True(t, repo.Profiles["u1"].TwoFactorEnabled)
}

// #1555 TwoFADone は 2FA 有効化後に meUpdated を publish する (upstream done.ts)。
func TestTwoFADone_PublishesMeUpdated(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	pub := &stubIMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)

	rec := postExtra(h.TwoFARegister, `{"password":"pass"}`, user)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	token, err := totp.GenerateCode(resp["secret"].(string), time.Now())
	require.NoError(t, err)

	rec2 := postExtra(h.TwoFADone, `{"token":"`+token+`"}`, user)
	require.Equal(t, http.StatusOK, rec2.Code)
	requireMeUpdated(t, pub, "u1")
}

// #1555 TwoFAUnregister は 2FA 解除後に meUpdated を publish する
// (upstream unregister.ts)。
func TestTwoFAUnregister_PublishesMeUpdated(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	pub := &stubIMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)

	rec := postExtra(h.TwoFAUnregister, `{"password":"pass"}`, user)
	require.Equal(t, http.StatusNoContent, rec.Code)
	requireMeUpdated(t, pub, "u1")
}

// requireMeUpdated asserts the publisher captured at least one meUpdated event
// for userID。
func requireMeUpdated(t *testing.T, pub *stubIMainStreamPublisher, userID string) {
	t.Helper()
	for _, c := range pub.calls {
		if c.eventType == "meUpdated" && c.userID == userID {
			return
		}
	}
	t.Fatalf("expected a meUpdated event for %s, got %d events", userID, len(pub.calls))
}

func TestTwoFADone_NoTempSecret(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	// register を飛ばして done を呼ぶ
	rec := postExtra(h.TwoFADone, `{"token":"123456"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	_ = repo
}

func TestTwoFADone_InvalidToken(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	// tempSecret を手動でセット
	tempSecret := "JBSWY3DPEHPK3PXP"
	repo.Profiles["u1"].TwoFactorTempSecret = &tempSecret

	rec := postExtra(h.TwoFADone, `{"token":"000000"}`, user)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// newReplayGuard は miniredis-backed ReplayGuard を返す。テスト終了時に自動 close。
func newReplayGuard(t *testing.T) twofactor.ReplayGuard {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return twofactor.NewRedisReplayGuard(client)
}

// TestTwoFADone_ReplayRejected: i/2fa/done でも TOTP replay 保護が効くこと。
// 実運用では done 後 tempSecret が消えるので二度目は別 path で弾かれるが、
// guard の挙動を 3 経路で揃えるための回帰テスト。
func TestTwoFADone_ReplayRejected(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	tempSecret := "JBSWY3DPEHPK3PXP"
	repo.Profiles["u1"].TwoFactorTempSecret = &tempSecret
	h.SetTOTPReplayGuard(newReplayGuard(t))

	token, err := totp.GenerateCode(tempSecret, time.Now())
	require.NoError(t, err)

	// 1 回目は通る (= 2FA が enable され tempSecret が secret に昇格)
	rec1 := postExtra(h.TwoFADone, `{"token":"`+token+`"}`, user)
	require.Equal(t, http.StatusOK, rec1.Code)

	// 2 回目: tempSecret を意図的に同じ値で復活させ、純粋に「同じ TOTP コードを
	// もう一度送ったら refuse される」ことを検証する。replay guard が無ければ
	// (= 旧実装) ここで再度 200 を返してしまう。
	repo.Profiles["u1"].TwoFactorTempSecret = &tempSecret
	rec2 := postExtra(h.TwoFADone, `{"token":"`+token+`"}`, user)
	assert.Equal(t, http.StatusForbidden, rec2.Code, "same TOTP code must be refused as replay on the second submit")
}

// TestVerify2FAToken_ReplayRejected: verify2FAToken 経由
// (TwoFARegisterKey / TwoFAKeyDone から呼ばれる sensitive 操作) でも TOTP
// replay 保護が効くこと。
func TestVerify2FAToken_ReplayRejected(t *testing.T) {
	h, repo := newExtraHandler(t)
	setupUserWithPassword(repo, "u1", "pass")
	secret := "JBSWY3DPEHPK3PXP"
	repo.Profiles["u1"].TwoFactorSecret = &secret
	repo.Profiles["u1"].TwoFactorEnabled = true
	h.SetTOTPReplayGuard(newReplayGuard(t))

	token, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	profile := repo.Profiles["u1"]

	// 1 回目は accept
	assert.True(t, h.verify2FAToken(t.Context(), profile, token))
	// 2 回目は refuse (replay)
	assert.False(t, h.verify2FAToken(t.Context(), profile, token))
}

func TestTwoFAUnregister_Success(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	// 事前に 2FA を有効化状態にする
	secret := "JBSWY3DPEHPK3PXP"
	repo.Profiles["u1"].TwoFactorSecret = &secret
	repo.Profiles["u1"].TwoFactorEnabled = true
	h.SetTOTPReplayGuard(newReplayGuard(t))

	// upstream 互換: 2FA 有効中の unregister は TOTP token も要求される。
	token, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	rec := postExtra(h.TwoFAUnregister, `{"password":"pass","token":"`+token+`"}`, user)
	require.Equal(t, http.StatusNoContent, rec.Code)

	profile := repo.Profiles["u1"]
	assert.False(t, profile.TwoFactorEnabled)
	assert.Nil(t, profile.TwoFactorSecret)
}

// TestTwoFAUnregister_MissingTOTP_With2FAEnabled guards the new TOTP gate:
// 2FA 有効ユーザが token 無しで unregister を呼ぶと 403 で refuse されること。
func TestTwoFAUnregister_MissingTOTP_With2FAEnabled(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	secret := "JBSWY3DPEHPK3PXP"
	repo.Profiles["u1"].TwoFactorSecret = &secret
	repo.Profiles["u1"].TwoFactorEnabled = true

	rec := postExtra(h.TwoFAUnregister, `{"password":"pass"}`, user)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_TOKEN")
}

// upstream order regression guard (see TestTwoFARegister_With2FA_WrongPassword
// AndToken_ReturnsTokenError)。unregister 経路でも同様。
func TestTwoFAUnregister_With2FA_WrongPasswordAndToken_ReturnsTokenError(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	secret := "JBSWY3DPEHPK3PXP"
	repo.Profiles["u1"].TwoFactorSecret = &secret
	repo.Profiles["u1"].TwoFactorEnabled = true

	rec := postExtra(h.TwoFAUnregister, `{"password":"wrong","token":"wrong"}`, user)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_TOKEN",
		"TOTP gate must fire before password check (upstream Misskey TS order)")
	assert.NotContains(t, rec.Body.String(), "INCORRECT_PASSWORD")
}

func TestTwoFAUnregister_WrongPassword(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "correct")
	rec := postExtra(h.TwoFAUnregister, `{"password":"wrong"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTwoFAUnregister_NoProfile(t *testing.T) {
	h, repo := newExtraHandler(t)
	repo.Users["u1"] = &model.User{ID: "u1"}
	rec := postExtra(h.TwoFAUnregister, `{"password":"pass"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- TwoFA / WebAuthn parameter validation ---
//
// 空 body での 400 を確認するだけの軽量テスト。flow と一緒に置いて TwoFA*
// 関連の挙動が一覧で見つかるようにする。

func TestTwoFARegister_NoPassword(t *testing.T) {
	h, _ := newExtraHandler(t)
	// パスワード未指定 → BadRequest
	assert.Equal(t, http.StatusBadRequest, postExtra(h.TwoFARegister, `{}`, stubUser).Code)
}

func TestTwoFADone_NoToken(t *testing.T) {
	h, _ := newExtraHandler(t)
	// トークン未指定 → BadRequest
	assert.Equal(t, http.StatusBadRequest, postExtra(h.TwoFADone, `{}`, stubUser).Code)
}

func TestTwoFAUnregister_NoPassword(t *testing.T) {
	h, _ := newExtraHandler(t)
	// パスワード未指定 → BadRequest
	assert.Equal(t, http.StatusBadRequest, postExtra(h.TwoFAUnregister, `{}`, stubUser).Code)
}

// 5 つの WebAuthn handler は実装後はパラメータ必須なので、空 body で 400 を返す。
// password 等を渡したケースでの正常系は別 webauthn テストに追加する。
func TestTwoFARegisterKey(t *testing.T) {
	h, _ := newExtraHandler(t)
	assert.Equal(t, http.StatusBadRequest, postExtra(h.TwoFARegisterKey, `{}`, stubUser).Code)
}

func TestTwoFAKeyDone(t *testing.T) {
	h, _ := newExtraHandler(t)
	assert.Equal(t, http.StatusBadRequest, postExtra(h.TwoFAKeyDone, `{}`, stubUser).Code)
}

func TestTwoFARemoveKey(t *testing.T) {
	h, _ := newExtraHandler(t)
	assert.Equal(t, http.StatusBadRequest, postExtra(h.TwoFARemoveKey, `{}`, stubUser).Code)
}

func TestTwoFAUpdateKey(t *testing.T) {
	h, _ := newExtraHandler(t)
	assert.Equal(t, http.StatusBadRequest, postExtra(h.TwoFAUpdateKey, `{}`, stubUser).Code)
}

// upstream Misskey TS は paramDef で password を要求しないので、空 body でも
// value=false 扱いで no-content 成功する (#758)。詳細な branch の test は
// handler_webauthn_test.go の TestTwoFAPasswordLess_* 群を参照。
func TestTwoFAPasswordLess(t *testing.T) {
	h, _ := newExtraHandler(t)
	assert.Equal(t, http.StatusNoContent, postExtra(h.TwoFAPasswordLess, `{}`, stubUser).Code)
}

// 2FA を解除したら usePasswordLessLogin も落ちること。
//
// **落とさないと、登録済みのパスキーでパスワード無しのログインが通り続ける。**
// `signin-with-passkey` は `TwoFactorEnabled` を見ずこの列だけを見る。しかも
// `twoFactorEnabled` が false だと `securityKeysList` は空配列に潰れるので、
// 利用者からは残った鍵が見えず削除もできない。
func TestTwoFAUnregister_ClearsPasswordLessLogin(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	secret := "JBSWY3DPEHPK3PXP"
	repo.Profiles["u1"].TwoFactorSecret = &secret
	repo.Profiles["u1"].TwoFactorEnabled = true
	repo.Profiles["u1"].UsePasswordLessLogin = true
	h.SetTOTPReplayGuard(newReplayGuard(t))

	// 2FA 有効中の unregister は TOTP token も要求される。
	token, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	rec := postExtra(h.TwoFAUnregister, `{"password":"pass","token":"`+token+`"}`, user)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.False(t, repo.Profiles["u1"].TwoFactorEnabled, "2FA が解除されること")
	require.False(t, repo.Profiles["u1"].UsePasswordLessLogin,
		"**usePasswordLessLogin も落ちること** — 残るとパスキーでのパスワード無しログインが通り続ける")
}

// **パスワードレスの書き込みが失敗したら成功を返さないこと。**
//
// 捨てると、利用者が切ったつもりでも DB は真のままで `signin-with-passkey` が
// 通り続け、しかも直後の publish が要求値を流すので UI は「オフ」を表示する。
func TestTwoFAPasswordLess_FailureIsNotReportedAsSuccess(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	repo.Profiles["u1"].UsePasswordLessLogin = true
	repo.UpdateProfileErr = errors.New("db down")

	rec := postExtra(h.TwoFAPasswordLess, `{"value":false}`, user)
	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"書き込みに失敗したら 500 を返すこと")
}
