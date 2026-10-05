package federation_test

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newReactionHook(t *testing.T) (
	*federation.ReactionDeliveryHook,
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
	idGen, _ := id.NewGenerator("aidx")
	hook := federation.NewReactionDeliveryHook(deliver, renderer, urls, idGen, userRepo)
	return hook, enq, userRepo, keypairRepo
}

func setupReactor(t *testing.T, userRepo *testutil.MockUserRepository, keypairRepo *testutil.MockUserKeypairRepository) *model.User {
	t.Helper()
	reactor := &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["alice"] = reactor
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}
	return reactor
}

func remoteAuthor(userRepo *testutil.MockUserRepository) (*model.User, *model.Note) {
	host := "remote.example"
	uri := "https://remote.example/users/bob"
	inbox := "https://remote.example/users/bob/inbox"
	bob := &model.User{ID: "bob", Username: "bob", Host: &host, URI: &uri, Inbox: &inbox}
	userRepo.Users["bob"] = bob
	noteURI := "https://remote.example/notes/n1"
	target := &model.Note{ID: "n1", UserID: "bob", URI: &noteURI}
	return bob, target
}

func TestReactionHook_Added_RemoteAuthor(t *testing.T) {
	hook, enq, userRepo, keypairRepo := newReactionHook(t)
	reactor := setupReactor(t, userRepo, keypairRepo)
	_, target := remoteAuthor(userRepo)

	hook.OnReactionAdded(reactor, target, "🎉")
	require.Len(t, enq.calls, 1)
	assert.Equal(t, "https://remote.example/users/bob/inbox", enq.calls[0].Inbox)

	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Like", got["type"])
	assert.Equal(t, "🎉", got["content"])
	assert.Equal(t, "https://remote.example/notes/n1", got["object"])
}

func TestReactionHook_Added_LocalAuthorSkipped(t *testing.T) {
	hook, enq, userRepo, keypairRepo := newReactionHook(t)
	reactor := setupReactor(t, userRepo, keypairRepo)
	bob := &model.User{ID: "bob", Username: "bob"}
	userRepo.Users["bob"] = bob
	target := &model.Note{ID: "n1", UserID: "bob"}

	hook.OnReactionAdded(reactor, target, "🎉")
	assert.Empty(t, enq.calls)
}

func TestReactionHook_Added_RemoteReactorSkipped(t *testing.T) {
	hook, enq, _, _ := newReactionHook(t)
	host := "remote.example"
	reactor := &model.User{ID: "rem", Username: "rem", Host: &host}
	target := &model.Note{ID: "n1", UserID: "bob"}

	hook.OnReactionAdded(reactor, target, "🎉")
	assert.Empty(t, enq.calls)
}

func TestReactionHook_Added_NilArgs(t *testing.T) {
	hook, enq, _, _ := newReactionHook(t)
	hook.OnReactionAdded(nil, &model.Note{}, "x")
	hook.OnReactionAdded(&model.User{}, nil, "x")
	assert.Empty(t, enq.calls)
}

func TestReactionHook_Added_AuthorNotFound(t *testing.T) {
	hook, enq, userRepo, keypairRepo := newReactionHook(t)
	reactor := setupReactor(t, userRepo, keypairRepo)
	target := &model.Note{ID: "n1", UserID: "ghost"}

	hook.OnReactionAdded(reactor, target, "🎉")
	assert.Empty(t, enq.calls)
}

func TestReactionHook_Removed_RemoteAuthor(t *testing.T) {
	hook, enq, userRepo, keypairRepo := newReactionHook(t)
	reactor := setupReactor(t, userRepo, keypairRepo)
	_, target := remoteAuthor(userRepo)

	hook.OnReactionRemoved(reactor, target, "🎉")
	require.Len(t, enq.calls, 1)

	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Undo", got["type"])
	inner, ok := got["object"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Like", inner["type"])
}

func TestReactionHook_Removed_LocalAuthorSkipped(t *testing.T) {
	hook, enq, userRepo, keypairRepo := newReactionHook(t)
	reactor := setupReactor(t, userRepo, keypairRepo)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	target := &model.Note{ID: "n1", UserID: "bob"}

	hook.OnReactionRemoved(reactor, target, "🎉")
	assert.Empty(t, enq.calls)
}

func TestReactionHook_LocalNoteFallbackURI(t *testing.T) {
	// target.URI が nil でローカル note の場合は urls.NoteURI() を使う。
	// ただし作者はリモートにしないと配信されないので、targetはリモート → URI 必須。
	// この経路は実際には起きにくいが、buildLike の URI フォールバック分岐をカバー
	// するために URI=nil のリモートnoteで試す。
	hook, enq, userRepo, keypairRepo := newReactionHook(t)
	reactor := setupReactor(t, userRepo, keypairRepo)
	host := "remote.example"
	uri := "https://remote.example/users/bob"
	inbox := "https://remote.example/users/bob/inbox"
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &host, URI: &uri, Inbox: &inbox}
	target := &model.Note{ID: "n1", UserID: "bob"} // URI=nil

	hook.OnReactionAdded(reactor, target, "🎉")
	require.Len(t, enq.calls, 1)
	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "https://example.com/notes/n1", got["object"])
}

