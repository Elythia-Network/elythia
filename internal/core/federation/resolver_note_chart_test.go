package federation_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/model"
)

// TestResolver_NoteChartHook_Ephemeral pins that a note the resolver puts in
// the ephemeral store is still counted, as it was when the processor fired
// the hook (#3330), and that a URI already in the database is not counted.
func TestResolver_NoteChartHook_Ephemeral(t *testing.T) {
	doc := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "relay hello",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	r, sink, _, _ := ephResolverDocs(t, map[string]string{
		"https://remote.example/notes/1":     doc,
		"https://remote.example/users/alice": ephActorDoc,
	})
	hook := newRecordingNoteChartHook()
	r.SetNoteChartHook(hook)

	note, err := r.ResolveNoteEphemeral("https://remote.example/notes/1")
	require.NoError(t, err)
	require.Len(t, sink.notes, 1)
	assert.Equal(t, note.ID, hook.waitForOne(t))
	hook.expectNoMoreCalls(t, 100*time.Millisecond)
}

// TestResolver_NoteChartHook_ExistingRowNotCounted pins that resolving a note
// already in the database does not count it again.
func TestResolver_NoteChartHook_ExistingRowNotCounted(t *testing.T) {
	r, _, noteRepo, _ := ephResolver(t, ephActorDoc)
	uri := "https://remote.example/notes/1"
	noteRepo.Notes["existing"] = &model.Note{ID: "existing", UserID: "u1", URI: &uri}
	hook := newRecordingNoteChartHook()
	r.SetNoteChartHook(hook)

	got, err := r.ResolveNote(uri)
	require.NoError(t, err)
	assert.Equal(t, "existing", got.ID)
	hook.expectNoMoreCalls(t, 100*time.Millisecond)
}

// TestResolver_NoteChartHook_EphemeralThenDirectCountedOnce pins that a note
// first stored ephemerally (relay) and later delivered directly, which creates
// its database row and drops the ephemeral entry, is counted only once
// (#3330): the ephemeral store already counted it.
func TestResolver_NoteChartHook_EphemeralThenDirectCountedOnce(t *testing.T) {
	doc := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "relay hello",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	r, sink, noteRepo, _ := ephResolverDocs(t, map[string]string{
		"https://remote.example/notes/1":     doc,
		"https://remote.example/users/alice": ephActorDoc,
	})
	hook := newRecordingNoteChartHook()
	r.SetNoteChartHook(hook)

	_, err := r.ResolveNoteEphemeral("https://remote.example/notes/1")
	require.NoError(t, err)
	hook.waitForOne(t)

	note, created, err := r.IngestNoteWithCreated([]byte(doc), "https://remote.example/users/alice")
	require.NoError(t, err)
	require.True(t, created, "the direct delivery creates the database row")
	require.Contains(t, noteRepo.Notes, note.ID)
	assert.Empty(t, sink.notes, "the ephemeral entry is superseded")
	hook.expectNoMoreCalls(t, 200*time.Millisecond)
}
