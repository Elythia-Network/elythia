package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
)

// TestHideAppUserWithoutReadEndToEnd drives a real token through the same
// composition as router.go (global Authenticate + /api group
// HideAppUserWithoutRead + route gates) and reports which user each handler
// saw.
//
// read:accountを持たないapp tokenは、kindの無い公開endpoint (notes/show) では
// 匿名として扱われ、kindを持つendpointでは本人として扱われる。
func TestHideAppUserWithoutReadEndToEnd(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()
	appUser := &model.User{ID: "app-user"}
	userRepo.Tokens["native"] = &model.User{ID: "native-user"}
	tokenRepo.Tokens["write-only"] = &model.AccessToken{Token: "write-only", Permission: model.StringArray{"write:notes"}, User: appUser}
	tokenRepo.Tokens["reader"] = &model.AccessToken{Token: "reader", Permission: model.StringArray{"read:account"}, User: appUser}

	e := echo.New()
	e.Use(middleware.NewAuthMiddleware(userRepo, tokenRepo).Authenticate())
	api := e.Group("/api")
	api.Use(middleware.HideAppUserWithoutRead())
	whoami := func(c echo.Context) error {
		if u := middleware.GetUser(c); u != nil {
			return c.String(http.StatusOK, u.ID)
		}
		return c.String(http.StatusOK, "anonymous")
	}
	api.POST("/notes/show", whoami)
	api.POST("/notes/create", whoami, middleware.RequireAuth(), middleware.RequireScope("write:notes"))
	api.POST("/i/revoke-token", whoami, middleware.AdmitAppUser())

	do := func(path, token string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	for _, tt := range []struct {
		name  string
		path  string
		token string
		want  string
	}{
		{"kindless endpoint, write-only app token", "/api/notes/show", "write-only", "anonymous"},
		{"kindless endpoint, read:account app token", "/api/notes/show", "reader", "app-user"},
		{"kindless endpoint, native token", "/api/notes/show", "native", "native-user"},
		{"endpoint with the held kind", "/api/notes/create", "write-only", "app-user"},
		{"revoke-token admits the app user", "/api/i/revoke-token", "write-only", "app-user"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			code, body := do(tt.path, tt.token)
			assert.Equal(t, http.StatusOK, code)
			assert.Equal(t, tt.want, body)
		})
	}
}
