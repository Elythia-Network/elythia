package clip

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本家 2026.10.1 (#17658) の clip チャンネルへの発行 (#3809)。

type clipEvent struct{ clipID, eventType string }

type recordingClipPublisher struct {
	mu     sync.Mutex
	events []clipEvent
}

func (p *recordingClipPublisher) PublishClipEvent(clipID, eventType string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, clipEvent{clipID, eventType})
}

func (p *recordingClipPublisher) snapshot() []clipEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]clipEvent(nil), p.events...)
}

// fakeTimers stands in for time.AfterFunc: timers fire only when the test
// calls fire, and a stopped timer never fires (but its callback can still be
// invoked by hand to model a timer that fired before it was stopped).
type fakeTimers struct {
	timers []*fakeTimer
}

type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (ft *fakeTimers) afterFunc(d time.Duration, f func()) func() bool {
	t := &fakeTimer{d: d, f: f}
	ft.timers = append(ft.timers, t)
	return func() bool {
		was := !t.stopped
		t.stopped = true
		return was
	}
}

// live returns the timers that were not stopped.
func (ft *fakeTimers) live() []*fakeTimer {
	var out []*fakeTimer
	for _, t := range ft.timers {
		if !t.stopped {
			out = append(out, t)
		}
	}
	return out
}

func newPublishingSvc(t *testing.T) (*Service, *recordingClipPublisher, *fakeTimers, *testutil.MockClipRepository, *testutil.MockNoteRepository) {
	t.Helper()
	repo := testutil.NewMockClipRepository()
	noteRepo := testutil.NewMockClipNoteRepository()
	notes := testutil.NewMockNoteRepository()
	noteRepo.Notes = notes.Notes
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(repo, noteRepo, notes, idGen)
	pub := &recordingClipPublisher{}
	timers := &fakeTimers{}
	svc.SetStreamPublisher(pub)
	svc.afterFunc = timers.afterFunc
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1", Name: "one"}
	repo.Clips["c2"] = &model.Clip{ID: "c2", UserID: "u1", Name: "two"}
	return svc, pub, timers, repo, notes
}

func updated(id string) clipEvent { return clipEvent{id, "updated"} }

func TestPublishUpdated_LeadingThenTrailingAfterQuiet(t *testing.T) {
	svc, pub, timers, _, _ := newPublishingSvc(t)
	name := "renamed"

	_, err := svc.Update("u1", "c1", UpdateInput{Name: &name})
	require.NoError(t, err)
	assert.Equal(t, []clipEvent{updated("c1")}, pub.snapshot(), "先頭は即座に流す")
	require.Len(t, timers.live(), 1)
	assert.Equal(t, 3*time.Second, timers.live()[0].d)

	// 窓の中の更新は、その場では流さず、窓を張り直す。
	_, err = svc.Update("u1", "c1", UpdateInput{Name: &name})
	require.NoError(t, err)
	_, err = svc.Update("u1", "c1", UpdateInput{Name: &name})
	require.NoError(t, err)
	assert.Len(t, pub.snapshot(), 1)
	require.Len(t, timers.live(), 1, "張り直した窓だけが残る")

	timers.live()[0].f()
	assert.Equal(t, []clipEvent{updated("c1"), updated("c1")}, pub.snapshot(), "末尾で 1 回だけ流す")

	// 窓が閉じたら、次の更新はまた先頭として即座に流す。
	_, err = svc.Update("u1", "c1", UpdateInput{Name: &name})
	require.NoError(t, err)
	assert.Len(t, pub.snapshot(), 3)
}

func TestPublishUpdated_NoTrailingWithoutFurtherChanges(t *testing.T) {
	svc, pub, timers, _, _ := newPublishingSvc(t)
	name := "renamed"
	_, err := svc.Update("u1", "c1", UpdateInput{Name: &name})
	require.NoError(t, err)
	timers.live()[0].f()
	assert.Equal(t, []clipEvent{updated("c1")}, pub.snapshot(), "窓の中で更新が無ければ末尾は流さない")
	_, err = svc.Update("u1", "c1", UpdateInput{Name: &name})
	require.NoError(t, err)
	assert.Len(t, pub.snapshot(), 2, "窓が閉じているので先頭として流す")
}

// 張り直す前に発火してしまった古いタイマーは、窓を閉じない。
func TestPublishUpdated_StaleTimerIsIgnored(t *testing.T) {
	svc, pub, timers, _, _ := newPublishingSvc(t)
	name := "renamed"
	_, err := svc.Update("u1", "c1", UpdateInput{Name: &name})
	require.NoError(t, err)
	first := timers.timers[0]
	_, err = svc.Update("u1", "c1", UpdateInput{Name: &name})
	require.NoError(t, err)

	first.f()
	assert.Len(t, pub.snapshot(), 1, "古いタイマーは末尾を流さない")
	_, err = svc.Update("u1", "c1", UpdateInput{Name: &name})
	require.NoError(t, err)
	assert.Len(t, pub.snapshot(), 1, "窓はまだ開いている")

	timers.live()[0].f()
	assert.Equal(t, []clipEvent{updated("c1"), updated("c1")}, pub.snapshot())
}

