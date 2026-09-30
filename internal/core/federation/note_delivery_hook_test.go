package federation_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/core/federation"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newNoteDeliveryHook(t *testing.T) (
	*federation.NoteDeliveryHook,
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
	idGen, _ := id.NewGenerator("aidx")
	hook := federation.NewNoteDeliveryHook(deliver, renderer, urls, idGen, userRepo, noteRepo)
	return hook, enq, userRepo, followingRepo, keypairRepo, noteRepo
}

func makeLocalAuthor(t *testing.T, userRepo *testutil.MockUserRepository, keypairRepo *testutil.MockUserKeypairRepository) *model.User {
	t.Helper()
	user := &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["alice"] = user
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{
		UserID:     "alice",
		PublicKey:  "PUB",
		PrivateKey: "PEM",
	}
	return user
}

func makeNote(authorID string, visibility model.NoteVisibility) *model.Note {
	idGen, _ := id.NewGenerator("aidx")
	return &model.Note{
		ID:         idGen.Generate(time.Now()),
		UserID:     authorID,
		Visibility: visibility,
	}
}

func TestNoteDeliveryHook_Public_Followers(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes[author.ID] = []string{"https://r.example/inbox"}
	note := makeNote(author.ID, model.NoteVisibilityPublic)

	hook.OnNoteCreated(note, author)
	require.Len(t, enq.calls, 1)
	assert.Equal(t, "https://r.example/inbox", enq.calls[0].Inbox)

	// 配信bodyにCreate activityが入っているか軽く確認
	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Create", got["type"])
}

func TestNoteDeliveryHook_Home_DeliversToFollowers(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes[author.ID] = []string{"https://r.example/inbox"}
	note := makeNote(author.ID, model.NoteVisibilityHome)

	hook.OnNoteCreated(note, author)
	assert.Len(t, enq.calls, 1)
}

func TestNoteDeliveryHook_FollowersOnly_DeliversToFollowers(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes[author.ID] = []string{"https://r.example/inbox"}
	note := makeNote(author.ID, model.NoteVisibilityFollowers)

	hook.OnNoteCreated(note, author)
	assert.Len(t, enq.calls, 1)
}

func TestNoteDeliveryHook_LocalOnly_Skips(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	note := makeNote(author.ID, model.NoteVisibilityPublic)
	note.LocalOnly = true

	hook.OnNoteCreated(note, author)
	assert.Empty(t, enq.calls)
}

func TestNoteDeliveryHook_RemoteAuthor_Skips(t *testing.T) {
	hook, enq, _, _, _, _ := newNoteDeliveryHook(t)
	host := "remote.example"
	author := &model.User{ID: "bob", Username: "bob", Host: &host}
	note := makeNote(author.ID, model.NoteVisibilityPublic)

	hook.OnNoteCreated(note, author)
	assert.Empty(t, enq.calls)
}

func TestNoteDeliveryHook_NilArgs(t *testing.T) {
	hook, enq, _, _, _, _ := newNoteDeliveryHook(t)
	// note nil
	hook.OnNoteCreated(nil, &model.User{ID: "x"})
	// author nil
	hook.OnNoteCreated(&model.Note{}, nil)
	assert.Empty(t, enq.calls)
}

func TestNoteDeliveryHook_Specified_RemoteUsers(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)

	// 配信先: リモートユーザー1人 + ローカル1人 + 不在1人
	host := "remote.example"
	inbox := "https://remote.example/users/r1/inbox"
	userRepo.Users["r1"] = &model.User{ID: "r1", Username: "r1", Host: &host, Inbox: &inbox}
	userRepo.Users["local"] = &model.User{ID: "local", Username: "local"}

	note := makeNote(author.ID, model.NoteVisibilitySpecified)
	note.VisibleUserIDs = []string{"r1", "local", "missing"}

	hook.OnNoteCreated(note, author)
	require.Len(t, enq.calls, 1)
	assert.Equal(t, inbox, enq.calls[0].Inbox)
}

