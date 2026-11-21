package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	echomw "github.com/labstack/echo/v5/middleware"
	"github.com/stretchr/testify/assert"
)

// **アクセスログに credential を出さない。**
//
// request URI は query を含むので、素で出すと `?i=<token>` の形で**有効な token**が
// ログファイルに残る。`redact.URI` でそこだけ伏せている。**この配線を固定する
// テストが長らく 1 つも無く**、`LoggerWithConfig` (SA1019) から
// `RequestLoggerWithConfig` へ移すときに黙って壊せる状態だった。形ではなく
// **出力そのもの**を見るのはそのため。
//
// 変異検証: `LogValuesFunc` の `redact.URI(v.URI)` を素の `v.URI` に戻すと token が
// そのまま出て落ちる。`redact.Placeholder` にすると今度は `limit=10` まで消えて落ちる
// (伏せすぎも検出する)。**書式の golden も置いてある** — `LoggerWithConfig` からの移行で
// 「出力は完全に同じ」と主張している以上、区切り・status・末尾の改行が動いたら落ちる
// 必要がある (レビューで `LogStatus` を外す変異が素通りした)。
func TestAccessLogConfig_RedactsToken(t *testing.T) {
	var buf bytes.Buffer

	e := echo.New()
	e.Use(echomw.RequestLoggerWithConfig(accessLogConfig(&buf)))
	e.GET("/api/notes/timeline", func(c *echo.Context) error {
		return c.NoContent(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/notes/timeline?i=SECRET-TOKEN-VALUE&limit=10", nil)
	e.ServeHTTP(httptest.NewRecorder(), req)

	line := buf.String()
	assert.NotContains(t, line, "SECRET-TOKEN-VALUE",
		"token がアクセスログに残っている。LogValuesFunc の redact.URI が外れた")
	assert.Contains(t, line, "limit=10",
		"秘密でないパラメータまで消している。redact が広すぎる")
	assert.Contains(t, line, "/api/notes/timeline", "パス自体は残すこと")
	assert.Contains(t, line, "GET", "メソッドを残すこと")

	// **1 行の書式を固定する。** 旧 `LoggerWithConfig` の
	// `${time_rfc3339} ${method} ${custom} ${status} ${latency_human}` と同じ並び。
	// latency は実行ごとに変わるので `\S+` で受ける。
	assert.Regexp(t,
		`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(Z|[+-]\d{2}:\d{2}) `+
			`GET /api/notes/timeline\?i=REDACTED&limit=10 200 \S+\n$`,
		line, "アクセスログ 1 行の書式が変わった")
}
