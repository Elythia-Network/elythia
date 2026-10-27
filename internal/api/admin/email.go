package admin

import (
	"context"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/misc/smtp"
	"github.com/labstack/echo/v4"
)

// SendEmail handles POST /api/admin/send-email.
//
// 内部で smtp.SubjectBodySenderFromMeta を経由することで、router.go の他
// 3 経路 (signup / reset / i/update-email) と SMTP 設定読み出しロジック
// (= per-call 再 Fetch + smtpSecure 反映 + 未設定時 no-op) を共有する。
// goroutine 内で送信して呼び出し元に 204 を即返すのは admin UI 既存挙動。
func (h *Handler) SendEmail(c echo.Context) error {
	var req struct {
		To      *string `json:"to"`
		Subject *string `json:"subject"`
		Text    *string `json:"text"`
	}
	// upstream send-email.ts は paramDef required:['to','subject','text']。欠落は
	// schema validation 段で 400 になるため、いずれか欠ければ INVALID_PARAM を返す
	// (旧 mk-go は to のみ確認し subject/text 欠落でも 204 だった、#1539)。
	// 空文字は本家と同じく受ける (minLength が無い、#3861)。
	if err := c.Bind(&req); err != nil || req.To == nil || req.Subject == nil || req.Text == nil {
		return apierr.JSONInvalidParam(c)
	}
	// 宛先が空なら送れる相手がいないので、送らずに受理だけする。本家は、メールが
	// 有効なら送ろうとしてSMTPの段で失敗し、500を返す見込み(無効なら204)。
	// Elythiaは送信を裏で行うので、送信の失敗はもともと応答に出さない。
	if h.metaRepo != nil && *req.To != "" {
		to, subject, text := *req.To, *req.Subject, *req.Text
		sender := smtp.SubjectBodySenderFromMeta(h.metaRepo, h.smtpProxyURL)
		h.background.Go(func(context.Context) { sender(to, subject, text) })
	}
	return c.NoContent(http.StatusNoContent)
}
