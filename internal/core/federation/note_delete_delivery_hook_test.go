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

func newDeleteHook(t *testing.T) (
	*federation.NoteDeleteDeliveryHook,
	*stubEnqueuer,
	*testutil.MockUserRepository,
	*testutil.MockFollowingRepository,
	*testutil.MockUserKeypairRepository,
) {
	hook, enq, userRepo, followingRepo, keypairRepo, _ := newDeleteHookWithNotes(t)
	return hook, enq, userRepo, followingRepo, keypairRepo
}

// newDeleteHookWithNotes also wires a note repository as the renderer's
// NoteResolver so tests can control the URI of a boosted note (pure renote の
// Undo(Announce) はブースト元の URI を object に置く)。
func newDeleteHookWithNotes(t *testing.T) (
	*federation.NoteDeleteDeliveryHook,
	*stubEnqueuer,
	*testutil.MockUserRepository,
	*testutil.MockFollowingRepository,
	*testutil.MockUserKeypairRepository,
	*testutil.MockNoteRepository,
) {
	t.Helper()
	enq := &stubEnqueuer{}
	userRepo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	keypairRepo := testutil.NewMockUserKeypairRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	deliver := federation.NewDeliverService(enq, userRepo, followingRepo, keypairRepo, urls)
	renderer := activitypub.NewRenderer(urls)
	renderer.SetNoteResolver(noteRepo)
	hook := federation.NewNoteDeleteDeliveryHook(deliver, renderer, urls)
	return hook, enq, userRepo, followingRepo, keypairRepo, noteRepo
}

func TestNoteDeleteHook_LocalAuthor(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo := newDeleteHook(t)
	author := &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["alice"] = author
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}
	followingRepo.RemoteInboxes["alice"] = []string{"https://r.example/inbox"}

	note := &model.Note{ID: "n1", UserID: "alice"}
	hook.OnNoteDeleted(author, note)
	require.Len(t, enq.calls, 1)

	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Delete", got["type"])
	tomb, ok := got["object"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Tombstone", tomb["type"])
	assert.Equal(t, "https://example.com/notes/n1", tomb["id"])
}

func TestNoteDeleteHook_RemoteNoteURI(t *testing.T) {
	// 取り込まれたリモート note を削除する経路 (実際にはほぼ起きないが、URI
	// フォールバック分岐をカバーする)。 author=local、note.URI=remote の混乱した
	// ケースで URI フォールバックが動くか確認。
	hook, enq, userRepo, followingRepo, keypairRepo := newDeleteHook(t)
	author := &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["alice"] = author
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}
	followingRepo.RemoteInboxes["alice"] = []string{"https://r.example/inbox"}

	uri := "https://other.example/notes/x"
	note := &model.Note{ID: "n1", UserID: "alice", URI: &uri}
	hook.OnNoteDeleted(author, note)
	require.Len(t, enq.calls, 1)

	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	tomb := got["object"].(map[string]any)
	assert.Equal(t, uri, tomb["id"])
}

func TestNoteDeleteHook_RemoteAuthorSkipped(t *testing.T) {
	hook, enq, _, _, _ := newDeleteHook(t)
	host := "remote.example"
	author := &model.User{ID: "bob", Host: &host}
	hook.OnNoteDeleted(author, &model.Note{ID: "n1", UserID: "bob"})
	assert.Empty(t, enq.calls)
}

func TestNoteDeleteHook_LocalOnlySkipped(t *testing.T) {
	hook, enq, userRepo, _, _ := newDeleteHook(t)
	author := &model.User{ID: "alice"}
	userRepo.Users["alice"] = author
	note := &model.Note{ID: "n1", UserID: "alice", LocalOnly: true}
	hook.OnNoteDeleted(author, note)
	assert.Empty(t, enq.calls)
}

