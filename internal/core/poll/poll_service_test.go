package poll_test

import (
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/poll"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errStub = errors.New("stub error")

func newSvc(t *testing.T) (*poll.Service, *testutil.MockNoteRepository, *testutil.MockPollRepository, *testutil.MockPollVoteRepository) {
	t.Helper()
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	voteRepo := testutil.NewMockPollVoteRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := poll.NewService(noteRepo, pollRepo, voteRepo, nil, idGen)
	return svc, noteRepo, pollRepo, voteRepo
}

func seedPollNote(noteRepo *testutil.MockNoteRepository, pollRepo *testutil.MockPollRepository, noteID, userID string, multiple bool, expiresAt *time.Time) {
	noteRepo.Notes[noteID] = &model.Note{
		ID: noteID, UserID: userID, Visibility: model.NoteVisibilityPublic, HasPoll: true,
	}
	pollRepo.Polls[noteID] = &model.Poll{
		NoteID:    noteID,
		Multiple:  multiple,
		Choices:   model.StringArray{"A", "B", "C"},
		Votes:     model.Int64Array{0, 0, 0},
		ExpiresAt: expiresAt,
	}
}

func TestVote_NilUser(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	err := svc.Vote(nil, "n1", 0)
	require.Error(t, err)
}

func TestVote_NoteNotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	err := svc.Vote(&model.User{ID: "u"}, "ghost", 0)
	require.ErrorIs(t, err, poll.ErrNoteNotFound)
}

func TestVote_NotVisible(t *testing.T) {
	svc, noteRepo, _, _ := newSvc(t)
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "author", Visibility: model.NoteVisibilityFollowers, HasPoll: true,
	}
	err := svc.Vote(&model.User{ID: "viewer"}, "n1", 0)
	require.ErrorIs(t, err, poll.ErrNoteNotVisible)
}

func TestVote_NoPoll(t *testing.T) {
	svc, noteRepo, _, _ := newSvc(t)
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "author", Visibility: model.NoteVisibilityPublic, HasPoll: false,
	}
	err := svc.Vote(&model.User{ID: "viewer"}, "n1", 0)
	require.ErrorIs(t, err, poll.ErrNoPoll)
}

func TestVote_PollNotFoundForNote(t *testing.T) {
	svc, noteRepo, _, _ := newSvc(t)
	// HasPoll=true だが pollRepo にレコードが無い
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "author", Visibility: model.NoteVisibilityPublic, HasPoll: true,
	}
	err := svc.Vote(&model.User{ID: "viewer"}, "n1", 0)
	require.ErrorIs(t, err, poll.ErrNoPoll)
}

// stubBlockingChecker is a configurable BlockingChecker for the vote-blocking
// path (#1538). blocked/err はそのまま IsBlocked から返す。
type stubBlockingChecker struct {
	blocked bool
	err     error
}

func (s stubBlockingChecker) IsBlocked(_, _ string) (bool, error) { return s.blocked, s.err }

func TestVote_BlockedByAuthor(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	seedPollNote(noteRepo, pollRepo, "n1", "author", false, nil)
	svc.SetBlockingChecker(stubBlockingChecker{blocked: true})
	err := svc.Vote(&model.User{ID: "viewer"}, "n1", 0)
	require.ErrorIs(t, err, poll.ErrYouHaveBeenBlocked)
}

func TestVote_NotBlockedProceeds(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	seedPollNote(noteRepo, pollRepo, "n1", "author", false, nil)
	svc.SetBlockingChecker(stubBlockingChecker{blocked: false})
	err := svc.Vote(&model.User{ID: "viewer"}, "n1", 0)
	require.NoError(t, err, "ブロックされていなければ通常通り投票できる")
}

func TestVote_BlockingCheckerError(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	seedPollNote(noteRepo, pollRepo, "n1", "author", false, nil)
	svc.SetBlockingChecker(stubBlockingChecker{err: errStub})
	err := svc.Vote(&model.User{ID: "viewer"}, "n1", 0)
	require.ErrorIs(t, err, errStub)
}

func TestVote_SelfVoteSkipsBlockCheck(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	seedPollNote(noteRepo, pollRepo, "n1", "author", false, nil)
	// blocked=true でも自分の poll への投票は block 判定を skip する。
	svc.SetBlockingChecker(stubBlockingChecker{blocked: true})
	err := svc.Vote(&model.User{ID: "author"}, "n1", 0)
	require.NoError(t, err)
}

