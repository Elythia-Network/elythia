package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/server/middleware"
)

// Referrer-Policy は全応答に付く。除外は設けていないので、path によらず同じ値。
func TestReferrerPolicy_AppliedToEveryPath(t *testing.T) {
	paths := []string{
		"/",
		"/notes/abc123",
		"/@alice",
		"/api/notes/create",
		"/files/xyz",
		"/embed/notes/abc123",
		"/inbox",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			e := echo.New()
			h := middleware.ReferrerPolicy()(func(c echo.Context) error {
				return c.NoContent(http.StatusOK)
			})
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodGet, p, nil), rec)

			require.NoError(t, h(c))
			assert.Equal(t, "strict-origin-when-cross-origin", rec.Header().Get("Referrer-Policy"))
		})
	}
}

// handler がエラーを返しても header は書かれている (next 呼び出し前に設定する)。
func TestReferrerPolicy_SetBeforeHandlerRuns(t *testing.T) {
	e := echo.New()
	h := middleware.ReferrerPolicy()(func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusInternalServerError, "boom")
	})
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)

	require.Error(t, h(c))
	assert.Equal(t, "strict-origin-when-cross-origin", rec.Header().Get("Referrer-Policy"))
}

// downgrade (HTTPS→HTTP) で Referer を送らない値であることを固定する。
// no-referrer に強めると連合先からの流入が origin 単位でも追えなくなるため、
// 意図してこの値を選んでいる (referrer_policy.go のコメント参照)。
func TestReferrerPolicy_Value(t *testing.T) {
	e := echo.New()
	h := middleware.ReferrerPolicy()(func(c echo.Context) error { return c.NoContent(http.StatusOK) })
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	require.NoError(t, h(c))

	got := rec.Header().Get("Referrer-Policy")
	assert.NotEqual(t, "no-referrer", got, "origin レベルの流入把握は残す")
	assert.NotEqual(t, "unsafe-url", got, "path を cross-origin へ送らない")
	assert.Equal(t, "strict-origin-when-cross-origin", got)
}