func TestPublishUpdated_WindowsArePerClip(t *testing.T) {
	svc, pub, _, _, _ := newPublishingSvc(t)
	name := "renamed"
	_, err := svc.Update("u1", "c1", UpdateInput{Name: &name})
	require.NoError(t, err)
	_, err = svc.Update("u1", "c2", UpdateInput{Name: &name})
	require.NoError(t, err)
	assert.Equal(t, []clipEvent{updated("c1"), updated("c2")}, pub.snapshot())
}

func TestPublishUpdated_AddAndRemoveNote(t *testing.T) {
	svc, pub, timers, _, notes := newPublishingSvc(t)
	notes.Notes["n1"] = &model.Note{ID: "n1"}

	require.NoError(t, svc.AddNote("u1", "c1", "n1"))
	assert.Equal(t, []clipEvent{updated("c1")}, pub.snapshot())
	require.NoError(t, svc.RemoveNote("u1", "c1", "n1"))
	assert.Len(t, pub.snapshot(), 1, "窓の中")
	timers.live()[0].f()
	assert.Equal(t, []clipEvent{updated("c1"), updated("c1")}, pub.snapshot())
}

func TestPublishUpdated_NotOnFailure(t *testing.T) {
	svc, pub, _, _, _ := newPublishingSvc(t)
	name := "renamed"
	_, err := svc.Update("u2", "c1", UpdateInput{Name: &name})
	require.ErrorIs(t, err, ErrClipNotFound)
	require.ErrorIs(t, svc.AddNote("u1", "c1", "missing"), ErrNoteNotFound)
	require.ErrorIs(t, svc.RemoveNote("u1", "c1", "missing"), ErrNoteNotFound)
	require.ErrorIs(t, svc.Delete("u2", "c1"), ErrClipNotFound)
	assert.Empty(t, pub.snapshot())
}

func TestPublishDeleted_ClosesTheWindowAndPublishesAtOnce(t *testing.T) {
	svc, pub, timers, repo, _ := newPublishingSvc(t)
	name := "renamed"
	_, err := svc.Update("u1", "c1", UpdateInput{Name: &name})
	require.NoError(t, err)
	_, err = svc.Update("u1", "c1", UpdateInput{Name: &name})
	require.NoError(t, err)

	require.NoError(t, svc.Delete("u1", "c1"))
	assert.Equal(t, []clipEvent{updated("c1"), {"c1", "deleted"}}, pub.snapshot())
	assert.Empty(t, timers.live(), "窓のタイマーを止める")
	assert.NotContains(t, repo.Clips, "c1")

	// 止めそこねたタイマーが後から発火しても、末尾の updated は流さない。
	for _, tm := range timers.timers {
		tm.f()
	}
	assert.Len(t, pub.snapshot(), 2)

	// 窓が無いクリップの削除もすぐに流す。
	require.NoError(t, svc.Delete("u1", "c2"))
	assert.Equal(t, clipEvent{"c2", "deleted"}, pub.snapshot()[2])
}

type failingDeleteClipRepo struct {
	*testutil.MockClipRepository
}

func (failingDeleteClipRepo) Delete(*model.Clip) error { return errors.New("boom") }

func TestPublishDeleted_NotWhenDeleteFails(t *testing.T) {
	repo := testutil.NewMockClipRepository()
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(failingDeleteClipRepo{repo}, testutil.NewMockClipNoteRepository(), testutil.NewMockNoteRepository(), idGen)
	pub := &recordingClipPublisher{}
	svc.SetStreamPublisher(pub)
	require.Error(t, svc.Delete("u1", "c1"))
	assert.Empty(t, pub.snapshot())
}

func TestPublish_NoPublisherIsNoOp(t *testing.T) {
	repo := testutil.NewMockClipRepository()
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(repo, testutil.NewMockClipNoteRepository(), testutil.NewMockNoteRepository(), idGen)
	timers := &fakeTimers{}
	svc.afterFunc = timers.afterFunc
	name := "renamed"
	_, err := svc.Update("u1", "c1", UpdateInput{Name: &name})
	require.NoError(t, err)
	require.NoError(t, svc.Delete("u1", "c1"))
	assert.Empty(t, timers.timers, "publisher が無ければ窓も作らない")
}

// 実際のタイマー (time.AfterFunc) でも、窓の後に末尾が流れる。
func TestPublishUpdated_RealTimer(t *testing.T) {
	svc, pub, _, _, _ := newPublishingSvc(t)
	svc.afterFunc = nil
	name := "renamed"
	_, err := svc.Update("u1", "c1", UpdateInput{Name: &name})
	require.NoError(t, err)
	_, err = svc.Update("u1", "c1", UpdateInput{Name: &name})
	require.NoError(t, err)
	assert.Len(t, pub.snapshot(), 1)
	assert.Eventually(t, func() bool { return len(pub.snapshot()) == 2 }, UpdatedThrottle+5*time.Second, 50*time.Millisecond)
}

func TestService_HasStreamPublisher(t *testing.T) {
	assert.False(t, (&Service{}).HasStreamPublisher(), "未配線なら false")
	x := &Service{}
	x.SetStreamPublisher(&recordingClipPublisher{})
	assert.True(t, x.HasStreamPublisher(), "配線したら true")
}
