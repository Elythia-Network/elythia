package notes

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"testing"
	"time"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	coretimeline "github.com/elythia-network/elythia/internal/core/timeline"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

var testRedis *testutil.TestRedis

// TestMain provisions a shared Redis container for the timeline handler tests.
// 既存の non-timeline テスト群はRedisを必要としないので、コンテナ起動失敗時は
// テストを単純にスキップして他テストの実行を妨げないようにしている。
func TestMain(m *testing.M) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		log.Printf("redis setup failed: %v (skipping timeline handler tests)", err)
	} else {
		testRedis = tr
	}
	code := m.Run()
	if testRedis != nil {
		testRedis.Teardown(ctx)
	}
	os.Exit(code)
}

// requireRedis skips the test if no Redis container is available.
func requireRedis(t *testing.T) {
	t.Helper()
	if testRedis == nil {
		t.Skip("redis container not available")
	}
}

// newRealTimelineService creates a coretimeline.Service backed by the shared
// Redis container.
func newRealTimelineService(t *testing.T, noteRepo *testutil.MockNoteRepository) (*coretimeline.Service, *coretimeline.FanoutTimelineService) {
	t.Helper()
	requireRedis(t)
	testRedis.FlushAll(context.Background())
	idGen, _ := id.NewGenerator("aidx")
	fanout := coretimeline.NewFanoutTimelineService(testRedis.Client, idGen, "")
	svc := coretimeline.NewService(fanout, noteRepo, testutil.NewMockFollowingRepository())
	return svc, fanout
}

// closedRedisClient returns a redis.Client whose connection has been closed,
// so any command on it returns an error.
func closedRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = c.Close()
	return c
}

func newFailingTimelineService(t *testing.T) *coretimeline.Service {
	t.Helper()
	idGen, _ := id.NewGenerator("aidx")
	fanout := coretimeline.NewFanoutTimelineService(closedRedisClient(t), idGen, "")
	return coretimeline.NewService(fanout, testutil.NewMockNoteRepository(), nil)
}

func newTimelineHandler(t *testing.T, noteRepo *testutil.MockNoteRepository, tl *coretimeline.Service) *Handler {
	t.Helper()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	return NewHandler(noteRepo, createSvc, deleteSvc, querySvc, tl, nil, nil, nil, idGen)
}

// seedTimelineNote creates a note in the repo and returns its id.
func seedTimelineNote(repo *testutil.MockNoteRepository) string {
	idGen, _ := id.NewGenerator("aidx")
	noteID := idGen.Generate(time.Now())
	repo.Notes[noteID] = &model.Note{
		ID:         noteID,
		UserID:     "author",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		User: &model.User{
			ID:                "author",
			Username:          "author",
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		},
	}
	return noteID
}

