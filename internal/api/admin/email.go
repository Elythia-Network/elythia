package admin

import (
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
		To      string `json:"to"`
		Subject string `json:"subject"`
		Text    string `json:"text"`
	}
	// upstream send-email.ts は paramDef required:['to','subject','text']。欠落は
	// schema validation 段で 400 になるため、いずれか欠ければ INVALID_PARAM を返す
	// (旧 mk-go は to のみ確認し subject/text 欠落でも 204 だった、#1539)。
	if err := c.Bind(&req); err != nil || req.To == "" || req.Subject == "" || req.Text == "" {
		return apierr.JSONInvalidParam(c)
	}
	if h.metaRepo != nil {
		sender := smtp.SubjectBodySenderFromMeta(h.metaRepo, h.smtpProxyURL)
		go sender(req.To, req.Subject, req.Text)
	}
	return c.NoContent(http.StatusNoContent)
}
