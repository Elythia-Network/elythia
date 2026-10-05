package federation_test

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// customFollowersActor is an actor whose followers collection is not
// uri + "/followers".
const customFollowersActor = `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "https://remote.example/author/carol",
	"type": "Person",
	"preferredUsername": "carol",
	"inbox": "https://remote.example/author/carol/inbox",
	"followers": "https://remote.example/wp-json/actors/1/followers",
	"publicKey": {"publicKeyPem": "FAKE"}
}`

// #3330: the actor's followers collection is stored as followersUri (upstream
// ApPersonService.createPerson) and an inbound note addressed to it is a
// followers note, while one addressed to uri + "/followers" is not.
func TestIngestNote_FollowersMatchedAgainstStoredFollowersURI(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(customFollowersActor)}, idGen)

	actor, err := r.ResolveActor("https://remote.example/author/carol")
	require.NoError(t, err)
	require.NotNil(t, actor.FollowersURI, "followersUri を保存する")
	assert.Equal(t, "https://remote.example/wp-json/actors/1/followers", *actor.FollowersURI)

	note := func(id, to string) []byte {
		return []byte(`{ "@context": "https://www.w3.org/ns/activitystreams",
			"id": "https://remote.example/notes/` + id + `",
			"type": "Note",
			"attributedTo": "https://remote.example/author/carol",
			"content": "hi",
			"to": ["` + to + `"],
			"cc": []
		}`)
	}
	got, err := r.IngestNote(note("f1", "https://remote.example/wp-json/actors/1/followers"))
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilityFollowers, got.Visibility)

	got, err = r.IngestNote(note("f2", "https://remote.example/author/carol/followers"))
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilitySpecified, got.Visibility, "followersUri があれば uri + /followers とは比べない")
}

// #3330: refreshing an actor stores a changed followers collection, and an
// actor that stops publishing one keeps the stored value (upstream
// updatePerson passes `undefined`).
func TestRefreshActor_UpdatesFollowersURI(t *testing.T) {
	cases := []struct {
		name      string
		followers string
		want      string
	}{
		{"new value stored", `, "followers": "https://remote.example/c/new-followers"`, "https://remote.example/c/new-followers"},
		{"missing keeps existing", "", "https://remote.example/c/old-followers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			uri := "https://remote.example/users/x"
			old := "https://remote.example/c/old-followers"
			stale := time.Now().Add(-48 * time.Hour)
			repo.Users["existing"] = &model.User{
				ID: "existing", Username: "x", URI: &uri, FollowersURI: &old, LastFetchedAt: &stale,
			}
			body := `{ "@context": "https://www.w3.org/ns/activitystreams",
				"id": "https://remote.example/users/x",
				"type": "Person",
				"preferredUsername": "x",
				"inbox": "https://remote.example/users/x/inbox",
				"publicKey": {"publicKeyPem": "FAKE"}` + tc.followers + `
			}`
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			r := federation.NewResolver(repo, testutil.NewMockNoteRepository(), urls, &stubFetcher{body: []byte(body)}, idGen)
			user, err := r.ResolveActor(uri)
			require.NoError(t, err)
			require.NotNil(t, user.FollowersURI)
			assert.Equal(t, tc.want, *user.FollowersURI)
			assert.Equal(t, tc.want, *repo.Users["existing"].FollowersURI, "DB にも書く")
		})
	}
}
