package test

import (
	"encoding/json"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/labstack/echo/v4"
)

// NewEchoHandler constructs a Handler for the public `/api/test` endpoint.
//
// **`/test` だけは `TestMode` で gate しない。** upstream は本番でも登録して
// おり、本家の backend e2e (`test/e2e/endpoints.ts`) がここを叩いて型変換と
// デフォルト値の挙動を確かめる。このパッケージの他の endpoint (`/reset-db`
// など) とは扱いが違うので、依存を持たない専用のコンストラクタを分けてある。
func NewEchoHandler() *Handler { return &Handler{} }

// Test mirrors upstream `test.ts`, an endpoint whose only job is to echo back
// the parameter-validation behaviour of the API framework.
//
// POST /api/test
func (h *Handler) Test(c echo.Context) error {
	var req struct {
		Required        *bool           `json:"required"`
		String          *string         `json:"string"`
		Default         *string         `json:"default"`
		NullableDefault json.RawMessage `json:"nullableDefault"`
		ID              *string         `json:"id"`
	}
	if err := c.Bind(&req); err != nil || req.Required == nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam())
	}
	// format:'misskey:id' は空文字を許さない。
	if req.ID != nil && *req.ID == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam())
	}
	out := map[string]any{"required": *req.Required}
	if req.String != nil {
		out["string"] = *req.String
	}
	out["default"] = "hello"
	if req.Default != nil {
		out["default"] = *req.Default
	}
	// キー省略なら default、`null` ならそのまま null。
	out["nullableDefault"] = "hello"
	if len(req.NullableDefault) > 0 {
		if string(req.NullableDefault) == "null" {
			out["nullableDefault"] = nil
		} else {
			var v string
			if err := json.Unmarshal(req.NullableDefault, &v); err != nil {
				return c.JSON(http.StatusBadRequest, apierr.InvalidParam())
			}
			out["nullableDefault"] = v
		}
	}
	if req.ID != nil {
		out["id"] = *req.ID
	}
	return c.JSON(http.StatusOK, out)
}
