package i

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// backupCodes returns the codes still stored on the profile.
func backupCodes(repo *testutil.MockUserRepository, uid string) []string {
	return []string(repo.Profiles[uid].TwoFactorBackupSecret)
}

// 2FA を検証したあとで password に失敗しても、バックアップコードを焼かない (#2852)。
//
// **打ち間違えるたびに 1 枚減っていた。** 2FA gate が password 検証より前にあり、
// verify した時点で `UpdateProfileFields` に書き戻していたため。upstream と同じ
// 順序 (token → password) は保ったまま、消費の確定だけ後ろへ動かす。
func TestChangePassword_WrongPasswordKeepsBackupCode(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "oldpass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	before := backupCodes(repo, "u1")
	require.Len(t, before, 2)

	rec := postExtra(h.ChangePassword,
		`{"currentPassword":"WRONG","newPassword":"newpass","token":"backup1"}`, user)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INCORRECT_PASSWORD")
	assert.Equal(t, before, backupCodes(repo, "u1"), "password 失敗でバックアップコードが焼けている")
}

// 新パスワードが長すぎて弾かれる場合も焼かない (#2852)。
//
// **password は合っているので 2FA gate も password 検証も通る。** 落ちるのは
// その後の `bcrypt` 上限なので、defer 経由の rollback でしか救えない。
func TestChangePassword_TooLongNewPasswordKeepsBackupCode(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "oldpass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	before := backupCodes(repo, "u1")

	body := `{"currentPassword":"oldpass","newPassword":"` + strings.Repeat("a", 73) + `","token":"backup1"}`
	rec := postExtra(h.ChangePassword, body, user)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "PASSWORD_TOO_LONG")
	assert.Equal(t, before, backupCodes(repo, "u1"))
}

// 成功したときは 1 枚消費する。
//
// **上の 2 本と対で意味を持つ。** rollback を常に走らせる直し方 (= 一度も
// 消費しない) を弾く。
func TestChangePassword_SuccessConsumesBackupCode(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "oldpass")
	enableTwoFactorWithBackupCodes(repo, "u1")

	rec := postExtra(h.ChangePassword,
		`{"currentPassword":"oldpass","newPassword":"newpass","token":"backup1"}`, user)

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"backup2"}, backupCodes(repo, "u1"), "成功したのに消費されていない")
}

// wrong-password + wrong-token は INVALID_TOKEN (403) のまま (#2852)。
//
// **順序を入れ替える直し方を弾く。** password を先に見ると INCORRECT_PASSWORD に
// drift する。同じ罠を handler_2fa.go が 3 箇所で明示的に禁じている。
func TestChangePassword_WrongBothStaysInvalidToken(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "oldpass")
	enableTwoFactorWithBackupCodes(repo, "u1")

	rec := postExtra(h.ChangePassword,
		`{"currentPassword":"WRONG","newPassword":"newpass","token":"WRONG"}`, user)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_TOKEN")
	assert.Len(t, backupCodes(repo, "u1"), 2)
}

