package timeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEphemeral serves notes that only exist in Redis (#2332).
type stubEphemeral struct {
	notes    map[string]*model.Note
	err      error
	askedFor []string
}

func (s *stubEphemeral) GetNotes(_ context.Context, ids []string) ([]*model.Note, error) {
	s.askedFor = append(s.askedFor, ids...)
	if s.err != nil {
		return nil, s.err
	}
	out := make([]*model.Note, 0, len(ids))
	for _, id := range ids {
		if n, ok := s.notes[id]; ok {
			out = append(out, n)
		}
	}
	return out, nil
}

func ephNote(id string) *model.Note {
	host := "remote.example"
	return &model.Note{
		ID: id, UserID: "ru", UserHost: &host,
		Visibility: model.NoteVisibilityPublic,
		User:       &model.User{ID: "ru", Host: &host},
	}
}

// newMergeService builds a Service whose fanout returns the given IDs and
// whose note repo holds only dbIDs.
func newMergeService(t *testing.T, dbIDs []string) (*Service, *testutil.MockNoteRepository) {
	t.Helper()
	repo := testutil.NewMockNoteRepository()
	for _, id := range dbIDs {
		repo.Notes[id] = &model.Note{ID: id, UserID: "lu", Visibility: model.NoteVisibilityPublic}
	}
	return NewService(nil, repo, testutil.NewMockFollowingRepository()), repo
}

func TestResolve_MergesEphemeralNotes(t *testing.T) {
	svc, _ := newMergeService(t, []string{"b"})
	eph := &stubEphemeral{notes: map[string]*model.Note{"a": ephNote("a"), "c": ephNote("c")}}
	svc.SetEphemeralLookup(eph)

	got, _, err := svc.resolve(context.Background(), []string{"c", "b", "a"})
	require.NoError(t, err)
	require.Len(t, got, 3)
	// **入力 ids の順序を保つこと。** resolve は ID でソートし直さない (#2720)。
	// ここでは入力が降順なので結果も降順になる。
	assert.Equal(t, []string{"c", "b", "a"}, []string{got[0].ID, got[1].ID, got[2].ID})
	// DB で見つかった分は Redis に問い合わせない。
	assert.ElementsMatch(t, []string{"a", "c"}, eph.askedFor)
}

func TestResolve_NoEphemeralLookupWired(t *testing.T) {
	svc, _ := newMergeService(t, []string{"b"})

	got, _, err := svc.resolve(context.Background(), []string{"a", "b"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "b", got[0].ID)
}

// 全件 DB で見つかったら Redis は引かない (ホットパスの無駄打ち防止)。
func TestResolve_SkipsLookupWhenAllFound(t *testing.T) {
	svc, _ := newMergeService(t, []string{"a", "b"})
	eph := &stubEphemeral{notes: map[string]*model.Note{}}
	svc.SetEphemeralLookup(eph)

	got, _, err := svc.resolve(context.Background(), []string{"a", "b"})
	require.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Empty(t, eph.askedFor, "DB で揃っていれば Redis を引かない")
}

// Redis 障害で timeline 全体を落とさない。DB 分だけ返す。
func TestResolve_EphemeralErrorDegradesToDB(t *testing.T) {
	svc, _ := newMergeService(t, []string{"b"})
	svc.SetEphemeralLookup(&stubEphemeral{err: errors.New("redis down")})

	got, _, err := svc.resolve(context.Background(), []string{"a", "b"})
	require.NoError(t, err, "Redis 障害は error にせず縮退する")
	require.Len(t, got, 1)
	assert.Equal(t, "b", got[0].ID)
}

// ephemeral 側にも無い ID は単に落ちる (TTL 切れ後の dangling ID)。
func TestResolve_MissingEverywhere(t *testing.T) {
	svc, _ := newMergeService(t, []string{"b"})
	svc.SetEphemeralLookup(&stubEphemeral{notes: map[string]*model.Note{}})

	got, _, err := svc.resolve(context.Background(), []string{"a", "b"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "b", got[0].ID)
}

func TestResolve_EmptyIDs(t *testing.T) {
	svc, _ := newMergeService(t, nil)
	svc.SetEphemeralLookup(&stubEphemeral{})

	got, _, err := svc.resolve(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// ephemeral note も ApplyFilter を通ること (ブロック / ミュートが効く)。
func TestResolve_EphemeralNotesGoThroughFilter(t *testing.T) {
	svc, _ := newMergeService(t, nil)
	svc.SetEphemeralLookup(&stubEphemeral{notes: map[string]*model.Note{"a": ephNote("a")}})

	got, _, err := svc.resolve(context.Background(), []string{"a"})
	require.NoError(t, err)
	require.Len(t, got, 1)

	// ApplyFilter は *model.Note を受け取るので ephemeral でもそのまま通せる。
	filtered := ApplyFilter(got, "viewer", TimelineFilter{WithRenotes: boolPtr(false)})
	assert.NotNil(t, filtered)
}

// --- RemoveNoteID (ephemeral が DB 行に置き換わったときの後始末) ---

func newRemoveHook(t *testing.T) (*FanoutHook, *FanoutTimelineService) {
	t.Helper()
	testRedis.FlushAll(context.Background())
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	return NewFanoutHook(fanout, testutil.NewMockFollowingRepository()), fanout
}

func TestRemoveNoteID_DropsFromGlobalAndUserTimeline(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	h, fanout := newRemoveHook(t)
	ctx := context.Background()
	host := "remote.example"

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "ra", UserHost: &host, Visibility: model.NoteVisibilityPublic}
	author := &model.User{ID: "ra", Host: &host}
	h.OnNoteCreated(n, author)

	h.RemoveNoteID(noteID, author, string(model.NoteVisibilityPublic), &host)

	global, err := fanout.Get(ctx, GlobalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.NotContains(t, global, noteID)
	user, err := fanout.Get(ctx, UserTimelineName("ra"), "", "", 10)
	require.NoError(t, err)
	assert.NotContains(t, user, noteID)
}

// ローカル著者なら localTimeline からも除く。
func TestRemoveNoteID_DropsFromLocalTimelineForLocalAuthor(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	h, fanout := newRemoveHook(t)
	ctx := context.Background()

	noteID := idGen.Generate(time.Now())
	author := &model.User{ID: "la"}
	h.OnNoteCreated(&model.Note{ID: noteID, UserID: "la", Visibility: model.NoteVisibilityPublic}, author)

	h.RemoveNoteID(noteID, author, string(model.NoteVisibilityPublic), nil)

	local, err := fanout.Get(ctx, LocalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.NotContains(t, local, noteID)
}

func TestRemoveNoteID_NilSafe(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	h, _ := newRemoveHook(t)
	assert.NotPanics(t, func() {
		h.RemoveNoteID("", &model.User{ID: "x"}, "public", nil)
		h.RemoveNoteID("id", nil, "public", nil)
		var nilHook *FanoutHook
		nilHook.RemoveNoteID("id", &model.User{ID: "x"}, "public", nil)
	})
}
