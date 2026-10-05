package timeline

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestServiceWithRepo wires a Service against the shared Redis container
// and an in-memory MockNoteRepository so that timeline reads return real notes.
func newTestServiceWithRepo(t *testing.T) (*Service, *FanoutTimelineService, *testutil.MockNoteRepository) {
	t.Helper()
	testRedis.FlushAll(context.Background())
	noteRepo := testutil.NewMockNoteRepository()
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	svc := NewService(fanout, noteRepo, testutil.NewMockFollowingRepository())
	return svc, fanout, noteRepo
}

func TestService_HomeTimelineRequiresUser(t *testing.T) {
	svc, _, _ := newTestServiceWithRepo(t)
	_, err := svc.HomeTimeline(context.Background(), nil, "", "", 10, TimelineFilter{})
	require.ErrorIs(t, err, ErrUnauthenticated)
}

func TestService_HomeTimeline(t *testing.T) {
	svc, fanout, repo := newTestServiceWithRepo(t)
	ctx := context.Background()

	user := &model.User{ID: "viewer"}
	noteID := idGen.Generate(time.Now())
	repo.Notes[noteID] = &model.Note{ID: noteID, UserID: "viewer", Visibility: model.NoteVisibilityPublic}
	require.NoError(t, fanout.Push(ctx, HomeTimelineName(user.ID), noteID, 100))

	out, err := svc.HomeTimeline(ctx, user, "", "", 10, TimelineFilter{})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, noteID, out[0].ID)
}

func TestService_HomeTimelineFanoutError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	fanout := NewFanoutTimelineService(closedClient(t), idGen, "")
	svc := NewService(fanout, noteRepo, nil)
	_, err := svc.HomeTimeline(context.Background(), &model.User{ID: "u"}, "", "", 10, TimelineFilter{})
	assert.Error(t, err)
}

func TestService_LocalTimeline(t *testing.T) {
	svc, fanout, repo := newTestServiceWithRepo(t)
	ctx := context.Background()

	noteID := idGen.Generate(time.Now())
	repo.Notes[noteID] = &model.Note{ID: noteID, UserID: "a", Visibility: model.NoteVisibilityPublic}
	require.NoError(t, fanout.Push(ctx, LocalTimeline, noteID, 100))

	out, err := svc.LocalTimeline(ctx, nil, "", "", 10, TimelineFilter{})
	require.NoError(t, err)
	require.Len(t, out, 1)
}

func TestService_LocalTimelineFanoutError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	svc := NewService(NewFanoutTimelineService(closedClient(t), idGen, ""), noteRepo, nil)
	_, err := svc.LocalTimeline(context.Background(), nil, "", "", 10, TimelineFilter{})
	assert.Error(t, err)
}

func TestService_GlobalTimeline(t *testing.T) {
	svc, fanout, repo := newTestServiceWithRepo(t)
	ctx := context.Background()

	noteID := idGen.Generate(time.Now())
	repo.Notes[noteID] = &model.Note{ID: noteID, UserID: "a", Visibility: model.NoteVisibilityPublic}
	require.NoError(t, fanout.Push(ctx, GlobalTimeline, noteID, 100))

	out, err := svc.GlobalTimeline(ctx, nil, "", "", 10, TimelineFilter{})
	require.NoError(t, err)
	require.Len(t, out, 1)
}

func TestService_GlobalTimelineFanoutError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	svc := NewService(NewFanoutTimelineService(closedClient(t), idGen, ""), noteRepo, nil)
	_, err := svc.GlobalTimeline(context.Background(), nil, "", "", 10, TimelineFilter{})
	assert.Error(t, err)
}

func TestService_HybridTimelineRequiresUser(t *testing.T) {
	svc, _, _ := newTestServiceWithRepo(t)
	_, err := svc.HybridTimeline(context.Background(), nil, "", "", 10, TimelineFilter{})
	require.ErrorIs(t, err, ErrUnauthenticated)
}

func TestService_HybridTimeline(t *testing.T) {
	svc, fanout, repo := newTestServiceWithRepo(t)
	ctx := context.Background()

	user := &model.User{ID: "viewer"}
	homeID := idGen.Generate(time.Now())
	localID := idGen.Generate(time.Now().Add(time.Millisecond))
	repo.Notes[homeID] = &model.Note{ID: homeID, UserID: "a", Visibility: model.NoteVisibilityPublic}
	repo.Notes[localID] = &model.Note{ID: localID, UserID: "b", Visibility: model.NoteVisibilityPublic}

	require.NoError(t, fanout.Push(ctx, HomeTimelineName(user.ID), homeID, 100))
	require.NoError(t, fanout.Push(ctx, LocalTimeline, localID, 100))

	out, err := svc.HybridTimeline(ctx, user, "", "", 10, TimelineFilter{})
	require.NoError(t, err)
	require.Len(t, out, 2)
}

