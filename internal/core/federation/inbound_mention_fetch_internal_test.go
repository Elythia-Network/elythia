package federation

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// uriCountingFetcher serves documents by URI and counts fetches per URI.
type uriCountingFetcher struct {
	mu     sync.Mutex
	bodies map[string]string
	calls  map[string]int
}

func (f *uriCountingFetcher) FetchObject(uri string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[uri]++
	if b, ok := f.bodies[uri]; ok {
		return []byte(b), nil
	}
	return nil, errors.New("not found")
}

func testActorDoc(uri, username string) string {
	b, _ := json.Marshal(map[string]any{
		"@context":          "https://www.w3.org/ns/activitystreams",
		"id":                uri,
		"type":              "Person",
		"preferredUsername": username,
		"inbox":             uri + "/inbox",
		"publicKey": map[string]any{
			"id":           uri + "#main-key",
			"owner":        uri,
			"publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----",
		},
	})
	return string(b)
}

// Only an entry ingest (depth 0, stored in the DB) fetches unknown mentioned
// actors: a nested note (quote target, featured pin) or a relay note kept out
// of the DB does not (docs/divergence.md).
func TestIngestNote_UnknownMentionFetchOnlyAtEntry(t *testing.T) {
	const author = "https://remote.example/users/alice"
	const carol = "https://other.example/users/carol"
	for _, tc := range []struct {
		name      string
		depth     int
		ephemeral bool
		wantFetch int
	}{
		{"entry", 0, false, 1},
		{"nested", 1, false, 0},
		{"ephemeral", 0, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			host := "remote.example"
			uri := author
			now := time.Now()
			repo.Users["uAlice"] = &model.User{ID: "uAlice", Username: "alice", UsernameLower: "alice", Host: &host, URI: &uri, LastFetchedAt: &now}
			f := &uriCountingFetcher{bodies: map[string]string{
				author: testActorDoc(author, "alice"),
				carol:  testActorDoc(carol, "carol"),
			}, calls: map[string]int{}}
			idGen, _ := id.NewGenerator("aidx")
			r := NewResolver(repo, testutil.NewMockNoteRepository(), activitypub.NewURLBuilder("https://example.com"), f, idGen)
			if tc.ephemeral {
				r.SetEphemeralSink(ledgerSink{byURI: map[string]*model.Note{}})
			}
			body, err := json.Marshal(map[string]any{
				"@context":     "https://www.w3.org/ns/activitystreams",
				"id":           "https://remote.example/notes/e1",
				"type":         "Note",
				"attributedTo": author,
				"content":      "hi",
				"to":           []string{activitypub.Public},
				"tag":          []any{map[string]any{"type": "Mention", "href": carol}},
			})
			require.NoError(t, err)
			note, _, err := r.ingestNoteWithCreated(body, "", tc.depth, tc.ephemeral, nil)
			require.NoError(t, err)
			require.NotNil(t, note, "メンションの段まで到達していること")
			assert.Equal(t, tc.wantFetch, f.calls[carol])
		})
	}
}

// LocalUserIDFromURI judges locality by host, not by URL prefix, and parses
// `/users/<id>` like upstream ApDbResolverService.parseUri.
func TestResolver_LocalUserIDFromURI(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	r := NewResolver(testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(), activitypub.NewURLBuilder("https://example.com"), &uriCountingFetcher{}, idGen)
	for _, tc := range []struct {
		uri       string
		wantID    string
		wantLocal bool
	}{
		{"https://example.com/users/u1", "u1", true},
		{"http://example.com/users/u1", "u1", true},
		{"https://EXAMPLE.com/users/u1/followers", "u1", true},
		{"https://example.com/notes/n1", "", true},
		{"https://example.com/users", "", true},
		{"https://example.com/", "", true},
		{"https://remote.example/users/u1", "", false},
		{"not a uri", "", false},
	} {
		id, local := r.LocalUserIDFromURI(tc.uri)
		assert.Equal(t, tc.wantLocal, local, tc.uri)
		assert.Equal(t, tc.wantID, id, tc.uri)
	}
}

// RefreshActor re-fetches a stored actor and reports a failed fetch, unlike
// ForceResolveActor, and never creates a row.
func TestResolver_RefreshActor(t *testing.T) {
	const bob = "https://remote.example/users/bob"
	newR := func(t *testing.T, stored bool, body string) (*Resolver, *testutil.MockUserRepository, *uriCountingFetcher) {
		t.Helper()
		repo := testutil.NewMockUserRepository()
		if stored {
			host := "remote.example"
			uri := bob
			old := time.Now().Add(-time.Minute)
			repo.Users["uBob"] = &model.User{ID: "uBob", Username: "bob", UsernameLower: "bob", Host: &host, URI: &uri, LastFetchedAt: &old}
		}
		f := &uriCountingFetcher{bodies: map[string]string{}, calls: map[string]int{}}
		if body != "" {
			f.bodies[bob] = body
		}
		idGen, _ := id.NewGenerator("aidx")
		return NewResolver(repo, testutil.NewMockNoteRepository(), activitypub.NewURLBuilder("https://example.com"), f, idGen), repo, f
	}
	t.Run("updates the stored row", func(t *testing.T) {
		doc := map[string]any{}
		require.NoError(t, json.Unmarshal([]byte(testActorDoc(bob, "bob")), &doc))
		doc["name"] = "Bob Updated"
		b, _ := json.Marshal(doc)
		r, repo, f := newR(t, true, string(b))
		got, err := r.RefreshActor(bob)
		require.NoError(t, err)
		assert.Equal(t, "uBob", got.ID)
		require.NotNil(t, repo.Users["uBob"].Name)
		assert.Equal(t, "Bob Updated", *repo.Users["uBob"].Name)
		assert.GreaterOrEqual(t, f.calls[bob], 1)
	})
	t.Run("fetch failure is reported", func(t *testing.T) {
		r, _, _ := newR(t, true, "")
		_, err := r.RefreshActor(bob)
		require.Error(t, err)
	})
	t.Run("missing row is not created", func(t *testing.T) {
		r, repo, f := newR(t, false, testActorDoc(bob, "bob"))
		_, err := r.RefreshActor(bob)
		require.Error(t, err)
		assert.Empty(t, repo.Users)
		assert.Zero(t, f.calls[bob])
	})
	t.Run("local uri", func(t *testing.T) {
		r, _, _ := newR(t, false, "")
		_, err := r.RefreshActor("https://example.com/users/u1")
		require.ErrorIs(t, err, ErrLocalActor)
	})
}