func TestTimeline_AuthRequired(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))

	c, rec := newJSONRequest(t, "/api/notes/timeline", `{}`)
	require.NoError(t, h.Timeline(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTimeline_InvalidJSON(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))

	c, rec := newJSONRequest(t, "/api/notes/timeline", `{invalid`)
	setAuthUser(c, &model.User{ID: "u"})
	require.NoError(t, h.Timeline(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTimeline_FanoutError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))

	c, rec := newJSONRequest(t, "/api/notes/timeline", `{}`)
	setAuthUser(c, &model.User{ID: "u"})
	require.NoError(t, h.Timeline(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestLocalTimeline_FanoutError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))

	c, rec := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	require.NoError(t, h.LocalTimeline(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestGlobalTimeline_FanoutError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))

	c, rec := newJSONRequest(t, "/api/notes/global-timeline", `{}`)
	require.NoError(t, h.GlobalTimeline(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestHybridTimeline_AuthRequired(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))

	c, rec := newJSONRequest(t, "/api/notes/hybrid-timeline", `{}`)
	require.NoError(t, h.HybridTimeline(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestHybridTimeline_FanoutError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))

	c, rec := newJSONRequest(t, "/api/notes/hybrid-timeline", `{}`)
	setAuthUser(c, &model.User{ID: "u"})
	require.NoError(t, h.HybridTimeline(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// #1026: ltlAvailable / gtlAvailable role policy gate。匿名でも認証済みでも
// 同じ pattern で gate される (upstream Misskey TS の getUserPolicies(me ?
// me.id : null) 経路と同 semantics、handler 内 gate)。
type stubTimelinePolicyProvider struct {
	policiesByUser map[string]map[string]any // userID -> policies (""=anonymous)
}

func (s *stubTimelinePolicyProvider) GetUserPolicies(userID string) map[string]any {
	if p, ok := s.policiesByUser[userID]; ok {
		return p
	}
	return map[string]any{}
}

func TestLocalTimeline_LtlDisabled_Anonymous(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))
	h.SetPolicyProvider(&stubTimelinePolicyProvider{
		policiesByUser: map[string]map[string]any{
			"": {"ltlAvailable": false},
		},
	})

	c, rec := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	require.NoError(t, h.LocalTimeline(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "policy=false で 403")
	assert.Contains(t, rec.Body.String(), "LTL_DISABLED")
	assert.Contains(t, rec.Body.String(), "45a6eb02-7695-4393-b023-dd3be9aaaefd")
}

func TestLocalTimeline_LtlDisabled_AuthenticatedUser(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))
	h.SetPolicyProvider(&stubTimelinePolicyProvider{
		policiesByUser: map[string]map[string]any{
			"alice": {"ltlAvailable": false},
		},
	})

	c, rec := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	setAuthUser(c, &model.User{ID: "alice"})
	require.NoError(t, h.LocalTimeline(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "LTL_DISABLED")
}

func TestLocalTimeline_LtlEnabled_PassesThrough(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))
	h.SetPolicyProvider(&stubTimelinePolicyProvider{
		policiesByUser: map[string]map[string]any{
			"": {"ltlAvailable": true},
		},
	})

	c, rec := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	require.NoError(t, h.LocalTimeline(c))
	// policy=true なら gate 通過、後段の failing service で 500 になる
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestGlobalTimeline_GtlDisabled(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))
	h.SetPolicyProvider(&stubTimelinePolicyProvider{
		policiesByUser: map[string]map[string]any{
			"": {"gtlAvailable": false},
		},
	})

	c, rec := newJSONRequest(t, "/api/notes/global-timeline", `{}`)
	require.NoError(t, h.GlobalTimeline(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "GTL_DISABLED")
	assert.Contains(t, rec.Body.String(), "0332fc13-6ab2-4427-ae80-a9fadffd1a6b")
}

// hybrid-timeline は ltlAvailable で gate する (= upstream と同 logic)。
// gtlAvailable=false でも hybrid は許可される (= ltl が effective なので)。
func TestHybridTimeline_LtlDisabledRejects(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))
	h.SetPolicyProvider(&stubTimelinePolicyProvider{
		policiesByUser: map[string]map[string]any{
			"alice": {"ltlAvailable": false, "gtlAvailable": true},
		},
	})

	c, rec := newJSONRequest(t, "/api/notes/hybrid-timeline", `{}`)
	setAuthUser(c, &model.User{ID: "alice"})
	require.NoError(t, h.HybridTimeline(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "hybrid は ltlAvailable で gate")
	// gate は ltlAvailable だが error code は STL_DISABLED (#1554)。
	assert.Contains(t, rec.Body.String(), "STL_DISABLED")
	assert.Contains(t, rec.Body.String(), "620763f4-f621-4533-ab33-0577a1a3c342")
}

// #1554 local-timeline は withReplies && withFiles 同時指定を
// BOTH_WITH_REPLIES_AND_WITH_FILES (local 固有 UUID) で弾く。
func TestLocalTimeline_BothWithRepliesAndWithFiles(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))
	c, rec := newJSONRequest(t, "/api/notes/local-timeline", `{"withReplies":true,"withFiles":true}`)
	require.NoError(t, h.LocalTimeline(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "BOTH_WITH_REPLIES_AND_WITH_FILES")
	assert.Contains(t, rec.Body.String(), "dd9c8400-1cb5-4eef-8a31-200c5f933793")
}

