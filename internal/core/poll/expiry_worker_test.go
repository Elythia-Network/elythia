package poll

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingNotifier captures CreateInput calls so tests can verify the worker
// dispatches `pollEnded` to the right user set. Goroutine-safe because
// ExpiryWorker.Run の goroutine から writes が来る一方で test の assertion
// 経路 (require.Eventually の polling) が並行読みする — `-race` 検出を
// 防ぐため mu で保護する。
type recordingNotifier struct {
	mu    sync.Mutex
	calls []notification.CreateInput
	err   error
}

func (r *recordingNotifier) Create(_ context.Context, in notification.CreateInput) (*notification.Notification, error) {
	r.mu.Lock()
	r.calls = append(r.calls, in)
	err := r.err
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// Notification 自体は NotifieeID を直接持たず Redis 鍵で識別するため、
	// テストでは Type / NoteID のみ詰めて返す。
	return &notification.Notification{Type: in.Type, NoteID: in.NoteID}, nil
}

// snapshot returns a copy of calls under the mutex so tests can iterate
// safely without racing the writer goroutine.
func (r *recordingNotifier) snapshot() []notification.CreateInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]notification.CreateInput, len(r.calls))
	copy(out, r.calls)
	return out
}

func (r *recordingNotifier) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func newWorkerSetup(t *testing.T, now time.Time) (
	*ExpiryWorker, *testutil.MockPollRepository, *testutil.MockPollVoteRepository,
	*testutil.MockNoteRepository, *testutil.MockUserRepository, *recordingNotifier,
) {
	t.Helper()
	pollRepo := testutil.NewMockPollRepository()
	voteRepo := testutil.NewMockPollVoteRepository()
	noteRepo := testutil.NewMockNoteRepository()
	userRepo := testutil.NewMockUserRepository()
	notif := &recordingNotifier{}
	w := NewExpiryWorker(pollRepo, voteRepo, noteRepo, userRepo, notif, 60*time.Second, 100)
	w.SetClock(func() time.Time { return now })
	return w, pollRepo, voteRepo, noteRepo, userRepo, notif
}

func seedExpiredPoll(t *testing.T, pollRepo *testutil.MockPollRepository, noteRepo *testutil.MockNoteRepository, noteID, authorID string, expiresAt time.Time) {
	t.Helper()
	require.NoError(t, noteRepo.Create(&model.Note{ID: noteID, UserID: authorID, HasPoll: true, Visibility: model.NoteVisibilityPublic}))
	require.NoError(t, pollRepo.Create(&model.Poll{
		NoteID:    noteID,
		UserID:    authorID,
		Choices:   []string{"a", "b"},
		Votes:     []int64{0, 0},
		ExpiresAt: &expiresAt,
	}))
}

func seedLocalUser(userRepo *testutil.MockUserRepository, id string) {
	userRepo.Users[id] = &model.User{ID: id}
}

func seedRemoteUser(userRepo *testutil.MockUserRepository, id, host string) {
	userRepo.Users[id] = &model.User{ID: id, Host: &host}
}

func TestExpiryWorker_NotifiesAuthor(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	w, pollRepo, _, noteRepo, userRepo, notif := newWorkerSetup(t, now)
	seedLocalUser(userRepo, "alice")
	seedExpiredPoll(t, pollRepo, noteRepo, "n1", "alice", now.Add(-time.Minute))

	require.NoError(t, w.Tick(context.Background()))

	require.Len(t, notif.snapshot(), 1)
	assert.Equal(t, "alice", notif.snapshot()[0].NotifieeID)
	assert.Equal(t, notification.TypePollEnded, notif.snapshot()[0].Type)
	assert.Equal(t, "n1", notif.snapshot()[0].NoteID)
	assert.Equal(t, "public", notif.snapshot()[0].NoteVisibility)
	// notifiedAt is stamped so the next tick skips this poll.
	require.NotNil(t, pollRepo.Polls["n1"].NotifiedAt)
}