func TestNoteDeleteHook_NilArgs(t *testing.T) {
	hook, enq, _, _, _ := newDeleteHook(t)
	hook.OnNoteDeleted(nil, &model.Note{})
	hook.OnNoteDeleted(&model.User{}, nil)
	assert.Empty(t, enq.calls)
}

func TestNoteDeleteHook_DeliverErrorDoesNotPanic(t *testing.T) {
	hook, _, userRepo, followingRepo, _ := newDeleteHook(t)
	author := &model.User{ID: "alice"}
	userRepo.Users["alice"] = author
	// keypair なしで signerCredentials が失敗する
	followingRepo.RemoteInboxes["alice"] = []string{"https://r.example/inbox"}
	note := &model.Note{ID: "n1", UserID: "alice"}
	hook.OnNoteDeleted(author, note)
}

func TestNoteDeleteHook_BroadcastToRemoteInboxes(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo := newDeleteHook(t)
	hook.SetUserRepo(userRepo)

	author := &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["alice"] = author
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}

	// 既知のリモートインスタンスを 2 つ追加。片方は sharedInbox 優先、もう片方は inbox。
	h := "remote1.example"
	shared := "https://remote1.example/inbox"
	userRepo.Users["r1"] = &model.User{ID: "r1", Host: &h, SharedInbox: &shared}
	h2 := "remote2.example"
	inbox := "https://remote2.example/users/bob/inbox"
	userRepo.Users["r2"] = &model.User{ID: "r2", Host: &h2, Inbox: &inbox}

	note := &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	hook.OnNoteDeleted(author, note)

	// Followers への配信 0 件 + 全リモート 2 件 = 2 件 enqueued.
	require.Len(t, enq.calls, 2)
	inboxes := []string{enq.calls[0].Inbox, enq.calls[1].Inbox}
	assert.Contains(t, inboxes, shared)
	assert.Contains(t, inboxes, inbox)
}

func TestNoteDeleteHook_BroadcastSkippedForFollowersVisibility(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo := newDeleteHook(t)
	hook.SetUserRepo(userRepo)

	author := &model.User{ID: "alice"}
	userRepo.Users["alice"] = author
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}

	h := "remote.example"
	shared := "https://remote.example/inbox"
	userRepo.Users["r1"] = &model.User{ID: "r1", Host: &h, SharedInbox: &shared}

	note := &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityFollowers}
	hook.OnNoteDeleted(author, note)

	// Followers 可視性では broadcast しないため、フォロワー 0 人分の 0 件。
	assert.Empty(t, enq.calls)
}

func TestNoteDeleteHook_BroadcastEmptyInboxes(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo := newDeleteHook(t)
	hook.SetUserRepo(userRepo)

	author := &model.User{ID: "alice"}
	userRepo.Users["alice"] = author
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}

	note := &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	hook.OnNoteDeleted(author, note)
	// 既知リモート 0 件で broadcast はスキップされる。
	assert.Empty(t, enq.calls)
}

// failingListInboxes makes ListRemoteInboxes return an error to exercise the
// error-logging branch in the delete delivery hook.
type failingListInboxes struct{ *testutil.MockUserRepository }

func (f *failingListInboxes) ListRemoteInboxes() ([]model.RemoteInbox, error) {
	return nil, assert.AnError
}

func TestNoteDeleteHook_BroadcastListError(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo := newDeleteHook(t)
	hook.SetUserRepo(&failingListInboxes{MockUserRepository: userRepo})

	author := &model.User{ID: "alice"}
	userRepo.Users["alice"] = author
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}

	note := &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	hook.OnNoteDeleted(author, note)
	// エラー時は broadcast されない。
	assert.Empty(t, enq.calls)
}

func TestNoteDeleteHook_BroadcastDeliverError_DoesNotPanic(t *testing.T) {
	hook, _, userRepo, _, _ := newDeleteHook(t)
	hook.SetUserRepo(userRepo)

	author := &model.User{ID: "alice"}
	userRepo.Users["alice"] = author
	// keypair なしで DeliverActivity 側が署名失敗 → panic しないこと。
	h := "remote.example"
	shared := "https://remote.example/inbox"
	userRepo.Users["r1"] = &model.User{ID: "r1", Host: &h, SharedInbox: &shared}

	note := &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	hook.OnNoteDeleted(author, note)
}

