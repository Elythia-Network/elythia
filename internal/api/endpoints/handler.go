// Package endpoints serves the API catalog endpoints (`endpoints` / `endpoint`).
//
// upstream の `endpoints.ts` / `endpoint.ts` に対応する。どちらも「この
// インスタンスがどの endpoint を持つか」を返す introspection なので、
// **ルータが自分の登録内容を教える**必要がある。
package endpoints

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
)

// Lister returns the names of every registered POST /api/* endpoint
// ("notes/create" のように `/api/` を落とした形)。
//
// **`*echo.Echo` そのものは受け取らない。** api 層がルータの実体を持つと
// 依存が逆向きになるので、必要な情報 (名前の一覧) だけを関数で注入する。
type Lister func() []string

// Handler serves the API catalog endpoints.
type Handler struct {
	list Lister
}

// NewHandler creates a Handler backed by the given endpoint lister.
func NewHandler(list Lister) *Handler { return &Handler{list: list} }

// names returns the catalog, tolerating an unwired lister.
//
// **nil でも空配列を返す。** upstream は必ず配列を返すので、null になると
// クライアント側の `.includes()` が落ちる。
func (h *Handler) names() []string {
	if h.list == nil {
		return []string{}
	}
	return h.list()
}

// Endpoints returns every registered endpoint name.
// POST /api/endpoints
func (h *Handler) Endpoints(c echo.Context) error {
	return c.JSON(http.StatusOK, h.names())
}

// Endpoint returns metadata for one endpoint, or null when it is unknown.
//
// POST /api/endpoint
//
// mk-go は per-endpoint の paramDef メタデータを保持しないため、param の
// 名前と型までは導出できない。response の shape (params は配列) と未知
// endpoint の null だけ upstream に揃える (#1695)。
func (h *Handler) Endpoint(c echo.Context) error {
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := c.Bind(&req); err != nil || req.Endpoint == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam())
	}
	// 未知 endpoint は null (upstream: ep == null → return null)。
	for _, n := range h.names() {
		if n == req.Endpoint {
			return c.JSON(http.StatusOK, map[string]any{"params": []any{}})
		}
	}
	return c.JSON(http.StatusOK, nil)
}
