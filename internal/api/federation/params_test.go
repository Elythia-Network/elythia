package federation

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedRemoteUsers(h *Handler, host string, ids ...string) {
	userRepo := testutil.NewMockUserRepository()
	for _, id := range ids {
		userRepo.Users[id] = &model.User{ID: id, Username: id, Host: &host}
	}
	h.SetUserRepo(userRepo)
}

func decodeRows(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	return rows
}

// 本家の paramDef は空の host を通し、host = ” に当たる行は無いので空の一覧を返す (#3770)。
func TestHostListings_EmptyHostReturnsEmptyList(t *testing.T) {
	h, _ := newHandler(t)
	seedRemoteUsers(h, "remote.example", "r1", "r2")
	followRepo := testutil.NewMockFollowingRepository()
	remote := "remote.example"
	followRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "local1", FolloweeID: "r1", FolloweeHost: &remote}
	followRepo.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "r2", FollowerHost: &remote, FolloweeID: "local2"}
	h.SetFollowingRepo(followRepo)

	for name, handler := range map[string]func(echo.Context) error{
		"followers": h.Followers,
		"following": h.Following,
		"users":     h.Users,
	} {
		t.Run(name, func(t *testing.T) {
			assert.Empty(t, decodeRows(t, postBody(handler, `{"host":""}`)))
		})
	}
}

// 本家の paramDef は整数の値を持つ小数 (10.0、1e1) を integer として通す (#3770)。
func TestHostListings_AcceptIntegerValuedFractions(t *testing.T) {
	h, _ := newHandler(t)
	seedRemoteUsers(h, "remote.example", "r1")
	h.SetFollowingRepo(testutil.NewMockFollowingRepository())

	for name, handler := range map[string]func(echo.Context) error{
		"followers": h.Followers,
		"following": h.Following,
		"users":     h.Users,
	} {
		for _, body := range []string{
			`{"host":"remote.example","limit":10.0}`,
			`{"host":"remote.example","limit":1e1}`,
			`{"host":"remote.example","sinceDate":1e1}`,
			`{"host":"remote.example","untilDate":4102444800000.0}`,
		} {
			t.Run(name+" "+body, func(t *testing.T) {
				decodeRows(t, postBody(handler, body))
			})
		}
	}
}

// offset は本家の paramDef に無く、本家は読まない (#3770)。
func TestUsers_IgnoresOffset(t *testing.T) {
	h, _ := newHandler(t)
	seedRemoteUsers(h, "remote.example", "r1", "r2")
	assert.Len(t, decodeRows(t, postBody(h.Users, `{"host":"remote.example","offset":1}`)), 2)
}

func TestStats_AcceptsIntegerValuedFractions(t *testing.T) {
	h, _ := newHandler(t)
	for _, body := range []string{`{"limit":10.0}`, `{"limit":1e1}`} {
		rec := postBody(h.Stats, body)
		assert.Equal(t, http.StatusOK, rec.Code, body)
	}

	// admin/overview は GET で叩く。本家は query を JSON.parse するので 10.0 も 10 になる。
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/federation/stats?limit=10.0", nil)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Stats(e.NewContext(req, rec)))
	assert.Equal(t, http.StatusOK, rec.Code)
}