// フォロワーは既知リモートの部分集合 (どちらも sharedInbox 優先で解決する) なので、
// フォロワー配送と broadcast を重ねると全フォロワーが同じ Delete を同じ URL に
// 2 回受け取る (#2575)。
func TestNoteDeleteHook_DoesNotDoubleDeliverToFollowers(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo := newDeleteHook(t)
	hook.SetUserRepo(userRepo)

	author := &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["alice"] = author
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}

	// フォロワー (shared inbox 持ち) と、フォローしていない既知リモート。
	host := "remote.example"
	sharedInbox := "https://remote.example/inbox"
	follower := &model.User{ID: "bob", Host: &host, SharedInbox: &sharedInbox}
	userRepo.Users["bob"] = follower
	otherHost := "other.example"
	otherInbox := "https://other.example/users/x/inbox"
	userRepo.Users["carol"] = &model.User{ID: "carol", Host: &otherHost, Inbox: &otherInbox}
	followingRepo.RemoteInboxes["alice"] = []string{sharedInbox}
	followingRepo.RemoteSharedInboxes[sharedInbox] = true

	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic,
	})

	byInbox := map[string]int{}
	shared := map[string]bool{}
	for _, c := range enq.calls {
		byInbox[c.Inbox]++
		shared[c.Inbox] = c.IsSharedInbox
	}
	assert.Equal(t, 1, byInbox[sharedInbox], "フォロワーの inbox は 1 回だけ")
	assert.Equal(t, 1, byInbox[otherInbox], "フォローしていない既知リモートにも届く")
	// **shared フラグを落とさない。** 410 Gone でインスタンス全体を suspend する
	// 判定 (#1811) がこれを見る。従来 broadcast は常に false で送っていた。
	assert.True(t, shared[sharedInbox], "shared inbox は IsSharedInbox=true")
	assert.False(t, shared[otherInbox], "個別 inbox は IsSharedInbox=false")
}

// 一覧が引けないときはフォロワーには届ける。**何も送らないのは今より悪い。**
func TestNoteDeleteHook_FallsBackToFollowersWhenListFails(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo := newDeleteHook(t)
	hook.SetUserRepo(&failingListInboxes{MockUserRepository: userRepo})

	author := &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["alice"] = author
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}
	followingRepo.RemoteInboxes["alice"] = []string{"https://remote.example/inbox"}

	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic,
	})

	require.Len(t, enq.calls, 1)
	assert.Equal(t, "https://remote.example/inbox", enq.calls[0].Inbox)
}

// Public / Home 以外は broadcast しないので、従来どおりフォロワーだけ。
func TestNoteDeleteHook_NonPublicStaysFollowersOnly(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo := newDeleteHook(t)
	hook.SetUserRepo(userRepo)

	author := &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["alice"] = author
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}
	otherHost := "other.example"
	otherInbox := "https://other.example/users/x/inbox"
	userRepo.Users["carol"] = &model.User{ID: "carol", Host: &otherHost, Inbox: &otherInbox}
	followingRepo.RemoteInboxes["alice"] = []string{"https://remote.example/inbox"}

	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityFollowers,
	})

	require.Len(t, enq.calls, 1)
	assert.Equal(t, "https://remote.example/inbox", enq.calls[0].Inbox)
}

// 既知リモートが 0 件でもフォロワー配送は走る。**0 件を「送るものが無い」と
// 解釈すると、片方のクエリだけが変わったときに黙って配送が消える。**
func TestNoteDeleteHook_EmptyBroadcastListStillDeliversToFollowers(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo := newDeleteHook(t)
	hook.SetUserRepo(userRepo)

	author := &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["alice"] = author
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}
	// userRepo にリモートユーザーを入れないので ListRemoteInboxes は空。
	followingRepo.RemoteInboxes["alice"] = []string{"https://remote.example/inbox"}

	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic,
	})

	require.Len(t, enq.calls, 1)
	assert.Equal(t, "https://remote.example/inbox", enq.calls[0].Inbox)
}

