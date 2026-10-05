package federation

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingEnqueuer captures DeliverPayloads enqueued via DeliverActivity so
// hook tests can assert what was scheduled without a real queue server. The
// non-deliver methods are no-ops to satisfy queue.Enqueuer.
type recordingEnqueuer struct {
	delivers []queue.DeliverPayload
}

func (r *recordingEnqueuer) EnqueueDeliver(p queue.DeliverPayload, _ ...driver.EnqueueOption) error {
	r.delivers = append(r.delivers, p)
	return nil
}
func (r *recordingEnqueuer) EnqueueExport(queue.ExportPayload) error { return nil }
func (r *recordingEnqueuer) EnqueueImport(queue.ImportPayload) error { return nil }
func (r *recordingEnqueuer) EnqueueWebPush(context.Context, queue.WebPushPayload) error {
	return nil
}
func (r *recordingEnqueuer) EnqueueUserWebhook(context.Context, queue.WebhookPayload) error {
	return nil
}
func (r *recordingEnqueuer) EnqueueSystemWebhook(context.Context, queue.WebhookPayload) error {
	return nil
}
func (r *recordingEnqueuer) EnqueueInbox(context.Context, queue.InboxPayload) error { return nil }
func (r *recordingEnqueuer) EnqueuePostScheduledNote(queue.PostScheduledNotePayload, ...driver.EnqueueOption) error {
	return nil
}
func (r *recordingEnqueuer) ClearScheduledNote(string) error { return nil }
func (r *recordingEnqueuer) Close() error                    { return nil }

func newHookSetup(t *testing.T) (*PollDeliveryHook, *recordingEnqueuer, *testutil.MockUserRepository, *testutil.MockUserKeypairRepository, id.Generator) {
	t.Helper()
	hook, enq, userRepo, keypairRepo, _, idGen := newHookSetupWithFollowing(t)
	return hook, enq, userRepo, keypairRepo, idGen
}

// newHookSetupWithFollowing also exposes the following repository so tests can
// seed remote followers and assert that they are NOT used as the recipient set.
func newHookSetupWithFollowing(t *testing.T) (*PollDeliveryHook, *recordingEnqueuer, *testutil.MockUserRepository, *testutil.MockUserKeypairRepository, *testutil.MockFollowingRepository, id.Generator) {
	t.Helper()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	urls := activitypub.NewURLBuilder("https://local.example")
	renderer := activitypub.NewRenderer(urls)
	enq := &recordingEnqueuer{}
	userRepo := testutil.NewMockUserRepository()
	keypairRepo := testutil.NewMockUserKeypairRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	deliver := NewDeliverService(enq, userRepo, followingRepo, keypairRepo, urls)
	hook := NewPollDeliveryHook(renderer, deliver, userRepo, urls, idGen)
	return hook, enq, userRepo, keypairRepo, followingRepo, idGen
}

func seedSigner(t *testing.T, userRepo *testutil.MockUserRepository, keypairRepo *testutil.MockUserKeypairRepository, userID string) {
	t.Helper()
	userRepo.Users[userID] = &model.User{ID: userID, Username: userID}
	keypairRepo.Keypairs[userID] = &model.UserKeypair{
		UserID: userID,
		// Test-only ECDSA-style stub key. DeliverActivity signs with the PEM
		// we pass through; the recording enqueuer ignores it.
		PrivateKey: "-----BEGIN PRIVATE KEY-----\nstub\n-----END PRIVATE KEY-----",
	}
}

func TestPollDeliveryHook_OnVote_LocalPollSkipped(t *testing.T) {
	hook, enq, _, _, _ := newHookSetup(t)
	target := &model.Note{ID: "n1", UserID: "author", UserHost: nil}
	hook.OnVote(&model.User{ID: "voter"}, target, 0, "A")
	assert.Empty(t, enq.delivers, "local poll must not enqueue any deliver")
}

func TestPollDeliveryHook_OnVote_NilTargetSkipped(t *testing.T) {
	hook, enq, _, _, _ := newHookSetup(t)
	hook.OnVote(&model.User{ID: "voter"}, nil, 0, "A")
	hook.OnVote(nil, &model.Note{}, 0, "A")
	assert.Empty(t, enq.delivers)
}

