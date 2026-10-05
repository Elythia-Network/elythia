package signup

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/api/apierr"
	coreemail "github.com/elythia-network/elythia/internal/core/email"
	"github.com/elythia-network/elythia/internal/model"
)

// SetEmailAvailabilityDB wires the database POST /api/email-address/available
// queries for duplicate addresses.
//
// **未配線なら重複判定だけを飛ばす** (format / banned domain は効く)。ここで
// 一律 false にすると、DB を渡していないテスト構成でサインアップ画面が
// 何も入力できなくなる。
func (h *Handler) SetEmailAvailabilityDB(db *gorm.DB) { h.emailDB = db }

// EmailAvailable reports whether an email address can be registered.
// POST /api/email-address/available
func (h *Handler) EmailAvailable(c echo.Context) error {
	var req struct {
		EmailAddress string `json:"emailAddress"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam())
	}
	// upstream paramDef は emailAddress に minLength を持たないため、空文字は
	// 400 ではなく format 不正として available:false / reason:"format" を返す。
	available := true
	var reason *string
	setReason := func(r string) {
		available = false
		reason = &r
	}
	switch {
	case !coreemail.ValidateFormat(req.EmailAddress):
		setReason("format")
	default:
		if h.emailAddressInUse(req.EmailAddress) {
			setReason("used")
		} else if h.emailDomainBanned(req.EmailAddress) {
			setReason("banned")
		}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"available": available,
		"reason":    reason,
	})
}

// emailAddressInUse reports whether a *verified* profile already holds the
// address.
//
// **emailVerified=true の行とだけ照合する** (upstream は
// `countBy({emailVerified:true, email})`)。未認証の行まで見ると、他人の
// アドレスを登録途中で放置するだけで本人の登録を妨害できる。
func (h *Handler) emailAddressInUse(addr string) bool {
	if h.emailDB == nil {
		return false
	}
	var count int64
	h.emailDB.Model(&model.UserProfile{}).
		Where(`"email" = ? AND "emailVerified" = ?`, addr, true).
		Count(&count)
	return count > 0
}

// emailDomainBanned reports whether the address' domain is on meta's blocklist.
func (h *Handler) emailDomainBanned(addr string) bool {
	if h.metaRepo == nil {
		return false
	}
	domain := ""
	if at := strings.IndexByte(addr, '@'); at >= 0 {
		domain = strings.ToLower(addr[at+1:])
	}
	m, err := h.metaRepo.Fetch()
	if err != nil || m == nil {
		return false
	}
	return coreemail.IsBannedDomain(domain, m.BannedEmailDomains)
}