// seedDeleteSigner registers a local author with a keypair so DeliverService can
// sign, plus a remote follower inbox.
func seedDeleteSigner(t *testing.T, userRepo *testutil.MockUserRepository, keypairRepo *testutil.MockUserKeypairRepository) *model.User {
	t.Helper()
	author := &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["alice"] = author
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}
	return author
}

func seedRemoteRecipient(userRepo *testutil.MockUserRepository, id, host, inbox string) {
	h := host
	in := inbox
	userRepo.Users[id] = &model.User{ID: id, Username: id, Host: &h, Inbox: &in}
}

// 純粋リノート (ブースト) の取り消しは Delete(Tombstone) では連合しない。受信側は
// Announce を `uri = <noteURI>/activity` で保存するのに対し Tombstone の id は
// `/activity` の付かない note URI なので一致しない。upstream と同じく
// Undo(Announce) を出す。
func TestNoteDeleteHook_PureRenoteSendsUndoAnnounce(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newDeleteHookWithNotes(t)
	author := seedDeleteSigner(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes["alice"] = []string{"https://r.example/inbox"}
	targetURI := "https://remote.example/notes/orig"
	noteRepo.Notes["orig"] = &model.Note{ID: "orig", UserID: "bob", URI: &targetURI}

	renoteID := "orig"
	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice", RenoteID: &renoteID,
		Visibility: model.NoteVisibilityPublic,
	})

	require.Len(t, enq.calls, 1)
	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Undo", got["type"], "pure renote の削除は Undo(Announce)")
	inner, ok := got["object"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Announce", inner["type"])
	assert.Equal(t, "https://example.com/notes/n1/activity", inner["id"],
		"Announce id は受信側が renote の uri として保存した値と一致させる")
	assert.Equal(t, targetURI, inner["object"], "object はブースト元の URI")
}

// text 付き renote (引用) は pure renote ではないので従来どおり Delete。
func TestNoteDeleteHook_QuoteRenoteStillSendsDelete(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newDeleteHookWithNotes(t)
	author := seedDeleteSigner(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes["alice"] = []string{"https://r.example/inbox"}
	targetURI := "https://remote.example/notes/orig"
	noteRepo.Notes["orig"] = &model.Note{ID: "orig", UserID: "bob", URI: &targetURI}

	renoteID := "orig"
	text := "quoting"
	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice", RenoteID: &renoteID, Text: &text,
		Visibility: model.NoteVisibilityPublic,
	})

	require.Len(t, enq.calls, 1)
	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Delete", got["type"])
}

// specified な pure renote は Announce 自体を連合していない (#1886) ので、
// 取り消す対象も無い。
func TestNoteDeleteHook_PureRenoteSpecifiedNotDelivered(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newDeleteHookWithNotes(t)
	// **userRepo を配線してから見る。** 配線しないと directInboxes が常に空を
	// 返すので、skip を外しても配送が起きず検査が空振りする。
	hook.SetUserRepo(userRepo)
	author := seedDeleteSigner(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes["alice"] = []string{"https://r.example/inbox"}
	noteRepo.Notes["orig"] = &model.Note{ID: "orig", UserID: "bob"}
	seedRemoteRecipient(userRepo, "dmpeer", "dmpeer.example", "https://dmpeer.example/inbox")

	renoteID := "orig"
	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice", RenoteID: &renoteID,
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"dmpeer"},
	})
	assert.Empty(t, enq.calls)
}

