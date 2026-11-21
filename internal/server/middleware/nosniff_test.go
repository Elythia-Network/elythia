package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
)

// serveHeaderMW runs one request through mw and returns the recorder.
func serveHeaderMW(t *testing.T, mw echo.MiddlewareFunc, path string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	e.Use(mw)
	e.Any("/*", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	e.Any("/", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// **除外を設けていないこと自体を固定する** (#2782)。`X-Content-Type-Options` は
// drive のファイル配信とプラグイン proxy にしか付いておらず、SPA shell も API も
// 素通しだった。route ごとに付ける形へ戻すと同じ漏れが再発する。
func TestNoSniff_AppliesEverywhere(t *testing.T) {
	for _, p := range []string{
		"/",                 // SPA shell
		"/@alice",           // user page
		"/api/meta",         // JSON API
		"/files/abc.pdf",    // drive (handler 側でも付ける = 多層防御)
		"/proxy/image.webp", // media proxy
		"/embed/notes/abc",  // iframe に埋め込まれる経路
		"/oauth/authorize",  // OAuth 同意画面
		"/.well-known/nodeinfo",
	} {
		assert.Equal(t, "nosniff", serveHeaderMW(t, NoSniff(), p).Header().Get("X-Content-Type-Options"),
			"path=%s", p)
	}
}
