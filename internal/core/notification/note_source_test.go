package notification

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sourceStreamPacker is a Packer + NoteSourcePacker that records the note it
// was handed, the way the streaming publisher embeds it.
type sourceStreamPacker struct {
	mu       sync.Mutex
	notes    []*model.Note
	plain    int
	loadErrs int
}

func (p *sourceStreamPacker) Pack(_ string, n *Notification) any {
	p.mu.Lock()
	p.plain++
	p.mu.Unlock()
	return n
}

func (p *sourceStreamPacker) PackWithNoteSource(_ string, n *Notification, src *NoteSource) any {
	note, err := src.Note()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.loadErrs++
		return n
	}
	p.notes = append(p.notes, note)
	return n
}

// loadedPushPacker is a NotePacker + LoadedNotePacker recording what the Web
// Push payload was packed from.
type loadedPushPacker struct {
	mu      sync.Mutex
	byID    int
	primary int
	loaded  []*model.Note
}

func (p *loadedPushPacker) PackNoteByIDOnPrimary(noteID, _ string) (map[string]any, bool) {
	p.mu.Lock()
	p.primary++
	p.mu.Unlock()
	return map[string]any{"id": noteID}, true
}

func (p *loadedPushPacker) PackNoteByID(noteID, _ string) (map[string]any, bool) {
	p.mu.Lock()
	p.byID++
	p.mu.Unlock()
	return map[string]any{"id": noteID}, true
}

func (p *loadedPushPacker) PackLoadedNote(n *model.Note, _ string) (map[string]any, bool) {
	p.mu.Lock()
	p.loaded = append(p.loaded, n)
	p.mu.Unlock()
	return map[string]any{"id": n.ID}, true
}

type countingLoader struct {
	calls atomic.Int32
	note  *model.Note
	err   error
}

func (l *countingLoader) load(id string) (*model.Note, error) {
	l.calls.Add(1)
	if l.err != nil {
		return nil, l.err
	}
	n := *l.note
	n.ID = id
	return &n, nil
}

func hydratedNote(id, userID string) *model.Note {
	return &model.Note{ID: id, UserID: userID, Visibility: model.NoteVisibilityPublic, User: &model.User{ID: userID}}
}

// existenceChecker answers the push's pre-send existence check.
type existenceChecker struct {
	mu      sync.Mutex
	deleted map[string]bool
	err     error
	checked [][]string
}