// **DM は sharedInbox へ送らない** (セキュリティ監査 2026-09-21)。
//
// upstream の direct recipe は `inboxes.set(recipe.to.inbox, false)` で必ず
// 個別 inbox を使う。sharedInbox へ送ると (a) shared inbox で非公開 activity を
// 扱わない実装では**黙って落ち**、(b) 410 Gone が host 単位の gone 判定へ届いて
// **DM 1 通でインスタンス全体を suspend** しうる。
func TestNoteDeliveryHook_Specified_UsesIndividualInbox(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)

	host := "remote.example"
	inbox := "https://remote.example/users/r1/inbox"
	shared := "https://remote.example/inbox"
	userRepo.Users["r1"] = &model.User{ID: "r1", Username: "r1", Host: &host, Inbox: &inbox, SharedInbox: &shared}

	note := makeNote(author.ID, model.NoteVisibilitySpecified)
	note.VisibleUserIDs = []string{"r1"}

	hook.OnNoteCreated(note, author)
	require.Len(t, enq.calls, 1)
	assert.Equal(t, inbox, enq.calls[0].Inbox, "DM を sharedInbox へ送らないこと")
	assert.False(t, enq.calls[0].IsSharedInbox,
		"DM 1 通の 410 が host 単位の gone 判定へ届かないこと")
}

func TestNoteDeliveryHook_Public_DeliverError_DoesNotPanic(t *testing.T) {
	hook, enq, userRepo, followingRepo, _, _ := newNoteDeliveryHook(t)
	author := &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["alice"] = author
	// keypair が無いので signerCredentials が失敗する → DeliverToFollowers は err
	followingRepo.RemoteInboxes[author.ID] = []string{"https://r.example/inbox"}

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	hook.OnNoteCreated(note, author) // panic しないこと
	assert.Empty(t, enq.calls)
}

func TestNoteDeliveryHook_Specified_DeliverError_DoesNotPanic(t *testing.T) {
	hook, enq, userRepo, _, _, _ := newNoteDeliveryHook(t)
	author := &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["alice"] = author
	host := "remote.example"
	inbox := "https://remote.example/users/r1/inbox"
	userRepo.Users["r1"] = &model.User{ID: "r1", Username: "r1", Host: &host, Inbox: &inbox}

	note := makeNote(author.ID, model.NoteVisibilitySpecified)
	note.VisibleUserIDs = []string{"r1"}

	hook.OnNoteCreated(note, author)
	assert.Empty(t, enq.calls) // signer key 不在で enqueue 失敗
}

func TestNoteDeliveryHook_MentionedRemoteUser_Delivered(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)

	host := "remote.example"
	inbox := "https://remote.example/users/bob/inbox"
	userRepo.Users["bob"] = &model.User{
		ID:            "bob",
		Username:      "bob",
		UsernameLower: "bob",
		Host:          &host,
		Inbox:         &inbox,
	}
	// Also cache a local mention that must NOT trigger remote delivery.
	userRepo.Users["charlie"] = &model.User{
		ID:            "charlie",
		Username:      "charlie",
		UsernameLower: "charlie",
	}

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	text := "hi @bob@remote.example and @charlie"
	note.Text = &text
	hook.OnNoteCreated(note, author)

	// No followers exist — only the mention delivery enqueues (1 call).
	require.Len(t, enq.calls, 1)
	assert.Equal(t, inbox, enq.calls[0].Inbox)
}

func TestNoteDeliveryHook_MentionedRemoteUser_UnknownSkipped(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	text := "hi @ghost@unknown.example"
	note.Text = &text
	hook.OnNoteCreated(note, author)

	// Unknown user resolution fails — no enqueue.
	assert.Empty(t, enq.calls)
}

func TestNoteDeliveryHook_MentionedRemoteUser_Duplicate(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)

	host := "remote.example"
	inbox := "https://remote.example/users/bob/inbox"
	bob := &model.User{
		ID:            "bob",
		Username:      "bob",
		UsernameLower: "bob",
		Host:          &host,
		Inbox:         &inbox,
	}
	userRepo.Users["bob"] = bob
	// Two distinct @-tokens resolve to the same user ID (username alias case),
	// so the mention delivery must dedup by user ID (not by mention token).
	userRepo.FindByUsernameLowerFn = func(_ string, _ *string) (*model.User, error) {
		return bob, nil
	}

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	text := "@bob@remote.example hi @bobalias@remote.example"
	note.Text = &text
	hook.OnNoteCreated(note, author)

	// Deduped by user ID: only one enqueue.
	require.Len(t, enq.calls, 1)
}

