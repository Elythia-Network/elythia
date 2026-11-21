package server

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
)

func noopHandler(c *echo.Context) error { return c.NoContent(http.StatusOK) }

func TestAPIEndpointNames(t *testing.T) {
	e := echo.New()
	e.POST("/api/notes/create", noopHandler)
	e.POST("/api/meta", noopHandler)
	e.POST("/api/notes/create", noopHandler) // duplicate → dedup
	e.GET("/api/users/show", noopHandler)    // GET → excluded
	e.POST("/health", noopHandler)           // no /api/ prefix → excluded
	e.POST("/api/*", noopHandler)            // catchall → excluded

	names := apiEndpointNames(e)
	assert.Contains(t, names, "notes/create")
	assert.Contains(t, names, "meta")
	assert.NotContains(t, names, "users/show", "GET routes are excluded")
	assert.NotContains(t, names, "/health", "non-/api/ routes are excluded")
	assert.NotContains(t, names, "*", "catchall is excluded")

	// dedup: notes/create は 2 回登録しても 1 つだけ。
	count := 0
	for _, n := range names {
		if n == "notes/create" {
			count++
		}
	}
	assert.Equal(t, 1, count, "duplicate routes are deduped")
}

// `/api/endpoint` の既知 / 未知の判定は `internal/api/endpoints` へ移った
// (#2791)。ここでは判定の材料になる一覧の中身を固定する。
func TestAPIEndpointNames_MembershipMaterial(t *testing.T) {
	e := echo.New()
	e.POST("/api/notes/create", noopHandler)
	e.GET("/api/users/show", noopHandler)

	names := apiEndpointNames(e)

	assert.Contains(t, names, "notes/create")
	assert.NotContains(t, names, "users/show", "GET-only route is not a POST endpoint")
	assert.NotContains(t, names, "nonexistent")
	assert.NotContains(t, names, "")
}
