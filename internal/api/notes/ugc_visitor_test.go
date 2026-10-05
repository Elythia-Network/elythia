package notes

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/search"
	coretimeline "github.com/elythia-network/elythia/internal/core/timeline"
	"github.com/elythia-network/elythia/internal/core/ugcvisibility"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// ugcRemoteHost is the host of the remote author in the visitor fixtures.
const ugcRemoteHost = "remote.example"

// Fixture note IDs (aidx, ascending in this order). Request bodies refer to
// them by the placeholders {root} / {local} / {remote}.
var (
	ugcRootID   = ugcFixtureID(0) // local root note
	ugcLocalID  = ugcFixtureID(1) // local reply to the root
	ugcRemoteID = ugcFixtureID(2) // remote reply to the root
	ugcQuoteID  = ugcFixtureID(3) // local quote renote of the root

	ugcBodyIDs = strings.NewReplacer("{root}", ugcRootID, "{local}", ugcLocalID, "{remote}", ugcRemoteID)
)

func ugcFixtureID(n int) string {
	idGen, _ := id.NewGenerator("aidx")
	return idGen.Generate(time.UnixMilli(1_700_000_000_000 + int64(n)))
}

// fttOff disables the fanout timeline so that timeline reads go to the DB path.
type fttOff struct{}

func (fttOff) FanoutTimelineEnabled() bool { return false }

// countingProvider wraps a search provider and counts SearchNote calls.
type countingProvider struct {
	search.Provider
	calls int
}

func (p *countingProvider) SearchNote(viewer *model.User, query string, opts search.SearchOpts, page search.Pagination) ([]*model.Note, error) {
	p.calls++
	return p.Provider.SearchNote(viewer, query, opts, page)
}

type ugcFixture struct {
	h        *Handler
	repo     *testutil.MockNoteRepository
	provider *countingProvider
	policy   string
}

func ugcNote(noteID string, host *string) *model.Note {
	userID := "local-author"
	if host != nil {
		userID = "remote-author"
	}
	text := "hello"
	return &model.Note{
		ID:         noteID,
		UserID:     userID,
		UserHost:   host,
		Text:       &text,
		Tags:       []string{"t"},
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		User: &model.User{
			ID:                userID,
			Username:          userID,
			Host:              host,
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		},
	}
}

// newUGCFixture wires a handler whose every anonymous-reachable list endpoint
// can serve the fixture notes: a local root, a local reply, a remote reply and
// a local quote renote.
func newUGCFixture(t *testing.T) *ugcFixture {
	t.Helper()
	repo := testutil.NewMockNoteRepository()
	remote := ugcRemoteHost
	root := ugcRootID
	repo.Notes[ugcRootID] = ugcNote(ugcRootID, nil)
	localReply := ugcNote(ugcLocalID, nil)
	localReply.ReplyID = &root
	repo.Notes[ugcLocalID] = localReply
	remoteReply := ugcNote(ugcRemoteID, &remote)
	remoteReply.ReplyID = &root
	repo.Notes[ugcRemoteID] = remoteReply
	quote := ugcNote(ugcQuoteID, nil)
	quote.RenoteID = &root
	repo.Notes[ugcQuoteID] = quote

	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(repo, testutil.NewMockPollRepository(), idGen, nil)
	deleteSvc := corenote.NewDeleteService(repo)
	querySvc := corenote.NewQueryService(repo, nil)
	tl := coretimeline.NewService(nil, repo, testutil.NewMockFollowingRepository())
	tl.SetFanoutToggle(fttOff{})
	provider := &countingProvider{Provider: search.NewSQLLikeProvider(repo, nil)}
	h := NewHandler(repo, createSvc, deleteSvc, querySvc, tl, nil, nil, search.NewService(provider), idGen)

	f := &ugcFixture{h: h, repo: repo, provider: provider, policy: ugcvisibility.All}
	h.SetUGCVisibilityLookup(func() string { return f.policy })
	return f
}

// call invokes endpoint with body as viewer (nil = anonymous) and returns the
// note IDs of the 200 response.
func (f *ugcFixture) call(t *testing.T, method, body string, viewer *model.User) []string {
	t.Helper()
	m := reflect.ValueOf(f.h).MethodByName(method)
	require.Truef(t, m.IsValid(), "no such method %s", method)
	endpoint, ok := m.Interface().(func(echo.Context) error)
	require.Truef(t, ok, "%s is not an endpoint", method)
	c, rec := newJSONRequest(t, "/", ugcBodyIDs.Replace(body))
	if viewer != nil {
		setAuthUser(c, viewer)
	}
	require.NoError(t, endpoint(c))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var items []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items), rec.Body.String())
	ids := make([]string, 0, len(items))
	for _, it := range items {
		s, _ := it["id"].(string)
		ids = append(ids, s)
	}
	sort.Strings(ids)
	return ids
}

type ugcEndpoint struct {
	name   string
	method string // Handler method name
	body   string
}

