package users_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePerUserReader implements users.FeaturedRankingReader for
// users/featured-notes ranking-path tests.
type fakePerUserReader struct {
	perUser map[string][]string
	err     error
}

func (f *fakePerUserReader) GetPerUserNotesRanking(_ context.Context, userID string, _ int) ([]string, error) {
	return f.perUser[userID], f.err
}

func featRespIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var out []map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	ids := make([]string, len(out))
	for i, n := range out {
		ids[i] = n["id"].(string)
	}
	return ids
}

// ranking が wired なら per-user ranking を id DESC で返す (#1687)。
func TestFeaturedNotes_RankingPath(t *testing.T) {
	h, _, noteRepo := newExtraHandler(t)
	for _, fid := range []string{"a", "b", "c"} {
		noteRepo.Notes[fid] = &model.Note{ID: fid, UserID: "u1", Visibility: "public", User: &model.User{ID: "u1"}}
	}
	h.SetFeaturedRanking(&fakePerUserReader{perUser: map[string][]string{"u1": {"b", "a", "c"}}})
	rec := postExtra(h.FeaturedNotes, `{"userId":"u1"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"c", "b", "a"}, featRespIDs(t, rec.Body.Bytes()))
}

func TestFeaturedNotes_RankingUntilID(t *testing.T) {
	h, _, noteRepo := newExtraHandler(t)
	for _, fid := range []string{"a", "b", "c"} {
		noteRepo.Notes[fid] = &model.Note{ID: fid, UserID: "u1", Visibility: "public", User: &model.User{ID: "u1"}}
	}
	h.SetFeaturedRanking(&fakePerUserReader{perUser: map[string][]string{"u1": {"a", "b", "c"}}})
	rec := postExtra(h.FeaturedNotes, `{"userId":"u1","untilId":"c"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"b", "a"}, featRespIDs(t, rec.Body.Bytes()))
}

// ranking が空なら SQL count-DESC fallback (fresh instance / Redis flush)。
func TestFeaturedNotes_RankingEmptyFallsBackToSQL(t *testing.T) {
	h, _, noteRepo := newExtraHandler(t)
	noteRepo.Notes["sql"] = &model.Note{ID: "sql", UserID: "u1", Visibility: "public", User: &model.User{ID: "u1"}}
	h.SetFeaturedRanking(&fakePerUserReader{perUser: nil})
	rec := postExtra(h.FeaturedNotes, `{"userId":"u1"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"sql"}, featRespIDs(t, rec.Body.Bytes()))
}

func TestFeaturedNotes_RankingErrorFallsBackToSQL(t *testing.T) {
	h, _, noteRepo := newExtraHandler(t)
	noteRepo.Notes["sql"] = &model.Note{ID: "sql", UserID: "u1", Visibility: "public", User: &model.User{ID: "u1"}}
	h.SetFeaturedRanking(&fakePerUserReader{err: context.DeadlineExceeded})
	rec := postExtra(h.FeaturedNotes, `{"userId":"u1"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"sql"}, featRespIDs(t, rec.Body.Bytes()))
}

// untilId が ranking の全 ID を弾くと空配列を返す (SQL fallback には落ちない:
// ranking 自体は非空なため)。
func TestFeaturedNotes_RankingUntilIDFiltersAll(t *testing.T) {
	h, _, noteRepo := newExtraHandler(t)
	noteRepo.Notes["a"] = &model.Note{ID: "a", UserID: "u1", Visibility: "public", User: &model.User{ID: "u1"}}
	noteRepo.Notes["b"] = &model.Note{ID: "b", UserID: "u1", Visibility: "public", User: &model.User{ID: "u1"}}
	h.SetFeaturedRanking(&fakePerUserReader{perUser: map[string][]string{"u1": {"a", "b"}}})
	rec := postExtra(h.FeaturedNotes, `{"userId":"u1","untilId":"a"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, featRespIDs(t, rec.Body.Bytes()))
}

// ranking が limit より多くても limit 件に切り詰める。
func TestFeaturedNotes_RankingLimitCap(t *testing.T) {
	h, _, noteRepo := newExtraHandler(t)
	ranking := make([]string, 0, 12)
	for _, fid := range []string{"n01", "n02", "n03", "n04", "n05", "n06", "n07", "n08", "n09", "n10", "n11", "n12"} {
		noteRepo.Notes[fid] = &model.Note{ID: fid, UserID: "u1", Visibility: "public", User: &model.User{ID: "u1"}}
		ranking = append(ranking, fid)
	}
	h.SetFeaturedRanking(&fakePerUserReader{perUser: map[string][]string{"u1": ranking}})
	rec := postExtra(h.FeaturedNotes, `{"userId":"u1","limit":10}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	ids := featRespIDs(t, rec.Body.Bytes())
	assert.Len(t, ids, 10)
	assert.Equal(t, "n12", ids[0]) // id DESC top
	assert.NotContains(t, ids, "n01")
	assert.NotContains(t, ids, "n02")
}
