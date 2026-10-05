package federation_test

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newFollowingHook(t *testing.T) (
	*federation.FollowingDeliveryHook,
	*stubEnqueuer,
	*testutil.MockUserRepository,
	*testutil.MockUserKeypairRepository,
) {
	t.Helper()
	enq := &stubEnqueuer{}
	userRepo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	keypairRepo := testutil.NewMockUserKeypairRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	deliver := federation.NewDeliverService(enq, userRepo, followingRepo, keypairRepo, urls)
	renderer := activitypub.NewRenderer(urls)
	hook := federation.NewFollowingDeliveryHook(deliver, renderer, urls)
	return hook, enq, userRepo, keypairRepo
}

func makeRemote(id, host, uri, inbox string) *model.User {
	h, u, i := host, uri, inbox
	return &model.User{ID: id, Username: id, Host: &h, URI: &u, Inbox: &i}
}

func makeLocal(id string) *model.User {
	return &model.User{ID: id, Username: id}
}

func TestFollowingHook_OnLocalFollowed_Remote(t *testing.T) {
	hook, enq, userRepo, keypairRepo := newFollowingHook(t)
	follower := makeLocal("alice")
	userRepo.Users["alice"] = follower
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}
	followee := makeRemote("bob", "remote.example", "https://remote.example/users/bob", "https://remote.example/users/bob/inbox")

	hook.OnLocalFollowed(follower, followee)
	require.Len(t, enq.calls, 1)
	assert.Equal(t, "https://remote.example/users/bob/inbox", enq.calls[0].Inbox)

	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Follow", got["type"])
}

func TestFollowingHook_OnLocalFollowed_LocalFolloweeSkipped(t *testing.T) {
	hook, enq, _, _ := newFollowingHook(t)
	hook.OnLocalFollowed(makeLocal("alice"), makeLocal("bob"))
	assert.Empty(t, enq.calls)
}

func TestFollowingHook_OnLocalFollowed_RemoteFollowerSkipped(t *testing.T) {
	hook, enq, _, _ := newFollowingHook(t)
	rem := makeRemote("alice", "h", "https://h/u", "https://h/u/inbox")
	rem2 := makeRemote("bob", "h", "https://h/u2", "https://h/u2/inbox")
	hook.OnLocalFollowed(rem, rem2)
	assert.Empty(t, enq.calls)
}

func TestFollowingHook_OnLocalFollowed_NilArgs(t *testing.T) {
	hook, enq, _, _ := newFollowingHook(t)
	hook.OnLocalFollowed(nil, makeLocal("bob"))
	hook.OnLocalFollowed(makeLocal("alice"), nil)
	assert.Empty(t, enq.calls)
}

func TestFollowingHook_OnLocalFollowed_RemoteWithoutURI(t *testing.T) {
	hook, enq, _, _ := newFollowingHook(t)
	host := "remote.example"
	bad := &model.User{ID: "bob", Username: "bob", Host: &host}
	hook.OnLocalFollowed(makeLocal("alice"), bad)
	assert.Empty(t, enq.calls)
}

func TestFollowingHook_OnLocalUnfollowed_Remote(t *testing.T) {
	hook, enq, userRepo, keypairRepo := newFollowingHook(t)
	follower := makeLocal("alice")
	userRepo.Users["alice"] = follower
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}
	followee := makeRemote("bob", "remote.example", "https://remote.example/users/bob", "https://remote.example/users/bob/inbox")

	hook.OnLocalUnfollowed(follower, followee)
	require.Len(t, enq.calls, 1)

	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Undo", got["type"])
	// #1993: upstream renderUndo は全 Undo に published(.000Z) を付ける。
	published, ok := got["published"].(string)
	require.True(t, ok, "Undo(Follow) に published がある (#1993)")
	assert.Regexp(t, `\.\d{3}Z$`, published, "published は toISOString() の .000Z 形式")
	inner, ok := got["object"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Follow", inner["type"])
	// inner Follow は @context を持たない (outer Undo のみ持つ)。
	_, hasCtx := inner["@context"]
	assert.False(t, hasCtx, "inner Follow は @context を持たない (#1993)")
}

