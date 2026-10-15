package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/model"
)

func TestAPICacheControl(t *testing.T) {
	const privateDefault = "private, max-age=0, must-revalidate"
	tests := []struct {
		name   string
		method string
		path   string
		status int
		user   bool
		token  bool
		skip   bool
		want   string
	}{
		{name: "anonymous GET to a cacheSec endpoint is public", method: http.MethodGet, path: "/emojis", status: http.StatusOK, want: "public, max-age=3600"},
		{name: "uses the endpoint's own seconds", method: http.MethodGet, path: "/server-info", status: http.StatusOK, want: "public, max-age=60"},
		{name: "nested endpoint name", method: http.MethodGet, path: "/charts/user/notes", status: http.StatusOK, want: "public, max-age=3600"},
		{name: "204 is a success too", method: http.MethodGet, path: "/retention", status: http.StatusNoContent, want: "public, max-age=3600"},
		{name: "POST keeps the private default", method: http.MethodPost, path: "/emojis", status: http.StatusOK, want: privateDefault},
		{name: "signed-in GET keeps the private default", method: http.MethodGet, path: "/emojis", status: http.StatusOK, user: true, want: privateDefault},
		{name: "GET with an unresolved token keeps the private default", method: http.MethodGet, path: "/emojis", status: http.StatusOK, token: true, want: privateDefault},
		{name: "error response keeps the private default", method: http.MethodGet, path: "/emoji", status: http.StatusBadRequest, want: privateDefault},
		{name: "handler fallback marked SkipPublicCache keeps the private default", method: http.MethodGet, path: "/emojis", status: http.StatusOK, skip: true, want: privateDefault},
		{name: "endpoint without cacheSec keeps the private default", method: http.MethodGet, path: "/meta", status: http.StatusOK, want: privateDefault},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			g := e.Group("/api")
			g.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
				return func(c echo.Context) error {
					c.Response().Header().Set(echo.HeaderCacheControl, privateDefault)
					if tt.user {
						c.Set(string(UserContextKey), &model.User{ID: "u1"})
					}
					if tt.token {
						c.Set(string(rawTokenPresentContextKey), true)
					}
					return next(c)
				}
			})
			g.Use(APICacheControl())
			h := func(c echo.Context) error {
				if tt.skip {
					SkipPublicCache(c)
				}
				if tt.status == http.StatusNoContent {
					return c.NoContent(tt.status)
				}
				return c.JSON(tt.status, map[string]any{})
			}
			g.GET(tt.path, h)
			g.POST(tt.path, h)

			req := httptest.NewRequest(tt.method, "/api"+tt.path, nil)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			assert.Equal(t, tt.status, rec.Code)
			assert.Equal(t, tt.want, rec.Header().Get(echo.HeaderCacheControl))
		})
	}
}
