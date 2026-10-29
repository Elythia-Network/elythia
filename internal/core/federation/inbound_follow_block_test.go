package federation_test

import (
	"strings"
	"testing"

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

// newProcessorForInboundFollowBlock wires the processor the same way as
// production (router.go): followingSvc.SetBlockingChecker(blockingSvc) +
// NewProcessor の blockingService (テストでは SetBlockingServiceForTest)。remote alice / local bob を
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
	resolver := federation.NewResolverNilDeps(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen, nil, nil, nil, nil)
	blockingSvc := coreblocking.NewService(repo, blockingRepo, idGen, followingSvc, nil, nil, nil)
	followingSvc.SetBlockingChecker(blockingSvc)
	p := federation.NewProcessorNilDeps(resolver, followingSvc, nil, nil, repo, noteRepo)
	p.SetBlockingServiceForTest(blockingSvc)
	p.SetLocalBaseURLForTest("https://example.com")

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

type inboundLocalReferenceObserver struct{ recorded []string }

func (o *inboundLocalReferenceObserver) IsExternallyAvailable(user *model.User) bool {
	return user != nil && user.FederationStatus == model.FederationStatusOn && !user.IsSuspended && !user.IsDeleted
}
func (o *inboundLocalReferenceObserver) ObserveExternalReference(userID, reason, host string) (bool, error) {
	o.recorded = append(o.recorded, userID+":"+reason+":"+host)
	return true, nil
}

func newProcessorForLocalReferenceTest(t *testing.T, status model.FederationStatus) (*federation.Processor, *testutil.MockFollowingRepository, *testutil.MockNoteRepository, *inboundLocalReferenceObserver) {
	t.Helper()
	users := testutil.NewMockUserRepository()
	notes := testutil.NewMockNoteRepository()
	followings := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	observer := &inboundLocalReferenceObserver{}
	resolver := federation.NewResolverNilDeps(users, notes, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	resolver.SetLocalFederationObserver(observer)
	followingSvc := corefollowing.NewService(users, followings, testutil.NewMockFollowRequestRepository(), idGen, nil, nil, nil, nil)
	p := federation.NewProcessorNilDeps(resolver, followingSvc, nil, nil, users, notes)
	p.SetLocalBaseURLForTest("https://example.com")
	aliceURI, remoteHost := "https://remote.example/users/alice", "remote.example"
	users.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", UsernameLower: "alice", URI: &aliceURI, Host: &remoteHost}
	users.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob", FederationStatus: status}
	return p, followings, notes, observer
}

func TestProcess_FollowChecksTheWorkerResolvedLocalUser(t *testing.T) {
	uris := []string{
		"https://example.com/users/bob/extra",
		"https://example.com/users/bob?x=1",
		"http://example.com/users/bob",
		"https://example.com/users/bob/",
	}
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending, model.FederationStatusOn} {
		for _, uri := range uris {
			t.Run(string(status)+"/"+uri, func(t *testing.T) {
				p, followings, _, observer := newProcessorForLocalReferenceTest(t, status)
				body := []byte(strings.Replace(string(inboundFollowBody), "https://example.com/users/bob", uri, 1))
				err := p.Process(body)
				if status == model.FederationStatusOn {
					require.NoError(t, err)
					assert.Len(t, followings.Followings, 1)
					assert.Equal(t, []string{"bob:inbox:remote.example"}, observer.recorded)
				} else {
					require.ErrorIs(t, err, federation.ErrInboundLocalReferenceUnavailable)
					assert.Empty(t, followings.Followings)
					assert.Empty(t, observer.recorded)
				}
			})
		}
	}
}

func TestProcess_CreateGuardsResolvedMentionBeforeSavingNote(t *testing.T) {
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending, model.FederationStatusOn} {
		t.Run(string(status), func(t *testing.T) {
			p, _, notes, observer := newProcessorForLocalReferenceTest(t, status)
			body := []byte(`{"type":"Create","id":"https://remote.example/activities/c1","actor":"https://remote.example/users/alice","object":{"id":"https://remote.example/notes/n1","type":"Note","attributedTo":"https://remote.example/users/alice","to":["https://www.w3.org/ns/activitystreams#Public"],"content":"hello","tag":[{"type":"Mention","href":"https://example.com/x/bob","name":"@bob@example.com"}]}}`)
			err := p.Process(body)
			if status != model.FederationStatusOn {
				require.ErrorIs(t, err, federation.ErrInboundLocalReferenceUnavailable)
				assert.Empty(t, notes.Notes, "the rejected body must not create its Note")
				assert.Empty(t, observer.recorded)
			} else {
				require.NoError(t, err)
				require.Len(t, notes.Notes, 1)
				assert.Equal(t, []string{"bob:inbox:remote.example"}, observer.recorded)
			}
		})
	}
}

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
	resolver := federation.NewResolverNilDeps(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen, nil, nil, nil, nil)
	blockingSvc := coreblocking.NewService(repo, blockingRepo, idGen, followingSvc, nil, nil, nil)
	followingSvc.SetBlockingChecker(blockingSvc)
	p := federation.NewProcessorNilDeps(resolver, followingSvc, nil, nil, repo, noteRepo)
	p.SetLocalBaseURLForTest("https://example.com")
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
	resolver := federation.NewResolverNilDeps(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen, nil, nil, nil, nil)
	p := federation.NewProcessorNilDeps(resolver, followingSvc, nil, nil, repo, noteRepo)
	p.SetLocalBaseURLForTest("https://example.com")
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