func TestExpiryWorker_NotifiesAuthorAndLocalVoters(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	w, pollRepo, voteRepo, noteRepo, userRepo, notif := newWorkerSetup(t, now)
	seedLocalUser(userRepo, "alice")
	seedLocalUser(userRepo, "bob")
	seedLocalUser(userRepo, "charlie")
	seedExpiredPoll(t, pollRepo, noteRepo, "n1", "alice", now.Add(-time.Minute))
	require.NoError(t, voteRepo.Create(&model.PollVote{ID: "v1", NoteID: "n1", UserID: "bob", Choice: 0}))
	require.NoError(t, voteRepo.Create(&model.PollVote{ID: "v2", NoteID: "n1", UserID: "charlie", Choice: 1}))

	require.NoError(t, w.Tick(context.Background()))

	notifiedIDs := []string{}
	for _, c := range notif.snapshot() {
		notifiedIDs = append(notifiedIDs, c.NotifieeID)
	}
	assert.ElementsMatch(t, []string{"alice", "bob", "charlie"}, notifiedIDs)
}

func TestExpiryWorker_DedupesAuthorWhoAlsoVoted(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	w, pollRepo, voteRepo, noteRepo, userRepo, notif := newWorkerSetup(t, now)
	seedLocalUser(userRepo, "alice")
	seedExpiredPoll(t, pollRepo, noteRepo, "n1", "alice", now.Add(-time.Minute))
	// alice votes on her own poll
	require.NoError(t, voteRepo.Create(&model.PollVote{ID: "v1", NoteID: "n1", UserID: "alice", Choice: 0}))

	require.NoError(t, w.Tick(context.Background()))
	// Only one notification despite alice being both author and voter.
	require.Len(t, notif.snapshot(), 1)
	assert.Equal(t, "alice", notif.snapshot()[0].NotifieeID)
}

func TestExpiryWorker_SkipsRemoteVoters(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	w, pollRepo, voteRepo, noteRepo, userRepo, notif := newWorkerSetup(t, now)
	seedLocalUser(userRepo, "alice")
	seedRemoteUser(userRepo, "bob_remote", "remote.example")
	seedExpiredPoll(t, pollRepo, noteRepo, "n1", "alice", now.Add(-time.Minute))
	require.NoError(t, voteRepo.Create(&model.PollVote{ID: "v1", NoteID: "n1", UserID: "bob_remote", Choice: 0}))

	require.NoError(t, w.Tick(context.Background()))

	// Only the local author gets a notification; remote voter is filtered.
	require.Len(t, notif.snapshot(), 1)
	assert.Equal(t, "alice", notif.snapshot()[0].NotifieeID)
}

func TestExpiryWorker_SkipsAlreadyNotified(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	w, pollRepo, _, noteRepo, userRepo, notif := newWorkerSetup(t, now)
	seedLocalUser(userRepo, "alice")
	seedExpiredPoll(t, pollRepo, noteRepo, "n1", "alice", now.Add(-time.Minute))
	notified := now.Add(-30 * time.Second)
	pollRepo.Polls["n1"].NotifiedAt = &notified

	require.NoError(t, w.Tick(context.Background()))

	assert.Empty(t, notif.snapshot(), "already-notified poll must be skipped")
}

func TestExpiryWorker_SkipsNotYetExpired(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	w, pollRepo, _, noteRepo, userRepo, notif := newWorkerSetup(t, now)
	seedLocalUser(userRepo, "alice")
	// expires in the future
	seedExpiredPoll(t, pollRepo, noteRepo, "n1", "alice", now.Add(time.Hour))

	require.NoError(t, w.Tick(context.Background()))

	assert.Empty(t, notif.snapshot())
	assert.Nil(t, pollRepo.Polls["n1"].NotifiedAt)
}

func TestExpiryWorker_TickWithNilWorker(t *testing.T) {
	var w *ExpiryWorker
	assert.NoError(t, w.Tick(context.Background()))
}

func TestExpiryWorker_TickWithUnconfiguredRepo(t *testing.T) {
	w := &ExpiryWorker{}
	assert.NoError(t, w.Tick(context.Background()))
}

