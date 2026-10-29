package notes

import (
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type localFederationActionObserver struct {
	available bool
	recorded  int
}

func (s *localFederationActionObserver) IsExternallyAvailable(*model.User) bool { return s.available }
func (s *localFederationActionObserver) ObserveExternalReference(string, string, string) (bool, error) {
	s.recorded++
	return true, nil
}

func TestReactionCreate_LocalOnlyAccountCannotReactToRemoteNote(t *testing.T) {
	h, notes, reactions := newReactionHandler(t)
	seedReactionNote(notes, "n1", "public")
	host := "remote.example"
	notes.Notes["n1"].UserHost = &host
	h.SetLocalFederationObserver(&localFederationActionObserver{})
	c, rec := newJSONRequest(t, "/api/notes/reactions/create", `{"noteId":"n1","reaction":"👍"}`)
	setAuthUser(c, &model.User{ID: "local", FederationStatus: model.FederationStatusOff})
	require.NoError(t, h.ReactionsCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, reactions.Reactions)
}

func TestPollVote_LocalOnlyAccountCannotVoteOnRemoteNote(t *testing.T) {
	h, notes, polls := newPollsHandler(t)
	seedPollNote(notes, polls, model.NoteVisibilityPublic, nil)
	host := "remote.example"
	notes.Notes["n1"].UserHost = &host
	h.SetLocalFederationObserver(&localFederationActionObserver{})
	c, rec := newJSONRequest(t, "/api/notes/polls/vote", `{"noteId":"n1","choice":1}`)
	setAuthUser(c, &model.User{ID: "local", FederationStatus: model.FederationStatusOff})
	require.NoError(t, h.PollsVote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, model.Int64Array{0, 0, 0}, polls.Polls["n1"].Votes)
}

func TestReactionCreate_AlreadyReactedDoesNotRecordExternalReference(t *testing.T) {
	h, notes, _ := newReactionHandler(t)
	seedReactionNote(notes, "n1", "public")
	host := "remote.example"
	notes.Notes["n1"].UserHost = &host
	user := &model.User{ID: "local", FederationStatus: model.FederationStatusOn}
	first, _ := newJSONRequest(t, "/api/notes/reactions/create", `{"noteId":"n1","reaction":"👍"}`)
	setAuthUser(first, user)
	require.NoError(t, h.ReactionsCreate(first))

	observer := &localFederationActionObserver{available: true}
	h.SetLocalFederationObserver(observer)
	second, rec := newJSONRequest(t, "/api/notes/reactions/create", `{"noteId":"n1","reaction":"👍"}`)
	setAuthUser(second, user)
	require.NoError(t, h.ReactionsCreate(second))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Zero(t, observer.recorded)
}

func TestPollVote_AlreadyVotedDoesNotRecordExternalReference(t *testing.T) {
	h, notes, polls := newPollsHandler(t)
	seedPollNote(notes, polls, model.NoteVisibilityPublic, nil)
	host := "remote.example"
	notes.Notes["n1"].UserHost = &host
	user := &model.User{ID: "local", FederationStatus: model.FederationStatusOn}
	first, _ := newJSONRequest(t, "/api/notes/polls/vote", `{"noteId":"n1","choice":0}`)
	setAuthUser(first, user)
	require.NoError(t, h.PollsVote(first))

	observer := &localFederationActionObserver{available: true}
	h.SetLocalFederationObserver(observer)
	second, rec := newJSONRequest(t, "/api/notes/polls/vote", `{"noteId":"n1","choice":1}`)
	setAuthUser(second, user)
	require.NoError(t, h.PollsVote(second))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Zero(t, observer.recorded)
}
