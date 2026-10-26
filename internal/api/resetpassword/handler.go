package resetpassword

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/bgwork"
	coreemail "github.com/elythia-network/elythia/internal/core/email"
	"github.com/elythia-network/elythia/internal/l10n"
	"github.com/elythia-network/elythia/internal/misc"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/password"
	miscsmtp "github.com/elythia-network/elythia/internal/misc/smtp"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/labstack/echo/v4"
)

// EmailSender sends an email message (subject + text + optional HTML).
// HTML が空なら text/plain only、設定されていれば multipart/alternative で
// 両方送出される (#600 item 4)。
type EmailSender func(to string, msg miscsmtp.Message)

// Handler handles password reset endpoints.
type Handler struct {
	// background は応答の後の処理 (リセットメールの送信) を起動する箱 (#3661)。世代の停止で
	// 待つ。nil なら素の goroutine で起動する (テスト)。
	background *bgwork.Group
	userRepo   repository.UserRepository
	resetRepo  repository.PasswordResetRequestRepository
	metaRepo   repository.MetaRepository
	idGen      id.Generator
	email      EmailSender
	serverURL  string
	// passwordHasherは新しいパスワードをハッシュする(#3780)。零値は既定のcost。
	passwordHasher password.Hasher
}

// NewHandler creates a new password reset Handler.
// background is the group the work after a response (the reset mail) runs
// in, so stopping the server generation waits for it (#3661); metaRepo is the
// meta lookup for the mail's locale fallback.
func NewHandler(userRepo repository.UserRepository, resetRepo repository.PasswordResetRequestRepository, idGen id.Generator,
	background *bgwork.Group, metaRepo repository.MetaRepository,
) *Handler {
	return &Handler{userRepo: userRepo, resetRepo: resetRepo, idGen: idGen, background: background, metaRepo: metaRepo}
}

// SetPasswordHasher sets the Hasher that the new password is hashed with.
// Unset, the default cost is used.
func (h *Handler) SetPasswordHasher(hasher password.Hasher) { h.passwordHasher = hasher }

// SetEmailSender attaches an EmailSender for sending reset emails.
func (h *Handler) SetEmailSender(s EmailSender) { h.email = s }

// SetServerURL sets the base URL for reset links.
func (h *Handler) SetServerURL(u string) { h.serverURL = u }

// RequestReset handles POST /api/request-reset-password.
// ユーザーが見つからない/email不一致/未認証でも成功レスポンスを返す（情報漏洩防止）。
func (h *Handler) RequestReset(c echo.Context) error {
	var raw struct {
		Username *string `json:"username"`
		Email    *string `json:"email"`
	}
	// 本家の paramDef は username・email を required の string とするだけなので、
	// 空文字も通し、該当する利用者がいないので 204 を返す (#3845)。
	if err := c.Bind(&raw); err != nil || raw.Username == nil || raw.Email == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "username and email are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	req := struct{ Username, Email string }{*raw.Username, *raw.Email}

	// ユーザー検索 (ローカルのみ)
	user, err := h.userRepo.FindByUsernameLower(req.Username, nil)
	if err != nil {
		return c.NoContent(http.StatusNoContent)
	}
	// **プラグインが管理するアカウント (#3468) には再設定を受け付けない。**
	// 居ない利用者と同じ応答にする。パスワードを作れると、そのままログイン
	// できる相手になる。
	if user.IsPluginManaged() {
		return c.NoContent(http.StatusNoContent)
	}

	// profile取得 → email照合 + emailVerified確認
	profile, err := h.userRepo.FindProfileByUserID(user.ID)
	if err != nil || profile.Email == nil || *profile.Email != req.Email || !profile.EmailVerified {
		return c.NoContent(http.StatusNoContent)
	}

	// 64文字のランダムトークン生成
	token := misc.SecureRandomHex(64)
	now := time.Now()
	resetReq := &model.PasswordResetRequest{
		ID:     h.idGen.Generate(now),
		Token:  token,
		UserID: user.ID,
	}
	if err := h.resetRepo.Create(resetReq); err != nil {
		return c.NoContent(http.StatusNoContent)
	}

	// リセットメール送信 (text + html multipart)
	if h.email != nil {
		var metaLangs []string
		if h.metaRepo != nil {
			if meta, err := h.metaRepo.Fetch(); err == nil {
				metaLangs = l10n.LangsFromMeta(meta)
			}
		}
		lang := l10n.Resolve(profile.Lang, metaLangs)
		subject, lead, linkLabel := l10n.PasswordReset(lang)
		link := fmt.Sprintf("%s/reset-password/%s", h.serverURL, token)
		text, bodyHTML := coreemail.LinkText(lead, linkLabel, link)
		html := coreemail.WrapHTML(coreemail.HTMLWrapInput{
			SiteURL: h.serverURL,
			Subject: subject,
			// reset-password は認証済 user 向けなので email-settings 二段 footer
			// を出す (TS の sendEmail と同じ二段構造)。
			EmailSettingsURL:   h.serverURL + "/settings/email",
			EmailSettingsLabel: l10n.EmailSettingsLabel(lang),
			BodyHTML:           bodyHTML,
		})
		to := *profile.Email
		msg := miscsmtp.Message{Subject: subject, Text: text, HTML: html}
		h.background.Go(func(context.Context) { h.email(to, msg) })
	}

	return c.NoContent(http.StatusNoContent)
}

