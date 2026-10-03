package federation_test

import (
	"testing"

	"github.com/shiroha-a/mk/internal/activitypub"
	coreblocking "github.com/shiroha-a/mk/internal/core/blocking"
	"github.com/shiroha-a/mk/internal/core/federation"
	corefollowing "github.com/shiroha-a/mk/internal/core/following"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newProcessorForInboundFollowBlock wires the processor the same way as
// production (router.go): followingSvc.SetBlockingChecker(blockingSvc) +
// processor.SetBlockingService(blockingSvc)。remote alice / local bob を
// pre-seed して resolver の fetch を回避する (#1631)。
func newProcessorForInboundFollowBlock(t *testing.T) (
	*federation.Processor,
	*testutil.MockBlockingRepository,
	*testutil.MockFollowingRepository,
	*stubInboundFollowAcceptor,
) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	blockingRepo := testutil.NewMockBlockingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	blockingSvc := coreblocking.NewService(repo, blockingRepo, idGen)
	blockingSvc.SetUnfollower(followingSvc)
	followingSvc.SetBlockingChecker(blockingSvc)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)
	p.SetBlockingService(blockingSvc)
	p.SetLocalBaseURL("https://example.com")

	// remote alice を URI 付きで pre-seed (resolver は FindByURI hit で再利用)
	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", UsernameLower: "alice", URI: &aliceURI, Host: &host}
	// local bob
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}

	acceptor := &stubInboundFollowAcceptor{}
	p.SetInboundFollowAcceptor(acceptor)
	return p, blockingRepo, followingRepo, acceptor
}

var inboundFollowBody = []byte(`{
	"type": "Follow",
	"id": "https://remote.example/follows/blocked1",
	"actor": "https://remote.example/users/alice",
	"object": "https://example.com/users/bob"
}`)

// local followee が remote follower を block している場合、エラーではなく
// Reject を配送して正常終了する (#1631、upstream UserFollowingService.follow)。
func TestProcess_FollowBlockedSendsReject(t *testing.T) {
	p, blockingRepo, followingRepo, acceptor := newProcessorForInboundFollowBlock(t)
	// bob (local) が alice (remote) を block
	blockingRepo.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: "bob", BlockeeID: "alice1"}

	require.NoError(t, p.Process(inboundFollowBody), "blocked inbound follow must ack, not error")
	assert.Empty(t, followingRepo.Followings, "no following row must be created")
	assert.Empty(t, acceptor.calls, "accept must not be sent")
	require.Len(t, acceptor.rejects, 1, "reject must be delivered")
	assert.Equal(t, "alice1", acceptor.rejects[0].followerID)
	assert.Equal(t, "bob", acceptor.rejects[0].followeeID)
	// original Follow の id を含む raw が Reject の object として渡ること
	assert.Contains(t, acceptor.rejects[0].followRaw, "https://remote.example/follows/blocked1")
}

// remote follower が local followee を block している場合、upstream と同じく
// 自動で block を解除して follow を続行する (#1631)。
func TestProcess_FollowBlockingAutoUnblocks(t *testing.T) {
	p, blockingRepo, followingRepo, acceptor := newProcessorForInboundFollowBlock(t)
	// alice (remote) が bob (local) を block している stale な状態
	blockingRepo.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: "alice1", BlockeeID: "bob"}

	require.NoError(t, p.Process(inboundFollowBody))
	assert.Empty(t, blockingRepo.Blockings, "stale remote block must be auto-unblocked")
	assert.Len(t, followingRepo.Followings, 1, "follow must proceed after auto-unblock")
	assert.Empty(t, acceptor.rejects)
	require.Len(t, acceptor.calls, 1, "accept must be sent for the established follow")
}

// 相互 block の場合は upstream の else-if 順と同じく blocked (Reject) を
// 優先し、remote 側の block 行は解除しない (#1631)。
func TestProcess_FollowMutualBlockPrefersReject(t *testing.T) {
	p, blockingRepo, followingRepo, acceptor := newProcessorForInboundFollowBlock(t)
	blockingRepo.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: "alice1", BlockeeID: "bob"}
	blockingRepo.Blockings["b2"] = &model.Blocking{ID: "b2", BlockerID: "bob", BlockeeID: "alice1"}

	require.NoError(t, p.Process(inboundFollowBody))
	require.Len(t, acceptor.rejects, 1, "reject must be delivered for the mutual block")
	assert.Empty(t, followingRepo.Followings)
	assert.Contains(t, blockingRepo.Blockings, "b1", "remote-side block must not be auto-unblocked when blocked wins")
	assert.Empty(t, acceptor.calls)
}

// blockingService 未配線の場合は従来どおりエラーで返る (regression guard)。
func TestProcess_FollowBlockingWithoutBlockingServiceErrors(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	blockingRepo := testutil.NewMockBlockingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	blockingSvc := coreblocking.NewService(repo, blockingRepo, idGen)
	blockingSvc.SetUnfollower(followingSvc)
	followingSvc.SetBlockingChecker(blockingSvc)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)
	p.SetLocalBaseURL("https://example.com")
	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", UsernameLower: "alice", URI: &aliceURI, Host: &host}
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	blockingRepo.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: "alice1", BlockeeID: "bob"}

	assert.Error(t, p.Process(inboundFollowBody),
		"without blockingService the ErrBlocking must propagate as before")
}

// inbound Follow が remote followee を対象にしている場合、local DB に
// remote->remote の Following 行を作らず skip して ack する (#1826、upstream
// follow() の `if (followee.host != null) return 'skip: ...'` gate)。これが無いと
// relay / 悪意ある remote actor が Follow(actor=remoteA, object=既知 remoteB) を
// 送るだけで following graph を汚染できる。
func TestProcess_FollowRemoteFolloweeSkipped(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)
	p.SetLocalBaseURL("https://example.com")
	acceptor := &stubInboundFollowAcceptor{}
	p.SetInboundFollowAcceptor(acceptor)

	// remote follower alice + remote followee charlie を URI 付きで pre-seed
	host := "remote.example"
	aliceURI := "https://remote.example/users/alice"
	charlieURI := "https://remote.example/users/charlie"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", UsernameLower: "alice", URI: &aliceURI, Host: &host}
	repo.Users["charlie1"] = &model.User{ID: "charlie1", Username: "charlie", UsernameLower: "charlie", URI: &charlieURI, Host: &host}

	body := []byte(`{
		"type": "Follow",
		"id": "https://remote.example/follows/remote1",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/users/charlie"
	}`)

	require.NoError(t, p.Process(body), "inbound follow targeting a remote followee must ack, not error")
	assert.Empty(t, followingRepo.Followings, "no following row must be created for a remote followee")
	assert.Empty(t, acceptor.calls, "no Accept for a remote followee")
	assert.Empty(t, acceptor.rejects, "no Reject for a remote followee")
}
