package federation

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func newHandler(t *testing.T) (*Handler, *testutil.MockInstanceRepository) {
	h, repo, _ := newHandlerWithMeta(t)
	return h, repo
}

// newHandlerWithMeta is like newHandler but also exposes the MockMetaRepository
// so tests can configure blockedHosts / silencedHosts / mediaSilencedHosts.
func newHandlerWithMeta(t *testing.T) (*Handler, *testutil.MockInstanceRepository, *testutil.MockMetaRepository) {
	t.Helper()
	repo := testutil.NewMockInstanceRepository()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreinstance.NewService(repo, metaRepo, idGen)
	return NewHandler(svc), repo, metaRepo
}

func newReq(t *testing.T, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

// postBody invokes a handler with the given JSON body and returns the recorded
// response. Used by the per-handler test files (host_listings_test.go,
// stats_test.go, update_remote_user_test.go).
func postBody(handler func(echo.Context) error, body string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = handler(c)
	return rec
}

// postStub is a shorthand for postBody with an empty JSON body.
func postStub(handler func(echo.Context) error) *httptest.ResponseRecorder {
	return postBody(handler, `{}`)
}

// postBodyAs is postBody with an authenticated viewer set in the context, used
// to exercise viewer-relative relation packing (#1957-a).
func postBodyAs(handler func(echo.Context) error, body, viewerID string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), &model.User{ID: viewerID})
	_ = handler(c)
	return rec
}

func seedInstance(t *testing.T, repo *testutil.MockInstanceRepository, host string) *model.Instance {
	t.Helper()
	inst := &model.Instance{
		ID:               "i_" + host,
		Host:             host,
		FirstRetrievedAt: time.Now(),
		SuspensionState:  model.SuspensionStateNone,
	}
	repo.Instances[host] = inst
	return inst
}

