package users

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// #1636: users/notes が viewer の muted/blocked-user を post-fetch で除外する
// ことを検証する。ListByUserIDFiltered は visibility のみ push down するため、
// 以前は target の note が「muted user の renote」でもフィルタされず漏れていた。
func TestNotes_FiltersMutedRenoteAuthor(t *testing.T) {
	h, userRepo := newTestHandler(t)
	// target (プロフィール所有者)
	userRepo.Users["user1"] = &model.User{ID: "user1", Username: "u1", UsernameLower: "u1", AvatarDecorations: jsonArr()}
	// viewer
	viewer := &model.User{ID: "viewer", Username: "v", UsernameLower: "v", AvatarDecorations: jsonArr()}
	userRepo.Users["viewer"] = viewer

	mutingRepo := testutil.NewMockMutingRepository()
	blockingRepo := testutil.NewMockBlockingRepository()
	h.SetMutingRepo(mutingRepo)
	h.SetBlockingRepo(blockingRepo)
	h.SetUserRepo(userRepo)

	// viewer は muted1 を mute している
	require.NoError(t, mutingRepo.Create(&model.Muting{ID: "m1", MuterID: "viewer", MuteeID: "muted1"}))

	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	mutedAuthor := "muted1"
	// user1 の note のうち 1 つは muted1 の投稿を renote したもの
	noteRepo.Notes["n_renote_muted"] = &model.Note{
		ID: "n_renote_muted", UserID: "user1", Visibility: model.NoteVisibilityPublic,
		RenoteID: strPtr1636("rt1"), RenoteUserID: &mutedAuthor,
	}
	noteRepo.Notes["n_clean"] = &model.Note{
		ID: "n_clean", UserID: "user1", Visibility: model.NoteVisibilityPublic,
	}

	rec := postStub(h.Notes, `{"userId":"user1"}`, viewer)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	ids := map[string]bool{}
	for _, n := range out {
		ids[n["id"].(string)] = true
	}
	assert.False(t, ids["n_renote_muted"], "renote of a muted user must be filtered from users/notes (#1636)")
	assert.True(t, ids["n_clean"], "clean note must remain")
}

// blocked-user: target の note が blocker (viewer を block している user) を
// renote している場合も除外される。
func TestNotes_FiltersBlockerRenoteAuthor(t *testing.T) {
	h, userRepo := newTestHandler(t)
	userRepo.Users["user1"] = &model.User{ID: "user1", Username: "u1", UsernameLower: "u1", AvatarDecorations: jsonArr()}
	viewer := &model.User{ID: "viewer", Username: "v", UsernameLower: "v", AvatarDecorations: jsonArr()}
	userRepo.Users["viewer"] = viewer

	mutingRepo := testutil.NewMockMutingRepository()
	blockingRepo := testutil.NewMockBlockingRepository()
	h.SetMutingRepo(mutingRepo)
	h.SetBlockingRepo(blockingRepo)
	h.SetUserRepo(userRepo)

	// blocker が viewer を block している
	require.NoError(t, blockingRepo.Create(&model.Blocking{ID: "b1", BlockerID: "blocker", BlockeeID: "viewer"}))

	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	blocker := "blocker"
	noteRepo.Notes["n_renote_blocker"] = &model.Note{
		ID: "n_renote_blocker", UserID: "user1", Visibility: model.NoteVisibilityPublic,
		RenoteID: strPtr1636("rt2"), RenoteUserID: &blocker,
	}
	noteRepo.Notes["n_clean2"] = &model.Note{
		ID: "n_clean2", UserID: "user1", Visibility: model.NoteVisibilityPublic,
	}

	rec := postStub(h.Notes, `{"userId":"user1"}`, viewer)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	ids := map[string]bool{}
	for _, n := range out {
		ids[n["id"].(string)] = true
	}
	assert.False(t, ids["n_renote_blocker"], "renote of a user who blocks the viewer must be filtered (#1636)")
	assert.True(t, ids["n_clean2"], "clean note must remain")
}

func strPtr1636(s string) *string { return &s }

func jsonArr() datatypes.JSON { return datatypes.JSON([]byte("[]")) }
