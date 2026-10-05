package federation_test

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	coreblocking "github.com/elythia-network/elythia/internal/core/blocking"
	"github.com/elythia-network/elythia/internal/core/federation"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newProcForInbound1560 wires a processor exposing user/note/following repos
// for the #1560 inbound tests.
func newProcForInbound1560(t *testing.T, fetcherBody string) (
	*federation.Processor, *testutil.MockUserRepository,
	*testutil.MockNoteRepository, *testutil.MockFollowingRepository,
) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	blockingRepo := testutil.NewMockBlockingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(fetcherBody)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	blockingSvc := coreblocking.NewService(repo, blockingRepo, idGen)
	blockingSvc.SetUnfollower(followingSvc)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)
	p.SetBlockingService(blockingSvc)
	p.SetLocalBaseURL("https://example.com")
	return p, repo, noteRepo, followingRepo
}

// #1560 [MEDIUM] Undo(Accept): remote followee が accept を撤回 → local
// follower の following を解除する。
func TestProcess_UndoAccept_Unfollows(t *testing.T) {
	p, repo, _, followingRepo := newProcForInbound1560(t, aliceActor)
	// remote followee alice
	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", UsernameLower: "alice", URI: &aliceURI, Host: &host}
	// local follower bob が alice を follow 済み
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "bob", FolloweeID: "alice1"}

	body := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Accept",
			"actor": "https://remote.example/users/alice",
			"object": {
				"type": "Follow",
				"actor": "https://example.com/users/bob",
				"object": "https://remote.example/users/alice"
			}
		}
	}`)
	require.NoError(t, p.Process(body))
	assert.Empty(t, followingRepo.Followings, "Undo(Accept) must remove the following")
}

// 内側の Follow が `"actor": {"id": ...}` 形式でも unfollow できること。
// 型エラーで早期 return すると、直後の normalizeActor (#999) が救うはずの
// object 形式が dead code になる (#2662)。
func TestProcess_UndoAccept_ObjectFormFollowActor(t *testing.T) {
	p, repo, _, followingRepo := newProcForInbound1560(t, aliceActor)
	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", UsernameLower: "alice", URI: &aliceURI, Host: &host}
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "bob", FolloweeID: "alice1"}

	body := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Accept",
			"actor": "https://remote.example/users/alice",
			"object": {
				"type": "Follow",
				"actor": {"id": "https://example.com/users/bob"},
				"object": "https://remote.example/users/alice"
			}
		}
	}`)
	require.NoError(t, p.Process(body))
	assert.Empty(t, followingRepo.Followings, "object 形式の actor でも following を解除すること")
}

// #1560 [LOW] suspended remote actor の activity は dispatch 前に drop。
func TestProcess_SuspendedActorDropped(t *testing.T) {
	p, repo, noteRepo, _ := newProcForInbound1560(t, aliceActor)
	suspendedURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", URI: &suspendedURI, Host: &host, IsSuspended: true}
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}

	// suspended actor からの Create は無視され note が作られない
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {"id":"https://remote.example/notes/x","type":"Note","attributedTo":"https://remote.example/users/alice","content":"hi","to":["https://www.w3.org/ns/activitystreams#Public"]}
	}`)
	require.NoError(t, p.Process(body))
	assert.Empty(t, noteRepo.Notes, "suspended actor activity must be dropped")
}

// #1560 [LOW] bearcaps (bear:) object URI は Create/Announce で skip。
func TestProcess_BearcapsObjectSkipped(t *testing.T) {
	p, repo, noteRepo, _ := newProcForInbound1560(t, aliceActor)
	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", URI: &aliceURI, Host: &host}

	create := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": "bear:?u=https://remote.example/notes/x&t=token"
	}`)
	require.NoError(t, p.Process(create), "bearcaps Create must be ack-skipped")
	assert.Empty(t, noteRepo.Notes)

	announce := []byte(`{
		"type": "Announce",
		"actor": "https://remote.example/users/alice",
		"object": "bear:?u=https://remote.example/notes/y&t=token"
	}`)
	require.NoError(t, p.Process(announce), "bearcaps Announce must be ack-skipped")
	assert.Empty(t, noteRepo.Notes)
}

// #1560 [LOW] Announce: 非公開 (followers/specified) note の boost は drop。
func TestProcess_AnnounceNonPublicSkipped(t *testing.T) {
	p, repo, noteRepo, _ := newProcForInbound1560(t, aliceActor)
	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", URI: &aliceURI, Host: &host}
	noteRepo.Notes["nf"] = &model.Note{ID: "nf", UserID: "bob", Visibility: model.NoteVisibilityFollowers}

	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/a1",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/nf"
	}`)
	require.NoError(t, p.Process(body))
	for _, n := range noteRepo.Notes {
		assert.Nil(t, n.RenoteID, "non-public note must not be announced (no renote created)")
	}
}

// #1560 [LOW] Announce: published < target createdAt の malformed timestamp は drop。
func TestProcess_AnnounceMalformedCreatedAtSkipped(t *testing.T) {
	p, repo, noteRepo, _ := newProcForInbound1560(t, aliceActor)
	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", URI: &aliceURI, Host: &host}

	// target note を未来時刻の aidx ID で作る → published(過去) より後に created。
	idGen, _ := id.NewGenerator("aidx")
	future := time.Now().Add(48 * time.Hour)
	futureID := idGen.Generate(future)
	noteRepo.Notes[futureID] = &model.Note{ID: futureID, UserID: "bob", Visibility: model.NoteVisibilityPublic}

	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/a2",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/` + futureID + `",
		"published": "2020-01-01T00:00:00Z"
	}`)
	require.NoError(t, p.Process(body))
	for _, n := range noteRepo.Notes {
		assert.Nil(t, n.RenoteID, "announce published before target createdAt must be dropped")
	}
}
