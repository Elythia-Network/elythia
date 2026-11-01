package federation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

const notesCountNoteDoc = `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "https://remote.example/notes/1",
	"type": "Note",
	"attributedTo": "https://remote.example/users/alice",
	"content": "hello",
	"to": ["https://www.w3.org/ns/activitystreams#Public"]
}`

func notesCountOf(t *testing.T, repo *testutil.MockUserRepository, uri string) int {
	t.Helper()
	u, err := repo.FindByURI(uri)
	require.NoError(t, err)
	require.NotNil(t, u)
	return u.NotesCount
}

// TestIngestNote_IncrementsAuthorNotesCount pins that storing a remote note
// adds one to its author's notesCount, as upstream NoteCreateService does for
// remote notes too (#3928), and that a second delivery of the same note does
// not count it again.
func TestIngestNote_IncrementsAuthorNotesCount(t *testing.T) {
	r, _, _, userRepo := ephResolverDocs(t, map[string]string{
		"https://remote.example/notes/1":     notesCountNoteDoc,
		"https://remote.example/users/alice": ephActorDoc,
	})

	_, created, err := r.IngestNoteWithCreated([]byte(notesCountNoteDoc), "https://remote.example/users/alice")
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, 1, notesCountOf(t, userRepo, "https://remote.example/users/alice"))

	_, created, err = r.IngestNoteWithCreated([]byte(notesCountNoteDoc), "https://remote.example/users/alice")
	require.NoError(t, err)
	require.False(t, created, "the second delivery hits the existing row")
	assert.Equal(t, 1, notesCountOf(t, userRepo, "https://remote.example/users/alice"))
}

// TestIngestNote_EphemeralCountsNotesOnlyWhenStored pins that a note put in
// the ephemeral store is not counted, and that it is counted once when a
// direct delivery later stores it in the database (#3928).
func TestIngestNote_EphemeralCountsNotesOnlyWhenStored(t *testing.T) {
	alice := &model.User{
		ID: "ualice", Username: "alice",
		Host: strPtr("remote.example"), URI: strPtr("https://remote.example/users/alice"),
	}
	r, sink, _, userRepo := ephResolverDocs(t, map[string]string{
		"https://remote.example/notes/1":     notesCountNoteDoc,
		"https://remote.example/users/alice": ephActorDoc,
	})
	userRepo.Users[alice.ID] = alice

	_, err := r.ResolveNoteEphemeral("https://remote.example/notes/1")
	require.NoError(t, err)
	require.Len(t, sink.notes, 1)
	assert.Equal(t, 0, alice.NotesCount, "the ephemeral store does not count")

	_, created, err := r.IngestNoteWithCreated([]byte(notesCountNoteDoc), "https://remote.example/users/alice")
	require.NoError(t, err)
	require.True(t, created)
	assert.Empty(t, sink.notes, "the ephemeral entry is superseded")
	assert.Equal(t, 1, alice.NotesCount)
}

// TestIngestNote_DedupRaceDoesNotIncrementNotesCount pins that losing the
// insert race to another ingest of the same URI does not count the note.
func TestIngestNote_DedupRaceDoesNotIncrementNotesCount(t *testing.T) {
	existingURI := "https://remote.example/notes/1"
	winner := &model.Note{ID: "winner", URI: &existingURI, UserID: "ualice"}
	raceRepo := &raceConflictNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository(), existing: winner}
	userRepo := testutil.NewMockUserRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolverNilDeps(userRepo, raceRepo, activitypub.NewURLBuilder("https://example.com"),
		&stubFetcher{body: []byte(ephActorDoc)}, idGen)

	got, created, err := r.IngestNoteWithCreated([]byte(notesCountNoteDoc), "")
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, "winner", got.ID)
	assert.Equal(t, 0, notesCountOf(t, userRepo, "https://remote.example/users/alice"))
}

// TestProcess_AnnounceIncrementsAnnouncerNotesCount pins that a renote made
// from an inbound Announce adds one to the announcer's notesCount, including
// a self boost: upstream announceNote goes through NoteCreateService, whose
// notesCount increment has no condition (#3928).
func TestProcess_AnnounceIncrementsAnnouncerNotesCount(t *testing.T) {
	cases := []struct {
		name       string
		targetUser string
	}{
		{name: "boost of another user's note", targetUser: "bob"},
		{name: "self boost", targetUser: "ualice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newFullProcessor(t, aliceActor)
			alice := &model.User{
				ID: "ualice", Username: "alice",
				Host: strPtr("remote.example"), URI: strPtr("https://remote.example/users/alice"),
			}
			env.userRepo.Users[alice.ID] = alice
			env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: tc.targetUser, Visibility: model.NoteVisibilityPublic}
			body := []byte(`{
				"type": "Announce",
				"id": "https://remote.example/announces/notes-count",
				"actor": "https://remote.example/users/alice",
				"object": "https://example.com/notes/n1"
			}`)
			require.NoError(t, env.processor.Process(body))
			assert.Equal(t, 1, alice.NotesCount)
		})
	}
}

// TestProcess_AnnounceNotesCountSkipsDroppedAnnounces pins that an Announce
// that makes no renote, a second delivery of the same activity or one of a
// note the announcer may not renote, does not count (#3928).
func TestProcess_AnnounceNotesCountSkipsDroppedAnnounces(t *testing.T) {
	t.Run("second delivery", func(t *testing.T) {
		env := newFullProcessor(t, aliceActor)
		seedAnnouncer(env)
		seedOwnNote(env, "bob", model.NoteVisibilityPublic)
		body := selfAnnounceBody("notes-count-dup", `["https://www.w3.org/ns/activitystreams#Public"]`, `[]`)
		require.NoError(t, env.processor.Process(body))
		require.NoError(t, env.processor.Process(body))
		assert.Equal(t, 1, env.userRepo.Users["alice1"].NotesCount)
	})
	t.Run("followers note of another user", func(t *testing.T) {
		env := newFullProcessor(t, aliceActor)
		seedAnnouncer(env)
		seedOwnNote(env, "bob", model.NoteVisibilityFollowers)
		require.NoError(t, env.processor.Process(selfAnnounceBody("notes-count-gated",
			`["https://remote.example/users/alice/followers"]`, `[]`)))
		assert.Equal(t, 0, env.userRepo.Users["alice1"].NotesCount)
	})
}
