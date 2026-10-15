package stream

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// switchableNoteVisibility returns a fixed note and lets the test flip
// whether the viewer may still see it.
type switchableNoteVisibility struct {
	mu      sync.Mutex
	note    model.Note
	visible bool
	calls   int
}

func (s *switchableNoteVisibility) RequireVisible(_ *model.User, noteID string) (*model.Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if !s.visible {
		return nil, errors.New("not visible")
	}
	n := s.note
	n.ID = noteID
	return &n, nil
}

func (s *switchableNoteVisibility) set(visible bool) {
	s.mu.Lock()
	s.visible = visible
	s.mu.Unlock()
}

func (s *switchableNoteVisibility) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

const reactedEvent = `{"type":"reacted","body":{"reaction":":smile:","userId":"u1"}}`

func newRecheckDispatcher(t *testing.T, viewer *model.User, vis *switchableNoteVisibility) (*Dispatcher, *stubBus, *fakeConn, *Connection) {
	t.Helper()
	fc := newFakeConn()
	conn := NewConnection("test", viewer, fc)
	go conn.Start()
	t.Cleanup(conn.Close)
	bus := newStubBus()
	d := NewDispatcher(conn, nil, bus)
	d.SetNoteVisibilityChecker(vis)
	return d, bus, fc, conn
}

func noteSubscribed(d *Dispatcher, bus *stubBus, noteID string) bool {
	bus.mu.Lock()
	_, onBus := bus.subs["noteStream:"+noteID]
	bus.mu.Unlock()
	d.noteSubMu.Lock()
	_, counted := d.noteSubs[noteID]
	d.noteSubMu.Unlock()
	return onBus && counted
}

func TestDispatcher_FollowersNote_FollowerViaSnapshotSkipsDB(t *testing.T) {
	vis := &switchableNoteVisibility{note: model.Note{UserID: "author", Visibility: model.NoteVisibilityFollowers}, visible: true}
	d, bus, fc, conn := newRecheckDispatcher(t, &model.User{ID: "viewer"}, vis)
	conn.SetFollowingSnapshot(map[string]bool{"author": false})

	d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))
	require.True(t, noteSubscribed(d, bus, "n1"))
	bus.deliver("noteStream:n1", []byte(reactedEvent))

	require.Eventually(t, func() bool { return fc.writeCount() == 1 }, time.Second, 5*time.Millisecond)
	assert.Equal(t, 1, vis.callCount(), "a follower listed in the snapshot must not hit the DB per event")
}

func TestDispatcher_FollowersNote_UnfollowStopsEventsAndDropsSub(t *testing.T) {
	vis := &switchableNoteVisibility{note: model.Note{UserID: "author", Visibility: model.NoteVisibilityFollowers}, visible: true}
	d, bus, fc, conn := newRecheckDispatcher(t, &model.User{ID: "viewer"}, vis)
	conn.SetFollowingSnapshot(map[string]bool{"author": false})
	d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))
	// 2 回 subNote しても (refcount 2)、見えなくなったら購読ごと外す。
	d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))

	// ブロックやフォロー解除でフォローが外れた。
	conn.UpdateFollowingSnapshot("author", false)
	vis.set(false)
	bus.deliver("noteStream:n1", []byte(reactedEvent))

	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, fc.writeCount(), "events must stop once the viewer no longer follows the author")
	assert.False(t, noteSubscribed(d, bus, "n1"), "the subscription must be dropped")
	d.noteSubMu.Lock()
	_, gated := d.noteFollowGates["n1"]
	d.noteSubMu.Unlock()
	assert.False(t, gated)

	// 外れた後の unsubNote は何もしない。
	d.HandleClientMessage("un", json.RawMessage(`{"id":"n1"}`))
	calls := vis.callCount()
	bus.deliver("noteStream:n1", []byte(reactedEvent))
	assert.Equal(t, calls, vis.callCount(), "a dropped subscription must not keep querying the DB")
}

