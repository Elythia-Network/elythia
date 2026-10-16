package users

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// seedHardMuteNotes3369 stores a clean note, a note containing "muted" and a
// pure renote of a note containing "muted", all authored by user1. It returns
// the clean note's ID.
func seedHardMuteNotes3369(t *testing.T, h *Handler, noteRepo *testutil.MockNoteRepository) string {
	t.Helper()
	author := &model.User{ID: "user1", Username: "testuser"}
	other := &model.User{ID: "other", Username: "other"}
	clean, muted, target := "clean ok", "this is muted", "target muted"
	now := time.Now()
	cleanNote := &model.Note{ID: h.idGen.Generate(now), UserID: author.ID, User: author, Text: &clean, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	mutedNote := &model.Note{ID: h.idGen.Generate(now), UserID: author.ID, User: author, Text: &muted, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	// renote先は別の利用者のノートにして、一覧そのものには載らないようにする。
	targetNote := &model.Note{ID: h.idGen.Generate(now), UserID: other.ID, User: other, Text: &target, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	// 本文の無い純粋なrenoteは、renote先の本文で判定される。
	pureRenote := &model.Note{ID: h.idGen.Generate(now), UserID: author.ID, User: author, RenoteID: &targetNote.ID, Renote: targetNote, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	for _, n := range []*model.Note{cleanNote, mutedNote, targetNote, pureRenote} {
		noteRepo.Notes[n.ID] = n
	}
	return cleanNote.ID
}

func setHardMuteRule3369(repo *testutil.MockUserRepository, viewerID string) {
	repo.Profiles[viewerID] = &model.UserProfile{UserID: viewerID, HardMutedWords: datatypes.JSON([]byte(`[["muted"]]`))}
}

func TestNotes_AppliesHardMute(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	h.SetUserRepo(repo)
	setHardMuteRule3369(repo, "viewer")
	cleanID := seedHardMuteNotes3369(t, h, h.noteRepo.(*testutil.MockNoteRepository))

	rec := postStub(h.Notes, `{"userId":"user1"}`, &model.User{ID: "viewer"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1, rec.Body.String())
	assert.Equal(t, cleanID, out[0]["id"])
}

func TestFeaturedNotes_AppliesHardMute(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	h.SetUserRepo(repo)
	setHardMuteRule3369(repo, "viewer")
	cleanID := seedHardMuteNotes3369(t, h, h.noteRepo.(*testutil.MockNoteRepository))

	rec := postStub(h.FeaturedNotes, `{"userId":"user1"}`, &model.User{ID: "viewer"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1, rec.Body.String())
	assert.Equal(t, cleanID, out[0]["id"])
}