// DM の Delete は宛先へ届かなければ意味が無い。宛先はフォロワーとは限らない。
func TestNoteDeleteHook_SpecifiedReachesRecipientsNotFollowers(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo := newDeleteHook(t)
	hook.SetUserRepo(userRepo)
	author := seedDeleteSigner(t, userRepo, keypairRepo)
	followerInbox := "https://follower.example/inbox"
	followingRepo.RemoteInboxes["alice"] = []string{followerInbox}
	dmInbox := "https://dmpeer.example/users/bob/inbox"
	seedRemoteRecipient(userRepo, "dmpeer", "dmpeer.example", dmInbox)

	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"dmpeer"},
	})

	require.Len(t, enq.calls, 1, "宛先のみ (フォロワーには送らない)")
	assert.Equal(t, dmInbox, enq.calls[0].Inbox)
}

// followers 可視性でメンションされたリモート user は Create を直接受け取って
// いる (note_delivery_hook の deliverToDirectRecipients) ので、Delete も直接
// 届けないと相手側に残り続ける。
func TestNoteDeleteHook_FollowersVisibilityReachesMentionedNonFollower(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo := newDeleteHook(t)
	hook.SetUserRepo(userRepo)
	author := seedDeleteSigner(t, userRepo, keypairRepo)
	followerInbox := "https://follower.example/inbox"
	followingRepo.RemoteInboxes["alice"] = []string{followerInbox}
	mentionInbox := "https://mentioned.example/users/carol/inbox"
	seedRemoteRecipient(userRepo, "carol", "mentioned.example", mentionInbox)

	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice",
		Visibility: model.NoteVisibilityFollowers,
		Mentions:   model.StringArray{"carol"},
	})

	inboxes := map[string]int{}
	for _, c := range enq.calls {
		inboxes[c.Inbox]++
	}
	assert.Equal(t, 1, inboxes[mentionInbox], "メンション先にも届く")
	assert.Equal(t, 1, inboxes[followerInbox], "フォロワーにも届く")
	assert.Len(t, enq.calls, 2)
}

// direct とフォロワーで同じ inbox を共有していても 2 回送らない。
func TestNoteDeleteHook_DirectAndFollowerInboxNotDuplicated(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo := newDeleteHook(t)
	hook.SetUserRepo(userRepo)
	author := seedDeleteSigner(t, userRepo, keypairRepo)
	shared := "https://mentioned.example/inbox"
	followingRepo.RemoteInboxes["alice"] = []string{shared}
	seedRemoteRecipient(userRepo, "carol", "mentioned.example", shared)

	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice",
		Visibility: model.NoteVisibilityFollowers,
		Mentions:   model.StringArray{"carol"},
	})
	require.Len(t, enq.calls, 1)
	assert.Equal(t, shared, enq.calls[0].Inbox)
}

// ローカルユーザー宛の DM は連合配送が要らない (宛先が全員ローカルなら 0 件)。
func TestNoteDeleteHook_SpecifiedLocalRecipientsOnly(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo := newDeleteHook(t)
	hook.SetUserRepo(userRepo)
	author := seedDeleteSigner(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes["alice"] = []string{"https://follower.example/inbox"}
	// inbox 列を持たせても送らない (local user は Host == nil で判定する)。
	localInbox := "https://example.com/users/localpeer/inbox"
	userRepo.Users["localpeer"] = &model.User{ID: "localpeer", Username: "localpeer", Inbox: &localInbox}

	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"localpeer"},
	})
	assert.Empty(t, enq.calls)
}

// --- #2995: renote / reply したリモート user へも届ける ---

// seedRenoterNote records a remote note that renotes or replies to targetID so
// the hook's note repository lookup finds its author.
func seedRenoterNote(noteRepo *testutil.MockNoteRepository, id, userID, host, targetID string, isReply bool) {
	t := targetID
	n := &model.Note{ID: id, UserID: userID, UserHost: &host}
	if isReply {
		n.ReplyID = &t
	} else {
		n.RenoteID = &t
	}
	noteRepo.Notes[id] = n
}