func TestPollDeliveryHook_OnVote_MissingMetadataSkipped(t *testing.T) {
	hook, enq, userRepo, _, _ := newHookSetup(t)
	host := "remote.example"
	uri := "https://remote.example/notes/r1"
	target := &model.Note{ID: "n1", UserID: "author_remote", UserHost: &host, URI: &uri}

	// 1. author lookup fails
	hook.OnVote(&model.User{ID: "voter"}, target, 0, "A")
	assert.Empty(t, enq.delivers)

	// 2. author exists but no inbox
	authorURI := "https://remote.example/users/author_remote"
	userRepo.Users["author_remote"] = &model.User{ID: "author_remote", URI: &authorURI}
	hook.OnVote(&model.User{ID: "voter"}, target, 0, "A")
	assert.Empty(t, enq.delivers)

	// 3. author exists with inbox but no URI
	inbox := "https://remote.example/inbox"
	userRepo.Users["author_remote"] = &model.User{ID: "author_remote", Inbox: &inbox}
	hook.OnVote(&model.User{ID: "voter"}, target, 0, "A")
	assert.Empty(t, enq.delivers)

	// 4. target.URI nil -> skip even with everything else set
	target2 := &model.Note{ID: "n1", UserID: "author_remote", UserHost: &host}
	userRepo.Users["author_remote"] = &model.User{ID: "author_remote", Inbox: &inbox, URI: &authorURI}
	hook.OnVote(&model.User{ID: "voter"}, target2, 0, "A")
	assert.Empty(t, enq.delivers)
}

func TestPollDeliveryHook_OnVote_RemoteEnqueues(t *testing.T) {
	hook, enq, userRepo, keypairRepo, _ := newHookSetup(t)
	seedSigner(t, userRepo, keypairRepo, "voter")

	host := "remote.example"
	uri := "https://remote.example/notes/r1"
	target := &model.Note{ID: "n1", UserID: "author_remote", UserHost: &host, URI: &uri}
	authorURI := "https://remote.example/users/author_remote"
	inbox := "https://remote.example/users/author_remote/inbox"
	userRepo.Users["author_remote"] = &model.User{ID: "author_remote", Inbox: &inbox, URI: &authorURI}

	hook.OnVote(&model.User{ID: "voter"}, target, 0, "Apple")
	require.Len(t, enq.delivers, 1, "remote poll must enqueue exactly 1 deliver to author inbox")
	assert.Equal(t, inbox, enq.delivers[0].Inbox)
	assert.Contains(t, string(enq.delivers[0].Body), `"name":"Apple"`)
	assert.Contains(t, string(enq.delivers[0].Body), `"inReplyTo":"https://remote.example/notes/r1"`)
}

func TestPollDeliveryHook_OnLocalPollUpdated_RemoteSkipped(t *testing.T) {
	hook, enq, _, _, _ := newHookSetup(t)
	host := "remote.example"
	hook.OnLocalPollUpdated(&model.Note{ID: "n1", UserHost: &host})
	assert.Empty(t, enq.delivers, "remote poll must not trigger Update broadcast")
}

func TestPollDeliveryHook_OnLocalPollUpdated_LocalOnlySkipped(t *testing.T) {
	hook, enq, _, _, _ := newHookSetup(t)
	hook.OnLocalPollUpdated(&model.Note{ID: "n1", LocalOnly: true})
	assert.Empty(t, enq.delivers, "local-only poll must not trigger Update broadcast")
}

func TestPollDeliveryHook_OnLocalPollUpdated_NoFollowersNoDeliver(t *testing.T) {
	hook, enq, userRepo, keypairRepo, _ := newHookSetup(t)
	seedSigner(t, userRepo, keypairRepo, "author")
	// followingRepo is empty -> ListRemoteFollowerInboxes returns []
	hook.OnLocalPollUpdated(&model.Note{ID: "n1", UserID: "author", HasPoll: true, Visibility: model.NoteVisibilityPublic})
	assert.Empty(t, enq.delivers, "no followers -> no deliver enqueued")
}

func TestPollDeliveryHook_NilSetterNoOp(t *testing.T) {
	var hook *PollDeliveryHook
	hook.OnVote(&model.User{}, &model.Note{}, 0, "x")
	hook.OnLocalPollUpdated(&model.Note{})
}

