package clips

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

func TestNotes_AppliesHardMute(t *testing.T) {
	h, repo, clipNoteRepo, notes := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Profiles["alice"] = &model.UserProfile{UserID: "alice", HardMutedWords: datatypes.JSON([]byte(`[["muted"]]`))}
	h.SetUserRepo(userRepo)

	author := &model.User{ID: "author", Username: "author"}
	clean, muted, target := "clean ok", "this is muted", "target muted"
	now := time.Now()
	cleanNote := &model.Note{ID: h.idGen.Generate(now), UserID: author.ID, User: author, Text: &clean, Visibility: model.NoteVisibilityPublic}
	mutedNote := &model.Note{ID: h.idGen.Generate(now), UserID: author.ID, User: author, Text: &muted, Visibility: model.NoteVisibilityPublic}
	targetNote := &model.Note{ID: h.idGen.Generate(now), UserID: author.ID, User: author, Text: &target, Visibility: model.NoteVisibilityPublic}
	// 本文の無い純粋なrenoteは、renote先の本文で判定される。
	pureRenote := &model.Note{ID: h.idGen.Generate(now), UserID: author.ID, User: author, RenoteID: &targetNote.ID, Renote: targetNote, Visibility: model.NoteVisibilityPublic}
	notes.Notes[targetNote.ID] = targetNote
	for i, n := range []*model.Note{cleanNote, mutedNote, pureRenote} {
		notes.Notes[n.ID] = n
		cnID := "cn" + string(rune('0'+i))
		clipNoteRepo.Entries[cnID] = &model.ClipNote{ID: cnID, ClipID: "c1", NoteID: n.ID}
	}

	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1, rec.Body.String())
	assert.Equal(t, cleanNote.ID, out[0]["id"])
}