func TestFollowingHook_OnLocalUnfollowed_LocalSkipped(t *testing.T) {
	hook, enq, _, _ := newFollowingHook(t)
	hook.OnLocalUnfollowed(makeLocal("a"), makeLocal("b"))
	assert.Empty(t, enq.calls)
}

// local followeeがremote followerを剥がす (/following/invalidate もしくは
// RejectRequest) ケースでは Reject(Follow) を followee 名義で送ること。
func TestFollowingHook_OnLocalUnfollowed_RemoveRemoteFollower_SendsReject(t *testing.T) {
	hook, enq, userRepo, keypairRepo := newFollowingHook(t)
	follower := makeRemote("bob", "remote.example", "https://remote.example/users/bob", "https://remote.example/users/bob/inbox")
	followee := makeLocal("alice")
	userRepo.Users["alice"] = followee
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}

	hook.OnLocalUnfollowed(follower, followee)
	require.Len(t, enq.calls, 1)

	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Reject", got["type"])
	inner, ok := got["object"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Follow", inner["type"])
	// actor は local followee (alice) のURI、object.actor は remote follower (bob)
	assert.Contains(t, got["actor"], "alice")
	assert.Equal(t, "https://remote.example/users/bob", inner["actor"])
}

func TestFollowingHook_OnLocalUnfollowed_RemoveRemoteFollower_NilURIs(t *testing.T) {
	hook, enq, _, _ := newFollowingHook(t)
	// URI 欠落の remote user は配信不可、no-op。
	host := "remote.example"
	follower := &model.User{ID: "bob", Username: "bob", Host: &host}
	followee := makeLocal("alice")
	hook.OnLocalUnfollowed(follower, followee)
	assert.Empty(t, enq.calls)
}

func TestFollowingHook_OnLocalUnfollowed_NilArgs(t *testing.T) {
	hook, enq, _, _ := newFollowingHook(t)
	hook.OnLocalUnfollowed(nil, makeLocal("a"))
	hook.OnLocalUnfollowed(makeLocal("a"), nil)
	assert.Empty(t, enq.calls)
}

func TestFollowingHook_OnLocalFollowAccepted_Remote(t *testing.T) {
	hook, enq, userRepo, keypairRepo := newFollowingHook(t)
	// follower がリモート、followee (=accept する側) がローカル
	follower := makeRemote("bob", "remote.example", "https://remote.example/users/bob", "https://remote.example/users/bob/inbox")
	followee := makeLocal("alice")
	userRepo.Users["alice"] = followee
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}

	hook.OnLocalFollowAccepted(follower, followee)
	require.Len(t, enq.calls, 1)

	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Accept", got["type"])
	inner, ok := got["object"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Follow", inner["type"])
	assert.Equal(t, *follower.URI, inner["actor"])
}

func TestFollowingHook_OnLocalFollowAccepted_NilArgs(t *testing.T) {
	hook, enq, _, _ := newFollowingHook(t)
	hook.OnLocalFollowAccepted(nil, makeLocal("a"))
	hook.OnLocalFollowAccepted(makeLocal("a"), nil)
	assert.Empty(t, enq.calls)
}

func TestFollowingHook_OnLocalFollowAccepted_BothLocalSkipped(t *testing.T) {
	hook, enq, _, _ := newFollowingHook(t)
	hook.OnLocalFollowAccepted(makeLocal("a"), makeLocal("b"))
	assert.Empty(t, enq.calls)
}

func TestFollowingHook_OnLocalFollowAccepted_RemoteFolloweeSkipped(t *testing.T) {
	hook, enq, _, _ := newFollowingHook(t)
	rem1 := makeRemote("a", "h", "https://h/a", "https://h/a/inbox")
	rem2 := makeRemote("b", "h", "https://h/b", "https://h/b/inbox")
	hook.OnLocalFollowAccepted(rem1, rem2)
	assert.Empty(t, enq.calls)
}