// TestPollDeliveryHook_OnVote_DeliverErrorLogged exercises the failure path
// where DeliverActivity returns an error (signer keypair missing). The hook
// only logs and returns — DB state is not rolled back.
func TestPollDeliveryHook_OnVote_DeliverErrorLogged(t *testing.T) {
	hook, _, userRepo, _, _ := newHookSetup(t)
	// voter ユーザーは作るが keypair は seed しないので signerCredentials が err を返す
	userRepo.Users["voter"] = &model.User{ID: "voter", Username: "voter"}

	host := "remote.example"
	uri := "https://remote.example/notes/r1"
	target := &model.Note{ID: "n1", UserID: "author_remote", UserHost: &host, URI: &uri}
	authorURI := "https://remote.example/users/author_remote"
	inbox := "https://remote.example/users/author_remote/inbox"
	userRepo.Users["author_remote"] = &model.User{ID: "author_remote", Inbox: &inbox, URI: &authorURI}

	// no panic, no return value — error is silently logged
	hook.OnVote(&model.User{ID: "voter"}, target, 0, "Apple")
}

// TestPollDeliveryHook_OnLocalPollUpdated_DeliverErrorLogged exercises the
// equivalent failure path on the Update(Question) broadcast side.
func TestPollDeliveryHook_OnLocalPollUpdated_DeliverErrorLogged(t *testing.T) {
	hook, _, userRepo, _, _ := newHookSetup(t)
	// author exists but no keypair — signer fetch fails when followers are present.
	userRepo.Users["author"] = &model.User{ID: "author"}
	hook.OnLocalPollUpdated(&model.Note{
		ID: "n1", UserID: "author", HasPoll: true, Visibility: model.NoteVisibilityPublic,
	})
}

// seedRemoteUser registers a remote user with an inbox so recipient lookups can
// resolve it.
func seedRemoteUser(userRepo *testutil.MockUserRepository, id, host, inbox string) *model.User {
	h := host
	in := inbox
	u := &model.User{ID: id, Username: id, Host: &h, Inbox: &in}
	userRepo.Users[id] = u
	return u
}

// Update(Question) は RenderNote をそのまま包むので content / cw / attachment が
// 丸ごと入る。specified (DM) なアンケートに票が入るたびにフォロワー全員へ
// 配送してはいけない。
func TestPollDeliveryHook_OnLocalPollUpdated_SpecifiedGoesToRecipientsOnly(t *testing.T) {
	hook, enq, userRepo, keypairRepo, followingRepo, _ := newHookSetupWithFollowing(t)
	seedSigner(t, userRepo, keypairRepo, "author")
	// 宛先ではないリモートフォロワー。
	followerInbox := "https://follower.example/inbox"
	followingRepo.RemoteInboxes["author"] = []string{followerInbox}
	// DM の宛先 (フォロワーではない)。
	dmInbox := "https://dmpeer.example/users/bob/inbox"
	seedRemoteUser(userRepo, "dmpeer", "dmpeer.example", dmInbox)

	secret := "secret DM body"
	hook.OnLocalPollUpdated(&model.Note{
		ID: "n1", UserID: "author", HasPoll: true, Text: &secret,
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"dmpeer"},
	})

	require.Len(t, enq.delivers, 1, "宛先 1 件のみへ配送する")
	assert.Equal(t, dmInbox, enq.delivers[0].Inbox)
	for _, d := range enq.delivers {
		assert.NotEqual(t, followerInbox, d.Inbox, "DM 本文をフォロワーへ配送しない")
	}
}

// 宛先がローカルユーザーだけなら配送先は無い (フォロワーへ落とさない)。
func TestPollDeliveryHook_OnLocalPollUpdated_SpecifiedLocalOnlyRecipients(t *testing.T) {
	hook, enq, userRepo, keypairRepo, followingRepo, _ := newHookSetupWithFollowing(t)
	seedSigner(t, userRepo, keypairRepo, "author")
	followingRepo.RemoteInboxes["author"] = []string{"https://follower.example/inbox"}
	// inbox 列を持たせても送らない (local user は Host == nil で判定する)。
	// 本番のローカル user は inbox が NULL だが、判定を host ではなく inbox の
	// 有無に寄せた実装だと連合先でない自分自身へ配送してしまう。
	localInbox := "https://local.example/users/localpeer/inbox"
	userRepo.Users["localpeer"] = &model.User{ID: "localpeer", Username: "localpeer", Inbox: &localInbox}

	hook.OnLocalPollUpdated(&model.Note{
		ID: "n1", UserID: "author", HasPoll: true,
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"localpeer"},
	})
	assert.Empty(t, enq.delivers, "ローカル宛先だけなら配送しない")
}