func TestReactionHook_DeliverErrorDoesNotPanic(t *testing.T) {
	hook, _, userRepo, _ := newReactionHook(t)
	// keypair なしで signerCredentials が失敗 → DeliverToUser が err
	reactor := &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["alice"] = reactor
	_, target := remoteAuthor(userRepo)

	hook.OnReactionAdded(reactor, target, "🎉")
	hook.OnReactionRemoved(reactor, target, "🎉")
}

// ---------------------------------------------------------------------------
// #636 Fanout coverage
// ---------------------------------------------------------------------------

// newReactionHookWithFollowers wires a follower inbox list onto the same
// repo set used elsewhere so per-test fanout assertions stay terse.
func newReactionHookWithFollowers(t *testing.T, reactorID string, followerInboxes []string) (
	*federation.ReactionDeliveryHook,
	*stubEnqueuer,
	*testutil.MockUserRepository,
	*testutil.MockUserKeypairRepository,
	*testutil.MockFollowingRepository,
) {
	t.Helper()
	enq := &stubEnqueuer{}
	userRepo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	keypairRepo := testutil.NewMockUserKeypairRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	deliver := federation.NewDeliverService(enq, userRepo, followingRepo, keypairRepo, urls)
	renderer := activitypub.NewRenderer(urls)
	idGen, _ := id.NewGenerator("aidx")
	hook := federation.NewReactionDeliveryHook(deliver, renderer, urls, idGen, userRepo)
	if len(followerInboxes) > 0 {
		followingRepo.RemoteInboxes[reactorID] = followerInboxes
	}
	return hook, enq, userRepo, keypairRepo, followingRepo
}

func collectInboxes(enq *stubEnqueuer) []string {
	out := make([]string, 0, len(enq.calls))
	for _, c := range enq.calls {
		out = append(out, c.Inbox)
	}
	return out
}

// public note へのリアクションは note 作者 (remote) + reactor の remote
// followers 全員に Like が届くこと (#636)。
func TestReactionHook_Added_PublicNote_FanoutToFollowers(t *testing.T) {
	hook, enq, userRepo, keypairRepo, _ := newReactionHookWithFollowers(t, "alice", []string{
		"https://c.example/users/charlie/inbox",
		"https://d.example/inbox",
	})
	reactor := setupReactor(t, userRepo, keypairRepo)
	_, target := remoteAuthor(userRepo)
	target.Visibility = model.NoteVisibilityPublic

	hook.OnReactionAdded(reactor, target, "🎉")
	inboxes := collectInboxes(enq)
	assert.Contains(t, inboxes, "https://remote.example/users/bob/inbox")
	assert.Contains(t, inboxes, "https://c.example/users/charlie/inbox")
	assert.Contains(t, inboxes, "https://d.example/inbox")
	assert.Len(t, inboxes, 3)
}

// 自分の public note へのリアクションでも remote followers にだけは fanout
// する (target 作者 local なので DirectRecipe は無し)。TS の DeliverManager
// と同じ条件 (#636)。
func TestReactionHook_Added_LocalAuthorPublicNote_FanoutToFollowersOnly(t *testing.T) {
	hook, enq, userRepo, keypairRepo, _ := newReactionHookWithFollowers(t, "alice", []string{
		"https://c.example/users/charlie/inbox",
	})
	reactor := setupReactor(t, userRepo, keypairRepo)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"} // local author
	target := &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}

	hook.OnReactionAdded(reactor, target, "🎉")
	inboxes := collectInboxes(enq)
	assert.Equal(t, []string{"https://c.example/users/charlie/inbox"}, inboxes)
}

// followers / home visibility も public と同じく follower fanout 対象。
func TestReactionHook_Added_FollowersVisibility_FanoutsToFollowers(t *testing.T) {
	hook, enq, userRepo, keypairRepo, _ := newReactionHookWithFollowers(t, "alice", []string{
		"https://c.example/users/charlie/inbox",
	})
	reactor := setupReactor(t, userRepo, keypairRepo)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	target := &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityFollowers}

	hook.OnReactionAdded(reactor, target, "🎉")
	assert.Equal(t, []string{"https://c.example/users/charlie/inbox"}, collectInboxes(enq))
}

