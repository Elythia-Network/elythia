package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// The user embed is read without signing in, so it must not show the users
// users/show hides from non-moderators.
func TestEmbedUser_HidesSuspendedAndDeletedUsers(t *testing.T) {
	tests := []struct {
		name      string
		user      *model.User
		wantEmbed bool
	}{
		{name: "active", user: &model.User{ID: "u1", Username: "bob"}, wantEmbed: true},
		{name: "suspended", user: &model.User{ID: "u1", Username: "bob", IsSuspended: true}, wantEmbed: false},
		{name: "deleted", user: &model.User{ID: "u1", Username: "bob", IsDeleted: true}, wantEmbed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			userRepo := testutil.NewMockUserRepository()
			userRepo.Users["u1"] = tt.user
			h := newEmbedHandlers(&config.Config{URL: "https://local.example"}, testutil.NewMockMetaRepository(), nil, nil, nil, nil, nil, EmbedDeps{
				UserRepo: userRepo,
				PackUser: func(*echo.Context, *model.User) (any, error) { return map[string]string{"id": "packed-u1"}, nil },
			})
			e := echo.New()
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodGet, "/embed/user-timeline/u1", nil), rec)
			c.SetPathValues(echo.PathValues{{Name: "user", Value: "u1"}})
			require.NoError(t, h.User(c))
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, tt.wantEmbed, strings.Contains(rec.Body.String(), "packed-u1"))
		})
	}
}
