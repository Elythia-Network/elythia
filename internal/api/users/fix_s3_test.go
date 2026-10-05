package users

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func showStatus(t *testing.T, h *Handler, body string, viewer *model.User) int {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if viewer != nil {
		c.Set(string(middleware.UserContextKey), viewer)
	}
	require.NoError(t, h.Show(c))
	return rec.Code
}

// #2106 S3: ugcVisibilityForVisitor='local' のとき匿名 visitor に remote user を出さない。
func TestShow_UgcVisibilityLocalHidesRemoteFromAnonymous(t *testing.T) {
	h, repo := newTestHandler(t)
	h.SetUGCVisibility("local")
	host := "remote.example"
	repo.Users["remote1"] = &model.User{ID: "remote1", Username: "bob", UsernameLower: "bob", Host: &host, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Profiles["remote1"] = &model.UserProfile{UserID: "remote1", Fields: datatypes.JSON([]byte("[]"))}

	assert.Equal(t, http.StatusNotFound, showStatus(t, h, `{"userId":"remote1"}`, nil),
		"anonymous must not see remote user under ugcVisibility=local")
	assert.Equal(t, http.StatusOK, showStatus(t, h, `{"userId":"remote1"}`, &model.User{ID: "viewer1"}),
		"authenticated viewer can see remote user")
}

// ugcVisibility='all' では匿名でも remote user が見える (gate しない)。
func TestShow_UgcVisibilityAllShowsRemoteToAnonymous(t *testing.T) {
	h, repo := newTestHandler(t)
	h.SetUGCVisibility("all")
	host := "remote.example"
	repo.Users["remote2"] = &model.User{ID: "remote2", Username: "carol", UsernameLower: "carol", Host: &host, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Profiles["remote2"] = &model.UserProfile{UserID: "remote2", Fields: datatypes.JSON([]byte("[]"))}

	assert.Equal(t, http.StatusOK, showStatus(t, h, `{"userId":"remote2"}`, nil),
		"ugcVisibility=all does not gate anonymous remote view")
}