func TestReactionHook_Added_HomeVisibility_FanoutsToFollowers(t *testing.T) {
	hook, enq, userRepo, keypairRepo, _ := newReactionHookWithFollowers(t, "alice", []string{
		"https://c.example/users/charlie/inbox",
	})
	reactor := setupReactor(t, userRepo, keypairRepo)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	target := &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityHome}

	hook.OnReactionAdded(reactor, target, "🎉")
	assert.Equal(t, []string{"https://c.example/users/charlie/inbox"}, collectInboxes(enq))
}

// specified visibility は follower fanout 無しで visibleUserIds のうち
// remote な user 全員に DirectRecipe (#636)。
func TestReactionHook_Added_SpecifiedNote_DirectToVisibleRemoteUsers(t *testing.T) {
	hook, enq, userRepo, keypairRepo, _ := newReactionHookWithFollowers(t, "alice", []string{
		// 来てはいけない follower inbox。
		"https://c.example/users/charlie/inbox",
	})
	reactor := setupReactor(t, userRepo, keypairRepo)
	bob, _ := remoteAuthor(userRepo)
	host2 := "remote2.example"
	uri2 := "https://remote2.example/users/dave"
	inbox2 := "https://remote2.example/users/dave/inbox"
	dave := &model.User{ID: "dave", Username: "dave", Host: &host2, URI: &uri2, Inbox: &inbox2}
	userRepo.Users["dave"] = dave
	// bob ローカル user (visibleUserIds に local が混じっても skip)
	userRepo.Users["eve"] = &model.User{ID: "eve", Username: "eve"}

	target := &model.Note{
		ID:             "n1",
		UserID:         bob.ID,
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{"dave", "eve"},
	}
	noteURI := "https://remote.example/notes/n1"
	target.URI = &noteURI

	hook.OnReactionAdded(reactor, target, "🎉")
	inboxes := collectInboxes(enq)
	assert.Contains(t, inboxes, "https://remote.example/users/bob/inbox")
	assert.Contains(t, inboxes, "https://remote2.example/users/dave/inbox")
	assert.NotContains(t, inboxes, "https://c.example/users/charlie/inbox", "specified は follower fanout 不要")
	assert.Len(t, inboxes, 2)
}

// localOnly な note に対してリアクションした場合は何も配信しない。
func TestReactionHook_Added_LocalOnlySkipped(t *testing.T) {
	hook, enq, userRepo, keypairRepo, _ := newReactionHookWithFollowers(t, "alice", []string{
		"https://c.example/users/charlie/inbox",
	})
	reactor := setupReactor(t, userRepo, keypairRepo)
	_, target := remoteAuthor(userRepo)
	target.Visibility = model.NoteVisibilityPublic
	target.LocalOnly = true

	hook.OnReactionAdded(reactor, target, "🎉")
	assert.Empty(t, enq.calls)
}

// direct と follower fanout で同じ inbox が重複した場合でも、同一 Like を
// 同一 inbox に 2 通 POST しないよう送信側で排除する (#2567)。受信側の
// idempotent 処理に依存しない。
func TestReactionHook_Added_DuplicateInbox_Deduped(t *testing.T) {
	// bob = remote 作者 / inbox A、follower inbox にも同じ A を仕込む。
	hook, enq, userRepo, keypairRepo, _ := newReactionHookWithFollowers(t, "alice", []string{
		"https://remote.example/users/bob/inbox", // 作者と完全一致
	})
	reactor := setupReactor(t, userRepo, keypairRepo)
	_, target := remoteAuthor(userRepo)
	target.Visibility = model.NoteVisibilityPublic

	hook.OnReactionAdded(reactor, target, "🎉")
	require.Len(t, enq.calls, 1, "direct と follower の重複 inbox は送信側で 1 通にまとめられる")
	for _, c := range enq.calls {
		assert.Equal(t, "https://remote.example/users/bob/inbox", c.Inbox)
	}
}

// Undo Like も同一 fanout 経路なので、重複 inbox は 1 通にまとめられる (#2567)。
func TestReactionHook_Removed_DuplicateInbox_Deduped(t *testing.T) {
	hook, enq, userRepo, keypairRepo, _ := newReactionHookWithFollowers(t, "alice", []string{
		"https://remote.example/users/bob/inbox", // 作者と完全一致
	})
	reactor := setupReactor(t, userRepo, keypairRepo)
	_, target := remoteAuthor(userRepo)
	target.Visibility = model.NoteVisibilityPublic

	hook.OnReactionRemoved(reactor, target, "🎉")
	require.Len(t, enq.calls, 1, "Undo Like も重複 inbox は 1 通にまとめられる")
	for _, c := range enq.calls {
		assert.Equal(t, "https://remote.example/users/bob/inbox", c.Inbox)
	}

	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Undo", got["type"])
}

