package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
)

func TestRewriteCharset(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{name: "html", in: "text/html; charset=UTF-8", want: "text/html; charset=utf-8", ok: true},
		{name: "json", in: "application/json; charset=UTF-8", want: "application/json; charset=utf-8", ok: true},
		{name: "already lower", in: "text/html; charset=utf-8", want: "text/html; charset=utf-8", ok: false},
		{name: "no charset", in: "image/png", want: "image/png", ok: false},
		{name: "empty", in: "", want: "", ok: false},
		// 他の charset は触らない。
		{name: "other charset", in: "text/plain; charset=Shift_JIS", want: "text/plain; charset=Shift_JIS", ok: false},
		// charset の後ろにパラメータが続く形でも壊さない。
		{
			name: "trailing param",
			in:   `application/ld+json; charset=UTF-8; profile="https://www.w3.org/ns/activitystreams"`,
			want: `application/ld+json; charset=utf-8; profile="https://www.w3.org/ns/activitystreams"`,
			ok:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := rewriteCharset(tt.in)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.ok, ok)
		})
	}
}

// middleware として通したときに実際のヘッダが書き換わること。
func TestLowercaseCharset_Middleware(t *testing.T) {
	e := echo.New()
	h := lowercaseCharset(func(c *echo.Context) error {
		return c.HTML(http.StatusOK, "<html></html>")
	})

	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	if err := h(c); err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get(echo.HeaderContentType))
}

// charset を持たない応答はそのまま通す。
func TestLowercaseCharset_LeavesOtherTypes(t *testing.T) {
	e := echo.New()
	h := lowercaseCharset(func(c *echo.Context) error {
		return c.Blob(http.StatusOK, "image/png", []byte{0x89, 'P', 'N', 'G'})
	})

	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	if err := h(c); err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, "image/png", rec.Header().Get(echo.HeaderContentType))
}
