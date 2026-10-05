package server

import (
	"net/http"
	"strings"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/labstack/echo/v4"
)

// OpenAPISpec serves /api.json.
//
// upstream は endpoint ごとの paramDef / res から詳細な OpenAPI を生成するが、
// mk-go は endpoint のスキーマをコード上に保持していないため同等のものは出せ
// ない。ここでは Echo に登録済みのルートから paths だけを組み立てる。
//
// 「どの endpoint が存在するか」は正確に反映されるので、疎通確認や存在確認と
// しては機能する。パラメータやレスポンスの型までは載らないので、本家 api.json
// の代替にはならない (#2345)。
func (s *Server) OpenAPISpec(c echo.Context) error {
	paths := map[string]any{}
	seen := map[string]bool{}
	for _, r := range s.echo.Routes() {
		if !strings.HasPrefix(r.Path, "/api/") {
			continue
		}
		// catchall (/api/*) は endpoint ではないので載せない。
		if strings.HasSuffix(r.Path, "/*") {
			continue
		}
		name := strings.TrimPrefix(r.Path, "/api/")
		if name == "" || seen[r.Method+" "+r.Path] {
			continue
		}
		seen[r.Method+" "+r.Path] = true

		method := strings.ToLower(r.Method)
		entry, _ := paths[name].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
			paths[name] = entry
		}
		entry[method] = map[string]any{
			"summary":     name,
			"operationId": name,
			"tags":        []string{topLevelTag(name)},
			"responses": map[string]any{
				"200": map[string]any{"description": "OK"},
			},
		}
	}

	spec := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"version": config.MisskeyVersion,
			"title":   "Misskey API",
		},
		"externalDocs": map[string]any{
			"description": "Repository",
			"url":         "https://github.com/misskey-dev/misskey",
		},
		"servers": []any{
			map[string]any{"url": strings.TrimSuffix(s.config.URL, "/") + "/api"},
		},
		"paths": paths,
	}

	c.Response().Header().Set("Cache-Control", "public, max-age=600")
	return c.JSON(http.StatusOK, spec)
}

// topLevelTag groups an endpoint under its first path segment so the generated
// document stays browsable (notes/create -> "notes").
func topLevelTag(name string) string {
	if i := strings.Index(name, "/"); i > 0 {
		return name[:i]
	}
	return name
}
