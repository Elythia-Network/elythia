package i

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/twofactor"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"gorm.io/gorm"
)

// wrapWebAuthnRequest builds a fresh *http.Request whose body is the
// browser-supplied attestation/assertion JSON. go-webauthn parses the body
// directly off the request, so we cannot pass through the original Echo
// request (its body has already been consumed by Bind()).
func wrapWebAuthnRequest(orig *http.Request, body json.RawMessage) (*http.Request, error) {
	req, err := http.NewRequestWithContext(orig.Context(), http.MethodPost, orig.URL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = orig.Header.Clone()
	req.Header.Set("Content-Type", "application/json")
	req.Body = io.NopCloser(bytes.NewReader(body))
	return req, nil
}

// TwoFARegister handles POST /api/i/2fa/register.
// TOTP秘密鍵を生成してtempSecretに保存、QRコードURLを返す。
func (h *Handler) TwoFARegister(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	if err := c.Bind(&req); err != nil || req.Password == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "password is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	profile, perr := h.userService.GetProfileErr(user.ID)
	// **DB 障害を「パスワード未設定」にしない** (#2799)。
	if perr != nil && !repository.IsNotFound(perr) {
		return apierr.JSONInternalError(c)
	}
	if profile == nil || profile.Password == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "No password set.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}
	// 2FA gate: upstream Misskey TS (2fa/register.ts:68/80) は twoFactorEnabled
	// なら token 必須 (= 既に 2FA 有効な user が secret を上書き再登録するケース)。
	// **必ず password check より先**: upstream は token-then-password 順なので、
	// wrong-password + wrong-token 同時送信時に upstream は INVALID_TOKEN を返す
	// (= mk-go も同じ shape にしないと frontend の error UI 分岐が崩れる)。
	// mk-go では旧来 password だけで通っていたため、password 漏洩 = 既存 2FA を
	// 攻撃者がコントロールする secret に置き換え可能だった。
	// **消費は password が通ってから確定させる** (#2852)。検証と同時に焼くと、
	// password を打ち間違えるだけでバックアップコードが 1 枚減る。
	var use twoFAUse
	if profile.TwoFactorEnabled {
		var ok bool
		if use, ok = h.check2FAToken(c.Request().Context(), profile, req.Token); !ok {
			return c.JSON(http.StatusForbidden, apierr.InvalidToken())
		}
	}
	committed := false
	defer func() {
		if !committed {
			use.Rollback()
		}
	}()
	if !h.comparePassword(c, user.ID, *profile.Password, req.Password, "78d6c839-20c9-4c66-b90a-fc0542168b48") {
		return nil
	}

	// issuer は upstream (register.ts) が config.host (instance hostname) を使う。
	// authenticator アプリのラベルとレスポンスの issuer field を一致させる (#1555)。
	issuer := h.twoFAIssuer()
	secret, uri, err := twofactor.GenerateSecret(issuer, user.Username)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// frontend (settings/2fa.qrdialog.vue) は `qr` を `<img :src=...>` で
	// 読み込むため、otpauth URI ではなく PNG data URL に変換する必要がある
	// (#697)。Misskey TS upstream の QRCode.toDataURL(url) と同形式。
	qrDataURL, err := twofactor.QRDataURL(uri)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// tempSecretに保存 (doneで確認後にsecretに移動)
	if err := h.userService.UpdateProfileFields(user.ID, map[string]any{"twoFactorTempSecret": secret}); err != nil {
		// 保存できていないと、次の `done` が必ず失敗する。QR を見せる前に落とす。
		slog.Error("i/2fa/register: failed to store the temp secret", "userId", user.ID, "err", err)
		return apierr.JSONInternalError(c)
	}

	// **tempSecret を書き込んでから確定する** (#2852)。
	_ = use.Commit()
	committed = true
	return c.JSON(http.StatusOK, map[string]any{
		"qr":     qrDataURL,
		"url":    uri,
		"secret": secret,
		"label":  user.Username,
		"issuer": issuer,
	})
}

// twoFAIssuer returns the TOTP issuer (instance hostname) derived from the
// configured server URL, matching upstream's config.host (register.ts)。
// serverURL 未配線 / parse 失敗時は "Misskey" にフォールバックする。
func (h *Handler) twoFAIssuer() string {
	if h.serverURL != "" {
		if u, err := url.Parse(h.serverURL); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return "Misskey"
}

// TwoFADone handles POST /api/i/2fa/done.
// TOTPコードを検証し、2FAを有効化する。同時にバックアップコードを生成して
// 一度だけレスポンスに含めて返す (ユーザーが控えておかないと二度と見られない)。
func (h *Handler) TwoFADone(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Token string `json:"token"`
	}
	if err := c.Bind(&req); err != nil || req.Token == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "token is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}

	profile, perr := h.userService.GetProfileErr(user.ID)
	if perr != nil && !repository.IsNotFound(perr) {
		return apierr.JSONInternalError(c)
	}
	if profile == nil || profile.TwoFactorTempSecret == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "2FA registration not started.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}

	// ValidateWithReplay は同一コードの replay を refuse する (RFC 6238 §5.2)。
	// temp secret は確認後すぐ permanent secret に昇格されるため strictly
	// 必要ではないが、3 経路で挙動を揃える方が監査・テスト時の予測可能性が
	// 高い。副作用: TwoFADone 成功直後 (replay guard TTL = TOTP acceptance
	// window 内) に同じ code で signin を試みると replay として 403 になる
	// (permanent secret に同 value で昇格するため Redis slot に hit する)。
	// 次の TOTP step に進めば解消する。
	// upstream done.ts:54 は `ps.token.replace(/\s/g, '')` で TOTP code から
	// 全空白を除去してから検証する (authenticator が "123 456" のように表示する
	// code を受け付ける、#1767)。strings.Fields は連続空白も畳む。
	token := strings.Join(strings.Fields(req.Token), "")
	if !twofactor.ValidateWithReplay(c.Request().Context(), h.totpReplayGuard, user.ID, token, *profile.TwoFactorTempSecret) {
		return c.JSON(http.StatusForbidden, apierr.InvalidToken())
	}

	// バックアップコードを生成。crypto/rand 失敗は実質起きないが、起きた場合は
	// 2FA セットアップ自体を中断して 500 を返す。
	backupCodes, err := twofactor.GenerateBackupCodes()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Failed to generate backup codes.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// tempSecretをsecretに移動、2FAを有効化、backup codes を保存
	// **書き込みの失敗を握り潰さない。** 捨てると、利用者は 2FA が有効になったと
	// 信じて、保存されていないバックアップコードを控えることになる。
	if err := h.userService.UpdateProfileFields(user.ID, map[string]any{
		"twoFactorSecret":       *profile.TwoFactorTempSecret,
		"twoFactorTempSecret":   nil,
		"twoFactorEnabled":      true,
		"twoFactorBackupSecret": model.StringArray(backupCodes),
	}); err != nil {
		slog.Error("i/2fa/done: failed to enable 2FA", "userId", user.ID, "err", err)
		return apierr.JSONInternalError(c)
	}

	// upstream (done.ts) は 2FA 有効化後に meUpdated を publish して UI を更新する
	// (twoFactorEnabled の反映)。key 系 handler と挙動を揃える (#1555)。
	h.publishMeUpdated(user.ID)

	return c.JSON(http.StatusOK, map[string]any{
		"backupCodes": backupCodes,
	})
}

// TwoFAUnregister handles POST /api/i/2fa/unregister.
// 2FAを無効化する。
func (h *Handler) TwoFAUnregister(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	if err := c.Bind(&req); err != nil || req.Password == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "password is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	profile, perr := h.userService.GetProfileErr(user.ID)
	// **DB 障害を「パスワード未設定」にしない** (#2799)。
	if perr != nil && !repository.IsNotFound(perr) {
		return apierr.JSONInternalError(c)
	}
	if profile == nil || profile.Password == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "No password set.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}
	// 2FA gate: upstream Misskey TS (2fa/unregister.ts:53/65) は twoFactorEnabled
	// なら token 必須。**必ず password check より先**: upstream は token-then-
	// password 順なので、wrong-password + wrong-token 同時送信時 upstream は
	// INVALID_TOKEN を返す (= mk-go も同じ shape にしないと drop-in 違反)。
	// mk-go では旧来 password だけで通っていたため、password 漏洩 = 2FA bypass
	// で 2FA 無効化 → 以後の sensitive 操作も password だけで通る連鎖が成立
	// していた。
	// **消費は password が通ってから確定させる** (#2852)。検証と同時に焼くと、
	// password を打ち間違えるだけでバックアップコードが 1 枚減る。
	var use twoFAUse
	if profile.TwoFactorEnabled {
		var ok bool
		if use, ok = h.check2FAToken(c.Request().Context(), profile, req.Token); !ok {
			return c.JSON(http.StatusForbidden, apierr.InvalidToken())
		}
	}
	committed := false
	defer func() {
		if !committed {
			use.Rollback()
		}
	}()
	if !h.comparePassword(c, user.ID, *profile.Password, req.Password, "7add0395-9901-4098-82f9-4f67af65f775") {
		return nil
	}

	// **ここでは Commit しない** (#2852)。この操作は 2FA を丸ごと消すので消費を
	// 包含しており、直後の clear が同じ列を nil にする。
	//
	// **`committed` は立てる。** rollback を止めるためで、TOTP で解除したときに
	// replay 記録が解放されると同じコードを window 内で使い回せてしまう
	// (`TestTwoFAUnregister_KeepsTOTPReplayRecord` が固定)。
	committed = true

	// **`usePasswordLessLogin` も落とす。**
	//
	// `signin-with-passkey` は `TwoFactorEnabled` を見ず、この列だけを見る。
	// 落とさないと、2FA を解除したあとも登録済みのパスキーで**パスワード
	// 無しのログインが通り続ける**。しかも `twoFactorEnabled` が false だと
	// `securityKeysList` は空配列に潰れるので、利用者からは残った鍵が見えず
	// (`i/2fa/remove-key` は credentialId を要求するので) 削除もできない。
	//
	// upstream の `i/2fa/unregister.ts` は同じ UPDATE に
	// `usePasswordLessLogin: false` を持つ。mk-go は `admin/unset-mfa` では
	// 落としており、自己解除の経路だけが取り残されていた。
	if err := h.userService.UpdateProfileFields(user.ID, map[string]any{
		"twoFactorSecret":       nil,
		"twoFactorEnabled":      false,
		"twoFactorBackupSecret": model.StringArray(nil),
		"usePasswordLessLogin":  false,
	}); err != nil {
		// **書き込みに失敗したら成功を返さない。** ここを握り潰すと、利用者は
		// 解除できたと信じたまま `usePasswordLessLogin` が真で残る。
		slog.Error("i/2fa/unregister: failed to clear 2FA fields", "userId", user.ID, "err", err)
		return apierr.JSONInternalError(c)
	}

	// upstream (unregister.ts) は 2FA 解除後に meUpdated を publish する (#1555)。
	h.publishMeUpdated(user.ID)

	return c.NoContent(http.StatusNoContent)
}

// --- WebAuthn handlers ---
//
// 共通プリチェック: パスワード再確認 + WebAuthn 依存性チェック。
// すべてのキー管理エンドポイントは現在のパスワードを要求する (本家 Misskey と
// 同じく直近の意図確認のため)。WebAuthn 依存性が未注入なら 503 を返す。
//
// 第 3 戻り値は「呼び出し側が処理を続行してよいか」のフラグ。false のとき
// レスポンスは既にこの helper 内で書き込まれているので、caller はそのまま
// nil を return する。
// incorrectPwID は INCORRECT_PASSWORD の endpoint 固有 id。upstream は
// register-key / key-done 等で別 id を割り当てるため、caller が渡す。
func (h *Handler) requireWebAuthn(c echo.Context, password, incorrectPwID string) (*model.User, *model.UserProfile, bool) {
	if h.webauthnSvc == nil || h.securityKeyRepo == nil {
		_ = c.JSON(http.StatusServiceUnavailable, apierr.Error("UNAVAILABLE", "WebAuthn is not configured.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		return nil, nil, false
	}
	user := middleware.GetUser(c)
	if user == nil {
		_ = c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
		return nil, nil, false
	}
	profile, perr := h.userService.GetProfileErr(user.ID)
	// **DB 障害を「パスワード未設定」にしない** (#2799)。この関数は
	// (user, profile, ok) を返すので、応答は自分で書いて false を返す。
	if perr != nil && !repository.IsNotFound(perr) {
		_ = apierr.JSONInternalError(c)
		return nil, nil, false
	}
	if profile == nil || profile.Password == nil {
		_ = c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "No password set.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
		return nil, nil, false
	}
	if !h.comparePassword(c, user.ID, *profile.Password, password, incorrectPwID) {
		return nil, nil, false
	}
	return user, profile, true
}

// twoFAUse is the deferred half of a 2FA verification: the token has been
// accepted, but whether that acceptance becomes permanent is still open.
//
// **2FA を焼くのは操作が成立したときだけ** (#2852)。verify した直後に確定させて
// いたので、この後ろで password を打ち間違えるだけでバックアップコードが 1 枚
// 減っていた。TOTP は同じコードが replay guard に残り、打ち直すと原因から遠い
// `INVALID_TOKEN` になる。
type twoFAUse struct {
	// commit makes the consumption permanent. nil のこともある (TOTP は検証時に
	// 記録済みなので確定作業が無い)。
	//
	// **error を返す。** 消費できたかどうかで応答を変えられる呼び出し元
	// (gate される操作より前に Commit する経路) が存在するため (#2862)。
	commit func() error
	// rollback undoes a consumption that was already recorded. nil のこともある
	// (backup code は commit するまで何も書いていない)。
	rollback func()
}

// Commit makes the 2FA consumption permanent. Safe on the zero value.
//
// **戻り値を捨ててよいのは、gate される操作を既に済ませた後で呼ぶ経路だけ。**
// そこで失敗しても返せるものが無い (操作は成立済み)。操作より前に Commit する
// 経路は error を見て拒否すること (#2862)。
func (u twoFAUse) Commit() error {
	if u.commit != nil {
		return u.commit()
	}
	return nil
}

// Rollback undoes a consumption recorded during verification. Safe on the
// zero value, and safe to call after Commit only if the caller never commits
// and rolls back the same use.
func (u twoFAUse) Rollback() {
	if u.rollback != nil {
		u.rollback()
	}
}

// check2FAToken validates a TOTP token (or single-use backup code) against the
// given profile **without making the consumption permanent**. The caller must
// call Commit on success or Rollback on failure.
//
// Misskey TS upstream の UserAuthService.twoFactorAuthenticate と同じ挙動:
// backup code に hit したら使い捨てで消費し、それ以外は TOTP として検証する。
// TOTP 経路は ValidateWithReplay で同一コードの replay を refuse する
// (RFC 6238 §5.2)。**upstream も 2026.6.0 で同等の機構を持つ**
// (`UserAuthService.validateOtp`)。mk-go が先行実装したもの。
//
// **replay の記録だけは検証時に行う。** SETNX は検査と記録を分けられないので、
// EXISTS と SETNX に割ると隙間に同じコードで 2 本通せてしまう。記録は今までどおり
// 検証時に行い、失敗が確定したら Rollback で取り消す。
func (h *Handler) check2FAToken(ctx context.Context, profile *model.UserProfile, token string) (twoFAUse, bool) {
	if token == "" {
		return twoFAUse{}, false
	}
	if _, err := twofactor.ConsumeBackupCode([]string(profile.TwoFactorBackupSecret), token); err == nil {
		userID := profile.UserID
		// **DB へ書く前に予約する** (#2852)。commit を password 検証の後ろへ動かした
		// ぶん、profile を読んでから書き戻すまでの間隔が伸びた。同じコードを載せた
		// リクエストが同時に来ると、全部が同じスナップショットを読んで全部が通り、
		// **1 枚の消費で複数の 2FA-gated 操作が成立する**。TOTP と同じ SETNX で
		// 予約すれば、原子的に 1 本だけに絞れる。
		//
		// guard が無い構成では今までどおり素通しする (fail-open)。
		if !twofactor.ReserveOnce(ctx, h.totpReplayGuard, userID, twofactor.BackupCodeGuardKey(token)) {
			return twoFAUse{}, false
		}
		return twoFAUse{
			commit: func() error {
				// **読んだ配列を書き戻さない** (#2852)。別のコードを使う同時実行が
				// 互いの消費を打ち消し合い、使ったはずのコードが復活する。
				if err := h.userService.RemoveBackupCode(userID, token); err != nil {
					// 消費に失敗したら予約を残さない — 残すと TTL のあいだ
					// 正当な利用者が同じコードで打ち直せなくなる。
					slog.Warn("2fa: failed to consume backup code", "userId", userID, "err", err)
					twofactor.ReleaseReservation(ctx, h.totpReplayGuard, userID, twofactor.BackupCodeGuardKey(token))
					return err
				}
				return nil
			},
			rollback: func() {
				twofactor.ReleaseReservation(ctx, h.totpReplayGuard, userID, twofactor.BackupCodeGuardKey(token))
			},
		}, true
	}
	if profile.TwoFactorSecret == nil {
		return twoFAUse{}, false
	}
	if !twofactor.ValidateWithReplay(ctx, h.totpReplayGuard, profile.UserID, token, *profile.TwoFactorSecret) {
		return twoFAUse{}, false
	}
	guard, userID := h.totpReplayGuard, profile.UserID
	return twoFAUse{rollback: func() {
		twofactor.ReleaseReservation(ctx, guard, userID, token)
	}}, true
}

// verify2FAToken validates a 2FA token and consumes it immediately.
//
// **password を先に検証している経路だけが使ってよい。** 2FA gate が password
// より前にある経路は check2FAToken を使い、password が通ってから Commit すること
// (#2852)。
func (h *Handler) verify2FAToken(ctx context.Context, profile *model.UserProfile, token string) bool {
	use, ok := h.check2FAToken(ctx, profile, token)
	if !ok {
		return false
	}
	// **消費できなかったら通さない** (#2862)。この経路は gate される操作より
	// 前に Commit するので拒否できる。通すと、書き込みが落ちているあいだ
	// 同じバックアップコードで何度でも操作が成立する (予約は Commit の中で
	// 解放されるので TTL も待たない)。signin と同じ判断。
	return use.Commit() == nil
}

// twoFAKeyNameMaxLen mirrors upstream の paramDef `name: { minLength: 1,
// maxLength: 30 }`。frontend (`os.inputText`) も 30 文字制限を掛けているが、
// API 直叩き経路の defense-in-depth として backend でも enforce する。
const twoFAKeyNameMaxLen = 30

// TwoFARegisterKey handles POST /api/i/2fa/register-key.
// 1 段階目: パスワード + 2FA token 認証 + WebAuthn registration challenge を返す。
// レスポンスは Misskey TS upstream と同じく PublicKeyCredentialCreationOptions
// そのもの (frontend は `{publicKey: ...}` で wrap して使う) (#698)。
//
// 前提: ユーザーは既に TOTP で 2FA を有効化済みであること。security key は
// 2FA の上に重ねる factor なので未有効化なら TWO_FACTOR_NOT_ENABLED を返す。
//
// 認証順序: requireWebAuthn で password を先に検証してから 2FA token を verify
// する。upstream は逆順 (token → password) だが、片方だけ正しい状態の検出可否は
// 対称なので情報リーク的に等価。`requireWebAuthn` を共有する都合で password 先に
// 揃えている。
//
// upstream Misskey #17354 (= 2026.5.1 / triage #1013): 同 endpoint は upstream
// 2026.5.1 でも refactor (passkey library 更新 / response schema 簡略化) で
// 残っており削除されていない。triage 当時 `git show --stat` の純減 line 数を
// 削除と誤読したが、実際は file は保持。mk-go は WebAuthn を go-webauthn lib で
// 実装しており upstream の @simplewebauthn lib 更新は無関係なので、本 handler
// は upstream と独立に維持する。
func (h *Handler) TwoFARegisterKey(c echo.Context) error {
	var req struct {
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	if err := c.Bind(&req); err != nil || req.Password == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "password is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	user, profile, ok := h.requireWebAuthn(c, req.Password, "38769596-efe2-4faf-9bec-abbb3f2cd9ba")
	if !ok {
		return nil
	}
	// **password を先に検証している経路なので即時消費でよい** (#2852)。
	if profile.TwoFactorEnabled && !h.verify2FAToken(c.Request().Context(), profile, req.Token) {
		return c.JSON(http.StatusForbidden, apierr.InvalidToken())
	}
	if !profile.TwoFactorEnabled {
		return c.JSON(http.StatusBadRequest, apierr.Error("TWO_FACTOR_NOT_ENABLED", "2fa not enabled.", "bf32b864-449b-47b8-974e-f9a5468546f1"))
	}

	existing, err := h.securityKeyRepo.ListByUser(user.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	creation, err := h.webauthnSvc.BeginRegistration(c.Request().Context(), user, existing)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Failed to begin registration.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// frontend は `parseCreationOptionsFromJSON({publicKey: <res>})` で読むので、
	// 内側の PublicKeyCredentialCreationOptions だけを返す。
	return c.JSON(http.StatusOK, creation.Response)
}

// TwoFAKeyDone handles POST /api/i/2fa/key-done.
// 2 段階目: ブラウザから返ってきた attestation response を検証して
// UserSecurityKey 行を作成する。リクエストボディは upstream 互換の
// `{ password, token, name, credential }`。`credential` は browser の
// `PublicKeyCredential.toJSON()` 出力 (ParsedCredentialCreationData フォーマット)。
// session は user 単位で 1 件保持されているので sessionId は不要 (#698)。
//
// 認証順序は TwoFARegisterKey と同じ (password → 2FA token)。
func (h *Handler) TwoFAKeyDone(c echo.Context) error {
	var req struct {
		Password   string          `json:"password"`
		Token      string          `json:"token"`
		Name       string          `json:"name"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := c.Bind(&req); err != nil || req.Password == "" || len(req.Credential) == 0 {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "password / credential are required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	if req.Name == "" {
		req.Name = "Security Key"
	}
	if len(req.Name) > twoFAKeyNameMaxLen {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "name is too long.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	user, profile, ok := h.requireWebAuthn(c, req.Password, "0d7ec6d2-e652-443e-a7bf-9ee9a0cd77b0")
	if !ok {
		return nil
	}
	// **password を先に検証している経路なので即時消費でよい** (#2852)。
	if profile.TwoFactorEnabled && !h.verify2FAToken(c.Request().Context(), profile, req.Token) {
		return c.JSON(http.StatusForbidden, apierr.InvalidToken())
	}
	if !profile.TwoFactorEnabled {
		return c.JSON(http.StatusBadRequest, apierr.Error("TWO_FACTOR_NOT_ENABLED", "2fa not enabled.", "798d6847-b1ed-4f9c-b1f9-163c42655995"))
	}

	existing, err := h.securityKeyRepo.ListByUser(user.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// go-webauthn の FinishRegistration は *http.Request からボディを読むので、
	// JSON-RPC 経由で受け取った credential を新しい http.Request にラップして渡す。
	httpReq, err := wrapWebAuthnRequest(c.Request(), req.Credential)
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "invalid credential payload.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	cred, err := h.webauthnSvc.FinishRegistration(c.Request().Context(), user, existing, httpReq)
	if err != nil {
		slog.Warn("2fa: webauthn FinishRegistration failed", "userId", user.ID, "err", err)
		return c.JSON(http.StatusForbidden, apierr.RegistrationFailed())
	}

	key := twofactor.CredentialToModel(cred, user.ID, req.Name)
	if err := h.securityKeyRepo.Create(key); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Failed to persist key.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// security key 1 つ以上 → securityKeysAvailable=true。本家と同じ挙動。
	if err := h.userService.UpdateProfileFields(user.ID, map[string]any{
		"securityKeysAvailable": true,
	}); err != nil {
		// 鍵自体は登録済みなので成功は返す。フラグのずれはログに残す。
		slog.Error("i/2fa/key-done: failed to set securityKeysAvailable", "userId", user.ID, "err", err)
	}

	// upstream Misskey TS と同じく `meUpdated` を publish して frontend の
	// `$i` (current user 状態) を即時更新する (#707)。これが無いと UI の
	// 設定→セキュリティ→パスキー一覧に登録直後の鍵が出ず、ユーザーは
	// 「登録できなかった」と誤認する。
	h.publishMeUpdated(user.ID)

	return c.JSON(http.StatusOK, map[string]any{
		"id":   key.ID,
		"name": key.Name,
	})
}

// publishMeUpdated は upstream `meUpdated` event を main stream に流す。
// 失敗は best-effort で握り潰す (publishing は副次的なので main flow を
// 止めない)。userService.ShowByID で User + Profile を 1 度に取得する。
//
// frontend (main-boot.ts) は payload を updateCurrentAccountPartial で
// 部分 merge するので、本 helper が送る MeDetailed の field がそのまま
// `$i` に反映される。upstream Misskey TS の meUpdated stream は
// MeDetailed shape を流すため、entity.PackMeDetailed で isExplorable /
// noCrawle / preventAiLearning など self-view-only field も含めて送る
// (#968)。なお usePasswordLessLogin など UserProfile 由来でも MeDetailed
// に含まれない field は publishMeUpdatedPartial で個別 publish する
// (#758 経緯)。
func (h *Handler) publishMeUpdated(userID string) {
	if h.mainStreamPublisher == nil {
		return
	}
	bundle, err := h.userService.ShowByID(userID)
	if err != nil {
		slog.Warn("2fa: meUpdated: load user failed", "userId", userID, "err", err)
		return
	}
	// 生 PackMeDetailed だと unread 系が default (0/[]/false) で出て、frontend の
	// updateCurrentAccountPartial が `$i` の実 unread 状態を clobber する。
	// meDetailedWithUnread で fillUnreadFields を通して実値を送る (#1258 fu)。
	packed := h.meDetailedWithUnread(context.Background(), bundle.User, bundle.Profile)
	h.mainStreamPublisher.PublishMainEvent(userID, "meUpdated", packed)
}

// publishMeUpdatedPartial は specific field のみを `meUpdated` payload と
// して送る。frontend は updateCurrentAccountPartial で部分 merge する
// ので、entity.PackUserDetailed が含まない private profile field を
// 更新した endpoint がこの helper で当該 field だけ publish できる
// (#758)。fields が空 / mainStreamPublisher 未配線なら no-op。
func (h *Handler) publishMeUpdatedPartial(userID string, fields map[string]any) {
	if h.mainStreamPublisher == nil || len(fields) == 0 {
		return
	}
	h.mainStreamPublisher.PublishMainEvent(userID, "meUpdated", fields)
}

// TwoFARemoveKey handles POST /api/i/2fa/remove-key.
// 自分の鍵 1 つを削除する。残り 0 件になったら securityKeysAvailable を false にする。
//
// 注: requireWebAuthn は使わず checks を inline している。upstream Misskey
// TS (2fa/remove-key.ts:57/69) は **TOTP gate → password check** 順なので、
// password を先に check する requireWebAuthn だと wrong-password + wrong-
// token 時の error code が upstream と drift する (INCORRECT_PASSWORD vs
// INVALID_TOKEN)。drop-in 互換性のため upstream 順で並べ直す。
func (h *Handler) TwoFARemoveKey(c echo.Context) error {
	var req struct {
		Password     string `json:"password"`
		Token        string `json:"token"`
		CredentialID string `json:"credentialId"`
	}
	if err := c.Bind(&req); err != nil || req.Password == "" || req.CredentialID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "password and credentialId are required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	if h.webauthnSvc == nil || h.securityKeyRepo == nil {
		return c.JSON(http.StatusServiceUnavailable, apierr.Error("UNAVAILABLE", "WebAuthn is not configured.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	user := middleware.GetUser(c)
	if user == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}
	profile, perr := h.userService.GetProfileErr(user.ID)
	// **DB 障害を「パスワード未設定」にしない** (#2799)。
	if perr != nil && !repository.IsNotFound(perr) {
		return apierr.JSONInternalError(c)
	}
	if profile == nil || profile.Password == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "No password set.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}
	// 2FA gate (passkey 削除は 2FA の物理 factor を 1 つ抜く操作なので強い
	// 認証が必要)。**必ず password check より先** に置く (upstream 順)。
	// **消費は password が通ってから確定させる** (#2852)。検証と同時に焼くと、
	// password を打ち間違えるだけでバックアップコードが 1 枚減る。
	var use twoFAUse
	if profile.TwoFactorEnabled {
		var ok bool
		if use, ok = h.check2FAToken(c.Request().Context(), profile, req.Token); !ok {
			return c.JSON(http.StatusForbidden, apierr.InvalidToken())
		}
	}
	committed := false
	defer func() {
		if !committed {
			use.Rollback()
		}
	}()
	if !h.comparePassword(c, user.ID, *profile.Password, req.Password, "141c598d-a825-44c8-9173-cfb9d92be493") {
		return nil
	}

	// upstream remove-key.ts:74-78 は delete を無条件に実行し no-match でも
	// 成功 ({}) を返す (NO_SUCH_KEY error は定義されていない、#1767)。no-match
	// (ErrRecordNotFound) は成功扱いで素通りし、実 DB error のみ 500 にする。
	if err := h.securityKeyRepo.Delete(req.CredentialID, user.ID); err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return apierr.JSONInternalError(c)
	}

	if remaining, err := h.securityKeyRepo.CountByUser(user.ID); err == nil && remaining == 0 {
		if err := h.userService.UpdateProfileFields(user.ID, map[string]any{
			"securityKeysAvailable": false,
			"usePasswordLessLogin":  false,
		}); err != nil {
			// **ここは開いたままになる向き。** 鍵が 0 本になったのに
			// `usePasswordLessLogin` が真で残る。
			slog.Error("i/2fa/remove-key: failed to clear passwordless login", "userId", user.ID, "err", err)
		}
	}
	// upstream 互換: 削除でも `meUpdated` を publish して frontend UI を即時更新 (#707)。
	h.publishMeUpdated(user.ID)
	// **key を消してから確定する** (#2852)。
	_ = use.Commit()
	committed = true
	// upstream は `return {}` なので 200 + 空オブジェクト (204 ではない)。
	return c.JSON(http.StatusOK, map[string]any{})
}

// TwoFAUpdateKey handles POST /api/i/2fa/update-key.
// 鍵の表示名を変更する。
func (h *Handler) TwoFAUpdateKey(c echo.Context) error {
	var req struct {
		CredentialID string `json:"credentialId"`
		Name         string `json:"name"`
	}
	if err := c.Bind(&req); err != nil || req.CredentialID == "" || req.Name == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "credentialId / name are required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	// upstream paramDef は name: { minLength: 1, maxLength: 30 }。TwoFAKeyDone と
	// 同じく API 直叩き経路の defense-in-depth として上限も enforce する (#1952)。
	if len(req.Name) > twoFAKeyNameMaxLen {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "name is too long.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	// upstream update-key.ts は paramDef を {name, credentialId} のみで宣言し、
	// secure:true の session 認証だけで password を再確認しない。standard misskey-js
	// client ({name, credentialId} のみ送信) で passkey rename が壊れるため、mk-go も
	// password 必須を撤回し session 認証 + 所有権チェックに依拠する (#1952、
	// TwoFAPasswordLess #758 と同方針)。
	if h.securityKeyRepo == nil {
		return c.JSON(http.StatusServiceUnavailable, apierr.Error("UNAVAILABLE", "WebAuthn is not configured.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	user := middleware.GetUser(c)
	if user == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}
	// upstream (update-key.ts) は key not-found (NO_SUCH_KEY) と not-owned
	// (ACCESS_DENIED) を区別する。UpdateName は両者を ErrRecordNotFound に
	// 畳むため、先に FindByID して所有権を判定する (#1555)。
	// upstream update-key.ts の noSuchKey / accessDenied は httpStatusCode 未指定
	// = kind 'client' default = 400 (#1767)。code/id は一致させつつ status を
	// 400 に揃える (以前は 404 / 403 を返していた)。
	key, err := h.securityKeyRepo.FindByID(req.CredentialID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil || key == nil {
		return c.JSON(http.StatusBadRequest, apierr.NoSuchKey())
	}
	if key.UserID != user.ID {
		return c.JSON(http.StatusBadRequest, apierr.AccessDenied())
	}
	if err := h.securityKeyRepo.UpdateName(req.CredentialID, user.ID, req.Name); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.NoSuchKey())
	}
	// 表示名変更も meUpdated で UI 反映 (#707)。
	h.publishMeUpdated(user.ID)
	// upstream は `return {}` なので 200 + 空オブジェクト (204 ではない)。
	return c.JSON(http.StatusOK, map[string]any{})
}

// TwoFAPasswordLess handles POST /api/i/2fa/password-less.
// `usePasswordLessLogin` フラグを切り替える。WebAuthn 鍵が登録されていないと
// 有効化できない (パスワードレスログインの前提が成立しないため)。
//
// upstream Misskey TS は paramDef を `{value: boolean}` (required: ['value'])
// で password を要求しない (#758)。secure endpoint なので RequireAuth
// middleware で session ベース認証が済んでいる前提。mk-go も合わせて
// password 必須を撤回する。
func (h *Handler) TwoFAPasswordLess(c echo.Context) error {
	var req struct {
		Value bool `json:"value"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "invalid request body.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	user := middleware.GetUser(c)
	if user == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}
	if req.Value {
		// security key が無いと passwordless 有効化不可。upstream は同じ
		// branch で profile を `usePasswordLessLogin=false` に巻き戻して
		// から error を返すので mk-go も合わせる。WebAuthn 未配線
		// (securityKeyRepo == nil) は鍵 0 件と等価扱い。
		hasKey := false
		if h.securityKeyRepo != nil {
			if n, err := h.securityKeyRepo.CountByUser(user.ID); err == nil && n > 0 {
				hasKey = true
			}
		}
		if !hasKey {
			if err := h.userService.UpdateProfileFields(user.ID, map[string]any{
				"usePasswordLessLogin": false,
			}); err != nil {
				slog.Error("i/2fa/password-less: failed to clear the flag", "userId", user.ID, "err", err)
			}
			return c.JSON(http.StatusBadRequest, apierr.NoSecurityKey())
		}
	}
	// **書き込みの失敗を握り潰さない。** 捨てると、利用者がパスワードレスを
	// 切ったつもりでも DB は真のままで `signin-with-passkey` が通り続け、
	// しかも直後の publish が**要求値**を流すので UI は「オフ」を表示する。
	if err := h.userService.UpdateProfileFields(user.ID, map[string]any{
		"usePasswordLessLogin": req.Value,
	}); err != nil {
		slog.Error("i/2fa/password-less: failed to update the flag", "userId", user.ID, "err", err)
		return apierr.JSONInternalError(c)
	}
	// usePasswordLessLogin は /api/i 経路の private profile field 群に属し、
	// entity.PackUserDetailed が含まないため publishMeUpdated (UserDetailed
	// publish) では frontend の $i に反映されない (#758)。partial helper で
	// 当該 field だけ送る。
	h.publishMeUpdatedPartial(user.ID, map[string]any{
		"usePasswordLessLogin": req.Value,
	})
	return c.NoContent(http.StatusNoContent)
}