func TestDispatcher_FollowersNote_NotInSnapshotFallsBackToDB(t *testing.T) {
	// メンションされた非フォロワーや、一覧の上限を超えた分はDBの判定で通す。
	vis := &switchableNoteVisibility{note: model.Note{UserID: "author", Visibility: model.NoteVisibilityFollowers}, visible: true}
	d, bus, fc, conn := newRecheckDispatcher(t, &model.User{ID: "viewer"}, vis)
	conn.SetFollowingSnapshot(map[string]bool{})
	d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))

	bus.deliver("noteStream:n1", []byte(reactedEvent))
	require.Eventually(t, func() bool { return fc.writeCount() == 1 }, time.Second, 5*time.Millisecond)
	assert.Equal(t, 2, vis.callCount())
	assert.True(t, noteSubscribed(d, bus, "n1"))
}

func TestDispatcher_NoteRecheck_NotNeeded(t *testing.T) {
	cases := []struct {
		name   string
		viewer *model.User
		note   model.Note
	}{
		{name: "public note", viewer: &model.User{ID: "viewer"}, note: model.Note{UserID: "author", Visibility: model.NoteVisibilityPublic}},
		{name: "home note", viewer: &model.User{ID: "viewer"}, note: model.Note{UserID: "author", Visibility: model.NoteVisibilityHome}},
		{name: "specified note", viewer: &model.User{ID: "viewer"}, note: model.Note{UserID: "author", Visibility: model.NoteVisibilitySpecified}},
		{name: "own followers note", viewer: &model.User{ID: "author"}, note: model.Note{UserID: "author", Visibility: model.NoteVisibilityFollowers}},
		{name: "anonymous public note", viewer: nil, note: model.Note{UserID: "author", Visibility: model.NoteVisibilityPublic}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vis := &switchableNoteVisibility{note: tc.note, visible: true}
			d, bus, fc, _ := newRecheckDispatcher(t, tc.viewer, vis)
			d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))
			// 判定が変わっても、見直さないものは届き続ける。
			vis.set(false)
			bus.deliver("noteStream:n1", []byte(reactedEvent))

			require.Eventually(t, func() bool { return fc.writeCount() == 1 }, time.Second, 5*time.Millisecond)
			assert.Equal(t, 1, vis.callCount(), "no per-event re-check for notes whose visibility cannot change")
		})
	}
}

func TestDispatcher_NoteFollowGates_ClearedOnUnsubEvictAndClose(t *testing.T) {
	vis := &switchableNoteVisibility{note: model.Note{UserID: "author", Visibility: model.NoteVisibilityFollowers}, visible: true}
	d, _, _, _ := newRecheckDispatcher(t, &model.User{ID: "viewer"}, vis)
	gates := func() int {
		d.noteSubMu.Lock()
		defer d.noteSubMu.Unlock()
		return len(d.noteFollowGates)
	}

	d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))
	require.Equal(t, 1, gates())
	d.HandleClientMessage("un", json.RawMessage(`{"id":"n1"}`))
	assert.Equal(t, 0, gates(), "unsubNote")

	for i := 0; i <= maxNoteSubsPerConnection; i++ {
		d.HandleClientMessage("s", json.RawMessage(`{"id":"n`+string(rune('a'+i%26))+string(rune('a'+i/26))+`"}`))
	}
	assert.Equal(t, maxNoteSubsPerConnection, gates(), "eviction")

	d.CloseAll()
	assert.Equal(t, 0, gates(), "CloseAll")
}

func TestDispatcher_DropNoteSub_Unknown(t *testing.T) {
	d := NewDispatcher(NewConnection("test", &model.User{ID: "viewer"}, newFakeConn()), nil, newStubBus())
	d.dropNoteSub("missing") // no panic
}

func TestNeedsFollowRecheck_NilInputs(t *testing.T) {
	assert.False(t, needsFollowRecheck(nil, &model.Note{Visibility: model.NoteVisibilityFollowers}))
	assert.False(t, needsFollowRecheck(&model.User{ID: "v"}, nil))
}
