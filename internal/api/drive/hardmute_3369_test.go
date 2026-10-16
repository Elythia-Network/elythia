package drive

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

// hardMuteNoteRepo3369 returns a fixed note list from ListByFileID.
type hardMuteNoteRepo3369 struct {
	*testutil.MockNoteRepository
	notes []*model.Note
}

func (r *hardMuteNoteRepo3369) ListByFileID(string, string, string, int) ([]*model.Note, error) {
	return r.notes, nil
}

func TestFilesAttachedNotes_AppliesHardMute(t *testing.T) {
	h, fileRepo, folderRepo := newHandler(t)
	author := &model.User{ID: "author", Username: "author"}
	clean, muted, target := "clean ok", "this is muted", "target muted"
	cleanNote := &model.Note{ID: h.idGen.Generate(time.Now()), UserID: author.ID, User: author, Text: &clean, Visibility: "public"}
	mutedNote := &model.Note{ID: h.idGen.Generate(time.Now()), UserID: author.ID, User: author, Text: &muted, Visibility: "public"}
	targetNote := &model.Note{ID: h.idGen.Generate(time.Now()), UserID: author.ID, User: author, Text: &target, Visibility: "public"}
	// 本文の無い純粋なrenoteは、renote先の本文で判定される。
	pureRenote := &model.Note{ID: h.idGen.Generate(time.Now()), UserID: author.ID, User: author, RenoteID: &targetNote.ID, Renote: targetNote, Visibility: "public"}
	h.SetRepos(fileRepo, folderRepo, &hardMuteNoteRepo3369{
		MockNoteRepository: testutil.NewMockNoteRepository(),
		notes:              []*model.Note{cleanNote, mutedNote, pureRenote},
	})
	userRepo := testutil.NewMockUserRepository()
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", HardMutedWords: datatypes.JSON([]byte(`[["muted"]]`))}
	h.SetUserRepo(userRepo)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid}

	c, rec := newJSONReq(t, `{"fileId":"f1"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesAttachedNotes(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1, rec.Body.String())
	assert.Equal(t, cleanNote.ID, out[0]["id"])
}