func TestInstances_Empty(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Instances(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1948-10: instance の firstRetrievedAt / infoUpdatedAt / latestRequestReceivedAt
// は upstream toISOString() (.000Z / null)。raw time.Time の RFC3339Nano だと乖離。
func TestShowInstance_DateFormat(t *testing.T) {
	h, repo := newHandler(t)
	inst := seedInstance(t, repo, "alpha.example")
	inst.FirstRetrievedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	infoUpdated := time.Date(2026, 6, 21, 1, 2, 3, 456_000_000, time.UTC)
	inst.InfoUpdatedAt = &infoUpdated
	// latestRequestReceivedAt は nil のまま → null。

	c, rec := newReq(t, `{"host":"alpha.example"}`)
	require.NoError(t, h.ShowInstance(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "2026-01-02T03:04:05.000Z", resp["firstRetrievedAt"], "firstRetrievedAt は .000Z (#1948-10)")
	assert.Equal(t, "2026-06-21T01:02:03.456Z", resp["infoUpdatedAt"], "infoUpdatedAt は .000Z (#1948-10)")
	assert.Nil(t, resp["latestRequestReceivedAt"], "nil は null (#1948-10)")
}

// fakeModerator implements ModeratorChecker for the moderationNote gate tests.
type fakeModerator struct{ ids map[string]bool }

func (f fakeModerator) IsModerator(userID string) bool { return f.ids[userID] }

// moderationNote は公開エンドポイントなので未認証 (non-moderator) には null。
func TestShowInstance_ModerationNoteHiddenFromPublic(t *testing.T) {
	h, repo := newHandler(t)
	inst := seedInstance(t, repo, "alpha.example")
	inst.ModerationNote = "secret moderator memo"

	c, rec := newReq(t, `{"host":"alpha.example"}`)
	require.NoError(t, h.ShowInstance(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp["moderationNote"], "moderationNote must be null for non-moderator")
	assert.NotContains(t, rec.Body.String(), "secret moderator memo")
}

// moderator として認証されていれば moderationNote の実値が返る。
func TestShowInstance_ModerationNoteVisibleToModerator(t *testing.T) {
	h, repo := newHandler(t)
	h.SetModeratorChecker(fakeModerator{ids: map[string]bool{"mod1": true}})
	inst := seedInstance(t, repo, "alpha.example")
	inst.ModerationNote = "secret moderator memo"

	c, rec := newReq(t, `{"host":"alpha.example"}`)
	c.Set(string(middleware.UserContextKey), &model.User{ID: "mod1"})
	require.NoError(t, h.ShowInstance(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "secret moderator memo", resp["moderationNote"])
}

// softwareSuspended: meta.deliverSuspendedSoftware に該当する software の
// instance は isSuspended=true、suspensionState='softwareSuspended' を返す (#1732)。
func TestShowInstance_SoftwareSuspended(t *testing.T) {
	h, repo, metaRepo := newHandlerWithMeta(t)
	metaRepo.Meta = &model.Meta{
		DeliverSuspendedSoftware: datatypes.JSON([]byte(`[{"software":"mastodon","versionRange":"*"}]`)),
	}
	inst := seedInstance(t, repo, "masto.example")
	name := "mastodon"
	ver := "4.2.0"
	inst.SoftwareName = &name
	inst.SoftwareVersion = &ver

	c, rec := newReq(t, `{"host":"masto.example"}`)
	require.NoError(t, h.ShowInstance(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["isSuspended"])
	assert.Equal(t, "softwareSuspended", resp["suspensionState"])
}

// 該当しない software の instance は従来どおり isSuspended=false / state=none。
func TestShowInstance_SoftwareNotSuspended(t *testing.T) {
	h, repo, metaRepo := newHandlerWithMeta(t)
	metaRepo.Meta = &model.Meta{
		DeliverSuspendedSoftware: datatypes.JSON([]byte(`[{"software":"mastodon","versionRange":"*"}]`)),
	}
	inst := seedInstance(t, repo, "mk.example")
	name := "misskey"
	inst.SoftwareName = &name

	c, rec := newReq(t, `{"host":"mk.example"}`)
	require.NoError(t, h.ShowInstance(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["isSuspended"])
	assert.Equal(t, "none", resp["suspensionState"])
}

// 手動 suspend 済みの instance は softwareSuspended に関わらず元の state を保つ。
func TestShowInstance_ManualSuspensionStatePreserved(t *testing.T) {
	h, repo, metaRepo := newHandlerWithMeta(t)
	metaRepo.Meta = &model.Meta{
		DeliverSuspendedSoftware: datatypes.JSON([]byte(`[{"software":"mastodon","versionRange":"*"}]`)),
	}
	inst := seedInstance(t, repo, "masto2.example")
	name := "mastodon"
	inst.SoftwareName = &name
	inst.SuspensionState = model.SuspensionStateManuallySuspended

	c, rec := newReq(t, `{"host":"masto2.example"}`)
	require.NoError(t, h.ShowInstance(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["isSuspended"])
	// none ではないので 'softwareSuspended' で上書きせず元の状態を返す。
	assert.Equal(t, "manuallySuspended", resp["suspensionState"])
}

// 認証済みでも moderator でなければ moderationNote は隠す。
func TestInstances_ModerationNoteHiddenFromNonModerator(t *testing.T) {
	h, repo := newHandler(t)
	h.SetModeratorChecker(fakeModerator{ids: map[string]bool{"mod1": true}})
	inst := seedInstance(t, repo, "alpha.example")
	inst.ModerationNote = "secret memo"

	c, rec := newReq(t, `{"host":"alpha"}`)
	c.Set(string(middleware.UserContextKey), &model.User{ID: "regular"})
	require.NoError(t, h.Instances(c))
	assert.NotContains(t, rec.Body.String(), "secret memo")
}

// Instances (複数行) でも moderator には moderationNote が実値で返る。
func TestInstances_ModerationNoteVisibleToModerator(t *testing.T) {
	h, repo := newHandler(t)
	h.SetModeratorChecker(fakeModerator{ids: map[string]bool{"mod1": true}})
	inst := seedInstance(t, repo, "alpha.example")
	inst.ModerationNote = "visible memo"

	c, rec := newReq(t, `{"host":"alpha"}`)
	c.Set(string(middleware.UserContextKey), &model.User{ID: "mod1"})
	require.NoError(t, h.Instances(c))
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp)
	assert.Equal(t, "visible memo", resp[0]["moderationNote"])
}

// SetModeratorChecker 未配線 (moderator==nil) なら認証済みでも fail-closed で hidden。
func TestShowInstance_ModerationNoteHiddenWhenCheckerUnwired(t *testing.T) {
	h, repo := newHandler(t) // SetModeratorChecker を呼ばない
	inst := seedInstance(t, repo, "alpha.example")
	inst.ModerationNote = "secret memo"

	c, rec := newReq(t, `{"host":"alpha.example"}`)
	c.Set(string(middleware.UserContextKey), &model.User{ID: "whoever"})
	require.NoError(t, h.ShowInstance(c))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp["moderationNote"])
}

func TestInstances_Filtered(t *testing.T) {
	h, repo := newHandler(t)
	seedInstance(t, repo, "alpha.example")
	seedInstance(t, repo, "beta.example")
	c, rec := newReq(t, `{"host":"alpha"}`)
	require.NoError(t, h.Instances(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "alpha.example")
	assert.NotContains(t, rec.Body.String(), "beta.example")
}

// TestInstances_BlockedFilter は federation 画面の「ブロック」タブのバグ
// (blocked/silenced フィルタが無視され全件返っていた) の regression guard。
// blocked:true は meta.blockedHosts に exact 一致する host だけ、blocked:false
// はそれ以外を返すこと。
func TestInstances_BlockedFilter(t *testing.T) {
	h, repo, metaRepo := newHandlerWithMeta(t)
	seedInstance(t, repo, "blocked.example")
	seedInstance(t, repo, "normal.example")
	metaRepo.Meta = &model.Meta{BlockedHosts: model.StringArray{"blocked.example"}}

	c, rec := newReq(t, `{"blocked":true}`)
	require.NoError(t, h.Instances(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "blocked.example")
	assert.NotContains(t, rec.Body.String(), "normal.example")

	c, rec = newReq(t, `{"blocked":false}`)
	require.NoError(t, h.Instances(c))
	assert.NotContains(t, rec.Body.String(), "blocked.example")
	assert.Contains(t, rec.Body.String(), "normal.example")
}

// TestInstances_BlockedEmptyList: blockedHosts が空のとき blocked:true は 0 件
// (本家の 1=0 相当)、blocked:false は全件返ること。
func TestInstances_BlockedEmptyList(t *testing.T) {
	h, repo, _ := newHandlerWithMeta(t)
	seedInstance(t, repo, "normal.example")

	c, rec := newReq(t, `{"blocked":true}`)
	require.NoError(t, h.Instances(c))
	assert.NotContains(t, rec.Body.String(), "normal.example")

	c, rec = newReq(t, `{"blocked":false}`)
	require.NoError(t, h.Instances(c))
	assert.Contains(t, rec.Body.String(), "normal.example")
}

// TestInstances_SilencedFilter は silenced タブの同等 regression guard。
func TestInstances_SilencedFilter(t *testing.T) {
	h, repo, metaRepo := newHandlerWithMeta(t)
	seedInstance(t, repo, "silenced.example")
	seedInstance(t, repo, "normal.example")
	metaRepo.Meta = &model.Meta{SilencedHosts: model.StringArray{"silenced.example"}}

	c, rec := newReq(t, `{"silenced":true}`)
	require.NoError(t, h.Instances(c))
	assert.Contains(t, rec.Body.String(), "silenced.example")
	assert.NotContains(t, rec.Body.String(), "normal.example")

	c, rec = newReq(t, `{"silenced":false}`)
	require.NoError(t, h.Instances(c))
	assert.NotContains(t, rec.Body.String(), "silenced.example")
	assert.Contains(t, rec.Body.String(), "normal.example")
}

// TestInstances_StatusFields verifies isBlocked / isSilenced / isMediaSilenced
// が meta との suffix-match で算出されレスポンスに載ること。サブドメインも
// suffix-match で拾う (本家 InstanceEntityService と同じ)。
func TestInstances_StatusFields(t *testing.T) {
	h, repo, metaRepo := newHandlerWithMeta(t)
	seedInstance(t, repo, "sub.blocked.example")
	metaRepo.Meta = &model.Meta{
		BlockedHosts:       model.StringArray{"blocked.example"},
		SilencedHosts:      model.StringArray{"sub.blocked.example"},
		MediaSilencedHosts: model.StringArray{"other.example"},
	}

	c, rec := newReq(t, `{"host":"sub.blocked.example"}`)
	require.NoError(t, h.Instances(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, true, got[0]["isBlocked"], "subdomain of blockedHosts entry is blocked")
	assert.Equal(t, true, got[0]["isSilenced"])
	assert.Equal(t, false, got[0]["isMediaSilenced"])
}

// TestInstances_MetaError: meta が読めないとき blocked/silenced 判定が
// できないので 500 を返す (本家 TS も metaService.fetch throw で 500)。
func TestInstances_MetaError(t *testing.T) {
	h, repo, metaRepo := newHandlerWithMeta(t)
	seedInstance(t, repo, "normal.example")
	metaRepo.Meta = nil
	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Instances(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestInstances_BadJSON(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, `{not json`)
	require.NoError(t, h.Instances(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingInstanceRepo causes List to fail.
type failingInstanceRepo struct {
	*testutil.MockInstanceRepository
}

func (f *failingInstanceRepo) List(_ model.InstanceListFilter) ([]*model.Instance, error) {
	return nil, errors.New("boom")
}

func TestInstances_RepoError(t *testing.T) {
	mock := testutil.NewMockInstanceRepository()
	repo := &failingInstanceRepo{MockInstanceRepository: mock}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreinstance.NewService(repo, metaRepo, idGen)
	h := NewHandler(svc)
	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Instances(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestShowInstance_Success(t *testing.T) {
	h, repo := newHandler(t)
	seedInstance(t, repo, "alpha.example")
	c, rec := newReq(t, `{"host":"alpha.example"}`)
	require.NoError(t, h.ShowInstance(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "alpha.example")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	shapetest.Assert(t, "FederationInstance", resp) // L3 (#1284)
}

func TestShowInstance_BadJSON(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	require.NoError(t, h.ShowInstance(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShowInstance_EmptyHost(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, `{}`)
	require.NoError(t, h.ShowInstance(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestShowInstance_NotFound: upstream Misskey TS と同じく未知 host は
// 204 No Content (= null 相当) を返す。旧実装は 404 + error body だったが、
// drop-in 互換のため 204 に揃えた (#915)。
func TestShowInstance_NotFound(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, `{"host":"missing.example"}`)
	require.NoError(t, h.ShowInstance(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String(), "204 response should have empty body")
}

// TestShowInstance_DBError は #915 review fix の regression guard。
// Service が raw DB error を返したら handler は 500 を返し、未知 host
// (= 204) と同じ扱いにしないこと。観測性確保。
func TestShowInstance_DBError(t *testing.T) {
	h, repo := newHandler(t)
	repo.FindByHostErr = errors.New("connection reset by peer")
	c, rec := newReq(t, `{"host":"alpha.example"}`)
	require.NoError(t, h.ShowInstance(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestShowInstance_MetaError: instance 行は見つかっても meta が読めなければ
// isBlocked 等を埋められないので 500。
func TestShowInstance_MetaError(t *testing.T) {
	h, repo, metaRepo := newHandlerWithMeta(t)
	seedInstance(t, repo, "alpha.example")
	metaRepo.Meta = nil
	c, rec := newReq(t, `{"host":"alpha.example"}`)
	require.NoError(t, h.ShowInstance(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestShowInstance_OmitsNonUpstreamFields verifies that the response does NOT
// include the federating/subscribing/publishing/notRespondingSince fields, which
// are upstream federation/instances query params (not InstanceEntity response
// fields). isNotResponding (boolean) is the upstream response field (#1991).
func TestShowInstance_OmitsNonUpstreamFields(t *testing.T) {
	h, repo := newHandler(t)
	inst := seedInstance(t, repo, "flags.example")
	inst.FollowingCount = 4
	inst.FollowersCount = 2

	c, rec := newReq(t, `{"host":"flags.example"}`)
	require.NoError(t, h.ShowInstance(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	for _, k := range []string{"federating", "subscribing", "publishing", "notRespondingSince"} {
		_, has := got[k]
		assert.False(t, has, k+" は upstream の response field でないため省略する (#1991)")
	}
	// upstream response の isNotResponding (boolean) は残す。
	_, hasIsNotResponding := got["isNotResponding"]
	assert.True(t, hasIsNotResponding, "isNotResponding は upstream response field")
}

// #2106 N9: GET /federation/instances は query param を束縛する (json タグだけだと
// echo の GET binder が無視し、limit/sort/blocked 等が全て効かなくなる)。
func TestInstances_GETBindsQueryParams(t *testing.T) {
	h, repo := newHandler(t)
	seedInstance(t, repo, "alpha.example")
	seedInstance(t, repo, "beta.example")
	seedInstance(t, repo, "gamma.example")

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/federation/instances?limit=1", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Instances(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1, "GET ?limit=1 must bind the query param (else default 30 returns all 3)")
}