// VisibleUserIDs に存在しない user ID が混ざっていても残りの remote user
// にだけ DirectRecipe が配信される。FindManyByIDs は missing rows を silent
// skip するので、遭遇するのは稀だが drop-in 復帰時 / 遅延 federation で
// 起こり得る (#644 #4)。
func TestReactionHook_Added_SpecifiedNote_UnknownVisibleUserSkipped(t *testing.T) {
	hook, enq, userRepo, keypairRepo, _ := newReactionHookWithFollowers(t, "alice", nil)
	reactor := setupReactor(t, userRepo, keypairRepo)
	bob, _ := remoteAuthor(userRepo)
	host2 := "remote2.example"
	uri2 := "https://remote2.example/users/dave"
	inbox2 := "https://remote2.example/users/dave/inbox"
	dave := &model.User{ID: "dave", Username: "dave", Host: &host2, URI: &uri2, Inbox: &inbox2}
	userRepo.Users["dave"] = dave

	target := &model.Note{
		ID:             "n1",
		UserID:         bob.ID,
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{"dave", "ghost", "missing"}, // ghost / missing は user table に無い
	}
	noteURI := "https://remote.example/notes/n1"
	target.URI = &noteURI

	hook.OnReactionAdded(reactor, target, "🎉")
	inboxes := collectInboxes(enq)
	assert.Contains(t, inboxes, "https://remote.example/users/bob/inbox")
	assert.Contains(t, inboxes, "https://remote2.example/users/dave/inbox")
	assert.Len(t, inboxes, 2, "missing user は silent skip して残りに配信を続ける")
}

// VisibleUserIDs に target.UserID 自身が含まれていても重複 lookup せず、
// 結果として作者 inbox を 1 度しか積まない (#644 #7 dedup)。
func TestReactionHook_Added_SpecifiedNote_AuthorInVisibleListDeduped(t *testing.T) {
	hook, enq, userRepo, keypairRepo, _ := newReactionHookWithFollowers(t, "alice", nil)
	reactor := setupReactor(t, userRepo, keypairRepo)
	bob, _ := remoteAuthor(userRepo)

	target := &model.Note{
		ID:             "n1",
		UserID:         bob.ID,
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{bob.ID, bob.ID, ""}, // 重複 + 空文字は skip
	}
	noteURI := "https://remote.example/notes/n1"
	target.URI = &noteURI

	hook.OnReactionAdded(reactor, target, "🎉")
	inboxes := collectInboxes(enq)
	assert.Equal(t, []string{"https://remote.example/users/bob/inbox"}, inboxes)
}

// Undo Like も同じ recipient 集合に届く (#636)。
func TestReactionHook_Removed_PublicNote_FanoutToFollowers(t *testing.T) {
	hook, enq, userRepo, keypairRepo, _ := newReactionHookWithFollowers(t, "alice", []string{
		"https://c.example/users/charlie/inbox",
	})
	reactor := setupReactor(t, userRepo, keypairRepo)
	_, target := remoteAuthor(userRepo)
	target.Visibility = model.NoteVisibilityPublic

	hook.OnReactionRemoved(reactor, target, "🎉")
	inboxes := collectInboxes(enq)
	assert.Contains(t, inboxes, "https://remote.example/users/bob/inbox")
	assert.Contains(t, inboxes, "https://c.example/users/charlie/inbox")

	// 中身が Undo であることも軽く確認
	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Undo", got["type"])
}

// **sharedInbox を持つフォロワーでも 1 通であること (敵対的レビュー)。**
//
// 既存の dedup テストは fixture が `SharedInbox` を持たず、direct と follower の
// URL がたまたま一致する形しか見ていなかった。**現実には
// `ListRemoteFollowerInboxes` が `COALESCE(NULLIF(sharedInbox,”), inbox)` を
// 返すので、direct 側が個別 inbox を使うと URL が食い違い、exclude が効かずに
// 2 通届く。**
func TestReactionHook_Added_SharedInboxFollower_Deduped(t *testing.T) {
	const shared = "https://remote.example/inbox"
	hook, enq, userRepo, keypairRepo, _ := newReactionHookWithFollowers(t, "alice", []string{shared})
	reactor := setupReactor(t, userRepo, keypairRepo)
	bob, target := remoteAuthor(userRepo)
	// bob は個別 inbox と sharedInbox の両方を持つ (リモートの通常の形)。
	s := shared
	bob.SharedInbox = &s
	target.Visibility = model.NoteVisibilityPublic

	hook.OnReactionAdded(reactor, target, "🎉")
	require.Len(t, enq.calls, 1,
		"sharedInbox を持つフォロワー宛に同じ Like が 2 通出ている (exclude が効いていない)")
	assert.Equal(t, shared, enq.calls[0].Inbox)
}
