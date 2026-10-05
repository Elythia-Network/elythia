package federation

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getStub drives a handler through a GET request (peers は GET only)。
func getStub(handler func(echo.Context) error) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/instance/peers", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = handler(c)
	return rec
}

func decodePeers(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var hosts []string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &hosts))
	return hosts
}

// 既知の instance の host が配列で返ること。
func TestPeers_ListsKnownHosts(t *testing.T) {
	h, repo := newHandler(t)
	require.NoError(t, repo.Create(&model.Instance{ID: "i1", Host: "beta.example"}))
	require.NoError(t, repo.Create(&model.Instance{ID: "i2", Host: "alpha.example"}))

	rec := getStub(h.Peers)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"alpha.example", "beta.example"}, decodePeers(t, rec))
}

// suspended な instance は除外されること (upstream の
// `where: {suspensionState: 'none'}` 相当)。
func TestPeers_ExcludesSuspended(t *testing.T) {
	h, repo := newHandler(t)
	require.NoError(t, repo.Create(&model.Instance{ID: "i1", Host: "live.example"}))
	require.NoError(t, repo.Create(&model.Instance{
		ID: "i2", Host: "gone.example", SuspensionState: model.SuspensionStateManuallySuspended,
	}))
	require.NoError(t, repo.Create(&model.Instance{
		ID: "i3", Host: "dead.example", SuspensionState: model.SuspensionStateAutoSuspendedForNotResponding,
	}))

	assert.Equal(t, []string{"live.example"}, decodePeers(t, getStub(h.Peers)))
}

// blocked / silenced / isNotResponding では除外しないこと。upstream は
// suspensionState しか見ないので、ここで絞ると統計サイトから見えるピア一覧が
// TS 実装と食い違う。
func TestPeers_KeepsBlockedAndNotRespondingHosts(t *testing.T) {
	h, repo, metaRepo := newHandlerWithMeta(t)
	metaRepo.Meta = &model.Meta{BlockedHosts: []string{"blocked.example"}, SilencedHosts: []string{"silenced.example"}}
	require.NoError(t, repo.Create(&model.Instance{ID: "i1", Host: "blocked.example"}))
	require.NoError(t, repo.Create(&model.Instance{ID: "i2", Host: "silenced.example"}))
	require.NoError(t, repo.Create(&model.Instance{ID: "i3", Host: "down.example", IsNotResponding: true}))

	assert.Equal(t,
		[]string{"blocked.example", "down.example", "silenced.example"},
		decodePeers(t, getStub(h.Peers)))
}

// instance が 1 件も無いときは null ではなく [] を返すこと。
func TestPeers_EmptyReturnsArrayNotNull(t *testing.T) {
	h, _ := newHandler(t)
	rec := getStub(h.Peers)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())
}

// service 未配線 (= 最小構成の unit test) でも panic せず [] を返すこと。
func TestPeers_NilServiceReturnsEmpty(t *testing.T) {
	h := NewHandler(nil)
	rec := getStub(h.Peers)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())
}

// DB error は 500 にすること。
func TestPeers_RepositoryError(t *testing.T) {
	h, repo := newHandler(t)
	repo.ListPeerHostsErr = errors.New("db down")
	assert.Equal(t, http.StatusInternalServerError, getStub(h.Peers).Code)
}