func TestNoteDeliveryHook_MentionedRemoteUser_DeliverError_DoesNotPanic(t *testing.T) {
	hook, enq, userRepo, _, _, _ := newNoteDeliveryHook(t)
	// author has no keypair → signer lookup fails → DeliverToUser returns error.
	author := &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["alice"] = author

	host := "remote.example"
	inbox := "https://remote.example/users/bob/inbox"
	userRepo.Users["bob"] = &model.User{
		ID:            "bob",
		Username:      "bob",
		UsernameLower: "bob",
		Host:          &host,
		Inbox:         &inbox,
	}
	note := makeNote(author.ID, model.NoteVisibilityPublic)
	text := "hi @bob@remote.example"
	note.Text = &text
	hook.OnNoteCreated(note, author) // panic しないこと
	assert.Empty(t, enq.calls)
}

func TestNoteDeliveryHook_MentionedRemoteUser_NoText(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	// No text at all; mention delivery should simply return.
	hook.OnNoteCreated(note, author)
	assert.Empty(t, enq.calls)
}

func TestNoteDeliveryHook_MentionedRemoteUser_NoMentions(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	text := "plain text with no mentions"
	note.Text = &text
	hook.OnNoteCreated(note, author)
	assert.Empty(t, enq.calls)
}

// --- #369: reply target / quote renote target への直接 deliver -----------

func TestNoteDeliveryHook_ReplyToRemote_DeliversToReplyAuthor(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, noteRepo := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)

	host := "remote.example"
	inbox := "https://remote.example/users/bob/inbox"
	bob := &model.User{
		ID: "bob", Username: "bob", UsernameLower: "bob",
		Host: &host, Inbox: &inbox,
	}
	userRepo.Users["bob"] = bob
	bobNote := &model.Note{ID: "bobnote", UserID: "bob"}
	noteRepo.Notes["bobnote"] = bobNote

	replyID := "bobnote"
	note := makeNote(author.ID, model.NoteVisibilityPublic)
	note.ReplyID = &replyID
	// 本文には bob への mention を含めない (mention 経路で誤って拾われていない
	// ことを確認するため)
	text := "just a plain reply without @mention"
	note.Text = &text

	hook.OnNoteCreated(note, author)

	// no followers、mention も text になし。reply target 経路のみで 1 回 enqueue。
	require.Len(t, enq.calls, 1)
	assert.Equal(t, inbox, enq.calls[0].Inbox)
}

func TestNoteDeliveryHook_ReplyToLocal_NoDelivery(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, noteRepo := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)

	localBob := &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	userRepo.Users["bob"] = localBob
	bobNote := &model.Note{ID: "bobnote", UserID: "bob"}
	noteRepo.Notes["bobnote"] = bobNote

	replyID := "bobnote"
	note := makeNote(author.ID, model.NoteVisibilityPublic)
	note.ReplyID = &replyID

	hook.OnNoteCreated(note, author)
	// local target は連合配信不要
	assert.Empty(t, enq.calls)
}

func TestNoteDeliveryHook_ReplyToSelf_NoDelivery(t *testing.T) {
	// 自分の note への reply (self-reply) は連合配信不要。seen dedup で弾く。
	hook, enq, userRepo, _, keypairRepo, noteRepo := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)

	selfNote := &model.Note{ID: "self1", UserID: author.ID}
	noteRepo.Notes["self1"] = selfNote
	replyID := "self1"

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	note.ReplyID = &replyID

	hook.OnNoteCreated(note, author)
	assert.Empty(t, enq.calls)
}

func TestNoteDeliveryHook_ReplyMentionDedup(t *testing.T) {
	// reply target が mention と同じリモートユーザーの場合、重複配信しない。
	hook, enq, userRepo, _, keypairRepo, noteRepo := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)

	host := "remote.example"
	inbox := "https://remote.example/users/bob/inbox"
	bob := &model.User{
		ID: "bob", Username: "bob", UsernameLower: "bob",
		Host: &host, Inbox: &inbox,
	}
	userRepo.Users["bob"] = bob
	bobNote := &model.Note{ID: "bobnote", UserID: "bob"}
	noteRepo.Notes["bobnote"] = bobNote

	replyID := "bobnote"
	note := makeNote(author.ID, model.NoteVisibilityPublic)
	note.ReplyID = &replyID
	text := "@bob@remote.example hi" // mention にも bob
	note.Text = &text

	hook.OnNoteCreated(note, author)
	// mention + reply target = 同一 user → 1 回のみ
	require.Len(t, enq.calls, 1)
}