func TestFollowingHook_OnLocalFollowAccepted_RemoteFollowerWithoutURI(t *testing.T) {
	hook, enq, _, _ := newFollowingHook(t)
	host := "h"
	rem := &model.User{ID: "bob", Username: "bob", Host: &host}
	hook.OnLocalFollowAccepted(rem, makeLocal("alice"))
	assert.Empty(t, enq.calls)
}

func TestFollowingHook_DeliverErrors_DoNotPanic(t *testing.T) {
	hook, _, userRepo, _ := newFollowingHook(t)
	// keypair 不在で signerCredentials が失敗する → DeliverToUser は err
	follower := makeLocal("alice")
	userRepo.Users["alice"] = follower
	followee := makeRemote("bob", "remote.example", "https://remote.example/users/bob", "https://remote.example/users/bob/inbox")
	hook.OnLocalFollowed(follower, followee)
	hook.OnLocalUnfollowed(follower, followee)
	hook.OnLocalFollowAccepted(followee, follower) // ← follower=remote, followee=local だが key不在
}

// --- SendRejectForInboundFollow (#1631) ---

func TestFollowingHook_SendRejectForInboundFollow_Remote(t *testing.T) {
	hook, enq, userRepo, keypairRepo := newFollowingHook(t)
	followee := makeLocal("bob")
	userRepo.Users["bob"] = followee
	keypairRepo.Keypairs["bob"] = &model.UserKeypair{UserID: "bob", PrivateKey: "PEM"}
	follower := makeRemote("alice", "remote.example", "https://remote.example/users/alice", "https://remote.example/users/alice/inbox")

	raw := json.RawMessage(`{"type":"Follow","id":"https://remote.example/follows/xyz","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	require.NoError(t, hook.SendRejectForInboundFollow(follower, followee, raw))
	require.Len(t, enq.calls, 1)
	assert.Equal(t, "https://remote.example/users/alice/inbox", enq.calls[0].Inbox)

	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Reject", got["type"])
	// Misskey/CherryPick の InboxProcessor は id 無し activity を silent drop
	// するため、Reject 自体の id が必ず入っていること。
	rejID, _ := got["id"].(string)
	assert.NotEmpty(t, rejID, "Reject must carry an id")
	// original Follow が object としてネストされ、id が保持されること。
	obj, _ := got["object"].(map[string]any)
	require.NotNil(t, obj, "original Follow must be nested as object")
	assert.Equal(t, "https://remote.example/follows/xyz", obj["id"])
	// actor は rejecting user (local followee)。
	assert.Equal(t, "https://example.com/users/bob", got["actor"])
}

func TestFollowingHook_SendRejectForInboundFollow_Guards(t *testing.T) {
	hook, enq, _, _ := newFollowingHook(t)
	raw := json.RawMessage(`{"type":"Follow"}`)

	// nil 引数は no-op
	require.NoError(t, hook.SendRejectForInboundFollow(nil, makeLocal("bob"), raw))
	require.NoError(t, hook.SendRejectForInboundFollow(makeRemote("a", "h", "u", "i"), nil, raw))
	// local follower / remote followee 方向は AP 配信不要
	require.NoError(t, hook.SendRejectForInboundFollow(makeLocal("alice"), makeLocal("bob"), raw))
	require.NoError(t, hook.SendRejectForInboundFollow(
		makeRemote("a", "remote.example", "https://remote.example/u/a", "https://remote.example/i"),
		makeRemote("b", "remote.example", "https://remote.example/u/b", "https://remote.example/i"), raw))
	// URI 無し remote follower は no-op
	noURI := &model.User{ID: "c", Username: "c", Host: strPtrFed("remote.example")}
	require.NoError(t, hook.SendRejectForInboundFollow(noURI, makeLocal("bob"), raw))
	assert.Empty(t, enq.calls)
}

func strPtrFed(s string) *string { return &s }
