package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
)

func runSecurityHeader(t *testing.T, mw echo.MiddlewareFunc) http.Header {
	t.Helper()
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	h := mw(func(c *echo.Context) error { return c.String(http.StatusOK, "ok") })
	assert.NoError(t, h(c))
	return rec.Header()
}

// upstream ServerService と同じ値・同じ条件で出すこと。
func TestHSTS(t *testing.T) {
	for name, tc := range map[string]struct {
		url      string
		disabled bool
		want     string
	}{
		"https なら出す":          {"https://example.com", false, hstsValue},
		"大文字の scheme も見る":     {"HTTPS://example.com", false, hstsValue},
		"disableHsts で出さない":   {"https://example.com", true, ""},
		"http では出さない":         {"http://example.com", false, ""},
		"http は disable でも同じ": {"http://example.com", true, ""},
		"空 URL では出さない":        {"", false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := runSecurityHeader(t, HSTS(tc.url, tc.disabled)).Get("Strict-Transport-Security")
			assert.Equal(t, tc.want, got)
		})
	}
}

// **平文構成に付けてはいけない。** 付くとブラウザが以後その host を https でしか
// 開かなくなり、設定を戻しても max-age の間は復旧できない。
func TestHSTS_NeverOnPlainHTTP(t *testing.T) {
	for _, url := range []string{"http://example.com", "ftp://example.com", "example.com", "//example.com"} {
		got := runSecurityHeader(t, HSTS(url, false)).Get("Strict-Transport-Security")
		assert.Empty(t, got, url)
	}
}

// upstream に合わせて includeSubDomains は付けない。
func TestHSTS_ValueMatchesUpstream(t *testing.T) {
	assert.Equal(t, "max-age=15552000; preload", hstsValue)
	assert.NotContains(t, hstsValue, "includeSubDomains")
}

func TestCOOP(t *testing.T) {
	for name, tc := range map[string]struct {
		mode string
		want string
	}{
		"既定は出さない":      {"", ""},
		"off は出さない":    {COOPOff, ""},
		"allow-popups": {COOPSameOriginAllowPopups, "same-origin-allow-popups"},
		"same-origin":  {COOPSameOrigin, "same-origin"},
		// **知らない値は出さない。** 適当な値をそのまま流すと、ブラウザが
		// 解釈できない header を付けることになる。
		"未知の値は出さない": {"unsafe-none", ""},
		"でたらめは出さない": {"yes", ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := runSecurityHeader(t, COOP(tc.mode)).Get("Cross-Origin-Opener-Policy")
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestValidCOOPMode(t *testing.T) {
	for _, ok := range []string{"", COOPOff, COOPSameOriginAllowPopups, COOPSameOrigin} {
		assert.True(t, ValidCOOPMode(ok), ok)
	}
	for _, ng := range []string{"unsafe-none", "same_origin", "SAME-ORIGIN", "on"} {
		assert.False(t, ValidCOOPMode(ng), ng)
	}
}