func TestService_HybridTimelineFanoutError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	svc := NewService(NewFanoutTimelineService(closedClient(t), idGen, ""), noteRepo, nil)
	_, err := svc.HybridTimeline(context.Background(), &model.User{ID: "u"}, "", "", 10, TimelineFilter{})
	assert.Error(t, err)
}

func TestService_HybridTimelineDeduplicates(t *testing.T) {
	svc, fanout, repo := newTestServiceWithRepo(t)
	ctx := context.Background()

	user := &model.User{ID: "viewer"}
	noteID := idGen.Generate(time.Now())
	repo.Notes[noteID] = &model.Note{ID: noteID, UserID: "a", Visibility: model.NoteVisibilityPublic}
	// 同じIDをhome/localの両方にpushする
	require.NoError(t, fanout.Push(ctx, HomeTimelineName(user.ID), noteID, 100))
	require.NoError(t, fanout.Push(ctx, LocalTimeline, noteID, 100))

	out, err := svc.HybridTimeline(ctx, user, "", "", 10, TimelineFilter{})
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestService_ResolveEmpty(t *testing.T) {
	svc, _, _ := newTestServiceWithRepo(t)
	out, err := svc.LocalTimeline(context.Background(), nil, "", "", 10, TimelineFilter{})
	require.NoError(t, err)
	assert.Empty(t, out)
}

// --- DB fallback tests (Redis empty) ---

func TestService_HomeTimeline_DBFallback(t *testing.T) {
	svc, _, repo := newTestServiceWithRepo(t)
	// Redisにpushしない → DBフォールバック
	repo.Notes["n1"] = &model.Note{ID: "n1", UserID: "viewer", Visibility: model.NoteVisibilityPublic}
	out, err := svc.HomeTimeline(context.Background(), &model.User{ID: "viewer"}, "", "", 10, TimelineFilter{})
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestService_LocalTimeline_DBFallback(t *testing.T) {
	svc, _, repo := newTestServiceWithRepo(t)
	repo.Notes["n1"] = &model.Note{ID: "n1", UserID: "a", Visibility: model.NoteVisibilityPublic}
	out, err := svc.LocalTimeline(context.Background(), nil, "", "", 10, TimelineFilter{})
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestService_GlobalTimeline_DBFallback(t *testing.T) {
	svc, _, repo := newTestServiceWithRepo(t)
	repo.Notes["n1"] = &model.Note{ID: "n1", UserID: "a", Visibility: model.NoteVisibilityPublic}
	out, err := svc.GlobalTimeline(context.Background(), nil, "", "", 10, TimelineFilter{})
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestService_HybridTimeline_DBFallback(t *testing.T) {
	svc, _, repo := newTestServiceWithRepo(t)
	repo.Notes["n1"] = &model.Note{ID: "n1", UserID: "viewer", Visibility: model.NoteVisibilityPublic}
	out, err := svc.HybridTimeline(context.Background(), &model.User{ID: "viewer"}, "", "", 10, TimelineFilter{})
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

// hybrid DB fallback の merge / dedup / sort path を直接 cover する
// (#819 PR-fix)。home / local がそれぞれ異なる note を返す mock を用意し、
// 結果が ID 単位で dedup され、ID 降順で並ぶことを check する。
type splitTimelineRepo struct {
	*testutil.MockNoteRepository
	homeNotes  []*model.Note
	localNotes []*model.Note
}

func (r *splitTimelineRepo) ListHomeTimeline(_ string, _ int, _, _ string, _ model.TimelineDBFilter) ([]*model.Note, error) {
	return r.homeNotes, nil
}
func (r *splitTimelineRepo) ListLocalTimeline(_ int, _, _ string, _ model.TimelineDBFilter) ([]*model.Note, error) {
	return r.localNotes, nil
}

func TestService_HybridDBFallback_DedupAndSort(t *testing.T) {
	testRedis.FlushAll(context.Background())
	repo := &splitTimelineRepo{
		MockNoteRepository: testutil.NewMockNoteRepository(),
		homeNotes: []*model.Note{
			{ID: "n3", UserID: "viewer", Visibility: model.NoteVisibilityPublic},
			{ID: "n1", UserID: "viewer", Visibility: model.NoteVisibilityPublic},
		},
		localNotes: []*model.Note{
			{ID: "n2", UserID: "outsider", Visibility: model.NoteVisibilityPublic},
			// n1 は home / local の両方に出現する想定 (= dedup 対象)
			{ID: "n1", UserID: "outsider", Visibility: model.NoteVisibilityPublic},
		},
	}
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	svc := NewService(fanout, repo, testutil.NewMockFollowingRepository())

	out, err := svc.HybridTimeline(context.Background(), &model.User{ID: "viewer"}, "", "", 10, TimelineFilter{})
	require.NoError(t, err)
	require.Len(t, out, 3)
	// dedup 後に ID 降順 sort されていること。
	assert.Equal(t, "n3", out[0].ID)
	assert.Equal(t, "n2", out[1].ID)
	assert.Equal(t, "n1", out[2].ID)
}

// limit < 全件 のとき merged 結果が ID 降順先頭 limit 件に truncate される
// path を直接 cover する (#819 PR-fix)。home / local 計 5 件投入、limit=3
// で先頭 3 件だけが返ることを check。
func TestService_HybridDBFallback_LimitTruncate(t *testing.T) {
	testRedis.FlushAll(context.Background())
	repo := &splitTimelineRepo{
		MockNoteRepository: testutil.NewMockNoteRepository(),
		homeNotes: []*model.Note{
			{ID: "n5", UserID: "viewer", Visibility: model.NoteVisibilityPublic},
			{ID: "n4", UserID: "viewer", Visibility: model.NoteVisibilityPublic},
		},
		localNotes: []*model.Note{
			{ID: "n3", UserID: "outsider", Visibility: model.NoteVisibilityPublic},
			{ID: "n2", UserID: "outsider", Visibility: model.NoteVisibilityPublic},
			{ID: "n1", UserID: "outsider", Visibility: model.NoteVisibilityPublic},
		},
	}
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	svc := NewService(fanout, repo, testutil.NewMockFollowingRepository())

	// limit=3 < 全 5 件 → ID 降順で先頭 3 件 (n5/n4/n3) のみ。
	out, err := svc.HybridTimeline(context.Background(), &model.User{ID: "viewer"}, "", "", 3, TimelineFilter{})
	require.NoError(t, err)
	require.Len(t, out, 3)
	assert.Equal(t, "n5", out[0].ID)
	assert.Equal(t, "n4", out[1].ID)
	assert.Equal(t, "n3", out[2].ID)
}

// hybridDBFallback の error 分岐 cover: home query / local query それぞれの
// error を Service 層が透過することを check (#819 PR-fix)。
type failingHybridRepo struct {
	*testutil.MockNoteRepository
	failHome  bool
	failLocal bool
}

func (r *failingHybridRepo) ListHomeTimeline(_ string, _ int, _, _ string, _ model.TimelineDBFilter) ([]*model.Note, error) {
	if r.failHome {
		return nil, assert.AnError
	}
	return nil, nil
}
func (r *failingHybridRepo) ListLocalTimeline(_ int, _, _ string, _ model.TimelineDBFilter) ([]*model.Note, error) {
	if r.failLocal {
		return nil, assert.AnError
	}
	return nil, nil
}

func TestService_HybridDBFallback_HomeError(t *testing.T) {
	testRedis.FlushAll(context.Background())
	repo := &failingHybridRepo{MockNoteRepository: testutil.NewMockNoteRepository(), failHome: true}
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	svc := NewService(fanout, repo, testutil.NewMockFollowingRepository())
	_, err := svc.HybridTimeline(context.Background(), &model.User{ID: "viewer"}, "", "", 10, TimelineFilter{})
	assert.Error(t, err)
}

func TestService_HybridDBFallback_LocalError(t *testing.T) {
	testRedis.FlushAll(context.Background())
	repo := &failingHybridRepo{MockNoteRepository: testutil.NewMockNoteRepository(), failLocal: true}
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	svc := NewService(fanout, repo, testutil.NewMockFollowingRepository())
	_, err := svc.HybridTimeline(context.Background(), &model.User{ID: "viewer"}, "", "", 10, TimelineFilter{})
	assert.Error(t, err)
}

func TestService_HomeTimeline_DefaultLimit(t *testing.T) {
	svc, _, _ := newTestServiceWithRepo(t)
	out, err := svc.HomeTimeline(context.Background(), &model.User{ID: "u"}, "", "", 0, TimelineFilter{})
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestService_LocalTimeline_DefaultLimit(t *testing.T) {
	svc, _, _ := newTestServiceWithRepo(t)
	out, err := svc.LocalTimeline(context.Background(), nil, "", "", 0, TimelineFilter{})
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestService_GlobalTimeline_DefaultLimit(t *testing.T) {
	svc, _, _ := newTestServiceWithRepo(t)
	out, err := svc.GlobalTimeline(context.Background(), nil, "", "", 0, TimelineFilter{})
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestService_HybridTimeline_DefaultLimit(t *testing.T) {
	svc, _, _ := newTestServiceWithRepo(t)
	out, err := svc.HybridTimeline(context.Background(), &model.User{ID: "u"}, "", "", 0, TimelineFilter{})
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestService_ResolveError(t *testing.T) {
	// RedisにIDはあるがrepoのFindManyByIDsWithUserがエラー
	testRedis.FlushAll(context.Background())
	failRepo := &failingFindManyRepo{testutil.NewMockNoteRepository()}
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	svc := NewService(fanout, failRepo, testutil.NewMockFollowingRepository())
	ctx := context.Background()

	noteID := idGen.Generate(time.Now())
	require.NoError(t, fanout.Push(ctx, LocalTimeline, noteID, 100))
	_, err := svc.LocalTimeline(ctx, nil, "", "", 10, TimelineFilter{})
	assert.Error(t, err)
}

type failingFindManyRepo struct{ *testutil.MockNoteRepository }

func (f *failingFindManyRepo) FindManyByIDsWithUser(_ []string) ([]*model.Note, error) {
	return nil, assert.AnError
}

func TestMergeIDs_LimitClamping(t *testing.T) {
	out := mergeIDs([][]string{{"a", "b"}, {"c", "d"}}, 2, false)
	assert.Len(t, out, 2)
	// 文字列降順
	assert.Equal(t, "d", out[0])
	assert.Equal(t, "c", out[1])
}

// TestMergeIDs_Ascending は昇順ページング (sinceId 単独) で**最古 N 件**を
// 切り出すことを固定する (#2720)。降順に並べてから truncate すると、
// cursor の直後ではなく最新 N 件が返る。
func TestMergeIDs_Ascending(t *testing.T) {
	out := mergeIDs([][]string{{"a", "b"}, {"c", "d"}}, 2, true)
	assert.Equal(t, []string{"a", "b"}, out)
}

// toDBFilter は viewerID が non-empty のとき UseMutingSubquery=true を set
// する production 経路を担保する (#894)。anonymous viewer (= "") では
// subquery 経路を使わず literal MutedUserIDs もそのまま 0 件で SQL filter
// は no-op になる。
func TestToDBFilter_UseMutingSubquery(t *testing.T) {
	out := toDBFilter(TimelineFilter{}, "viewer1")
	assert.True(t, out.UseMutingSubquery, "viewerID 有なら subquery を使う")
	assert.Equal(t, "viewer1", out.ViewerID)

	out = toDBFilter(TimelineFilter{}, "")
	assert.False(t, out.UseMutingSubquery, "anonymous viewer では subquery off")
	assert.Equal(t, "", out.ViewerID)

	// MutedUserIDs literal は SQL 経路に**伝搬しない**ことを明示的に
	// 検証する (#894)。subquery 経路が production の SQL 経路で、literal
	// は test override 専用の design intent を regression guard として固定。
	out = toDBFilter(TimelineFilter{MutedUserIDs: []string{"u1", "u2"}}, "viewer2")
	assert.Empty(t, out.MutedUserIDs, "literal は意図的に drop し subquery に委譲する")

	// MutedChannelIDs / その他 filter はそのまま伝搬する (regression guard)。
	withReplies := true
	out = toDBFilter(TimelineFilter{
		MutedChannelIDs: []string{"ch1"},
		WithReplies:     &withReplies,
	}, "viewer3")
	assert.Equal(t, []string{"ch1"}, out.MutedChannelIDs)
	assert.NotNil(t, out.WithReplies)
	assert.True(t, *out.WithReplies)
}