func TestNoteDeliveryHook_QuoteRenote_DeliversToRenoteAuthor(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, noteRepo := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)

	host := "remote.example"
	inbox := "https://remote.example/users/bob/inbox"
	bob := &model.User{
		ID: "bob", Username: "bob", UsernameLower: "bob",
		Host: &host, Inbox: &inbox,
	}
	userRepo.Users["bob"] = bob
	bobNote := &model.Note{ID: "bobnote", UserID: "bob"}
	noteRepo.Notes["bobnote"] = bobNote

	renoteID := "bobnote"
	note := makeNote(author.ID, model.NoteVisibilityPublic)
	note.RenoteID = &renoteID
	text := "my quote comment" // text があるので pure renote ではなく quote renote
	note.Text = &text

	hook.OnNoteCreated(note, author)
	require.Len(t, enq.calls, 1)
	assert.Equal(t, inbox, enq.calls[0].Inbox)
}

func TestNoteDeliveryHook_PureRenoteRemote_NoDirectDelivery(t *testing.T) {
	// pure renote は Announce で follower に配信されるので、quote 経路の
	// 直接配信は発動しない。
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	// follower は 0。Announce でも enqueue されない。
	followingRepo.RemoteInboxes[author.ID] = []string{}

	host := "remote.example"
	inbox := "https://remote.example/users/bob/inbox"
	bob := &model.User{
		ID: "bob", Username: "bob", UsernameLower: "bob",
		Host: &host, Inbox: &inbox,
	}
	userRepo.Users["bob"] = bob
	bobNote := &model.Note{ID: "bobnote", UserID: "bob"}
	noteRepo.Notes["bobnote"] = bobNote

	renoteID := "bobnote"
	note := makeNote(author.ID, model.NoteVisibilityPublic)
	note.RenoteID = &renoteID
	// text/cw/files なし → pure renote

	hook.OnNoteCreated(note, author)
	// follower 0 + pure renote → enqueue 無し (quote 直接配信は発動しない)
	assert.Empty(t, enq.calls)
}

func TestNoteDeliveryHook_PureRenote_LocalTarget_EmitsAnnounce(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes[author.ID] = []string{"https://r.example/inbox"}

	target := &model.Note{ID: "tgt1", UserID: "other", Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes["tgt1"] = target

	renoteID := "tgt1"
	renote := makeNote(author.ID, model.NoteVisibilityPublic)
	renote.RenoteID = &renoteID

	hook.OnNoteCreated(renote, author)
	require.Len(t, enq.calls, 1)

	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Announce", got["type"])
	assert.Equal(t, "https://example.com/notes/tgt1", got["object"])
}

func TestNoteDeliveryHook_PureRenote_RemoteTarget_UsesURI(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes[author.ID] = []string{"https://r.example/inbox"}

	uri := "https://remote.example/notes/orig"
	target := &model.Note{ID: "tgt1", UserID: "remote", Visibility: model.NoteVisibilityPublic, URI: &uri}
	noteRepo.Notes["tgt1"] = target

	renoteID := "tgt1"
	renote := makeNote(author.ID, model.NoteVisibilityPublic)
	renote.RenoteID = &renoteID

	hook.OnNoteCreated(renote, author)
	require.Len(t, enq.calls, 1)
	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Announce", got["type"])
	assert.Equal(t, uri, got["object"])
}

// specified visibility の pure renote は federate しない (#1886、upstream は
// renderAnnounce が specified で throw して federation を中止する)。public-addressed
// Announce を follower に漏らさない。
func TestNoteDeliveryHook_SpecifiedPureRenote_NoAnnounce(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes[author.ID] = []string{"https://r.example/inbox"}
	noteRepo.Notes["tgt1"] = &model.Note{ID: "tgt1", UserID: "other", Visibility: model.NoteVisibilityPublic}

	renoteID := "tgt1"
	renote := makeNote(author.ID, model.NoteVisibilitySpecified)
	renote.RenoteID = &renoteID

	hook.OnNoteCreated(renote, author)
	assert.Empty(t, enq.calls, "specified pure renote must not federate")
}

func TestNoteDeliveryHook_PureRenote_TargetNotFound(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)

	renoteID := "missing"
	renote := makeNote(author.ID, model.NoteVisibilityPublic)
	renote.RenoteID = &renoteID

	hook.OnNoteCreated(renote, author)
	assert.Empty(t, enq.calls)
}