// **この issue (#2995) の中核。** フォローしていないリモート user が renote した
// followers 限定ノートを消すと、フォロワー配送では相手に届かず、削除したはずの
// ノートが相手のサーバーに残り続ける。
func TestNoteDeleteHook_FollowersVisibilityReachesRenoterNonFollower(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newDeleteHookWithNotes(t)
	hook.SetUserRepo(userRepo)
	hook.SetNoteRepo(noteRepo)
	author := seedDeleteSigner(t, userRepo, keypairRepo)
	followerInbox := "https://follower.example/inbox"
	followingRepo.RemoteInboxes["alice"] = []string{followerInbox}
	renoterInbox := "https://renoter.example/users/dave/inbox"
	seedRemoteRecipient(userRepo, "dave", "renoter.example", renoterInbox)
	seedRenoterNote(noteRepo, "rn1", "dave", "renoter.example", "n1", false)

	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice",
		Visibility: model.NoteVisibilityFollowers,
	})

	inboxes := map[string]int{}
	for _, c := range enq.calls {
		inboxes[c.Inbox]++
	}
	assert.Equal(t, 1, inboxes[renoterInbox], "renote した非フォロワーに届かない")
	assert.Equal(t, 1, inboxes[followerInbox], "フォロワーにも届く")
	assert.Len(t, enq.calls, 2)
}

// **DM では renote / reply した相手を宛先にしない。** inbound の reply は
// 可視性を見ずに `replyId` を結ぶので、敵対的な host が任意のローカル note id を
// `replyId` に書いた note を投げておくだけで、「その DM が存在し、いつ誰に
// 消されたか」を Delete の配送で確かめられてしまう。正当な返信者は元から
// `visibleUserIDs` に居るので、届く相手は減らない。
func TestNoteDeleteHook_SpecifiedDoesNotReachNonRecipientReplier(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newDeleteHookWithNotes(t)
	hook.SetUserRepo(userRepo)
	hook.SetNoteRepo(noteRepo)
	author := seedDeleteSigner(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes["alice"] = []string{"https://follower.example/inbox"}
	// 宛先ではないのに reply 行だけを持っている相手。
	seedRemoteRecipient(userRepo, "mallory", "evil.example", "https://evil.example/users/mallory/inbox")
	seedRenoterNote(noteRepo, "rp1", "mallory", "evil.example", "n1", true)
	// 宛先の相手にはもちろん届く。
	recipientInbox := "https://recipient.example/users/erin/inbox"
	seedRemoteRecipient(userRepo, "erin", "recipient.example", recipientInbox)

	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"erin"},
	})

	require.Len(t, enq.calls, 1, "宛先でない相手に DM の Delete を配っている")
	assert.Equal(t, recipientInbox, enq.calls[0].Inbox)
}

// フォロワーかつ renote 者でも 1 回だけ。**renote だけの相手には別途届く**ので、
// この 2 つを同じテストで見る (片方だけだと「機能を丸ごと消しても緑」になる)。
func TestNoteDeleteHook_RenoterWhoIsAlsoFollowerNotDuplicated(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newDeleteHookWithNotes(t)
	hook.SetUserRepo(userRepo)
	hook.SetNoteRepo(noteRepo)
	author := seedDeleteSigner(t, userRepo, keypairRepo)
	shared := "https://renoter.example/inbox"
	followingRepo.RemoteInboxes["alice"] = []string{shared}
	// フォロワーでもある renote 者 (inbox はフォロワー配送と同じ)。
	seedRemoteRecipient(userRepo, "dave", "renoter.example", shared)
	seedRenoterNote(noteRepo, "rn1", "dave", "renoter.example", "n1", false)
	// フォローしていない renote 者 (この分は renote 経路でしか届かない)。
	onlyRenoter := "https://other.example/users/frank/inbox"
	seedRemoteRecipient(userRepo, "frank", "other.example", onlyRenoter)
	seedRenoterNote(noteRepo, "rn2", "frank", "other.example", "n1", false)

	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice",
		Visibility: model.NoteVisibilityFollowers,
	})

	inboxes := map[string]int{}
	for _, c := range enq.calls {
		inboxes[c.Inbox]++
	}
	assert.Equal(t, 1, inboxes[shared], "フォロワーかつ renote 者へ 2 回送っている")
	assert.Equal(t, 1, inboxes[onlyRenoter], "renote しかしていない相手に届かない")
	assert.Len(t, enq.calls, 2)
}