// 2FA gate を持つ他の endpoint も同じ扱いにする (#2852)。
//
// **change-password だけの問題ではなかった。** 2FA gate を持つ 6 endpoint の
// うち 6 つが 2FA gate を password より前に置いており、どれも同じ消費をしていた。
func TestTwoFAGatedEndpoints_WrongPasswordKeepsBackupCode(t *testing.T) {
	for _, tt := range []struct {
		name     string
		call     func(h *Handler) func(echo.Context) error
		body     string
		webauthn bool
	}{
		{name: "change-password", call: func(h *Handler) func(echo.Context) error { return h.ChangePassword },
			body: `{"currentPassword":"WRONG","newPassword":"newpass","token":"backup1"}`},
		{name: "delete-account", call: func(h *Handler) func(echo.Context) error { return h.DeleteAccount },
			body: `{"password":"WRONG","token":"backup1"}`},
		{name: "2fa/register", call: func(h *Handler) func(echo.Context) error { return h.TwoFARegister },
			body: `{"password":"WRONG","token":"backup1"}`},
		{name: "2fa/unregister", call: func(h *Handler) func(echo.Context) error { return h.TwoFAUnregister },
			body: `{"password":"WRONG","token":"backup1"}`},
		{name: "update-email", call: func(h *Handler) func(echo.Context) error { return h.UpdateEmail },
			body: `{"password":"WRONG","email":"new@example.com","token":"backup1"}`},
		// remove-key は WebAuthn 未設定だと 2FA gate の手前で 503 になるので配線する。
		{name: "2fa/remove-key", call: func(h *Handler) func(echo.Context) error { return h.TwoFARemoveKey },
			body: `{"password":"WRONG","token":"backup1","credentialId":"c1"}`, webauthn: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var h *Handler
			var repo *testutil.MockUserRepository
			if tt.webauthn {
				h, repo, _ = newWebAuthnHandler(t)
			} else {
				h, repo = newExtraHandler(t)
			}
			if tt.name == "delete-account" {
				h.SetRoleProvider(&stubRoleProvider{policies: map[string]any{
					role.PolicyCanDeleteAccount: true,
					role.PolicyCanPurgeAccount:  true,
				}})
			}
			user := setupUserWithPassword(repo, "u1", "oldpass")
			enableTwoFactorWithBackupCodes(repo, "u1")
			before := backupCodes(repo, "u1")

			rec := postExtra(tt.call(h), tt.body, user)

			assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
			assert.Equal(t, before, backupCodes(repo, "u1"), "password 失敗でバックアップコードが焼けている")
		})
	}
}

// TOTP の replay 保護は弱くならない (#2852)。
//
// **成功したコードは引き続き再利用できない。** rollback は失敗したときだけ走る。
func TestChangePassword_TOTPReplayStillRejectedAfterSuccess(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "oldpass")
	secret := enableTwoFactorWithTOTP(t, repo, "u1")
	guard := &countingReplayGuard{used: map[string]bool{}}
	h.SetTOTPReplayGuard(guard)
	code := totpCode(t, secret)

	rec := postExtra(h.ChangePassword,
		`{"currentPassword":"oldpass","newPassword":"newpass","token":"`+code+`"}`, user)
	require.Equal(t, http.StatusNoContent, rec.Code)

	// 同じコードで再度 (password は新しいほうで正しい)。
	rec2 := postExtra(h.ChangePassword,
		`{"currentPassword":"newpass","newPassword":"other","token":"`+code+`"}`, user)
	assert.Equal(t, http.StatusForbidden, rec2.Code, "成功した TOTP コードが再利用できている")
	assert.Equal(t, 0, guard.releases, "成功したのに replay 記録を取り消している")
}

// password に失敗した TOTP コードは打ち直せる (#2852)。
//
// **これが直したい症状そのもの。** 記録が残ると、利用者が同じ (まだ有効な)
// コードで打ち直したときに replay として弾かれ、原因から遠い INVALID_TOKEN になる。
func TestChangePassword_TOTPReusableAfterWrongPassword(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "oldpass")
	secret := enableTwoFactorWithTOTP(t, repo, "u1")
	guard := &countingReplayGuard{used: map[string]bool{}}
	h.SetTOTPReplayGuard(guard)
	code := totpCode(t, secret)

	rec := postExtra(h.ChangePassword,
		`{"currentPassword":"WRONG","newPassword":"newpass","token":"`+code+`"}`, user)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 1, guard.releases, "失敗したのに replay 記録を取り消していない")

	// 同じコードで打ち直せる。
	rec2 := postExtra(h.ChangePassword,
		`{"currentPassword":"oldpass","newPassword":"newpass","token":"`+code+`"}`, user)
	assert.Equal(t, http.StatusNoContent, rec2.Code, "打ち直しが replay 扱いで弾かれている")
}

// totpCode returns a currently valid TOTP code for the secret.
func totpCode(t *testing.T, secret string) string {
	t.Helper()
	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	return code
}