// #1554 hybrid-timeline も同様 (hybrid 固有 UUID)。
func TestHybridTimeline_BothWithRepliesAndWithFiles(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))
	c, rec := newJSONRequest(t, "/api/notes/hybrid-timeline", `{"withReplies":true,"withFiles":true}`)
	setAuthUser(c, &model.User{ID: "alice"})
	require.NoError(t, h.HybridTimeline(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "BOTH_WITH_REPLIES_AND_WITH_FILES")
	assert.Contains(t, rec.Body.String(), "dfaa3eb7-8002-4cb7-bcc4-1095df46656f")
}

// withReplies のみ / withFiles のみは通る (両方同時のときだけ弾く)。
func TestLocalTimeline_WithRepliesOnlyOK(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))
	c, rec := newJSONRequest(t, "/api/notes/local-timeline", `{"withReplies":true}`)
	require.NoError(t, h.LocalTimeline(c))
	// BOTH guard は通り、後段の failing service で 500 (= guard で弾かれていない)。
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// policyProvider 未配線時は gate を skip する (= 旧挙動互換 / test fixture)。
func TestLocalTimeline_NoPolicyProviderSkipsGate(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))
	// SetPolicyProvider を呼ばない

	c, rec := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	require.NoError(t, h.LocalTimeline(c))
	// gate を skip して後段の failing service で 500
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// policies map に該当 key が無い / 非 bool の場合は **fail-closed で reject**
// する (upstream の `if (!policies.ltlAvailable)` 評価と挙動を揃える、#1038
// review)。production 経路では DefaultPolicies が常に bool を返すのでこの
// path は発火しないが、test fixture で seal して挙動を固定する。
func TestLocalTimeline_MissingPolicyKeyDenies(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))
	h.SetPolicyProvider(&stubTimelinePolicyProvider{
		policiesByUser: map[string]map[string]any{
			"": {}, // empty policies map (= key 不在)
		},
	})

	c, rec := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	require.NoError(t, h.LocalTimeline(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "key 不在は fail-closed reject")
	assert.Contains(t, rec.Body.String(), "LTL_DISABLED")
}

// 非 bool 値 (e.g., 文字列) も fail-closed で reject される regression guard。
func TestLocalTimeline_NonBoolPolicyValueDenies(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, newFailingTimelineService(t))
	h.SetPolicyProvider(&stubTimelinePolicyProvider{
		policiesByUser: map[string]map[string]any{
			"": {"ltlAvailable": "yes"}, // 文字列 (production では起きないが defensive)
		},
	})

	c, rec := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	require.NoError(t, h.LocalTimeline(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "非 bool は fail-closed")
}

func TestTimeline_HappyPathHome(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteID := seedTimelineNote(noteRepo)
	tl, fanout := newRealTimelineService(t, noteRepo)
	require.NoError(t, fanout.Push(context.Background(), coretimeline.HomeTimelineName("viewer"), noteID, 100))

	h := newTimelineHandler(t, noteRepo, tl)
	c, rec := newJSONRequest(t, "/api/notes/timeline", `{}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.Timeline(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
}

func TestTimeline_HappyPathLocal(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteID := seedTimelineNote(noteRepo)
	tl, fanout := newRealTimelineService(t, noteRepo)
	require.NoError(t, fanout.Push(context.Background(), coretimeline.LocalTimeline, noteID, 100))

	h := newTimelineHandler(t, noteRepo, tl)
	c, rec := newJSONRequest(t, "/api/notes/local-timeline", `{"limit":100}`)
	require.NoError(t, h.LocalTimeline(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)

	// 範囲外 limit は 400 (upstream の ajv maximum)。
	c, rec = newJSONRequest(t, "/api/notes/local-timeline", `{"limit":1000}`)
	require.NoError(t, h.LocalTimeline(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTimeline_HappyPathGlobal(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteID := seedTimelineNote(noteRepo)
	tl, fanout := newRealTimelineService(t, noteRepo)
	require.NoError(t, fanout.Push(context.Background(), coretimeline.GlobalTimeline, noteID, 100))

	h := newTimelineHandler(t, noteRepo, tl)
	c, rec := newJSONRequest(t, "/api/notes/global-timeline", `{}`)
	require.NoError(t, h.GlobalTimeline(c))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestTimeline_HappyPathHybrid(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteID := seedTimelineNote(noteRepo)
	tl, fanout := newRealTimelineService(t, noteRepo)
	ctx := context.Background()
	require.NoError(t, fanout.Push(ctx, coretimeline.HomeTimelineName("viewer"), noteID, 100))

	h := newTimelineHandler(t, noteRepo, tl)
	c, rec := newJSONRequest(t, "/api/notes/hybrid-timeline", `{}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.HybridTimeline(c))
	require.Equal(t, http.StatusOK, rec.Code)
}