func TestVote_InvalidChoice(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	seedPollNote(noteRepo, pollRepo, "n1", "author", false, nil)
	err := svc.Vote(&model.User{ID: "viewer"}, "n1", 99)
	require.ErrorIs(t, err, poll.ErrInvalidChoice)

	err = svc.Vote(&model.User{ID: "viewer"}, "n1", -1)
	require.ErrorIs(t, err, poll.ErrInvalidChoice)
}

func TestVote_Expired(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	past := time.Now().Add(-1 * time.Hour)
	seedPollNote(noteRepo, pollRepo, "n1", "author", false, &past)

	err := svc.Vote(&model.User{ID: "viewer"}, "n1", 0)
	require.ErrorIs(t, err, poll.ErrPollExpired)
}

func TestVote_AlreadyVotedSameChoice(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	seedPollNote(noteRepo, pollRepo, "n1", "author", true, nil)

	require.NoError(t, svc.Vote(&model.User{ID: "viewer"}, "n1", 0))
	err := svc.Vote(&model.User{ID: "viewer"}, "n1", 0)
	require.ErrorIs(t, err, poll.ErrAlreadyVoted)
}

func TestVote_SingleAlreadyVoted(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	seedPollNote(noteRepo, pollRepo, "n1", "author", false, nil)

	require.NoError(t, svc.Vote(&model.User{ID: "viewer"}, "n1", 0))
	err := svc.Vote(&model.User{ID: "viewer"}, "n1", 1)
	require.ErrorIs(t, err, poll.ErrAlreadyVoted)
}

func TestVote_MultipleAllowed(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	seedPollNote(noteRepo, pollRepo, "n1", "author", true, nil)

	require.NoError(t, svc.Vote(&model.User{ID: "viewer"}, "n1", 0))
	require.NoError(t, svc.Vote(&model.User{ID: "viewer"}, "n1", 1))
	assert.Equal(t, int64(1), pollRepo.Polls["n1"].Votes[0])
	assert.Equal(t, int64(1), pollRepo.Polls["n1"].Votes[1])
}

// stubEventPub captures pollVoted events so we can assert the streaming path.
type stubEventPub struct {
	noteID    string
	eventType string
	body      any
	calls     int
}

func (s *stubEventPub) PublishToNoteStream(noteID, eventType string, body any) {
	s.calls++
	s.noteID = noteID
	s.eventType = eventType
	s.body = body
}

// stubFedHook captures federation hook fan-out so we can assert local vs
// remote routing.
type stubFedHook struct {
	voteCalls   int
	updateCalls int
	lastChoice  int
	lastName    string
	lastTarget  string
}

func (s *stubFedHook) OnVote(voter *model.User, target *model.Note, choice int, choiceName string) {
	s.voteCalls++
	s.lastChoice = choice
	s.lastName = choiceName
	s.lastTarget = target.ID
}
func (s *stubFedHook) OnLocalPollUpdated(target *model.Note) {
	s.updateCalls++
	s.lastTarget = target.ID
}

func TestVote_PublishesPollVotedEvent(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	seedPollNote(noteRepo, pollRepo, "n1", "author", false, nil)
	pub := &stubEventPub{}
	svc.SetEventPublisher(pub)

	require.NoError(t, svc.Vote(&model.User{ID: "viewer"}, "n1", 1))
	assert.Equal(t, 1, pub.calls)
	assert.Equal(t, "n1", pub.noteID)
	assert.Equal(t, "pollVoted", pub.eventType)
	body := pub.body.(map[string]any)
	assert.Equal(t, 1, body["choice"])
	assert.Equal(t, "viewer", body["userId"])
}

func TestVote_LocalPollFiresOnLocalPollUpdated(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	seedPollNote(noteRepo, pollRepo, "n_local", "author", false, nil)
	// note.UserHost is nil (default) -> local
	hook := &stubFedHook{}
	svc.SetFederationHook(hook)

	require.NoError(t, svc.Vote(&model.User{ID: "viewer"}, "n_local", 0))
	assert.Equal(t, 0, hook.voteCalls, "local poll must NOT trigger OnVote")
	assert.Equal(t, 1, hook.updateCalls, "local poll vote must trigger OnLocalPollUpdated")
	assert.Equal(t, "n_local", hook.lastTarget)
}