// countingReplayGuard is an in-memory ReplayGuard that also counts releases.
type countingReplayGuard struct {
	used     map[string]bool
	releases int
}

func (g *countingReplayGuard) MarkUsed(ctx context.Context, userID, code string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	k := userID + ":" + code
	if g.used[k] {
		return false, nil
	}
	g.used[k] = true
	return true, nil
}

// Release honours ctx so a rollback that forgot to detach from the request
// context is detectable. **本物の RedisReplayGuard は Del が
// `context canceled` で落ちる。** fake が ctx を捨てていると、その回帰を
// テストで捕まえられない。
func (g *countingReplayGuard) Release(ctx context.Context, userID, code string) error {
	g.releases++
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(g.used, userID+":"+code)
	return nil
}

var _ twofactor.ReplayGuard = (*countingReplayGuard)(nil)
var _ twofactor.ReplayReleaser = (*countingReplayGuard)(nil)

// 成功したときは 6 endpoint すべてでちょうど 1 枚消費する (#2852)。
//
// **失敗側だけ見ていると `Commit` を消しても緑になる。** それは「単回用の
// バックアップコードが永久に再利用できる」方向の劣化で、まさに 2FA が緩くなる側。
func TestTwoFAGatedEndpoints_SuccessConsumesExactlyOne(t *testing.T) {
	for _, tt := range []struct {
		name     string
		call     func(h *Handler) func(echo.Context) error
		body     string
		want     int
		webauthn bool
	}{
		{name: "change-password", call: func(h *Handler) func(echo.Context) error { return h.ChangePassword },
			body: `{"currentPassword":"oldpass","newPassword":"newpass","token":"backup1"}`, want: http.StatusNoContent},
		{name: "delete-account", call: func(h *Handler) func(echo.Context) error { return h.DeleteAccount },
			body: `{"password":"oldpass","token":"backup1"}`, want: http.StatusNoContent},
		{name: "2fa/register", call: func(h *Handler) func(echo.Context) error { return h.TwoFARegister },
			body: `{"password":"oldpass","token":"backup1"}`, want: http.StatusOK},
		{name: "update-email", call: func(h *Handler) func(echo.Context) error { return h.UpdateEmail },
			body: `{"password":"oldpass","email":"new@example.com","token":"backup1"}`, want: http.StatusOK},
		{name: "2fa/remove-key", call: func(h *Handler) func(echo.Context) error { return h.TwoFARemoveKey },
			body: `{"password":"oldpass","token":"backup1","credentialId":"c1"}`, want: http.StatusOK, webauthn: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var h *Handler
			var repo *testutil.MockUserRepository
			if tt.webauthn {
				h, repo, _ = newWebAuthnHandler(t)
			} else {
				h, repo = newExtraHandler(t)
			}
			if tt.name == "delete-account" {
				h.SetRoleProvider(&stubRoleProvider{policies: map[string]any{
					role.PolicyCanDeleteAccount: true,
					role.PolicyCanPurgeAccount:  true,
				}})
			}
			user := setupUserWithPassword(repo, "u1", "oldpass")
			enableTwoFactorWithBackupCodes(repo, "u1")

			rec := postExtra(tt.call(h), tt.body, user)

			require.Equal(t, tt.want, rec.Code, "body=%s", rec.Body.String())
			assert.Equal(t, []string{"backup2"}, backupCodes(repo, "u1"),
				"成功したのにちょうど 1 枚消費されていない")
		})
	}
}

// 2fa/unregister は成功後に 2FA クレデンシャルを残さない (#2852)。
//
// **この操作だけ Commit の位置が違う。** 解除は消費を包含するので、クリアより
// **前**に確定させる。後にすると commit の部分 UPDATE が
// `twoFactorBackupSecret` を書き戻してクリアを取り消す (実測で回帰させた)。
func TestTwoFAUnregister_ClearsBackupCodes(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "oldpass")
	enableTwoFactorWithBackupCodes(repo, "u1")

	rec := postExtra(h.TwoFAUnregister, `{"password":"oldpass","token":"backup1"}`, user)

	require.Equal(t, http.StatusNoContent, rec.Code)
	p := repo.Profiles["u1"]
	assert.False(t, p.TwoFactorEnabled)
	assert.Empty(t, backupCodes(repo, "u1"), "解除したのにバックアップコードが残っている")
	assert.Nil(t, p.TwoFactorSecret)
}