func TestNoteDeliveryHook_PureRenote_DeliverError_DoesNotPanic(t *testing.T) {
	hook, enq, userRepo, followingRepo, _, noteRepo := newNoteDeliveryHook(t)
	author := &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["alice"] = author
	followingRepo.RemoteInboxes[author.ID] = []string{"https://r.example/inbox"}
	noteRepo.Notes["tgt"] = &model.Note{ID: "tgt", UserID: "other"}

	renoteID := "tgt"
	renote := makeNote(author.ID, model.NoteVisibilityPublic)
	renote.RenoteID = &renoteID

	hook.OnNoteCreated(renote, author)
	assert.Empty(t, enq.calls)
}

// fakeRelayBroadcaster captures DeliverToAccepted calls to assert that
// NoteDeliveryHook only fan outs public notes to relays (not home /
// followers / specified).
type fakeRelayBroadcaster struct {
	calls []relayCall
	err   error
}

type relayCall struct {
	SignerID string
	Activity any
}

func (f *fakeRelayBroadcaster) DeliverToAccepted(_ context.Context, signerUserID string, activity any) error {
	f.calls = append(f.calls, relayCall{SignerID: signerUserID, Activity: activity})
	return f.err
}

func TestNoteDeliveryHook_Public_FansOutToRelay(t *testing.T) {
	hook, _, userRepo, _, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	relay := &fakeRelayBroadcaster{}
	hook.SetRelayBroadcaster(relay)

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	hook.OnNoteCreated(note, author)

	require.Len(t, relay.calls, 1)
	assert.Equal(t, author.ID, relay.calls[0].SignerID)
}

func TestNoteDeliveryHook_Home_SkipsRelay(t *testing.T) {
	hook, _, userRepo, _, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	relay := &fakeRelayBroadcaster{}
	hook.SetRelayBroadcaster(relay)

	note := makeNote(author.ID, model.NoteVisibilityHome)
	hook.OnNoteCreated(note, author)

	// home visibility は relay 対象外 (as:Public addressed でないため)
	assert.Empty(t, relay.calls)
}

func TestNoteDeliveryHook_RelayBroadcasterNil_IsSafe(t *testing.T) {
	hook, _, userRepo, _, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	// SetRelayBroadcaster を呼ばなくても panic しない
	hook.OnNoteCreated(note, author)
}

type stubQuoteApprovals struct {
	enq    *stubEnqueuer
	calls  []string
	err    error
	issued bool
}

func (s *stubQuoteApprovals) Prepare(n *model.Note, _ *model.User) error {
	s.calls = append(s.calls, fmt.Sprintf("prepare %s after %d deliveries", n.ID, len(s.enq.calls)))
	if s.err == nil {
		s.issued = true
	}
	return s.err
}

func (s *stubQuoteApprovals) RequestApproval(n *model.Note, _ *model.User) error {
	s.calls = append(s.calls, fmt.Sprintf("request %s after %d deliveries", n.ID, len(s.enq.calls)))
	return s.err
}

// 引用の承認 (#3234 段階 3): ローカル同士の承認は Create を描画する前に発行し
// (最初の Create から quoteAuthorization が付く。後から Update は送らない)、
// リモートへの QuoteRequest は Create の後に送る。失敗しても配送は止めない。
func TestNoteDeliveryHook_QuoteApprovalOrder(t *testing.T) {
	for _, fail := range []bool{false, true} {
		enq := &stubEnqueuer{}
		userRepo := testutil.NewMockUserRepository()
		followingRepo := testutil.NewMockFollowingRepository()
		keypairRepo := testutil.NewMockUserKeypairRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		renderer := activitypub.NewRenderer(urls)
		q := &stubQuoteApprovals{enq: enq}
		// renderer は Prepare が発行した承認を読む (発行前に描画すると付かない)。
		renderer.SetQuoteApprovalResolver(func(*model.Note) (string, error) {
			if q.issued {
				return "https://example.com/notes/target/quote-authorizations/a1", nil
			}
			return "", nil
		})
		idGen, _ := id.NewGenerator("aidx")
		hook := federation.NewNoteDeliveryHook(federation.NewDeliverService(enq, userRepo, followingRepo, keypairRepo, urls), renderer, urls, idGen, userRepo, testutil.NewMockNoteRepository())
		author := makeLocalAuthor(t, userRepo, keypairRepo)
		followingRepo.RemoteInboxes[author.ID] = []string{"https://r.example/inbox"}
		if fail {
			q.err = assert.AnError
		}
		hook.SetQuoteOutbox(q)
		text, target := "look", "target"
		note := makeNote(author.ID, model.NoteVisibilityPublic)
		note.Text, note.RenoteID = &text, &target
		hook.OnNoteCreated(note, author)
		assert.Equal(t, []string{
			"prepare " + note.ID + " after 0 deliveries",
			"request " + note.ID + " after 1 deliveries",
		}, q.calls)
		require.Len(t, enq.calls, 1, "the Create is delivered either way")
		var create map[string]any
		require.NoError(t, json.Unmarshal(enq.calls[0].Body, &create))
		obj := create["object"].(map[string]any)
		if fail {
			assert.NotContains(t, obj, "quoteAuthorization")
		} else {
			assert.Equal(t, "https://example.com/notes/target/quote-authorizations/a1", obj["quoteAuthorization"])
		}
	}
}

