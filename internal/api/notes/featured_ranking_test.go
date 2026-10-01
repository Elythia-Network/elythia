package notes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
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
	notes, err := h.featuredNotes(context.Background(), nil, "", "", 10, 0, false)
	require.NoError(t, err)
	// ranking 集合を id DESC で返す (upstream featured.ts)。
	assert.Equal(t, []string{"c", "b", "a"}, featuredTestIDs(notes))
}

func TestFeaturedNotes_ChannelRankingPath(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	ch := "ch1"
	noteRepo.Notes["x"] = &model.Note{ID: "x", UserID: "u", Visibility: "public", ChannelID: &ch, User: &model.User{ID: "u"}}
	h.SetFeaturedRanking(&fakeFeaturedReader{inCh: map[string][]string{"ch1": {"x"}}})
	notes, err := h.featuredNotes(context.Background(), nil, "ch1", "", 10, 0, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"x"}, featuredTestIDs(notes))
}

func TestFeaturedNotes_UntilIDFilter(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	for _, fid := range []string{"a", "b", "c"} {
		noteRepo.Notes[fid] = &model.Note{ID: fid, UserID: "u", Visibility: "public", User: &model.User{ID: "u"}}
	}
	h.SetFeaturedRanking(&fakeFeaturedReader{global: []string{"a", "b", "c"}})
	notes, err := h.featuredNotes(context.Background(), nil, "", "c", 10, 0, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"b", "a"}, featuredTestIDs(notes))
}

func TestFeaturedNotes_EmptyRankingFallsBackToSQL(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["sql"] = &model.Note{ID: "sql", UserID: "u", Visibility: "public", User: &model.User{ID: "u"}}
	h.SetFeaturedRanking(&fakeFeaturedReader{global: nil})
	notes, err := h.featuredNotes(context.Background(), nil, "", "", 10, 0, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"sql"}, featuredTestIDs(notes))
}

func TestFeaturedNotes_RankingErrorFallsBackToSQL(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["sql"] = &model.Note{ID: "sql", UserID: "u", Visibility: "public", User: &model.User{ID: "u"}}
	h.SetFeaturedRanking(&fakeFeaturedReader{globalErr: errors.New("redis down")})
	notes, err := h.featuredNotes(context.Background(), nil, "", "", 10, 0, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"sql"}, featuredTestIDs(notes))
}

func TestFeaturedNotes_UnwiredUsesSQL(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["sql"] = &model.Note{ID: "sql", UserID: "u", Visibility: "public", User: &model.User{ID: "u"}}
	notes, err := h.featuredNotes(context.Background(), nil, "", "", 10, 0, false)
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

func TestSortAndFilterFeaturedIDs(t *testing.T) {
	in := []string{"a", "c", "b"}
	out := sortAndFilterFeaturedIDs(in, "")
	assert.Equal(t, []string{"c", "b", "a"}, out)
	// 入力 slice を破壊しない (global cache 共有のため)。
	assert.Equal(t, []string{"a", "c", "b"}, in)
	assert.Equal(t, []string{"b", "a"}, sortAndFilterFeaturedIDs([]string{"a", "b", "c"}, "c"))
}

// The ranking path cuts to limit only after the viewer's mute / block filters,
// as upstream featured.ts does since 2026.10.0: when the top-ranked note is by
// a muted author the page must still hold limit notes.
func TestFeatured_RankingFiltersMutedBeforeLimit(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	for _, n := range []struct{ id, user string }{{"d", "muted"}, {"c", "u"}, {"b", "u"}, {"a", "u"}} {
		noteRepo.Notes[n.id] = &model.Note{ID: n.id, UserID: n.user, Visibility: "public", User: &model.User{ID: n.user}}
	}
	mutingRepo := testutil.NewMockMutingRepository()
	mutingRepo.Mutings["m1"] = &model.Muting{ID: "m1", MuterID: "viewer", MuteeID: "muted"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["viewer"] = &model.User{ID: "viewer", Username: "viewer", UsernameLower: "viewer"}
	h.SetMutingRepo(mutingRepo)
	h.SetBlockingRepo(testutil.NewMockBlockingRepository())
	h.SetUserRepo(userRepo)
	h.SetFeaturedRanking(&fakeFeaturedReader{global: []string{"a", "b", "c", "d"}})

	rec := postExtra(h.Featured, `{"limit":2}`, &model.User{ID: "viewer"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	got := make([]string, len(resp))
	for i, r := range resp {
		got[i], _ = r["id"].(string)
	}
	assert.Equal(t, []string{"c", "b"}, got)
}