func (c *existenceChecker) ExistingNoteIDsOnPrimary(ids []string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checked = append(c.checked, append([]string(nil), ids...))
	if c.err != nil {
		return nil, c.err
	}
	var out []string
	for _, id := range ids {
		if !c.deleted[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

func wireSourceHook(t *testing.T) (*Hook, *sourceStreamPacker, *loadedPushPacker, *stubWebPushPublisher, *countingLoader) {
	h, stream, push, pub, loader, _ := wireSourceHookWithExistence(t)
	return h, stream, push, pub, loader
}

func wireSourceHookWithExistence(t *testing.T) (*Hook, *sourceStreamPacker, *loadedPushPacker, *stubWebPushPublisher, *countingLoader, *existenceChecker) {
	t.Helper()
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "carol", "carol")
	addLocalUser(repo, "dave", "dave")
	stream := &sourceStreamPacker{}
	svc.SetPacker(stream)
	push := &loadedPushPacker{}
	pub := &stubWebPushPublisher{}
	h.SetWebPushPublisher(pub)
	h.SetPackers(stubUserPacker{out: map[string]any{"id": "bob"}}, push)
	loader := &countingLoader{note: hydratedNote("x", "alice")}
	h.SetNoteLoader(loader.load)
	exists := &existenceChecker{deleted: map[string]bool{}}
	h.SetNoteExistenceChecker(exists)
	return h, stream, push, pub, loader, exists
}

// The stream and the Web Push payload of one notification read the note once
// between them (#3611; they used to read it once each).
func TestHook_NoteSource_StreamAndPushShareOneRead(t *testing.T) {
	h, stream, push, pub, loader := wireSourceHook(t)

	h.OnPollVote("alice", "bob", "n_vote", 1)

	require.Len(t, pub.calls, 1)
	assert.Equal(t, int32(1), loader.calls.Load())
	require.Len(t, stream.notes, 1)
	require.Len(t, push.loaded, 1)
	assert.Same(t, stream.notes[0], push.loaded[0])
	assert.Equal(t, "n_vote", push.loaded[0].ID)
	assert.Zero(t, push.byID, "push must not read the note again")
	assert.Zero(t, stream.plain, "stream must not read the note again")
}

// The reaction loader handed in by the reaction service is used instead of the
// Hook's own loader, once for both payloads.
func TestHook_OnReactionCreatedWithNote_UsesCallerLoader(t *testing.T) {
	h, stream, push, pub, loader := wireSourceHook(t)
	var calls atomic.Int32
	fresh := hydratedNote("n_react", "alice")
	h.OnReactionCreatedWithNote("alice", "bob", "n_react", "\U0001F44D", func() (*model.Note, error) {
		calls.Add(1)
		return fresh, nil
	})

	require.Len(t, pub.calls, 1)
	assert.Equal(t, "\U0001F44D", pub.calls[0].body["reaction"])
	assert.Equal(t, int32(1), calls.Load())
	assert.Zero(t, loader.calls.Load())
	require.Len(t, stream.notes, 1)
	assert.Same(t, fresh, stream.notes[0])
	require.Len(t, push.loaded, 1)
	assert.Same(t, fresh, push.loaded[0])
}

// A suppressed notification (muted notifier) never runs the loader.
func TestHook_OnReactionCreatedWithNote_SuppressedDoesNotLoad(t *testing.T) {
	h, _, _, pub, _ := wireSourceHook(t)
	h.SetMuteChecker(&stubMuteChecker{muted: true})
	var calls atomic.Int32
	h.OnReactionCreatedWithNote("alice", "bob", "n_react", "x", func() (*model.Note, error) {
		calls.Add(1)
		return hydratedNote("n_react", "alice"), nil
	})
	assert.Empty(t, pub.calls)
	assert.Zero(t, calls.Load())
}

// A failed shared read drops the note from the stream payload, and the push
// reads it again on its own as it did before #3611 (review L-4).
func TestHook_NoteSource_LoadErrorPushReadsAgain(t *testing.T) {
	h, stream, push, pub, loader := wireSourceHook(t)
	loader.err = errors.New("boom")

	h.OnPollVote("alice", "bob", "n_vote", 1)

	require.Len(t, pub.calls, 1)
	assert.Equal(t, int32(1), loader.calls.Load(), "the shared read is not retried")
	assert.Equal(t, 1, stream.loadErrs)
	assert.Empty(t, push.loaded)
	assert.Equal(t, 1, push.byID, "the push falls back to its own read")
	note, ok := pub.calls[0].body["note"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "n_vote", note["id"])
}

// A note deleted between the creation of the notification and the push is
// not put in the push, as when the push read it again before #3611; the
// notification itself is still pushed. Upstream sends the creation-time note
// as is; Elythia keeps its earlier behaviour (#3611 review M-1).
func TestHook_NoteSource_DeletedNoteIsNotPushed(t *testing.T) {
	h, stream, push, pub, loader, exists := wireSourceHookWithExistence(t)
	exists.deleted["n_vote"] = true

	h.OnPollVote("alice", "bob", "n_vote", 1)

	require.Len(t, pub.calls, 1, "the notification is still pushed")
	_, hasNote := pub.calls[0].body["note"]
	assert.False(t, hasNote, "the deleted note's content is not pushed")
	assert.Equal(t, "n_vote", pub.calls[0].body["noteId"])
	assert.Equal(t, [][]string{{"n_vote"}}, exists.checked)
	assert.Equal(t, int32(1), loader.calls.Load())
	require.Len(t, stream.notes, 1, "the realtime stream is sent at creation, before the deletion")
	assert.Empty(t, push.loaded)
	assert.Zero(t, push.byID, "the check replaces the full read")
}

// The check is made on every push that reuses the shared note, and a failed
// check drops the note (fail closed).
func TestHook_NoteSource_ExistenceCheckedBeforeEachPush(t *testing.T) {
	h, _, push, pub, _, exists := wireSourceHookWithExistence(t)
	n := hydratedNote("n_m3", "bob")
	n.Mentions = model.StringArray{"alice", "carol"}
	h.OnNoteCreated(n, &model.User{ID: "bob"}, nil, nil)
	require.Len(t, pub.calls, 2)
	assert.Equal(t, [][]string{{"n_m3"}, {"n_m3"}}, exists.checked)
	assert.Len(t, push.loaded, 2)

	exists.err = errors.New("primary down")
	h.OnPollVote("alice", "bob", "n_vote", 1)
	require.Len(t, pub.calls, 3)
	_, hasNote := pub.calls[2].body["note"]
	assert.False(t, hasNote)
	assert.Zero(t, push.byID)
}

// Without the existence check wired, the push does not reuse the shared note
// and reads it again itself, as before #3611.
func TestHook_NoteSource_UnwiredExistenceCheckReadsAgain(t *testing.T) {
	h, stream, push, pub, loader := wireSourceHook(t)
	h.SetNoteExistenceChecker(nil)

	h.OnPollVote("alice", "bob", "n_vote", 1)

	require.Len(t, pub.calls, 1)
	assert.Equal(t, int32(1), loader.calls.Load())
	require.Len(t, stream.notes, 1)
	assert.Empty(t, push.loaded)
	assert.Equal(t, 1, push.byID)
}

// The check also covers the embedded notes, in one query: a reply target or a
// (depth-2) renote deleted before the push makes the push read the note again,
// so the deleted embed's content is not summarised into it (#3611 review M-1').
// Self-referencing note FKs were dropped in 000080 (#2623), so deleting an
// embedded note does not delete the note that embeds it.
func TestHook_NoteSource_DeletedEmbedMakesPushReadAgain(t *testing.T) {
	id := func(s string) *string { return &s }
	full := func(nid string) *model.Note { return hydratedNote(nid, "carol") }
	for _, tc := range []struct {
		name    string
		deleted string
	}{
		{"reply target", "reply"},
		{"renote", "quote"},
		{"renote's renote", "quoted"},
		{"renote's reply", "quote-reply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, push, pub, loader, exists := wireSourceHookWithExistence(t)
			quote := full("quote")
			quote.RenoteID, quote.Renote = id("quoted"), full("quoted")
			quote.ReplyID, quote.Reply = id("quote-reply"), full("quote-reply")
			n := hydratedNote("n_e", "bob")
			n.Mentions = model.StringArray{"alice"}
			n.ReplyID, n.Reply = id("reply"), full("reply")
			n.RenoteID, n.Renote = id("quote"), quote
			exists.deleted[tc.deleted] = true

			h.OnNoteCreated(n, &model.User{ID: "bob"}, nil, nil)

			require.Len(t, pub.calls, 1)
			assert.Equal(t, [][]string{{"n_e", "reply", "quote", "quote-reply", "quoted"}}, exists.checked, "one query")
			assert.Empty(t, push.loaded, "the creation-time note is not pushed")
			assert.Equal(t, 1, push.primary, "the push reads the note again from the primary")
			assert.Zero(t, push.byID, "not from the replica")
			assert.Zero(t, loader.calls.Load())
		})
	}

	// 埋め込み先がすべてあれば、作成時の値を使う。
	h, _, push, _, _, exists := wireSourceHookWithExistence(t)
	n := hydratedNote("n_e2", "bob")
	n.Mentions = model.StringArray{"alice"}
	n.ReplyID, n.Reply = id("reply"), full("reply")
	h.OnNoteCreated(n, &model.User{ID: "bob"}, nil, nil)
	assert.Equal(t, [][]string{{"n_e2", "reply"}}, exists.checked)
	assert.Len(t, push.loaded, 1)
	assert.Zero(t, push.byID)
}

// A created note without its relations is not read at all when it produces no
// notification (#3611 review L-3'): the read is deferred to the first packer.
func TestHook_OnNoteCreated_NoRecipientsDoesNotLoad(t *testing.T) {
	h, _, _, pub, loader := wireSourceHook(t)
	n := &model.Note{ID: "n_alone", UserID: "bob", Visibility: model.NoteVisibilityPublic}

	h.OnNoteCreated(n, &model.User{ID: "bob"}, nil, nil)

	assert.Empty(t, pub.calls)
	assert.Zero(t, loader.calls.Load())
}

// Every notification a new note produces reuses the created note when it is
// hydrated: no read at all (#3611; each mention used to read it twice).
func TestHook_OnNoteCreated_ReusesHydratedNote(t *testing.T) {
	h, stream, push, pub, loader := wireSourceHook(t)
	n := hydratedNote("n_m", "bob")
	n.Mentions = model.StringArray{"alice", "carol", "dave"}

	h.OnNoteCreated(n, &model.User{ID: "bob"}, nil, nil)

	require.Len(t, pub.calls, 3)
	assert.Zero(t, loader.calls.Load())
	require.Len(t, stream.notes, 3)
	require.Len(t, push.loaded, 3)
	for i := range stream.notes {
		assert.Same(t, n, stream.notes[i])
		assert.Same(t, n, push.loaded[i])
	}
}

// A created note without its relations is read once for all of its
// notifications, not once per notification.
func TestHook_OnNoteCreated_UnhydratedNoteReadOnce(t *testing.T) {
	h, stream, push, pub, loader := wireSourceHook(t)
	n := &model.Note{ID: "n_m2", UserID: "bob", Visibility: model.NoteVisibilityPublic, Mentions: model.StringArray{"alice", "carol", "dave"}}

	h.OnNoteCreated(n, &model.User{ID: "bob"}, nil, nil)

	require.Len(t, pub.calls, 3)
	assert.Equal(t, int32(1), loader.calls.Load())
	require.Len(t, stream.notes, 3)
	require.Len(t, push.loaded, 3)
	assert.NotSame(t, n, stream.notes[0])
	for i := range stream.notes {
		assert.Same(t, stream.notes[0], stream.notes[i])
		assert.Same(t, stream.notes[0], push.loaded[i])
	}
}

// Without a note loader the packers read by id as before.
func TestHook_NoteSource_UnwiredLoaderFallsBack(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	stream := &sourceStreamPacker{}
	svc.SetPacker(stream)
	push := &loadedPushPacker{}
	pub := &stubWebPushPublisher{}
	h.SetWebPushPublisher(pub)
	h.SetPackers(stubUserPacker{}, push)

	h.OnPollVote("alice", "bob", "n_vote", 1)

	require.Len(t, pub.calls, 1)
	assert.Equal(t, 1, stream.plain)
	assert.Equal(t, 1, push.byID)
	assert.Empty(t, push.loaded)
}

func TestHydratedForPack(t *testing.T) {
	id := func(s string) *string { return &s }
	u := &model.User{ID: "u"}
	full := func(nid string) *model.Note { return &model.Note{ID: nid, User: u} }
	completeRenote := func() *model.Note {
		return &model.Note{ID: "q", User: u, RenoteID: id("t"), Renote: full("t"), ReplyID: id("p"), Reply: full("p")}
	}
	cases := []struct {
		name string
		n    *model.Note
		want bool
	}{
		{"nil", nil, false},
		{"no user", &model.Note{ID: "a"}, false},
		{"plain", &model.Note{ID: "a", User: u}, true},
		{"poll missing", &model.Note{ID: "a", User: u, HasPoll: true}, false},
		{"poll present", &model.Note{ID: "a", User: u, HasPoll: true, Poll: &model.Poll{}}, true},
		{"reply missing", &model.Note{ID: "a", User: u, ReplyID: id("r")}, false},
		{"reply author missing", &model.Note{ID: "a", User: u, ReplyID: id("r"), Reply: &model.Note{ID: "r"}}, false},
		{"reply present", &model.Note{ID: "a", User: u, ReplyID: id("r"), Reply: full("r")}, true},
		{"reply poll is not needed", &model.Note{ID: "a", User: u, ReplyID: id("r"), Reply: &model.Note{ID: "r", User: u, HasPoll: true}}, true},
		{"renote missing", &model.Note{ID: "a", User: u, RenoteID: id("q")}, false},
		{"renote author missing", &model.Note{ID: "a", User: u, RenoteID: id("q"), Renote: &model.Note{ID: "q"}}, false},
		{"renote poll missing", &model.Note{ID: "a", User: u, RenoteID: id("q"), Renote: &model.Note{ID: "q", User: u, HasPoll: true}}, false},
		{"renote depth-2 renote missing", &model.Note{ID: "a", User: u, RenoteID: id("q"), Renote: &model.Note{ID: "q", User: u, RenoteID: id("t")}}, false},
		{"renote depth-2 renote author missing", &model.Note{ID: "a", User: u, RenoteID: id("q"), Renote: &model.Note{ID: "q", User: u, RenoteID: id("t"), Renote: &model.Note{ID: "t"}}}, false},
		{"renote depth-2 renote poll missing", &model.Note{ID: "a", User: u, RenoteID: id("q"), Renote: &model.Note{ID: "q", User: u, RenoteID: id("t"), Renote: &model.Note{ID: "t", User: u, HasPoll: true}}}, false},
		{"renote depth-2 reply missing", &model.Note{ID: "a", User: u, RenoteID: id("q"), Renote: &model.Note{ID: "q", User: u, ReplyID: id("t")}}, false},
		{"renote depth-2 reply author missing", &model.Note{ID: "a", User: u, RenoteID: id("q"), Renote: &model.Note{ID: "q", User: u, ReplyID: id("t"), Reply: &model.Note{ID: "t"}}}, false},
		{"renote complete", &model.Note{ID: "a", User: u, RenoteID: id("q"), Renote: completeRenote()}, true},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, hydratedForPack(tc.n), tc.name)
	}
}

func TestNoteSource_Basics(t *testing.T) {
	var nilSrc *NoteSource
	_, err := nilSrc.Note()
	assert.Error(t, err)

	_, err = NewNoteSource(nil).Note()
	assert.Error(t, err)

	_, err = NewNoteSource(func() (*model.Note, error) { return nil, nil }).Note()
	assert.Error(t, err, "a nil note without an error is still no note")

	n := &model.Note{ID: "a"}
	got, err := LoadedNoteSource(n).Note()
	require.NoError(t, err)
	assert.Same(t, n, got)

	var calls atomic.Int32
	src := NewNoteSource(func() (*model.Note, error) { calls.Add(1); return n, nil })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = src.Note() }()
	}
	wg.Wait()
	assert.Equal(t, int32(1), calls.Load())

	h := &Hook{}
	assert.Nil(t, h.noteSourceFor("", nil))
	assert.Nil(t, h.noteSourceFor("a", nil), "unwired loader")
	given := LoadedNoteSource(n)
	assert.Same(t, given, h.noteSourceFor("a", given))
}