func TestNoteDeliveryHook_SendQuoteRequest(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	host, inbox := "remote.example", "https://remote.example/users/bob/inbox"
	bob := &model.User{ID: "bob", Username: "bob", Host: &host, Inbox: &inbox}
	text, target := "look", "bobnote"
	note := makeNote(author.ID, model.NoteVisibilityPublic)
	note.Text, note.RenoteID = &text, &target

	require.NoError(t, hook.SendQuoteRequest(note, "https://remote.example/notes/1", bob))
	require.Len(t, enq.calls, 1)
	assert.Equal(t, inbox, enq.calls[0].Inbox)
	// 配送の queue では再試行しない (#3238。再試行は状態を確かめる定期処理だけ)。
	assert.True(t, enq.opts[0].MaxRetrySet)
	assert.Equal(t, 0, enq.opts[0].MaxRetry)
	// 後へ回されて遅れて届かないよう、期限を付ける (5 分)。
	assert.NotZero(t, enq.calls[0].NotAfter)
	assert.WithinDuration(t, time.Now().Add(5*time.Minute), time.UnixMilli(enq.calls[0].NotAfter), 10*time.Second)
	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "QuoteRequest", got["type"])
	assert.Equal(t, "https://remote.example/notes/1", got["object"])

	// ローカルの相手には送らない。送れなければ error。
	enq.calls = nil
	require.NoError(t, hook.SendQuoteRequest(note, "https://remote.example/notes/1", &model.User{ID: "dave"}))
	assert.Empty(t, enq.calls)
	enq.err = assert.AnError
	assert.Error(t, hook.SendQuoteRequest(note, "https://remote.example/notes/1", bob))
}

func TestNoteDeliveryHook_SendNoteUpdate(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes[author.ID] = []string{"https://r.example/inbox"}
	host, inbox := "remote.example", "https://remote.example/users/bob/inbox"
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &host, Inbox: &inbox}
	noteRepo.Notes["bobnote"] = &model.Note{ID: "bobnote", UserID: "bob"}
	text, target := "look", "bobnote"
	relay := &fakeRelayBroadcaster{}
	hook.SetRelayBroadcaster(relay)

	for _, vis := range []model.NoteVisibility{model.NoteVisibilityPublic, model.NoteVisibilityHome, model.NoteVisibilityFollowers} {
		enq.calls, relay.calls = nil, nil
		note := makeNote(author.ID, vis)
		note.Text, note.RenoteID = &text, &target
		require.NoError(t, hook.SendNoteUpdate(note, author))
		// フォロワーと、引用される作者 (Create と同じ宛先) へ届く。
		var inboxes []string
		for _, c := range enq.calls {
			inboxes = append(inboxes, c.Inbox)
			var got map[string]any
			require.NoError(t, json.Unmarshal(c.Body, &got))
			assert.Equal(t, "Update", got["type"])
		}
		assert.ElementsMatch(t, []string{"https://r.example/inbox", inbox}, inboxes, vis)
		// relay 経由で Create を受け取ったサーバーにも届ける (公開のときだけ)。
		if vis == model.NoteVisibilityPublic {
			require.Len(t, relay.calls, 1, vis)
			assert.IsType(t, &activitypub.Update{}, relay.calls[0].Activity)
		} else {
			assert.Empty(t, relay.calls, vis)
		}
	}

	// ダイレクトの引用は承認を取りに行かないので、配り直しもしない
	// (引用される作者へも送らない)。
	// ローカル限定の投稿は連合しない。
	enq.calls = nil
	lonly := makeNote(author.ID, model.NoteVisibilityPublic)
	lonly.Text, lonly.RenoteID, lonly.LocalOnly = &text, &target, true
	require.NoError(t, hook.SendNoteUpdate(lonly, author))
	assert.Empty(t, enq.calls)

	enq.calls = nil
	dm := makeNote(author.ID, model.NoteVisibilitySpecified)
	dm.Text, dm.RenoteID = &text, &target
	require.NoError(t, hook.SendNoteUpdate(dm, author))
	assert.Empty(t, enq.calls, "direct notes never ask for approval")

	enq.err = assert.AnError
	assert.Error(t, hook.SendNoteUpdate(makeNote(author.ID, model.NoteVisibilityPublic), author))
}