// 6 endpoint すべてで rollback が配線されている (#2852)。
//
// **失敗時に replay 記録が残ると、打ち直しが INVALID_TOKEN になる。**
// change-password だけ見ていると他 5 つの defer を外しても緑のまま通る。
func TestTwoFAGatedEndpoints_WrongPasswordReleasesReservation(t *testing.T) {
	for _, tt := range []struct {
		name     string
		call     func(h *Handler) func(echo.Context) error
		body     string
		webauthn bool
	}{
		{name: "change-password", call: func(h *Handler) func(echo.Context) error { return h.ChangePassword },
			body: `{"currentPassword":"WRONG","newPassword":"newpass","token":"backup1"}`},
		{name: "delete-account", call: func(h *Handler) func(echo.Context) error { return h.DeleteAccount },
			body: `{"password":"WRONG","token":"backup1"}`},
		{name: "2fa/register", call: func(h *Handler) func(echo.Context) error { return h.TwoFARegister },
			body: `{"password":"WRONG","token":"backup1"}`},
		{name: "2fa/unregister", call: func(h *Handler) func(echo.Context) error { return h.TwoFAUnregister },
			body: `{"password":"WRONG","token":"backup1"}`},
		{name: "update-email", call: func(h *Handler) func(echo.Context) error { return h.UpdateEmail },
			body: `{"password":"WRONG","email":"new@example.com","token":"backup1"}`},
		{name: "2fa/remove-key", call: func(h *Handler) func(echo.Context) error { return h.TwoFARemoveKey },
			body: `{"password":"WRONG","token":"backup1","credentialId":"c1"}`, webauthn: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var h *Handler
			var repo *testutil.MockUserRepository
			if tt.webauthn {
				h, repo, _ = newWebAuthnHandler(t)
			} else {
				h, repo = newExtraHandler(t)
			}
			if tt.name == "delete-account" {
				h.SetRoleProvider(&stubRoleProvider{policies: map[string]any{
					role.PolicyCanDeleteAccount: true,
					role.PolicyCanPurgeAccount:  true,
				}})
			}
			user := setupUserWithPassword(repo, "u1", "oldpass")
			enableTwoFactorWithBackupCodes(repo, "u1")
			guard := &countingReplayGuard{used: map[string]bool{}}
			h.SetTOTPReplayGuard(guard)

			postExtra(tt.call(h), tt.body, user)

			assert.Equal(t, 1, guard.releases, "失敗したのに予約を解放していない")
			assert.Empty(t, guard.used, "予約が残っている (打ち直しが弾かれる)")
		})
	}
}

