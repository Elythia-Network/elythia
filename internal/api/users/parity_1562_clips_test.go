package users

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// users/clips の favoritedCount / isFavorited / notesCount 出し分け (#1562)。
func TestClips_FavoritedCountAndIsFavorited(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockClipRepository()
	require.NoError(t, repo.Create(&model.Clip{ID: "c1", UserID: "owner", Name: "pub", IsPublic: true}))
	h.SetClipRepo(repo)
	// notesCount は clip_note の実カウント (#2243)。owner 閲覧時のみ出る。
	clipNoteRepo := testutil.NewMockClipNoteRepository()
	for i := 0; i < 4; i++ {
		require.NoError(t, clipNoteRepo.Create(&model.ClipNote{
			ID: fmt.Sprintf("cn%d", i), ClipID: "c1", NoteID: fmt.Sprintf("n%d", i),
		}))
	}
	h.SetClipNoteRepo(clipNoteRepo)
	favRepo := testutil.NewMockClipFavoriteRepository()
	favRepo.Favorites["viewer:c1"] = &model.ClipFavorite{ID: "f1", UserID: "viewer", ClipID: "c1"}
	favRepo.Favorites["other:c1"] = &model.ClipFavorite{ID: "f2", UserID: "other", ClipID: "c1"}
	h.SetClipFavoriteRepo(favRepo)

	// 認証 viewer (非 owner): favoritedCount 実数 + isFavorited true、notesCount 省略
	rec := postStub(h.Clips, `{"userId":"owner"}`, &model.User{ID: "viewer"})
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.EqualValues(t, 2, rows[0]["favoritedCount"])
	assert.Equal(t, true, rows[0]["isFavorited"])
	_, hasNotesCount := rows[0]["notesCount"]
	assert.False(t, hasNotesCount, "notesCount must be omitted for non-owner viewers")

	// owner 閲覧: notesCount が出る
	rec = postStub(h.Clips, `{"userId":"owner"}`, &model.User{ID: "owner"})
	require.Equal(t, http.StatusOK, rec.Code)
	rows = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.EqualValues(t, 4, rows[0]["notesCount"])
	assert.Equal(t, false, rows[0]["isFavorited"])

	// 匿名: isFavorited 自体が出ない
	rec = postStub(h.Clips, `{"userId":"owner"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	rows = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	_, hasIsFavorited := rows[0]["isFavorited"]
	assert.False(t, hasIsFavorited, "isFavorited must be omitted for anonymous viewers")
	assert.EqualValues(t, 2, rows[0]["favoritedCount"])
}