// Reset handles POST /api/reset-password.
func (h *Handler) Reset(c echo.Context) error {
	var raw struct {
		Token    *string `json:"token"`
		Password *string `json:"password"`
	}
	if err := c.Bind(&raw); err != nil || raw.Token == nil || raw.Password == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "token and password are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	req := struct{ Token, Password string }{*raw.Token, *raw.Password}

	// 本家の paramDef は token・password を required の string とするだけで、
	// 空文字も通す。空の token は存在しない token と同じ応答にする (#3845)。
	if req.Token == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid token.", "6382759e-0a0d-4e32-893e-0e1e66cec4d5"))
	}
	resetReq, err := h.resetRepo.FindByToken(req.Token)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid token.", "6382759e-0a0d-4e32-893e-0e1e66cec4d5"))
	}

	// 30分の有効期限チェック (IDからタイムスタンプを算出)
	issuedAt, err := h.idGen.ParseTime(resetReq.ID)
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid token.", "6382759e-0a0d-4e32-893e-0e1e66cec4d5"))
	}

	// Misskey本家: 30分経過で期限切れ
	if time.Since(issuedAt) > 30*time.Minute {
		_ = h.resetRepo.Delete(resetReq.ID)
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Token expired.", "6382759e-0a0d-4e32-893e-0e1e66cec4d5"))
	}

	// **プラグインが管理するアカウント (#3468) のパスワードは作らない。**
	// 受付 (RequestReset) で弾いているので普通は来ないが、受付より前に作られた
	// 要求や手で入れた行で通さない。無効な token と同じ応答にし、要求は消す。
	target, err := h.userRepo.FindByID(resetReq.UserID)
	if err != nil && !repository.IsNotFound(err) {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err == nil && target.IsPluginManaged() {
		_ = h.resetRepo.Delete(resetReq.ID)
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid token.", "6382759e-0a0d-4e32-893e-0e1e66cec4d5"))
	}

	// 空の password と、bcrypt が受け付けない 72 バイトを超える password は拒否する。
	// 本家は空の password をそのままハッシュにし、72 バイトを超えた分は bcrypt が
	// 切り詰めて設定するが、どちらも利用者の意図と違うパスワードになる。token の
	// 照合の後で見るので、本家と同じく無効な token には token の誤りを返す (#3845)。
	if req.Password == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Password is empty.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if len(req.Password) > 72 {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Password too long.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	// cost は misc/password に集約してある。**ここだけ 8 を直書きしていたので、
	// リセットするとハッシュの強度が下がっていた。**
	hashed, err := h.passwordHasher.Hash(req.Password)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// パスワード更新
	pw := hashed
	if err := h.userRepo.UpdateProfile(resetReq.UserID, map[string]any{"password": pw}); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// トークン削除
	_ = h.resetRepo.Delete(resetReq.ID)

	return c.NoContent(http.StatusNoContent)
}