// 同じスナップショットを読んだ 2 本目は予約で弾かれる (#2852)。
//
// **消費の確定を password 検証の後ろへ動かしたぶん、profile を読んでから書き戻す
// までの間隔が伸びた。** 同じスナップショットを読んだリクエストが両方通ると、
// 1 枚の消費で 2 つの 2FA-gated 操作が成立する。gate 時点の予約で 1 本に絞る。
//
// **handler を 2 回叩く形では検証できない。** 1 本目の commit で DB から
// コードが消えるので、予約が無くても 2 本目は落ちる (実測で空振りした)。
// 競合そのものを再現するには、commit 前の check を 2 回通す。
func TestCheck2FAToken_BackupCodeReservedOnce(t *testing.T) {
	h, repo := newExtraHandler(t)
	setupUserWithPassword(repo, "u1", "oldpass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	h.SetTOTPReplayGuard(&countingReplayGuard{used: map[string]bool{}})
	profile := repo.Profiles["u1"]
	ctx := context.Background()

	_, ok1 := h.check2FAToken(ctx, profile, "backup1")
	require.True(t, ok1, "1 本目が通らない")

	// commit していないので profile のスナップショットは 1 本目と同じ。
	_, ok2 := h.check2FAToken(ctx, profile, "backup1")
	assert.False(t, ok2, "同じバックアップコードが同時に 2 本通っている")

	// 別のコードは影響を受けない。
	_, ok3 := h.check2FAToken(ctx, profile, "backup2")
	assert.True(t, ok3, "無関係なコードまで弾いている")
}

// **ReleaseReservation の ctx 切り離しは internal/core/twofactor で検証する** (#2862)。
// 関数が住んでいるパッケージに置かないと、そちらのカバレッジに計上されず、
// 移動したときに検証ごと迷子になる。

// バックアップコードの予約は TOTP の keyspace と衝突しない (#2852)。
//
// **同じ guard を共有している。** namespace を外すと、同じ文字列のバックアップ
// コードと TOTP コードが互いを弾き合う。今の生成規則では桁数が違うので起きないが、
// 片方を変えたときに「一方を使うともう一方が使えない」形で壊れる。
func TestCheck2FAToken_BackupCodeAndTOTPKeyspacesAreSeparate(t *testing.T) {
	h, repo := newExtraHandler(t)
	setupUserWithPassword(repo, "u1", "oldpass")
	secret := enableTwoFactorWithTOTP(t, repo, "u1")
	code := totpCode(t, secret)
	// TOTP と同じ文字列をバックアップコードとしても持たせる。
	repo.Profiles["u1"].TwoFactorBackupSecret = model.StringArray{code}
	h.SetTOTPReplayGuard(&countingReplayGuard{used: map[string]bool{}})
	profile := repo.Profiles["u1"]

	// backup code として消費される (ConsumeBackupCode が先に hit する)。
	_, ok := h.check2FAToken(context.Background(), profile, code)
	require.True(t, ok)

	// 同じ文字列を TOTP として使う経路が塞がっていないこと。
	repo.Profiles["u1"].TwoFactorBackupSecret = nil
	_, ok2 := h.check2FAToken(context.Background(), repo.Profiles["u1"], code)
	assert.True(t, ok2, "バックアップコードの予約が TOTP の keyspace を潰している")
}

// 503 (検証枠が取れない) でもバックアップコードを焼かない (#2852)。
//
// **issue の主要動機がこれ。** サーバー都合の一時障害で利用者の 2FA が減るのは
// おかしい。password は正しく、落ちるのは検証枠だけなので、rollback でしか救えない。
func TestChangePassword_VerifierUnavailableKeepsBackupCode(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithArgon2Password(repo, "u1", "oldpass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	before := backupCodes(repo, "u1")
	require.Len(t, before, 2)

	rec := postExtraCanceled(h.ChangePassword,
		`{"currentPassword":"oldpass","newPassword":"newpass","token":"backup1"}`, user)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, before, backupCodes(repo, "u1"), "503 でバックアップコードが焼けている")
}

// 別々のコードを使う同時実行でも、消したコードが復活しない (#2852)。
//
// **`ReserveOnce` は同一コードしか塞がない。** gate 時点のスナップショットから
// `remaining` を組み立てて丸ごと書き戻すと、A が c1 を、B が c2 を消したときに
// 互いの結果を打ち消し合い、**使ったはずのコードが戻る**。DB 側で
// `array_remove` する形でしか解けない。
func TestCheck2FAToken_ConcurrentDistinctCodesDoNotResurrect(t *testing.T) {
	h, repo := newExtraHandler(t)
	setupUserWithPassword(repo, "u1", "oldpass")
	repo.Profiles["u1"].TwoFactorEnabled = true
	repo.Profiles["u1"].TwoFactorBackupSecret = model.StringArray{"c1", "c2", "c3"}
	h.SetTOTPReplayGuard(&countingReplayGuard{used: map[string]bool{}})
	profile := repo.Profiles["u1"]
	ctx := context.Background()

	// 2 本とも gate 時点の同じスナップショットを読む。
	useA, okA := h.check2FAToken(ctx, profile, "c1")
	require.True(t, okA)
	useB, okB := h.check2FAToken(ctx, profile, "c2")
	require.True(t, okB)

	require.NoError(t, useA.Commit())
	require.NoError(t, useB.Commit())

	assert.ElementsMatch(t, []string{"c3"}, backupCodes(repo, "u1"),
		"消したコードが復活している (書き戻しが互いを打ち消している)")
}

// 消費の書き込みに失敗したら予約を残さない (#2852)。
//
// **残すと TTL のあいだ正当な利用者が締め出される。** 同じ (まだ有効で未消費の)
// コードで打ち直すと INVALID_TOKEN になる。
func TestCheck2FAToken_ReleasesReservationWhenConsumeFails(t *testing.T) {
	h, repo := newExtraHandler(t)
	setupUserWithPassword(repo, "u1", "oldpass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	repo.RemoveBackupCodeFn = func(string, string) error { return errors.New("db down") }
	guard := &countingReplayGuard{used: map[string]bool{}}
	h.SetTOTPReplayGuard(guard)

	use, ok := h.check2FAToken(context.Background(), repo.Profiles["u1"], "backup1")
	require.True(t, ok)
	// 消費の失敗がこのテストの主題 (実測で `db down` が返る)。
	require.Error(t, use.Commit())

	assert.Equal(t, 1, guard.releases, "消費に失敗したのに予約を残している")
	assert.Empty(t, guard.used)
}

// 2fa/unregister が成功したら TOTP の replay 記録を残す (#2852)。
//
// **`committed` を立てないと rollback が記録を解放する。** この操作自体は
// バックアップコードを clear するので消費は観測できないが、TOTP で解除した
// 場合は話が別で、**同じコードを window 内で他所に使い回せてしまう**。
func TestTwoFAUnregister_KeepsTOTPReplayRecord(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "oldpass")
	secret := enableTwoFactorWithTOTP(t, repo, "u1")
	guard := &countingReplayGuard{used: map[string]bool{}}
	h.SetTOTPReplayGuard(guard)
	code := totpCode(t, secret)

	rec := postExtra(h.TwoFAUnregister,
		`{"password":"oldpass","token":"`+code+`"}`, user)

	require.Equal(t, http.StatusNoContent, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, 0, guard.releases, "成功したのに replay 記録を解放している")
	assert.Contains(t, guard.used, "u1:"+code, "replay 記録が残っていない (同じコードを使い回せる)")
}

// **消費できなかったら通さない** (#2862)。`verify2FAToken` を使う 2 経路
// (`i/2fa/register-key` / `i/2fa/key-done`) は gate される操作より前に Commit
// するので、拒否できる。通すと書き込みが落ちているあいだ同じバックアップ
// コードで何度でも操作が成立する (予約は Commit の中で解放されるので TTL も
// 待たない)。signin と同じ判断。
//
// **mutation の後に Commit する経路 (change-password 等) には適用できない** —
// そこで失敗しても返せるものが無い。だから一律 fail-closed にはしない。
func TestVerify2FAToken_ConsumeFailureRejects(t *testing.T) {
	h, repo := newExtraHandler(t)
	setupUserWithPassword(repo, "u1", "oldpass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	h.SetTOTPReplayGuard(&countingReplayGuard{used: map[string]bool{}})
	repo.RemoveBackupCodeFn = func(string, string) error { return errors.New("db down") }

	assert.False(t, h.verify2FAToken(context.Background(), repo.Profiles["u1"], "backup1"),
		"消費に失敗したのに通している")
}

// 消費に成功したら通る (上の変異で全部落ちる形にしない)。
func TestVerify2FAToken_SuccessPasses(t *testing.T) {
	h, repo := newExtraHandler(t)
	setupUserWithPassword(repo, "u1", "oldpass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	h.SetTOTPReplayGuard(&countingReplayGuard{used: map[string]bool{}})

	assert.True(t, h.verify2FAToken(context.Background(), repo.Profiles["u1"], "backup1"))
}
