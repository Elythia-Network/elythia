package server

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
)

// apiEndpointNames returns the deduped names of every registered POST /api/*
// route — the endpoint catalog. Names are the route path with the leading
// "/api/" stripped (e.g. "notes/create"). GET-only routes, the no-prefix
// routes, and the "*" catchall are excluded.
//
// `internal/api/endpoints` の Handler に `Lister` として注入され、
// `/api/endpoints` の一覧と `/api/endpoint` (#1695) の既知 / 未知の判定の両方に
// 使われる (#2791 で移設。それまでは router.go の inline closure が直接呼んで
// いた)。**呼ばれるまで評価できない** — 登録時点ではまだ全ルートが生えていない
// ので、注入は closure 越しになる。
func apiEndpointNames(e *echo.Echo) []string {
	names := make([]string, 0)
	seen := make(map[string]struct{})
	for _, r := range e.Router().Routes() {
		if r.Method != http.MethodPost {
			continue
		}
		name := strings.TrimPrefix(r.Path, "/api/")
		if name == r.Path || name == "" || name == "*" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}
