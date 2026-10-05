package smtp

import (
	"log/slog"

	"github.com/elythia-network/elythia/internal/model"
)

// MetaFetcher is the minimal interface needed to look up the current
// SMTP configuration. Implemented by repository.MetaRepository; declared
// here to avoid a circular import (smtp → repository → model → smtp).
type MetaFetcher interface {
	Fetch() (*model.Meta, error)
}

// SenderFromMeta returns an EmailSender function that re-reads the SMTP
// configuration from metaRepo on every call. Handlers can therefore be
// wired unconditionally at startup, and admin UI edits to SMTP settings
// take effect immediately without a process restart.
//
// Behaviour when meta is unavailable or SMTP is not yet configured:
//   - meta Fetch error → Warn log + skip (= operator が気付ける)
//   - EnableEmail = false / SMTPHost 未設定 / Email 未設定 → Debug log +
//     skip (= 設計どおり未設定状態、noisy にしない)
//
// proxyURL は forward proxy URL (config.proxySmtp) で、SOCKS5 / HTTP
// CONNECT tunnel として SMTP TCP 接続を経路化する場合に渡す。
// meta.SMTPSecure フラグも自動で Options.Secure に反映され、implicit
// TLS / STARTTLS の経路選択が runtime config に追従する (#1111)。
func SenderFromMeta(metaRepo MetaFetcher, proxyURL string) func(to string, msg Message) {
	return func(to string, msg Message) {
		m, err := metaRepo.Fetch()
		if err != nil {
			slog.Warn("email: meta fetch failed, skipping send",
				"err", err, "to", to)
			return
		}
		if !m.EnableEmail || m.SMTPHost == nil || *m.SMTPHost == "" || m.Email == nil || *m.Email == "" {
			slog.Debug("email: SMTP not configured, skipping send", "to", to)
			return
		}
		port := 587
		if m.SMTPPort != nil {
			port = *m.SMTPPort
		}
		SendMessage(*m.SMTPHost, port, m.SMTPUser, m.SMTPPass, *m.Email, to, msg, Options{
			ProxyURL: proxyURL,
			Secure:   m.SMTPSecure,
		})
	}
}

// SubjectBodySenderFromMeta is the (to, subject, body) flavoured wrapper
// around SenderFromMeta, used by handlers whose EmailSender alias takes
// plain subject/body strings instead of a Message (例: admin handler の
// password reset path)。挙動は SenderFromMeta と同じく per-call meta
// 再 Fetch + smtpSecure 反映 + 未設定時 no-op。
func SubjectBodySenderFromMeta(metaRepo MetaFetcher, proxyURL string) func(to, subject, body string) {
	sender := SenderFromMeta(metaRepo, proxyURL)
	return func(to, subject, body string) {
		sender(to, Message{Subject: subject, Text: body})
	}
}
