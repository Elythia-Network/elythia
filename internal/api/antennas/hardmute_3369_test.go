package antennas

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
	h, repo, noteRepo := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Profiles["alice"] = &model.UserProfile{UserID: "alice", HardMutedWords: datatypes.JSON([]byte(`[["muted"]]`))}
	h.SetUserRepo(userRepo)

	author := &model.User{ID: "author", Username: "author"}
	clean, muted, target := "clean ok", "this is muted", "target muted"
	now := time.Now()
	cleanNote := &model.Note{ID: idGen.Generate(now), UserID: author.ID, User: author, Text: &clean, Visibility: model.NoteVisibilityPublic}
	mutedNote := &model.Note{ID: idGen.Generate(now), UserID: author.ID, User: author, Text: &muted, Visibility: model.NoteVisibilityPublic}
	targetNote := &model.Note{ID: idGen.Generate(now), UserID: author.ID, User: author, Text: &target, Visibility: model.NoteVisibilityPublic}
	// 本文の無い純粋なrenoteは、renote先の本文で判定される。
	pureRenote := &model.Note{ID: idGen.Generate(now), UserID: author.ID, User: author, RenoteID: &targetNote.ID, Renote: targetNote, Visibility: model.NoteVisibilityPublic}
	for _, n := range []*model.Note{cleanNote, mutedNote, pureRenote} {
		noteRepo.Notes[n.ID] = n
		seedAntennaNote(t, "antennaTimeline:a1", n.ID)
	}

	c, rec := newReq(t, `{"antennaId":"a1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1, rec.Body.String())
	assert.Equal(t, cleanNote.ID, out[0]["id"])
}
