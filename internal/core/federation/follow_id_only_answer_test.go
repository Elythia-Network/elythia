package federation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

type idOnlyFixture struct {
	p         *federation.Processor
	following *testutil.MockFollowingRepository
	requests  *testutil.MockFollowRequestRepository
	followID  string
}

// newIDOnlyFixture has a pending local follow request bob -> remote alice and
// the Follow id this instance sent for it.
func newIDOnlyFixture(t *testing.T) idOnlyFixture {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	reqRepo := testutil.NewMockFollowRequestRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, testutil.NewMockNoteRepository(), urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	svc := corefollowing.NewService(repo, followingRepo, reqRepo, idGen)
	p := federation.NewProcessor(resolver, svc, nil, nil, repo, testutil.NewMockNoteRepository())
	p.SetLocalBaseURL("https://example.com")
	aliceURI := "https://remote.example/users/alice"
	evilURI := "https://remote.example/users/evil"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", UsernameLower: "alice", URI: &aliceURI, Host: &host}
	repo.Users["evil1"] = &model.User{ID: "evil1", Username: "evil", UsernameLower: "evil", URI: &evilURI, Host: &host}
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	reqRepo.Requests["r1"] = &model.FollowRequest{ID: "r1", FollowerID: "bob", FolloweeID: "alice1", FolloweeHost: &host}
	return idOnlyFixture{p: p, following: followingRepo, requests: reqRepo, followID: urls.FollowURI("bob", aliceURI)}
}

// Accept の object が Follow の id だけの文字列でも、申請を成立させる (#3491)。
// 本家 ApInboxService.accept は id を解決し、自インスタンスの `/follows/` から
// Follow を組み直す。
func TestProcess_AcceptWithFollowIDOnly(t *testing.T) {
	f := newIDOnlyFixture(t)
	body := []byte(`{"type":"Accept","id":"https://remote.example/accept/1","actor":"https://remote.example/users/alice","object":"` + f.followID + `"}`)
	require.NoError(t, f.p.Process(body))
	assert.Len(t, f.following.Followings, 1, "an id-only Accept did not establish the follow")
	assert.Empty(t, f.requests.Requests)
}

// 別の actor が、他人宛ての Follow の id で Accept しても成立させない。成立させる
// AcceptRequest が (follower, followee) の組で申請を引くので、id と actor の束縛
// (localFollowFromID) が無くても通らない。束縛は念のための厳しさ。
func TestProcess_AcceptWithFollowIDOnlyFromAnotherActor(t *testing.T) {
	f := newIDOnlyFixture(t)
	body := []byte(`{"type":"Accept","id":"https://remote.example/accept/2","actor":"https://remote.example/users/evil","object":"` + f.followID + `"}`)
	require.NoError(t, f.p.Process(body))
	assert.Empty(t, f.following.Followings, "an Accept from another actor established the follow")
	assert.Len(t, f.requests.Requests, 1)
}

// Reject の object が id だけでも申請を消す。
func TestProcess_RejectWithFollowIDOnly(t *testing.T) {
	f := newIDOnlyFixture(t)
	body := []byte(`{"type":"Reject","id":"https://remote.example/reject/1","actor":"https://remote.example/users/alice","object":"` + f.followID + `"}`)
	require.NoError(t, f.p.Process(body))
	assert.Empty(t, f.requests.Requests, "an id-only Reject left the request behind")
	assert.Empty(t, f.following.Followings)
}
