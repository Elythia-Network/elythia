package notes

import (
	"context"
	"errors"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeFeaturedReader implements FeaturedRankingReader for notes/featured tests.
type fakeFeaturedReader struct {
	global      []string
	inCh        map[string][]string
	globalErr   error
	globalCalls int
}

func (f *fakeFeaturedReader) GetGlobalNotesRanking(_ context.Context, _ int) ([]string, error) {
	f.globalCalls++
	return f.global, f.globalErr
}

func (f *fakeFeaturedReader) GetInChannelNotesRanking(_ context.Context, channelID string, _ int) ([]string, error) {
	return f.inCh[channelID], nil
}

func featuredTestIDs(notes []*model.Note) []string {
	ids := make([]string, len(notes))
	for i, n := range notes {
		ids[i] = n.ID
	}
	return ids
}

func TestFeaturedNotes_GlobalRankingPath(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	for _, fid := range []string{"a", "b", "c"} {
		noteRepo.Notes[fid] = &model.Note{ID: fid, UserID: "u", Visibility: "public", User: &model.User{ID: "u"}}
	}
	h.SetFeaturedRanking(&fakeFeaturedReader{global: []string{"b", "a", "c"}})
	notes, err := h.featuredNotes(context.Background(), "", "", 10, 0, false)
	require.NoError(t, err)
	// ranking 集合を id DESC で返す (upstream featured.ts)。
	assert.Equal(t, []string{"c", "b", "a"}, featuredTestIDs(notes))
}

func TestFeaturedNotes_ChannelRankingPath(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	ch := "ch1"
	noteRepo.Notes["x"] = &model.Note{ID: "x", UserID: "u", Visibility: "public", ChannelID: &ch, User: &model.User{ID: "u"}}
	h.SetFeaturedRanking(&fakeFeaturedReader{inCh: map[string][]string{"ch1": {"x"}}})
	notes, err := h.featuredNotes(context.Background(), "ch1", "", 10, 0, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"x"}, featuredTestIDs(notes))
}

func TestFeaturedNotes_UntilIDFilter(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	for _, fid := range []string{"a", "b", "c"} {
		noteRepo.Notes[fid] = &model.Note{ID: fid, UserID: "u", Visibility: "public", User: &model.User{ID: "u"}}
	}
	h.SetFeaturedRanking(&fakeFeaturedReader{global: []string{"a", "b", "c"}})
	notes, err := h.featuredNotes(context.Background(), "", "c", 10, 0, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"b", "a"}, featuredTestIDs(notes))
}

func TestFeaturedNotes_EmptyRankingFallsBackToSQL(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["sql"] = &model.Note{ID: "sql", UserID: "u", Visibility: "public", User: &model.User{ID: "u"}}
	h.SetFeaturedRanking(&fakeFeaturedReader{global: nil})
	notes, err := h.featuredNotes(context.Background(), "", "", 10, 0, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"sql"}, featuredTestIDs(notes))
}

func TestFeaturedNotes_RankingErrorFallsBackToSQL(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["sql"] = &model.Note{ID: "sql", UserID: "u", Visibility: "public", User: &model.User{ID: "u"}}
	h.SetFeaturedRanking(&fakeFeaturedReader{globalErr: errors.New("redis down")})
	notes, err := h.featuredNotes(context.Background(), "", "", 10, 0, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"sql"}, featuredTestIDs(notes))
}

func TestFeaturedNotes_UnwiredUsesSQL(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["sql"] = &model.Note{ID: "sql", UserID: "u", Visibility: "public", User: &model.User{ID: "u"}}
	notes, err := h.featuredNotes(context.Background(), "", "", 10, 0, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"sql"}, featuredTestIDs(notes))
}

func TestCachedGlobalRanking_CachesWithinTTL(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	fake := &fakeFeaturedReader{global: []string{"a"}}
	h.SetFeaturedRanking(fake)
	_, err := h.cachedGlobalRanking(context.Background())
	require.NoError(t, err)
	_, err = h.cachedGlobalRanking(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, fake.globalCalls, "30分以内は cache され Redis を再呼び出ししない")
}

func TestSortAndPageFeaturedIDs(t *testing.T) {
	in := []string{"a", "c", "b"}
	out := sortAndPageFeaturedIDs(in, "", 10)
	assert.Equal(t, []string{"c", "b", "a"}, out)
	// 入力 slice を破壊しない (global cache 共有のため)。
	assert.Equal(t, []string{"a", "c", "b"}, in)
	assert.Equal(t, []string{"b", "a"}, sortAndPageFeaturedIDs([]string{"a", "b", "c"}, "c", 10))
	assert.Equal(t, []string{"c", "b"}, sortAndPageFeaturedIDs([]string{"a", "b", "c"}, "", 2))
}
