package resetpassword

import (
	"fmt"
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
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
	userRepo  repository.UserRepository
	resetRepo repository.PasswordResetRequestRepository
	metaRepo  repository.MetaRepository
	idGen     id.Generator
	email     EmailSender
	serverURL string
}

// NewHandler creates a new password reset Handler.
func NewHandler(userRepo repository.UserRepository, resetRepo repository.PasswordResetRequestRepository, idGen id.Generator) *Handler {
	return &Handler{userRepo: userRepo, resetRepo: resetRepo, idGen: idGen}
}

// SetMetaRepo wires meta lookup for email locale fallback.
func (h *Handler) SetMetaRepo(r repository.MetaRepository) { h.metaRepo = r }

// HasMetaRepo reports whether the meta repository was wired for email l10n.
func (h *Handler) HasMetaRepo() bool { return h.metaRepo != nil }

// SetEmailSender attaches an EmailSender for sending reset emails.
func (h *Handler) SetEmailSender(s EmailSender) { h.email = s }

// SetServerURL sets the base URL for reset links.
func (h *Handler) SetServerURL(u string) { h.serverURL = u }

// RequestReset handles POST /api/request-reset-password.
// ユーザーが見つからない/email不一致/未認証でも成功レスポンスを返す（情報漏洩防止）。
func (h *Handler) RequestReset(c echo.Context) error {
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
	}
	if err := c.Bind(&req); err != nil || req.Username == "" || req.Email == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "username and email are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

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
		go h.email(*profile.Email, miscsmtp.Message{
			Subject: subject,
			Text:    text,
			HTML:    html,
		})
	}

	return c.NoContent(http.StatusNoContent)
}

// Reset handles POST /api/reset-password.
func (h *Handler) Reset(c echo.Context) error {
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := c.Bind(&req); err != nil || req.Token == "" || req.Password == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "token and password are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	// bcryptは72バイトまでしか受け付けない
	if len(req.Password) > 72 {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Password too long.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
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

	// cost は misc/password に集約してある。**ここだけ 8 を直書きしていたので、
	// リセットするとハッシュの強度が下がっていた。**
	hashed, err := password.Hash(req.Password)
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