// public / home / followers は従来どおり follower fanout。
func TestPollDeliveryHook_OnLocalPollUpdated_NonSpecifiedFansOutToFollowers(t *testing.T) {
	for _, vis := range []model.NoteVisibility{
		model.NoteVisibilityPublic,
		model.NoteVisibilityHome,
		model.NoteVisibilityFollowers,
	} {
		t.Run(string(vis), func(t *testing.T) {
			hook, enq, userRepo, keypairRepo, followingRepo, _ := newHookSetupWithFollowing(t)
			seedSigner(t, userRepo, keypairRepo, "author")
			followerInbox := "https://follower.example/inbox"
			followingRepo.RemoteInboxes["author"] = []string{followerInbox}

			hook.OnLocalPollUpdated(&model.Note{
				ID: "n1", UserID: "author", HasPoll: true, Visibility: vis,
			})
			require.Len(t, enq.delivers, 1)
			assert.Equal(t, followerInbox, enq.delivers[0].Inbox)
		})
	}
}

// **メンション先にも票数の Update を送る (2 周目レビュー H1)。** mk-go は
// upstream と同じく mention を `visibleUserIds` に入れない (一方向) ので、
// 宛先だけに送ると**メンションされたリモート利用者はノート本文は受け取るのに
// 票数の Update を一度も受け取らない** — 票が永久に初期値のまま見える。
// `Create` も `Delete` も宛先 + メンション先へ送っている。
func TestPollDeliveryHook_OnLocalPollUpdated_SpecifiedReachesMentioned(t *testing.T) {
	hook, enq, userRepo, keypairRepo, followingRepo, _ := newHookSetupWithFollowing(t)
	seedSigner(t, userRepo, keypairRepo, "author")
	followerInbox := "https://follower.example/inbox"
	followingRepo.RemoteInboxes["author"] = []string{followerInbox}
	dmInbox := "https://dmpeer.example/users/bob/inbox"
	seedRemoteUser(userRepo, "dmpeer", "dmpeer.example", dmInbox)
	mentionedInbox := "https://mentioned.example/users/carol/inbox"
	seedRemoteUser(userRepo, "mentioned", "mentioned.example", mentionedInbox)

	secret := "secret DM body @carol@mentioned.example"
	hook.OnLocalPollUpdated(&model.Note{
		ID: "n1", UserID: "author", HasPoll: true, Text: &secret,
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"dmpeer"},
		Mentions:       model.StringArray{"mentioned"},
	})

	got := make([]string, 0, len(enq.delivers))
	for _, d := range enq.delivers {
		got = append(got, d.Inbox)
		assert.NotEqual(t, followerInbox, d.Inbox, "DM 本文をフォロワーへ配送しない")
	}
	assert.ElementsMatch(t, []string{dmInbox, mentionedInbox}, got,
		"メンション先に票数の Update が届いていない")
}

// 引用の承認を引けないときは、票数の Update を送らない (#3234)。承認の抜けた
// Update を受けた Mastodon は、承認済みの引用を未承認に戻す。
func TestPollDeliveryHook_OnLocalPollUpdated_SkipsWhenQuoteApprovalFails(t *testing.T) {
	hook, enq, userRepo, keypairRepo, followingRepo, _ := newHookSetupWithFollowing(t)
	seedSigner(t, userRepo, keypairRepo, "author")
	followingRepo.RemoteInboxes["author"] = []string{"https://follower.example/inbox"}
	hook.renderer.SetQuoteApprovalResolver(func(*model.Note) (string, error) { return "", assert.AnError })
	text, target := "poll quote", "quoted"
	hook.OnLocalPollUpdated(&model.Note{
		ID: "n1", UserID: "author", HasPoll: true, Visibility: model.NoteVisibilityPublic, Text: &text, RenoteID: &target,
	})
	assert.Empty(t, enq.delivers)
}