func TestVote_RemotePollFiresOnVote(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	host := "remote.example"
	noteRepo.Notes["n_remote"] = &model.Note{
		ID: "n_remote", UserID: "remoteUser", Visibility: model.NoteVisibilityPublic,
		HasPoll: true, UserHost: &host,
	}
	pollRepo.Polls["n_remote"] = &model.Poll{
		NoteID: "n_remote", Multiple: false,
		Choices: model.StringArray{"X", "Y"}, Votes: model.Int64Array{0, 0},
	}
	hook := &stubFedHook{}
	svc.SetFederationHook(hook)

	require.NoError(t, svc.Vote(&model.User{ID: "viewer"}, "n_remote", 1))
	assert.Equal(t, 1, hook.voteCalls, "remote poll vote must trigger OnVote")
	assert.Equal(t, 0, hook.updateCalls, "remote poll vote must NOT trigger OnLocalPollUpdated")
	assert.Equal(t, 1, hook.lastChoice)
	assert.Equal(t, "Y", hook.lastName)
}

// failingPollVoteRepo lets us trigger CountByUserAndNote / Create errors.
type failingPollVoteRepo struct {
	*testutil.MockPollVoteRepository
	failCount  bool
	failCreate bool
}

func (f *failingPollVoteRepo) CountByUserAndNote(userID, noteID string) (int64, error) {
	if f.failCount {
		return 0, errStub
	}
	return f.MockPollVoteRepository.CountByUserAndNote(userID, noteID)
}

func (f *failingPollVoteRepo) Create(v *model.PollVote) error {
	if f.failCreate {
		return errStub
	}
	return f.MockPollVoteRepository.Create(v)
}

func TestVote_CountError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	seedPollNote(noteRepo, pollRepo, "n1", "author", false, nil)
	idGen, _ := id.NewGenerator("aidx")
	voteRepo := &failingPollVoteRepo{MockPollVoteRepository: testutil.NewMockPollVoteRepository(), failCount: true}
	svc := poll.NewService(noteRepo, pollRepo, voteRepo, nil, idGen)
	err := svc.Vote(&model.User{ID: "viewer"}, "n1", 0)
	assert.ErrorIs(t, err, errStub)
}

func TestVote_CreateError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	seedPollNote(noteRepo, pollRepo, "n1", "author", true, nil)
	idGen, _ := id.NewGenerator("aidx")
	voteRepo := &failingPollVoteRepo{MockPollVoteRepository: testutil.NewMockPollVoteRepository(), failCreate: true}
	svc := poll.NewService(noteRepo, pollRepo, voteRepo, nil, idGen)
	err := svc.Vote(&model.User{ID: "viewer"}, "n1", 0)
	assert.ErrorIs(t, err, errStub)
}

// recordingHook captures vote notification.
type recordingHook struct {
	called   bool
	notifiee string
	notifier string
	noteID   string
	choice   int
}

func (h *recordingHook) OnPollVote(notifieeID, notifierID, noteID string, choice int) {
	h.called = true
	h.notifiee = notifieeID
	h.notifier = notifierID
	h.noteID = noteID
	h.choice = choice
}

// TestVote_DoesNotInvokeNotificationHook: Misskey TS には per-vote の
// notification 種別が無く (frontend が unknown type で空 body の通知を出して
// しまう) ため mk-go でも hook を呼ばない契約 (#690)。NotificationHook 自体は
// 別 path (例: pollEnded) で再利用される可能性があるので interface は残す。
func TestVote_DoesNotInvokeNotificationHook(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	seedPollNote(noteRepo, pollRepo, "n1", "author", false, nil)
	hook := &recordingHook{}
	svc.SetNotificationHook(hook)

	require.NoError(t, svc.Vote(&model.User{ID: "viewer"}, "n1", 0))
	assert.False(t, hook.called, "vote must not trigger pollVote notification (Misskey TS has no such type)")
}

func TestVote_HappyPathIncrementsVotes(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	seedPollNote(noteRepo, pollRepo, "n1", "author", false, nil)

	require.NoError(t, svc.Vote(&model.User{ID: "viewer"}, "n1", 1))
	assert.Equal(t, int64(1), pollRepo.Polls["n1"].Votes[1])
}

// #2106 L32: 期限切れ poll への範囲外 choice は invalidChoice より先に ALREADY_EXPIRED を返す。
func TestVote_ExpiredBeforeInvalidChoice(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	past := time.Now().Add(-time.Hour)
	seedPollNote(noteRepo, pollRepo, "n1", "author", false, &past)
	err := svc.Vote(&model.User{ID: "viewer"}, "n1", 99) // choice=99 は範囲外
	require.ErrorIs(t, err, poll.ErrPollExpired)
}
