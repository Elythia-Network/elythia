package users

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
)

// resyncStubResolver is a RemoteUserResolver that also re-syncs stored users.
type resyncStubResolver struct {
	stubRemoteResolver
	resyncErr error
	resynced  int
}

func (r *resyncStubResolver) ResyncIfStale(u *model.User) (*model.User, error) {
	r.resynced++
	if r.resyncErr != nil {
		return nil, r.resyncErr
	}
	return u, nil
}

func postJSON(t *testing.T, path, body string, handle func(echo.Context) error) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, handle(e.NewContext(req, rec)))
	return rec
}

// users/show with a host re-syncs a stored remote user like upstream
// resolveUser, and a failed re-sync is FAILED_TO_RESOLVE_REMOTE_USER.
// users/followers answers from the DB only (upstream followers.ts) and does
// not re-sync.
func TestShow_ByUsernameWithHost_ResyncsStoredRemoteUser(t *testing.T) {
	rs := &resyncStubResolver{resyncErr: errors.New("webfinger failed")}
	h, userRepo := newTestHandlerWithRemoteResolver(t, rs)
	host := "remote.example"
	userRepo.Users["uR"] = &model.User{
		ID: "uR", Username: "remote", UsernameLower: "remote", Host: &host,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	rec := postJSON(t, "/api/users/show", `{"username":"remote","host":"remote.example"}`, h.Show)
	assert.Equal(t, 1, rs.resynced)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj, _ := resp["error"].(map[string]any)
	assert.Equal(t, "FAILED_TO_RESOLVE_REMOTE_USER", errObj["code"])

	rs.resyncErr = nil
	rec = postJSON(t, "/api/users/show", `{"username":"remote","host":"remote.example"}`, h.Show)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 2, rs.resynced)

	_ = postJSON(t, "/api/users/followers", `{"username":"remote","host":"remote.example"}`, h.Followers)
	assert.Equal(t, 2, rs.resynced, "followers は再同期しない")
}