func TestExpiryWorker_NotifierErrorContinuesBatch(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	w, pollRepo, _, noteRepo, userRepo, notif := newWorkerSetup(t, now)
	seedLocalUser(userRepo, "alice")
	seedLocalUser(userRepo, "bob")
	seedExpiredPoll(t, pollRepo, noteRepo, "n1", "alice", now.Add(-time.Minute))
	seedExpiredPoll(t, pollRepo, noteRepo, "n2", "bob", now.Add(-time.Minute))
	notif.err = errors.New("notify down")

	require.NoError(t, w.Tick(context.Background()))

	// Notifier was called for both polls (best-effort) and both polls were
	// stamped as notified so the next tick doesn't retry forever.
	assert.Len(t, notif.snapshot(), 2)
	require.NotNil(t, pollRepo.Polls["n1"].NotifiedAt)
	require.NotNil(t, pollRepo.Polls["n2"].NotifiedAt)
}

// TestExpiryWorker_Run_ProcessesBacklogThenStops verifies the loop runs the
// initial tick (catching up backlog from before startup) and exits cleanly
// when ctx is cancelled.
func TestExpiryWorker_Run_ProcessesBacklogThenStops(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	w, pollRepo, _, noteRepo, userRepo, notif := newWorkerSetup(t, now)
	w.interval = 50 * time.Millisecond
	seedLocalUser(userRepo, "alice")
	seedExpiredPoll(t, pollRepo, noteRepo, "n1", "alice", now.Add(-time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	require.Eventually(t, func() bool { return notif.callCount() >= 1 },
		2*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after ctx cancel")
	}
}

// TestExpiryWorker_Run_NilWorkerNoOp は nil receiver / 未配線 repo の
// Run() が即 return することを guard する (defensive: Service 構築直後で
// 配線途中のままだった場合に goroutine が pinned しない)。
func TestExpiryWorker_Run_NilWorkerNoOp(t *testing.T) {
	var w *ExpiryWorker
	w.Run(context.Background())
	w2 := &ExpiryWorker{}
	w2.Run(context.Background())
}

// TestNewExpiryWorker_DefaultsClampInterval covers the constructor's clamp
// logic: 非正値の interval / batchSize は安全な default に置き換える。
func TestNewExpiryWorker_DefaultsClampInterval(t *testing.T) {
	w := NewExpiryWorker(nil, nil, nil, nil, nil, 0, 0)
	require.NotNil(t, w)
	assert.Equal(t, 60*time.Second, w.interval)
	assert.Equal(t, 100, w.batchSize)

	w2 := NewExpiryWorker(nil, nil, nil, nil, nil, -1, -1)
	assert.Equal(t, 60*time.Second, w2.interval)
	assert.Equal(t, 100, w2.batchSize)
}

func TestExpiryWorker_ContextCancelStopsTick(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	w, pollRepo, _, noteRepo, userRepo, notif := newWorkerSetup(t, now)
	seedLocalUser(userRepo, "alice")
	seedExpiredPoll(t, pollRepo, noteRepo, "n1", "alice", now.Add(-time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := w.Tick(ctx)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, notif.snapshot())
}

// #739: Run の nil-receiver / unconfigured guard 分岐を踏む。loop 入る前に
// 早期 return することを確認 (Tick 経由ではなく Run 直呼び)。
func TestExpiryWorker_Run_NilWorkerReturns(t *testing.T) {
	var w *ExpiryWorker
	w.Run(context.Background()) // パニックせず即座に return
}

func TestExpiryWorker_Run_UnconfiguredRepoReturns(t *testing.T) {
	w := &ExpiryWorker{}
	w.Run(context.Background()) // パニックせず即座に return
}

// #739: notifyOne の nil-poll guard 分岐を踏む。
func TestExpiryWorker_NotifyOne_NilPoll(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	w, _, _, _, _, _ := newWorkerSetup(t, now)
	w.notifyOne(context.Background(), nil, now) // パニックせず no-op
}

// #739: Tick で ctx を予め cancel しておくと、batch loop の途中で ctx.Err()
// により early return する。
func TestExpiryWorker_Tick_CtxCancelMidBatch(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	w, pollRepo, _, noteRepo, userRepo, _ := newWorkerSetup(t, now)
	seedLocalUser(userRepo, "alice")
	seedExpiredPoll(t, pollRepo, noteRepo, "n1", "alice", now.Add(-time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 既に cancel 済み
	err := w.Tick(ctx)
	assert.Error(t, err) // ctx.Err() がそのまま返る
}