// ugcVisibilityQueryEndpoints are the anonymous-reachable endpoints of this
// package whose upstream query applies generateVisibilityQuery or
// generateUgcVisibilityQueryForVisitor (or an explicit `none` check), so that
// an anonymous visitor gets [] under ugcVisibilityForVisitor=none.
var ugcVisibilityQueryEndpoints = []ugcEndpoint{
	{"global-timeline", "GlobalTimeline", `{}`},
	{"local-timeline", "LocalTimeline", `{}`},
	{"notes", "BulkShow", `{}`},
	{"notes(noteIds)", "BulkShow", `{"noteIds":["{local}","{remote}"]}`},
	{"featured", "Featured", `{}`},
	{"search-by-tag", "SearchByTag", `{"tag":"t"}`},
	{"search", "Search", `{"query":"hello"}`},
	{"children", "Children", `{"noteId":"{root}"}`},
	{"replies", "Replies", `{"noteId":"{root}"}`},
	{"renotes", "Renotes", `{"noteId":"{root}"}`},
	{"conversation", "Conversation", `{"noteId":"{local}"}`},
	{"show-partial-bulk", "ShowPartialBulk", `{"noteIds":["{local}","{remote}"]}`},
}

// ugcExemptEndpoints lists the remaining endpoint methods of Handler and why
// the anonymous `none` gate above does not apply to them. A new endpoint
// method fails TestUGCVisitor_EveryEndpointIsClassified until it is put in
// one of the two lists.
var ugcExemptEndpoints = map[string]string{
	"Show":                "single note: CONTENT_RESTRICTED_BY_SERVER (tested in handler_parity1538_test.go)",
	"Reactions":           "upstream notes/reactions does not consult ugcVisibilityForVisitor",
	"Clips":               "upstream notes/clips does not consult ugcVisibilityForVisitor",
	"Create":              "requires credential",
	"Delete":              "requires credential",
	"State":               "requires credential",
	"Timeline":            "requires credential",
	"HybridTimeline":      "requires credential",
	"Mentions":            "requires credential",
	"UserListTimeline":    "requires credential",
	"ReactionsCreate":     "requires credential",
	"ReactionsDelete":     "requires credential",
	"PollsVote":           "requires credential",
	"PollsRecommendation": "requires credential",
	"FavoritesCreate":     "requires credential",
	"FavoritesDelete":     "requires credential",
	"Unrenote":            "requires credential",
	"Translate":           "requires credential",
	"DraftsList":          "requires credential",
	"DraftsCreate":        "requires credential",
	"DraftsUpdate":        "requires credential",
	"DraftsDelete":        "requires credential",
	"DraftsCount":         "requires credential",
	"ThreadMutingCreate":  "requires credential",
	"ThreadMutingDelete":  "requires credential",
}

// TestUGCVisitor_EveryEndpointIsClassified makes a newly added endpoint
// method fail until it is classified as gated or exempt above.
func TestUGCVisitor_EveryEndpointIsClassified(t *testing.T) {
	gated := map[string]bool{}
	for _, ep := range ugcVisibilityQueryEndpoints {
		gated[ep.method] = true
	}
	endpointType := reflect.TypeOf((func(echo.Context) error)(nil))
	hv := reflect.ValueOf(&Handler{})
	ht := hv.Type()
	seen := 0
	for i := 0; i < ht.NumMethod(); i++ {
		name := ht.Method(i).Name
		if hv.Method(i).Type() != endpointType {
			continue
		}
		seen++
		_, exempt := ugcExemptEndpoints[name]
		assert.Truef(t, gated[name] || exempt, "%s: classify it in ugcVisibilityQueryEndpoints or ugcExemptEndpoints", name)
		assert.Falsef(t, gated[name] && exempt, "%s is listed in both tables", name)
	}
	// 抽出が空振りしていないこと (型の比較が壊れると 0 件で緑になる)。
	assert.Equal(t, len(gated)+len(ugcExemptEndpoints), seen, "a listed method is missing or the exempt table has a stale entry")
}

// anon + none → [] on every listing endpoint; a signed-in viewer is not affected.
func TestUGCVisitor_NoneReturnsEmptyForVisitor(t *testing.T) {
	for _, ep := range ugcVisibilityQueryEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			f := newUGCFixture(t)

			f.policy = ugcvisibility.All
			require.NotEmpty(t, f.call(t, ep.method, ep.body, nil), "fixture must yield notes for visitors under all")

			f.policy = ugcvisibility.None
			assert.Empty(t, f.call(t, ep.method, ep.body, nil), "visitor under none")
			assert.NotEmpty(t, f.call(t, ep.method, ep.body, &model.User{ID: "viewer"}), "signed-in viewer under none")
		})
	}
}

