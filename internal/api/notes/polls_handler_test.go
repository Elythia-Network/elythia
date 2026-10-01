package notes

import (
	"net/http"
	"testing"
	"time"

	corenote "github.com/shiroha-a/mk/internal/core/note"
	corepoll "github.com/shiroha-a/mk/internal/core/poll"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newPollsHandler(t *testing.T) (*Handler, *testutil.MockNoteRepository, *testutil.MockPollRepository) {
	t.Helper()
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	voteRepo := testutil.NewMockPollVoteRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	pollSvc := corepoll.NewService(noteRepo, pollRepo, voteRepo, nil, idGen)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, pollSvc, nil, idGen)
	return h, noteRepo, pollRepo
}

func seedPollNote(noteRepo *testutil.MockNoteRepository, pollRepo *testutil.MockPollRepository, vis model.NoteVisibility, expiresAt *time.Time) {
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "author", Visibility: vis, HasPoll: true,
	}
	pollRepo.Polls["n1"] = &model.Poll{
		NoteID:    "n1",
		Multiple:  false,
		Choices:   model.StringArray{"A", "B", "C"},
		Votes:     model.Int64Array{0, 0, 0},
		ExpiresAt: expiresAt,
	}
}

func TestPollsVote_Success(t *testing.T) {
	h, noteRepo, pollRepo := newPollsHandler(t)
	seedPollNote(noteRepo, pollRepo, model.NoteVisibilityPublic, nil)

	c, rec := newJSONRequest(t, "/api/notes/polls/vote", `{"noteId":"n1","choice":1}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.PollsVote(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestPollsVote_InvalidParam(t *testing.T) {
	h, _, _ := newPollsHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/polls/vote", `{}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.PollsVote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPollsVote_InvalidJSON(t *testing.T) {
	h, _, _ := newPollsHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/polls/vote", `{invalid`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.PollsVote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPollsVote_NoteNotFound(t *testing.T) {
	h, _, _ := newPollsHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/polls/vote", `{"noteId":"ghost","choice":0}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.PollsVote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPollsVote_NotVisible(t *testing.T) {
	h, noteRepo, pollRepo := newPollsHandler(t)
	seedPollNote(noteRepo, pollRepo, model.NoteVisibilityFollowers, nil)
	c, rec := newJSONRequest(t, "/api/notes/polls/vote", `{"noteId":"n1","choice":0}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.PollsVote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// An invisible note must be indistinguishable from a missing one (upstream
	// vote.ts throws noSuchNote for both), so the whole body must match.
	cMissing, recMissing := newJSONRequest(t, "/api/notes/polls/vote", `{"noteId":"ghost","choice":0}`)
	setAuthUser(cMissing, &model.User{ID: "viewer"})
	require.NoError(t, h.PollsVote(cMissing))
	assert.Equal(t, recMissing.Code, rec.Code)
	assert.JSONEq(t, recMissing.Body.String(), rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"NO_SUCH_NOTE"`)
	assert.Contains(t, rec.Body.String(), `"id":"ecafbd2e-c283-4d6d-aecb-1a0a33b75396"`)
}

func TestPollsVote_InvalidChoice(t *testing.T) {
	h, noteRepo, pollRepo := newPollsHandler(t)
	seedPollNote(noteRepo, pollRepo, model.NoteVisibilityPublic, nil)
	c, rec := newJSONRequest(t, "/api/notes/polls/vote", `{"noteId":"n1","choice":99}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.PollsVote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPollsVote_Expired(t *testing.T) {
	h, noteRepo, pollRepo := newPollsHandler(t)
	past := time.Now().Add(-1 * time.Hour)
	seedPollNote(noteRepo, pollRepo, model.NoteVisibilityPublic, &past)
	c, rec := newJSONRequest(t, "/api/notes/polls/vote", `{"noteId":"n1","choice":0}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.PollsVote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPollsVote_AlreadyVoted(t *testing.T) {
	h, noteRepo, pollRepo := newPollsHandler(t)
	seedPollNote(noteRepo, pollRepo, model.NoteVisibilityPublic, nil)

	c1, _ := newJSONRequest(t, "/api/notes/polls/vote", `{"noteId":"n1","choice":0}`)
	setAuthUser(c1, &model.User{ID: "viewer"})
	require.NoError(t, h.PollsVote(c1))

	c2, rec := newJSONRequest(t, "/api/notes/polls/vote", `{"noteId":"n1","choice":1}`)
	setAuthUser(c2, &model.User{ID: "viewer"})
	require.NoError(t, h.PollsVote(c2))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPollsVote_NoPoll(t *testing.T) {
	h, noteRepo, _ := newPollsHandler(t)
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "author", Visibility: model.NoteVisibilityPublic, HasPoll: false,
	}
	c, rec := newJSONRequest(t, "/api/notes/polls/vote", `{"noteId":"n1","choice":0}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.PollsVote(c))
	// upstream noPoll は httpStatusCode 未指定 = 400 (#1765)。
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// upstream vote.ts は poll を持たない note に専用の NO_POLL を返す (#1538)。
	assert.Contains(t, rec.Body.String(), "NO_POLL")
	assert.Contains(t, rec.Body.String(), "5f979967-52d9-4314-a911-1c673727f92f")
}

// failingVoteRepo causes Create to fail.
type failingVoteRepo struct {
	*testutil.MockPollVoteRepository
}

func (f *failingVoteRepo) Create(_ *model.PollVote) error {
	return testutil.ErrNotFound
}

func TestPollsVote_RepoError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	seedPollNote(noteRepo, pollRepo, model.NoteVisibilityPublic, nil)
	voteRepo := &failingVoteRepo{MockPollVoteRepository: testutil.NewMockPollVoteRepository()}
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	pollSvc := corepoll.NewService(noteRepo, pollRepo, voteRepo, nil, idGen)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, pollSvc, nil, idGen)

	c, rec := newJSONRequest(t, "/api/notes/polls/vote", `{"noteId":"n1","choice":0}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.PollsVote(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}
