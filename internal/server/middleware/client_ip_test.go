package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordedIP struct {
	userID string
	ip     string
}

type fakeIPObserver struct{ got []recordedIP }

func (f *fakeIPObserver) Record(userID, ip string) {
	f.got = append(f.got, recordedIP{userID, ip})
}

// runRecordClientIP drives the middleware with (or without) an authenticated
// user in the context.
func runRecordClientIP(t *testing.T, observer middleware.IPObserver, user *model.User, remoteAddr string) bool {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	reached := false
	h := middleware.RecordClientIP(observer)(func(echo.Context) error {
		reached = true
		return c.NoContent(http.StatusOK)
	})
	require.NoError(t, h(c))
	return reached
}

func TestRecordClientIP_RecordsAuthenticatedRequests(t *testing.T) {
	obs := &fakeIPObserver{}

	assert.True(t, runRecordClientIP(t, obs, &model.User{ID: "u1"}, "192.0.2.1:41234"))

	require.Len(t, obs.got, 1)
	assert.Equal(t, "u1", obs.got[0].userID)
	// **port は落ちた形で渡る。** echo の RealIP が RemoteAddr を分解する。
	assert.Equal(t, "192.0.2.1", obs.got[0].ip)
}

// 未認証のリクエストでは記録しない。誰の IP か分からない観測を入れても、
// 関連アカウントの材料にならない。
func TestRecordClientIP_SkipsAnonymous(t *testing.T) {
	obs := &fakeIPObserver{}

	assert.True(t, runRecordClientIP(t, obs, nil, "192.0.2.1:41234"))

	assert.Empty(t, obs.got)
}

// observer 未配線でもリクエストは通る。
func TestRecordClientIP_UnwiredStillServes(t *testing.T) {
	assert.True(t, runRecordClientIP(t, nil, &model.User{ID: "u1"}, "192.0.2.1:41234"))
}