// timeline JSON cache (enableTimelineCache) の handler-level 統合テスト。
// cache primitive の unit test (timeline_cache_test.go) とは別に、serveTimeline
// への組み込み (first-page gate / key 構築 / cursor bypass / per-viewer 分離 /
// c.JSON との byte 一致) を検証する。

// first-page は cache され、TTL 内の 2nd request は 1st と byte 一致で stale を
// 返す (= cache hit が効いている証拠)。
func TestTimeline_Cache_ServesStaleWithinTTL(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	note1 := seedTimelineNote(noteRepo)
	tl, fanout := newRealTimelineService(t, noteRepo)
	ctx := context.Background()
	require.NoError(t, fanout.Push(ctx, coretimeline.LocalTimeline, note1, 100))

	h := newTimelineHandler(t, noteRepo, tl)
	h.EnableTimelineJSONCache(3 * time.Second)

	c1, rec1 := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	require.NoError(t, h.LocalTimeline(c1))
	require.Equal(t, http.StatusOK, rec1.Code)
	body1 := rec1.Body.String()
	var resp1 []map[string]any
	require.NoError(t, json.Unmarshal([]byte(body1), &resp1))
	require.Len(t, resp1, 1)

	// データを変える。cache が効いていれば 2nd には反映されない。
	note2 := seedTimelineNote(noteRepo)
	require.NoError(t, fanout.Push(ctx, coretimeline.LocalTimeline, note2, 100))

	c2, rec2 := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	require.NoError(t, h.LocalTimeline(c2))
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, body1, rec2.Body.String(), "cache hit は 1st と byte 一致 (stale)")
	var resp2 []map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Len(t, resp2, 1, "cache hit なので note2 は反映されない")
}

// cursor (untilId) 付き request は cache を bypass し fresh データを返す。
func TestTimeline_Cache_CursorBypasses(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	note1 := seedTimelineNote(noteRepo)
	tl, fanout := newRealTimelineService(t, noteRepo)
	ctx := context.Background()
	require.NoError(t, fanout.Push(ctx, coretimeline.LocalTimeline, note1, 100))

	h := newTimelineHandler(t, noteRepo, tl)
	h.EnableTimelineJSONCache(3 * time.Second)

	// first-page を 1 回引いて cache を温める。
	c0, rec0 := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	require.NoError(t, h.LocalTimeline(c0))
	require.Equal(t, http.StatusOK, rec0.Code)

	note2 := seedTimelineNote(noteRepo)
	require.NoError(t, fanout.Push(ctx, coretimeline.LocalTimeline, note2, 100))

	// untilId 付き → cache を bypass して fresh (note1 + note2)。
	c1, rec1 := newJSONRequest(t, "/api/notes/local-timeline", `{"untilId":"zzzzzzzzzzzzzzzz"}`)
	require.NoError(t, h.LocalTimeline(c1))
	require.Equal(t, http.StatusOK, rec1.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp))
	assert.Len(t, resp, 2, "cursor 付きは cache を bypass し fresh データ (2 件) を返す")
}

