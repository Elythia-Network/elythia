// Package email provides pluggable email validation for signup and
// update-email flows. Validators (format, MX, verifymail, truemail,
// bannedDomains) are composed by Service based on the active Meta config.
package email

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/elythia-network/elythia/internal/model"
)

// Validation failure reasons — match the original Misskey's
// EmailService.validateEmailForAccount return values.
var (
	ErrFormat     = errors.New("email: invalid format")
	ErrMX         = errors.New("email: no MX record")
	ErrDisposable = errors.New("email: disposable address")
	ErrSMTP       = errors.New("email: smtp validation failed")
	ErrBanned     = errors.New("email: domain is banned")
	ErrNetwork    = errors.New("email: network error")
	ErrBlacklist  = errors.New("email: blacklisted")
)

// Service validates an email address against all enabled checks.
type Service struct {
	bannedDomains []string
	activeCheck   bool
	verifymail    *verifymailClient
	truemail      *truemailClient
}

// NewService builds a Service from the given meta config. Equivalent to
// NewServiceWithClient(meta, nil): falls back to http.DefaultClient for the
// external SaaS calls. Production paths should prefer NewServiceWithClient
// with an SSRF-safe + forward-proxy-aware client (#638).
func NewService(meta *model.Meta) *Service {
	return NewServiceWithClient(meta, nil)
}

// NewServiceWithClient builds a Service that uses client for verifymail /
// truemail outbound calls. Pass nil to fall back to http.DefaultClient.
func NewServiceWithClient(meta *model.Meta, client *http.Client) *Service {
	s := &Service{
		bannedDomains: meta.BannedEmailDomains,
	}

	if meta.EnableVerifymailAPI && meta.VerifymailAuthKey != nil && *meta.VerifymailAuthKey != "" {
		s.verifymail = newVerifymailClient(*meta.VerifymailAuthKey, client)
	} else if meta.EnableTruemailAPI && meta.TruemailInstance != nil && meta.TruemailAuthKey != nil {
		s.truemail = newTruemailClient(*meta.TruemailInstance, *meta.TruemailAuthKey, client)
	} else if meta.EnableActiveEmailValidation {
		s.activeCheck = true
	}

	return s
}

// Validate checks the email address against all enabled validations:
// 1. Format (always)
// 2. Banned domains (always, if list non-empty)
// 3. External service or MX check (if configured)
func (s *Service) Validate(ctx context.Context, email string) error {
	if !ValidateFormat(email) {
		return ErrFormat
	}

	domain := domainOf(email)
	// #2106 L47: upstream は banned 判定を external validation (disposable/mx/smtp) の後段に
	// 置くが、mk-go は意図的に banned を先に短絡する (既知 banned ドメインに対し外部
	// verifymail/truemail/MX への問い合わせを発生させない = 外部漏洩・コスト回避)。caller は
	// reason を一律 UNAVAILABLE に潰すため client から観測できる差は無い。
	if IsBannedDomain(domain, s.bannedDomains) {
		return ErrBanned
	}

	switch {
	case s.verifymail != nil:
		return s.verifymail.verify(ctx, email)
	case s.truemail != nil:
		return s.truemail.verify(ctx, email)
	case s.activeCheck:
		return CheckMX(domain)
	}

	return nil
}

// domainOf extracts the domain part from an email address.
// Assumes format validation has already passed.
func domainOf(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}