// **引けなくても他の宛先は配る。** ここで諦めると、元から届いていたメンション先への
// Delete まで道連れになる。
func TestNoteDeleteHook_RenoteLookupFailureStillDeliversMentions(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newDeleteHookWithNotes(t)
	hook.SetUserRepo(userRepo)
	hook.SetNoteRepo(noteRepo)
	noteRepo.ListRenoteOrReplyErr = assert.AnError
	author := seedDeleteSigner(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes["alice"] = []string{}
	mentionInbox := "https://mentioned.example/users/carol/inbox"
	seedRemoteRecipient(userRepo, "carol", "mentioned.example", mentionInbox)

	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice",
		Visibility: model.NoteVisibilitySpecified,
		Mentions:   model.StringArray{"carol"},
	})
	require.Len(t, enq.calls, 1)
	assert.Equal(t, mentionInbox, enq.calls[0].Inbox)
}

// note repository 未配線では従来どおり (メンション先と DM 宛先だけ)。
func TestNoteDeleteHook_WithoutNoteRepoSkipsRenoters(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newDeleteHookWithNotes(t)
	hook.SetUserRepo(userRepo)
	assert.False(t, hook.HasNoteRepo())
	author := seedDeleteSigner(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes["alice"] = []string{}
	seedRemoteRecipient(userRepo, "dave", "renoter.example", "https://renoter.example/users/dave/inbox")
	seedRenoterNote(noteRepo, "rn1", "dave", "renoter.example", "n1", false)

	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice",
		Visibility: model.NoteVisibilityFollowers,
	})
	assert.Empty(t, enq.calls)
}

// **sharedInbox を持つフォロワーでも 1 通であること (敵対的レビュー)。**
//
// `seedRemoteRecipient` は `SharedInbox` を設定しないので、既存の dedup テストは
// 「direct と follower が同じ URL」という現実には起きにくい形しか見ていなかった。
// **実際の `ListRemoteFollowerInboxes` は `COALESCE(NULLIF(sharedInbox,”), inbox)`
// を返す**ので、direct 側が個別 inbox を使うと URL が食い違い、exclude が
// 効かずに 2 通届く。
func TestNoteDeleteHook_SharedInboxFollower_Deduped(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo := newDeleteHook(t)
	hook.SetUserRepo(userRepo)
	author := seedDeleteSigner(t, userRepo, keypairRepo)

	const shared = "https://mentioned.example/inbox"
	// carol はメンション先かつフォロワー。個別 inbox と sharedInbox の両方を持つ
	// (リモートの通常の形)。
	h := "mentioned.example"
	in := "https://mentioned.example/users/carol/inbox"
	s := shared
	userRepo.Users["carol"] = &model.User{
		ID: "carol", Username: "carol", Host: &h, Inbox: &in, SharedInbox: &s,
	}
	// フォロワー側は sharedInbox を返す (repository の COALESCE と同じ)。
	followingRepo.RemoteInboxes["alice"] = []string{shared}

	hook.OnNoteDeleted(author, &model.Note{
		ID: "n1", UserID: "alice",
		Visibility: model.NoteVisibilityFollowers,
		Mentions:   model.StringArray{"carol"},
	})
	require.Len(t, enq.calls, 1,
		"sharedInbox を持つフォロワー宛に同じ Delete が 2 通出ている (exclude が効いていない)")
	assert.Equal(t, shared, enq.calls[0].Inbox)
}