// Standalone listings drop notes whose own author is remote for visitors under
// `local`, keep them for signed-in viewers and for visitors under `all`.
func TestUGCVisitor_LocalHidesRemoteAuthors(t *testing.T) {
	standalone := map[string]bool{
		"global-timeline": true, "notes": true, "notes(noteIds)": true,
		"featured": true, "search-by-tag": true, "search": true,
	}
	for _, ep := range ugcVisibilityQueryEndpoints {
		if !standalone[ep.name] {
			continue
		}
		t.Run(ep.name, func(t *testing.T) {
			f := newUGCFixture(t)

			f.policy = ugcvisibility.Local
			anon := f.call(t, ep.method, ep.body, nil)
			assert.Contains(t, anon, ugcLocalID, "local author stays")
			assert.NotContains(t, anon, ugcRemoteID, "visitor under local")
			assert.Contains(t, f.call(t, ep.method, ep.body, &model.User{ID: "viewer"}), ugcRemoteID, "signed-in viewer under local")

			f.policy = ugcvisibility.All
			assert.Contains(t, f.call(t, ep.method, ep.body, nil), ugcRemoteID, "visitor under all")
		})
	}
}

// upstream leaves `local` alone on the thread / diff endpoints (only `none`
// is checked there), so a remote reply stays visible.
func TestUGCVisitor_LocalDoesNotFilterThreadEndpoints(t *testing.T) {
	for _, name := range []string{"children", "replies", "show-partial-bulk"} {
		for _, ep := range ugcVisibilityQueryEndpoints {
			if ep.name != name {
				continue
			}
			t.Run(name, func(t *testing.T) {
				f := newUGCFixture(t)
				f.policy = ugcvisibility.Local
				assert.Contains(t, f.call(t, ep.method, ep.body, nil), ugcRemoteID)
			})
		}
	}
}

// notes/search under `none` returns [] without running the search backend.
func TestUGCVisitor_SearchNoneSkipsProvider(t *testing.T) {
	f := newUGCFixture(t)
	f.policy = ugcvisibility.None
	assert.Empty(t, f.call(t, "Search", `{"query":"hello"}`, nil))
	assert.Equal(t, 0, f.provider.calls)

	// 空クエリは従来どおり INVALID_PARAM (none でも 200 [] に化けない)。
	c, rec := newJSONRequest(t, "/", `{"query":""}`)
	require.NoError(t, f.h.Search(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// The anonymous first page of global-timeline is cached per policy, so
// tightening or loosening the policy takes effect without waiting for the TTL.
func TestUGCVisitor_GlobalTimelineCacheKeyedByPolicy(t *testing.T) {
	f := newUGCFixture(t)
	f.h.EnableTimelineJSONCache(time.Minute)

	f.policy = ugcvisibility.Local
	assert.NotContains(t, f.call(t, "GlobalTimeline", `{}`, nil), ugcRemoteID)
	f.policy = ugcvisibility.All
	assert.Contains(t, f.call(t, "GlobalTimeline", `{}`, nil), ugcRemoteID, "cached local-only page must not be served under all")
	f.policy = ugcvisibility.Local
	assert.NotContains(t, f.call(t, "GlobalTimeline", `{}`, nil), ugcRemoteID, "cached all page must not be served under local")
}

// The featured ranking is filtered before it is cut to limit, as upstream
// does; cutting first would leave the page empty when the top is remote.
func TestUGCVisitor_FeaturedFiltersBeforeLimit(t *testing.T) {
	f := newUGCFixture(t)
	f.h.SetFeaturedRanking(&fakeFeaturedReader{global: []string{ugcRemoteID, ugcLocalID}})
	f.policy = ugcvisibility.Local
	assert.Equal(t, []string{ugcLocalID}, f.call(t, "Featured", `{"limit":1}`, nil))
	assert.Equal(t, []string{ugcRemoteID}, f.call(t, "Featured", `{"limit":1}`, &model.User{ID: "viewer"}))
}

// After the local-author filter, the ranking path still honours limit even
// though it fetched every ranked note.
func TestUGCVisitor_FeaturedLocalStillCapsAtLimit(t *testing.T) {
	f := newUGCFixture(t)
	f.h.SetFeaturedRanking(&fakeFeaturedReader{global: []string{ugcRemoteID, ugcRootID, ugcLocalID, ugcQuoteID}})
	f.policy = ugcvisibility.Local
	got := f.call(t, "Featured", `{"limit":2}`, nil)
	assert.Len(t, got, 2)
	assert.NotContains(t, got, ugcRemoteID)
}

// The fanout (Redis) path of global-timeline drops remote authors too.
func TestUGCVisitor_GlobalTimelineFanoutPath(t *testing.T) {
	f := newUGCFixture(t)
	tl, fanout := newRealTimelineService(t, f.repo)
	ctx := context.Background()
	for _, nid := range []string{ugcRootID, ugcLocalID, ugcRemoteID, ugcQuoteID} {
		require.NoError(t, fanout.Push(ctx, coretimeline.GlobalTimeline, nid, 100))
	}
	f.h.timelineService = tl

	// allowPartial で DB fallback を止め、Redis 経路の判定だけを見る。
	f.policy = ugcvisibility.Local
	anon := f.call(t, "GlobalTimeline", `{"allowPartial":true}`, nil)
	assert.Contains(t, anon, ugcLocalID)
	assert.NotContains(t, anon, ugcRemoteID)
	f.policy = ugcvisibility.All
	assert.Contains(t, f.call(t, "GlobalTimeline", `{"allowPartial":true}`, nil), ugcRemoteID)
}
