package i

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	coreemail "github.com/shiroha-a/mk/internal/core/email"
	"github.com/shiroha-a/mk/internal/l10n"
	miscsmtp "github.com/shiroha-a/mk/internal/misc/smtp"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// generateVerifyCode returns a random 16-char hex code for email verification.
func generateVerifyCode() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// verifyURL builds the email verification URL.
func (h *Handler) verifyURL(code string) string {
	base := h.serverURL
	if base == "" {
		base = "https://localhost"
	}
	return base + "/verify-email/" + code
}

// UpdateEmail handles POST /api/i/update-email.
// 本家 Misskey と同じフロー:
//  1. パスワード検証 (必須)
//  2. email が null → emailRequiredForSignup ならエラー、そうでなければクリア
//  3. email が非null → フォーマット + banned + active/API 検証
//  4. profile の email / emailVerified / emailVerifyCode を更新
//  5. 新 email があれば確認メールを送信
func (h *Handler) UpdateEmail(c echo.Context) error {
	u := middleware.GetUser(c)
	var req struct {
		Password string  `json:"password"`
		Email    *string `json:"email"`
		Token    string  `json:"token"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}

	profile := h.userService.GetProfile(u.ID)
	if profile == nil || profile.Password == nil {
		return apierr.JSONInternalError(c)
	}

	// 2FA gate: upstream Misskey TS (update-email.ts) は twoFactorEnabled
	// なら token 必須。mk-go では旧来 password だけで通っていたため、
	// password 漏洩 = 2FA bypass で email 乗っ取り → password reset で
	// account takeover まで到達可能だった (drop-in regression、認証強度
	// として最も影響が大きい経路の 1 つ)。check2FAToken 経由なので
	// replay 保護も自動で効く。
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

	// パスワード検証。upstream Misskey TS は ApiError(meta.errors.incorrectPassword)
	// を framework が 400 (= client error) に変換する (#885)。mk-go も
	// drop-in 互換のため 400 に揃える (旧 mk-go は 403)。
	if !h.comparePassword(c, u.ID, *profile.Password, req.Password, "e54c1d7e-e7d6-4103-86b6-0a95069b4ad3") {
		return nil
	}

	fields := map[string]any{
		"emailVerified":   false,
		"emailVerifyCode": nil,
	}

	if req.Email == nil {
		// email クリア。emailRequiredForSignup 時はクリア不可。
		if h.metaRepo != nil {
			// upstream update-email: email=null かつ emailRequiredForSignup なら
			// EMAIL_REQUIRED (324c7a88) を返す (#1546)。
			if m, err := h.metaRepo.Fetch(); err == nil && m.EmailRequiredForSignup {
				return c.JSON(http.StatusBadRequest, apierr.Error("EMAIL_REQUIRED", "Email is required.", "324c7a88-59f2-492f-903f-89134f93e47e"))
			}
		}
		fields["email"] = nil
	} else {
		addr := *req.Email
		// email validation (banned + format + active/API)。verifymail / truemail
		// API への outbound は SSRF-safe + forward proxy 経由 (#638)。
		if h.metaRepo != nil {
			if m, err := h.metaRepo.Fetch(); err == nil {
				svc := coreemail.NewServiceWithClient(m, h.emailValidationClient)
				if verr := svc.Validate(c.Request().Context(), addr); verr != nil {
					return c.JSON(http.StatusBadRequest, apierr.Error("UNAVAILABLE", "Email is not available.", "a2defefb-f220-8849-0af6-17f816099323"))
				}
			}
		}
		// **確認済みの重複を弾く。** 他人が確認済みのアドレスを設定できると、
		// 確認メールがその相手に飛び、相手が (自分宛だと思って) リンクを踏むと
		// `/api/verify-email` は未認証・コードのみで照合するため**こちら側の
		// プロフィール**が確認済みになる。以後そのアドレスに攻撃者アカウントの
		// 通知が届き、アビューズ調査でアドレスが一意識別子として使えなくなる。
		if h.userRepo != nil {
			inUse, ierr := h.userRepo.EmailVerifiedInUse(addr)
			if ierr != nil {
				slog.Error("i/update-email: cannot check whether the email is in use", "err", ierr)
				return apierr.JSONInternalError(c)
			}
			if inUse {
				return c.JSON(http.StatusBadRequest, apierr.Error("UNAVAILABLE", "Email is not available.", "a2defefb-f220-8849-0af6-17f816099323"))
			}
		}
		fields["email"] = addr

		// 確認コード生成 + メール送信
		code := generateVerifyCode()
		fields["emailVerifyCode"] = code

		if h.emailSender != nil {
			var metaLangs []string
			if h.metaRepo != nil {
				if m, err := h.metaRepo.Fetch(); err == nil {
					metaLangs = l10n.LangsFromMeta(m)
				}
			}
			lang := l10n.Resolve(profile.Lang, metaLangs)
			subject, lead, linkLabel := l10n.VerifyEmail(lang)
			text, bodyHTML := coreemail.LinkText(lead, linkLabel, h.verifyURL(code))
			html := coreemail.WrapHTML(coreemail.HTMLWrapInput{
				SiteURL:            h.serverURL,
				Subject:            subject,
				EmailSettingsURL:   h.serverURL + "/settings/email",
				EmailSettingsLabel: l10n.EmailSettingsLabel(lang),
				BodyHTML:           bodyHTML,
			})
			go h.emailSender(addr, miscsmtp.Message{
				Subject: subject,
				Text:    text,
				HTML:    html,
			})
		}
	}

	if err := h.userService.UpdateProfileFields(u.ID, fields); err != nil {
		return apierr.JSONInternalError(c)
	}

	// **profile を書き換えられてから確定する** (#2852)。
	_ = use.Commit()
	committed = true
	return h.Me(c)
}

// VerifyEmail handles POST /api/verify-email.
// emailVerifyCode が一致すれば emailVerified を true にする。
func (h *Handler) VerifyEmail(c echo.Context) error {
	var req struct {
		Code string `json:"code"`
	}
	if err := c.Bind(&req); err != nil || req.Code == "" {
		return apierr.JSONInvalidParam(c)
	}

	profile, err := h.userService.FindProfileByVerifyCode(req.Code)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CODE", "No such code.", "97c1f576-e4b8-4b8a-a6dc-9cb65e7f6f85"))
	}

	if verr := h.userService.UpdateProfileFields(profile.UserID, map[string]any{
		"emailVerified":   true,
		"emailVerifyCode": nil,
	}); verr != nil {
		return apierr.JSONInternalError(c)
	}

	// upstream verify-email.ts は update 後に meUpdated を main stream へ publish
	// しクライアントへ即時反映する (#1551)。2FA handler と同じ publisher を使う。
	h.publishMeUpdated(profile.UserID)

	return c.NoContent(http.StatusNoContent)
}
