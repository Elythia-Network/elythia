package notes

import (
	"net/http"
	"testing"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubModChecker struct{ mods map[string]bool }

func (s stubModChecker) IsModerator(id string) bool { return s.mods[id] }

func newDeleteHandler(mods map[string]bool) (*Handler, *testutil.MockNoteRepository) {
	noteRepo := testutil.NewMockNoteRepository()
	svc := corenote.NewDeleteService(noteRepo)
	svc.SetUserRepo(testutil.NewMockUserRepository())
	return &Handler{deleteService: svc, moderatorChecker: stubModChecker{mods: mods}}, noteRepo
}

// #1538: moderator/admin can delete other users' notes.
func TestNotesDelete_ModeratorDeletesOthers(t *testing.T) {
	h, noteRepo := newDeleteHandler(map[string]bool{"mod": true})
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author"}
	rec := postDraft(h.Delete, `{"noteId":"n1"}`, &model.User{ID: "mod"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, noteRepo.Notes)
}

func TestNotesDelete_NonModeratorForbidden(t *testing.T) {
	h, noteRepo := newDeleteHandler(map[string]bool{})
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author"}
	rec := postDraft(h.Delete, `{"noteId":"n1"}`, &model.User{ID: "intruder"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Len(t, noteRepo.Notes, 1, "non-moderator non-author must not delete")
}

func TestNotesDelete_AuthorDeletesOwn(t *testing.T) {
	h, noteRepo := newDeleteHandler(map[string]bool{})
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author"}
	rec := postDraft(h.Delete, `{"noteId":"n1"}`, &model.User{ID: "author"})
	require.Equal(t, http.StatusNoContent, rec.Code)
}