// 承認を引けなければ Update を作らずに error を返す (再試行させる)。
func TestNoteDeliveryHook_SendNoteUpdate_ApprovalLookupFails(t *testing.T) {
	enq := &stubEnqueuer{}
	userRepo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	keypairRepo := testutil.NewMockUserKeypairRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	renderer := activitypub.NewRenderer(urls)
	renderer.SetQuoteApprovalResolver(func(*model.Note) (string, error) { return "", assert.AnError })
	idGen, _ := id.NewGenerator("aidx")
	hook := federation.NewNoteDeliveryHook(federation.NewDeliverService(enq, userRepo, followingRepo, keypairRepo, urls), renderer, urls, idGen, userRepo, testutil.NewMockNoteRepository())
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes[author.ID] = []string{"https://r.example/inbox"}
	text, target := "look", "bobnote"
	note := makeNote(author.ID, model.NoteVisibilityPublic)
	note.Text, note.RenoteID = &text, &target

	assert.ErrorIs(t, hook.SendNoteUpdate(note, author), assert.AnError)
	assert.Empty(t, enq.calls)
}

// 承認の取り消し (#3234 段階 4): Delete は作者のフォロワーと、リモートの相手の inbox へ。
func TestQuoteRequestDeliveryHook_SendApprovalDelete(t *testing.T) {
	_, enq, userRepo, followingRepo, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes[author.ID] = []string{"https://r.example/inbox"}
	urls := activitypub.NewURLBuilder("https://example.com")
	hook := federation.NewQuoteRequestDeliveryHook(federation.NewDeliverService(enq, userRepo, followingRepo, keypairRepo, urls), activitypub.NewRenderer(urls))
	host, inbox := "remote.example", "https://remote.example/users/bob/inbox"
	bob := &model.User{ID: "bob", Username: "bob", Host: &host, Inbox: &inbox}
	a := &model.NoteQuoteAuthorization{ID: "a1", NoteID: "n1", QuoterID: "bob", QuotingURI: "https://remote.example/notes/q"}

	require.NoError(t, hook.SendApprovalDelete(author, bob, a))
	var inboxes []string
	for _, c := range enq.calls {
		inboxes = append(inboxes, c.Inbox)
		var got map[string]any
		require.NoError(t, json.Unmarshal(c.Body, &got))
		assert.Equal(t, "Delete", got["type"])
		assert.Equal(t, "https://example.com/notes/n1/quote-authorizations/a1", got["object"].(map[string]any)["id"])
	}
	assert.ElementsMatch(t, []string{"https://r.example/inbox", inbox}, inboxes)

	// 相手がローカルなら、フォロワーだけ (引用する投稿の Update は別に配る)。
	enq.calls = nil
	require.NoError(t, hook.SendApprovalDelete(author, &model.User{ID: "dave"}, a))
	require.Len(t, enq.calls, 1)
	assert.Equal(t, "https://r.example/inbox", enq.calls[0].Inbox)

	// 相手と同じ inbox を持つフォロワーには二度送らない。
	enq.calls = nil
	followingRepo.RemoteInboxes[author.ID] = []string{"https://r.example/inbox", inbox}
	require.NoError(t, hook.SendApprovalDelete(author, bob, a))
	inboxes = nil
	for _, c := range enq.calls {
		inboxes = append(inboxes, c.Inbox)
	}
	assert.ElementsMatch(t, []string{"https://r.example/inbox", inbox}, inboxes)
	// 相手へは先に送る (フォロワーへの送信が落ちても相手には届く)。
	assert.Equal(t, inbox, enq.calls[0].Inbox)

	enq.err = assert.AnError
	assert.Error(t, hook.SendApprovalDelete(author, bob, a))
}