// per-viewer key: viewer A の cache が viewer B へ漏れない。local-timeline は
// fanout list を共有するが cache key は viewerID で分かれる。A が cache を温めた
// 後にデータを増やすと、別 viewer B は fresh (= A の stale cache を受けない)、
// 一方 A 自身は stale cache を引く、という非対称で isolation を示す。
func TestTimeline_Cache_PerViewerIsolation(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	note1 := seedTimelineNote(noteRepo)
	tl, fanout := newRealTimelineService(t, noteRepo)
	ctx := context.Background()
	require.NoError(t, fanout.Push(ctx, coretimeline.LocalTimeline, note1, 100))

	h := newTimelineHandler(t, noteRepo, tl)
	h.EnableTimelineJSONCache(3 * time.Second)

	// viewer A が cache を温める (note1 のみ)。
	cA, recA := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	setAuthUser(cA, &model.User{ID: "A"})
	require.NoError(t, h.LocalTimeline(cA))
	require.Equal(t, http.StatusOK, recA.Code)
	var rA []map[string]any
	require.NoError(t, json.Unmarshal(recA.Body.Bytes(), &rA))
	require.Len(t, rA, 1)

	// データを増やす。
	note2 := seedTimelineNote(noteRepo)
	require.NoError(t, fanout.Push(ctx, coretimeline.LocalTimeline, note2, 100))

	// viewer B は別 key なので miss → fresh (2 件)。A の 1 件 cache を受けない。
	cB, recB := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	setAuthUser(cB, &model.User{ID: "B"})
	require.NoError(t, h.LocalTimeline(cB))
	require.Equal(t, http.StatusOK, recB.Code)
	var rB []map[string]any
	require.NoError(t, json.Unmarshal(recB.Body.Bytes(), &rB))
	assert.Len(t, rB, 2, "別 viewer B は fresh (2 件) を受け取る (A の stale cache を受けない)")

	// viewer A は自分の key で hit → stale (1 件) のまま。
	cA2, recA2 := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	setAuthUser(cA2, &model.User{ID: "A"})
	require.NoError(t, h.LocalTimeline(cA2))
	var rA2 []map[string]any
	require.NoError(t, json.Unmarshal(recA2.Body.Bytes(), &rA2))
	assert.Len(t, rA2, 1, "viewer A は自分の cache で stale (1 件)")
}

// cache miss 経路 (marshal + 改行) の応答が cache 無効時の c.JSON 経路と byte 一致
// すること (= 改行整合の回帰 guard)。
func TestTimeline_Cache_ByteIdenticalToNonCached(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	note1 := seedTimelineNote(noteRepo)
	tl, fanout := newRealTimelineService(t, noteRepo)
	require.NoError(t, fanout.Push(context.Background(), coretimeline.LocalTimeline, note1, 100))

	hNo := newTimelineHandler(t, noteRepo, tl)
	cNo, recNo := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	require.NoError(t, hNo.LocalTimeline(cNo))

	hYes := newTimelineHandler(t, noteRepo, tl)
	hYes.EnableTimelineJSONCache(3 * time.Second)
	cYes, recYes := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	require.NoError(t, hYes.LocalTimeline(cYes))

	assert.Equal(t, recNo.Body.String(), recYes.Body.String(),
		"cache miss 経路 (marshal+改行) は c.JSON 経路と byte 一致")
}

// #1681 loadBlockerIDs / loadMutedInstances loader の coverage。
func TestLoadBlockerIDs(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, nil)
	// nil viewer / 未配線 blockingRepo は nil。
	assert.Nil(t, h.loadBlockerIDs(nil))
	assert.Nil(t, h.loadBlockerIDs(&model.User{ID: "v"}))
	blk := testutil.NewMockBlockingRepository()
	blk.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: "blocker", BlockeeID: "v"}
	h.SetBlockingRepo(blk)
	assert.Equal(t, []string{"blocker"}, h.loadBlockerIDs(&model.User{ID: "v"}))
}

func TestLoadMutedInstances(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	h := newTimelineHandler(t, noteRepo, nil)
	// nil viewer / 未配線 userRepo は nil。
	assert.Nil(t, h.loadMutedInstances(nil))
	assert.Nil(t, h.loadMutedInstances(&model.User{ID: "v"}))
	userRepo := testutil.NewMockUserRepository()
	userRepo.Profiles["v"] = &model.UserProfile{UserID: "v", MutedInstances: datatypes.JSON([]byte(`["Bad.Example","good.tld"]`))}
	userRepo.Profiles["empty"] = &model.UserProfile{UserID: "empty"}
	h.SetUserRepo(userRepo)
	// 大文字 host は lowercase 化される。
	assert.ElementsMatch(t, []string{"bad.example", "good.tld"}, h.loadMutedInstances(&model.User{ID: "v"}))
	// 空 mutedInstances は nil。
	assert.Nil(t, h.loadMutedInstances(&model.User{ID: "empty"}))
}
