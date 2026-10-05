package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **プロセス内 API 呼び出しに印が付いていること。**
//
// `pluginCaller.Call` は `RemoteAddr` に `127.0.0.1:0` を置く (IP を見る
// middleware が解釈に困らないようにするため) が、**その IP は実在しない**。
// 印が無いと利用者の `user_ip` に `127.0.0.1` が入って関連アカウント検索の
// 材料が汚れ、レート制限の IP バケットも全利用者で共有される。
//
// 述語 (`MarkInternalCall` / `IsInternalCall`) のテストは別にあるが、
// **唯一の呼び出し側が検証されていなかった** — 呼び出しを外す変異を入れても
// `internal/server` は緑のままだった。
func TestPluginCaller_MarksInternalCall(t *testing.T) {
	e := echo.New()
	var seen []bool
	e.POST("/api/probe", func(c echo.Context) error {
		seen = append(seen, middleware.IsInternalCall(c.Request().Context()))
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})

	caller := (&pluginAPI{echo: e, host: "example.test"}).Anonymous()
	out, err := caller.Call(context.Background(), "probe", map[string]any{})
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	require.Equal(t, true, got["ok"])

	require.Len(t, seen, 1)
	assert.True(t, seen[0],
		"プロセス内 API 呼び出しに印が付いていない (user_ip とレート制限の IP バケットが汚れる)")
}

// **外から来たリクエストには印が付かないこと** (上が「常に真」な実装でも緑に
// ならないようにする)。
func TestPluginCaller_OrdinaryRequestIsNotMarked(t *testing.T) {
	e := echo.New()
	var seen []bool
	e.POST("/api/probe", func(c echo.Context) error {
		seen = append(seen, middleware.IsInternalCall(c.Request().Context()))
		return c.NoContent(http.StatusNoContent)
	})

	req, err := http.NewRequest(http.MethodPost, "/api/probe", nil)
	require.NoError(t, err)
	req.RemoteAddr = "203.0.113.5:1234"
	e.ServeHTTP(httptest.NewRecorder(), req)

	require.Len(t, seen, 1)
	assert.False(t, seen[0], "外から来たリクエストに印が付いている")
}
