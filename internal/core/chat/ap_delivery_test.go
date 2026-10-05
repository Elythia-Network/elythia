package chat_test

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeAPDeliverer struct {
	called    int
	lastBody  []byte
	returnErr error
}

func (f *fakeAPDeliverer) DeliverToUser(_ string, _ *model.User, body []byte) error {
	f.called++
	f.lastBody = body
	return f.returnErr
}

func newAPService(t *testing.T) (*corechat.Service, *testutil.MockUserRepository, *fakeAPDeliverer) {
	t.Helper()
	chatRepo := newFakeRepo()
	idGen, _ := id.NewGenerator("aidx")
	svc := corechat.NewService(chatRepo, idGen)
	userRepo := testutil.NewMockUserRepository()
	urls := activitypub.NewURLBuilder("https://local.example")
	renderer := activitypub.NewRenderer(urls)
	deliverer := &fakeAPDeliverer{}
	svc.SetAPDelivery(userRepo, renderer, urls, deliverer)
	return svc, userRepo, deliverer
}

func TestCreateMessageToUser_DeliversToRemoteUser(t *testing.T) {
	svc, userRepo, deliverer := newAPService(t)
	remoteHost := "remote.example"
	remoteURI := "https://remote.example/users/bob"
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &remoteHost, URI: &remoteURI}

	msg, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hello remote", "")
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, 1, deliverer.called)
	body := string(deliverer.lastBody)
	// CherryPick 互換 wire format: Create + Note(_misskey_talk:true) (#692)。
	// 旧 `Misskey:ChatMessage` 独自 type は廃止。
	assert.Contains(t, body, `"type":"Create"`)
	assert.Contains(t, body, `"type":"Note"`)
	assert.Contains(t, body, `"_misskey_talk":true`)
	assert.Contains(t, body, "hello remote")
	assert.NotContains(t, body, "Misskey:ChatMessage", "旧独自 activity type は廃止されたこと")
}

func TestCreateMessageToUser_SkipsLocalRecipient(t *testing.T) {
	svc, userRepo, deliverer := newAPService(t)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "local msg", "")
	require.NoError(t, err)
	assert.Equal(t, 0, deliverer.called)
}

func TestCreateMessageToUser_DeliveryFailureIsSwallowed(t *testing.T) {
	svc, userRepo, deliverer := newAPService(t)
	deliverer.returnErr = assert.AnError
	remoteHost := "remote.example"
	remoteURI := "https://remote.example/users/bob"
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &remoteHost, URI: &remoteURI}

	msg, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	require.NoError(t, err, "delivery failure must not propagate")
	require.NotNil(t, msg)
	assert.Equal(t, 1, deliverer.called)
}

func TestCreateMessageViaAP(t *testing.T) {
	chatRepo := newFakeRepo()
	idGen, _ := id.NewGenerator("aidx")
	svc := corechat.NewService(chatRepo, idGen)
	sender := &model.User{ID: "remote1", Username: "remote1"}

	msg, err := svc.CreateMessageViaAP(context.Background(), "https://remote.example/chat-messages/1", sender, "local1", "hello from remote", "")
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, "remote1", msg.FromUserID)
	require.NotNil(t, msg.ToUserID)
	assert.Equal(t, "local1", *msg.ToUserID)
	require.NotNil(t, msg.URI)
	assert.Equal(t, "https://remote.example/chat-messages/1", *msg.URI)
}

func TestCreateMessageViaAP_InvalidTarget(t *testing.T) {
	chatRepo := newFakeRepo()
	idGen, _ := id.NewGenerator("aidx")
	svc := corechat.NewService(chatRepo, idGen)

	_, err := svc.CreateMessageViaAP(context.Background(), "", nil, "local1", "hi", "")
	assert.Error(t, err)

	_, err = svc.CreateMessageViaAP(context.Background(), "", &model.User{ID: "x"}, "", "hi", "")
	assert.Error(t, err)
}

// --- chatScope (#692) ---
//
// recipient.chatScope 別に CreateMessageToUser / CreateMessageViaAP が
// CherryPick の `recipient is cannot chat (...)` 相当の判定を行うことを確認。

func newScopeService(t *testing.T) (*corechat.Service, *testutil.MockUserRepository, *testutil.MockFollowingRepository) {
	t.Helper()
	chatRepo := newFakeRepo()
	idGen, _ := id.NewGenerator("aidx")
	svc := corechat.NewService(chatRepo, idGen)
	userRepo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://local.example")
	renderer := activitypub.NewRenderer(urls)
	svc.SetAPDelivery(userRepo, renderer, urls, &fakeAPDeliverer{})
	svc.SetFollowingRepo(followingRepo)
	return svc, userRepo, followingRepo
}

func TestCreateMessageToUser_ScopeNone_Rejected(t *testing.T) {
	svc, userRepo, _ := newScopeService(t)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", ChatScope: "none"}

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "blocked", "")
	require.ErrorIs(t, err, corechat.ErrChatScopeViolation)
}

func TestCreateMessageToUser_ScopeEveryone_Allowed(t *testing.T) {
	svc, userRepo, _ := newScopeService(t)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", ChatScope: "everyone"}

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "ok", "")
	require.NoError(t, err)
}

func TestCreateMessageToUser_ScopeFollowers_OnlyFollowers(t *testing.T) {
	svc, userRepo, followingRepo := newScopeService(t)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", ChatScope: "followers"}
	// alice is NOT a follower of bob → rejected
	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "x", "")
	require.ErrorIs(t, err, corechat.ErrChatScopeViolation)
	// alice follows bob (i.e., alice is a follower of bob) → allowed
	require.NoError(t, followingRepo.Create(&model.Following{ID: "f1", FollowerID: "alice", FolloweeID: "bob"}))
	_, err = svc.CreateMessageToUser(context.Background(), "alice", "bob", "ok", "")
	require.NoError(t, err)
}

func TestCreateMessageToUser_ScopeFollowing_OnlyFollowing(t *testing.T) {
	svc, userRepo, followingRepo := newScopeService(t)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", ChatScope: "following"}
	// bob does NOT follow alice → rejected
	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "x", "")
	require.ErrorIs(t, err, corechat.ErrChatScopeViolation)
	// bob follows alice → allowed
	require.NoError(t, followingRepo.Create(&model.Following{ID: "f1", FollowerID: "bob", FolloweeID: "alice"}))
	_, err = svc.CreateMessageToUser(context.Background(), "alice", "bob", "ok", "")
	require.NoError(t, err)
}

func TestCreateMessageToUser_ScopeMutual(t *testing.T) {
	svc, userRepo, followingRepo := newScopeService(t)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", ChatScope: "mutual"}

	// 0 directions → rejected
	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "x", "")
	require.ErrorIs(t, err, corechat.ErrChatScopeViolation)
	// 1 direction → still rejected
	require.NoError(t, followingRepo.Create(&model.Following{ID: "f1", FollowerID: "alice", FolloweeID: "bob"}))
	_, err = svc.CreateMessageToUser(context.Background(), "alice", "bob", "x", "")
	require.ErrorIs(t, err, corechat.ErrChatScopeViolation)
	// 2 directions → allowed
	require.NoError(t, followingRepo.Create(&model.Following{ID: "f2", FollowerID: "bob", FolloweeID: "alice"}))
	_, err = svc.CreateMessageToUser(context.Background(), "alice", "bob", "ok", "")
	require.NoError(t, err)
}

func TestCreateMessageToUser_RemoteRecipient_ScopeNone_Rejected(t *testing.T) {
	// remote recipient の `_misskey_canChat: false` は resolver で
	// chatScope = "none" に翻訳されるため、mk-go 側で事前 reject される (#692)。
	svc, userRepo, _ := newScopeService(t)
	remoteHost := "remote.example"
	remoteURI := "https://remote.example/users/bob"
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", ChatScope: "none", Host: &remoteHost, URI: &remoteURI}

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "x", "")
	require.ErrorIs(t, err, corechat.ErrChatScopeViolation)
}

func TestCreateMessageToUser_RemoteRecipient_ScopeEveryone_Allowed(t *testing.T) {
	// remote recipient で chatScope == "everyone" (`_misskey_canChat: true`
	// 由来) なら mk-go 側は通過させ、granular な判定は remote 側に委ねる。
	svc, userRepo, _ := newScopeService(t)
	remoteHost := "remote.example"
	remoteURI := "https://remote.example/users/bob"
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", ChatScope: "everyone", Host: &remoteHost, URI: &remoteURI}

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "ok", "")
	require.NoError(t, err)
}

func TestCreateMessageToUser_RemoteRecipient_StaleMutualScope_NotRejected(t *testing.T) {
	// 過去に作成された remote user は chatScope の DB default (= "mutual") の
	// ままだが、followingRepo に follow 関係が無くても reject してはいけない。
	// 「mk-go では remote の granular scope を判定しない」契約 (#692)。
	svc, userRepo, _ := newScopeService(t)
	remoteHost := "remote.example"
	remoteURI := "https://remote.example/users/bob"
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", ChatScope: "mutual", Host: &remoteHost, URI: &remoteURI}

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "ok", "")
	require.NoError(t, err, "remote の granular scope は無視され、'none' 以外は通過する")
}

func TestCreateMessageViaAP_ScopeEnforced(t *testing.T) {
	svc, userRepo, _ := newScopeService(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", ChatScope: "none"}
	sender := &model.User{ID: "remote-alice", Username: "alice"}

	_, err := svc.CreateMessageViaAP(context.Background(), "https://remote.example/cm/1", sender, "bob", "x", "")
	require.ErrorIs(t, err, corechat.ErrChatScopeViolation)
}

// followingRepo 未配線で granular scope が設定されている場合は fail-closed
// (`ErrChatScopeUnconfigured`) になる。production wiring 忘れが silent に
// open relay 化するのを防ぐ (#708 review #2)。
func TestCreateMessageToUser_GranularScope_NoFollowingRepo_FailClosed(t *testing.T) {
	chatRepo := newFakeRepo()
	idGen, _ := id.NewGenerator("aidx")
	svc := corechat.NewService(chatRepo, idGen)
	userRepo := testutil.NewMockUserRepository()
	urls := activitypub.NewURLBuilder("https://local.example")
	renderer := activitypub.NewRenderer(urls)
	svc.SetAPDelivery(userRepo, renderer, urls, &fakeAPDeliverer{})
	// SetFollowingRepo は意図的に呼ばない

	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	for _, scope := range []string{"followers", "following", "mutual"} {
		t.Run(scope, func(t *testing.T) {
			userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", ChatScope: scope}
			_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "x", "")
			require.ErrorIs(t, err, corechat.ErrChatScopeUnconfigured)
		})
	}
}

// followingRepo 未配線でも "everyone" / "none" 判定は granular check 不要
// なので通過する (none は ErrChatScopeViolation, everyone は OK)。
func TestCreateMessageToUser_NonGranularScope_NoFollowingRepoOK(t *testing.T) {
	chatRepo := newFakeRepo()
	idGen, _ := id.NewGenerator("aidx")
	svc := corechat.NewService(chatRepo, idGen)
	userRepo := testutil.NewMockUserRepository()
	urls := activitypub.NewURLBuilder("https://local.example")
	renderer := activitypub.NewRenderer(urls)
	svc.SetAPDelivery(userRepo, renderer, urls, &fakeAPDeliverer{})

	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", ChatScope: "everyone"}
	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "ok", "")
	require.NoError(t, err)
}

// newInvitationService builds a chat service with AP delivery wired and keeps
// the chat repo reference so room rows can be seeded for invitation tests.
func newInvitationService(t *testing.T) (*corechat.Service, *testutil.MockChatRepository, *testutil.MockUserRepository, *fakeAPDeliverer) {
	t.Helper()
	chatRepo := newFakeRepo()
	idGen, _ := id.NewGenerator("aidx")
	svc := corechat.NewService(chatRepo, idGen)
	userRepo := testutil.NewMockUserRepository()
	urls := activitypub.NewURLBuilder("https://local.example")
	renderer := activitypub.NewRenderer(urls)
	deliverer := &fakeAPDeliverer{}
	svc.SetAPDelivery(userRepo, renderer, urls, deliverer)
	return svc, chatRepo, userRepo, deliverer
}

func TestFederateInvitation_DeliversInviteToRemoteInvitee(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	remoteURI := "https://remote.example/users/bob"
	userRepo.Users["owner1"] = &model.User{ID: "owner1", Username: "owner1"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &remoteHost, URI: &remoteURI}
	require.NoError(t, chatRepo.CreateRoom(remoteRoomRow("room1", "General", "owner1")))

	svc.FederateInvitation("room1", "bob")
	assert.Equal(t, 1, deliverer.called)
	body := string(deliverer.lastBody)
	assert.Contains(t, body, `"type":"Invite"`)
	assert.Contains(t, body, `"type":"Group"`)
	assert.Contains(t, body, "https://local.example/chat/rooms/room1")
	assert.Contains(t, body, `"target":"https://remote.example/users/bob"`)
}

func TestFederateInvitation_SkipsLocalInvitee(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	userRepo.Users["owner1"] = &model.User{ID: "owner1", Username: "owner1"}
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
	require.NoError(t, chatRepo.CreateRoom(remoteRoomRow("room1", "General", "owner1")))

	svc.FederateInvitation("room1", "carol")
	assert.Equal(t, 0, deliverer.called)
}

func TestFederateInvitation_SkipsRemoteOwnedRoom(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	ownerURI := "https://remote.example/users/owner"
	inviteeURI := "https://remote.example/users/bob"
	// owner が remote の room は本インスタンスから署名配送できないため no-op。
	userRepo.Users["remoteOwner"] = &model.User{ID: "remoteOwner", Username: "owner", Host: &remoteHost, URI: &ownerURI}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &remoteHost, URI: &inviteeURI}
	require.NoError(t, chatRepo.CreateRoom(remoteRoomRow("room1", "General", "remoteOwner")))

	svc.FederateInvitation("room1", "bob")
	assert.Equal(t, 0, deliverer.called)
}

func TestFederateInvitation_NoOpWhenRoomMissing(t *testing.T) {
	svc, _, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	remoteURI := "https://remote.example/users/bob"
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &remoteHost, URI: &remoteURI}

	// room が存在しなければ deliver しない (warn log のみ)。
	svc.FederateInvitation("ghost", "bob")
	assert.Equal(t, 0, deliverer.called)
}

func TestFederateInvitation_DeliveryFailureIsSwallowed(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	deliverer.returnErr = assert.AnError
	remoteHost := "remote.example"
	remoteURI := "https://remote.example/users/bob"
	userRepo.Users["owner1"] = &model.User{ID: "owner1", Username: "owner1"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &remoteHost, URI: &remoteURI}
	require.NoError(t, chatRepo.CreateRoom(remoteRoomRow("room1", "General", "owner1")))

	// 配送失敗は panic/propagate せず swallow される。
	svc.FederateInvitation("room1", "bob")
	assert.Equal(t, 1, deliverer.called)
}

// --- #1748: chatApproval bypass ---

// fakeApprovalRepo is an in-memory repository.ChatApprovalRepository for the
// chatApproval bypass tests.
type fakeApprovalRepo struct {
	approvals   map[string]bool // key: userID+"/"+otherID
	createCount int
	createErr   error
}

func newFakeApprovalRepo() *fakeApprovalRepo {
	return &fakeApprovalRepo{approvals: map[string]bool{}}
}

func (f *fakeApprovalRepo) Create(a *model.ChatApproval) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.createCount++
	f.approvals[a.UserID+"/"+a.OtherID] = true
	return nil
}
func (f *fakeApprovalRepo) Delete(userID, otherID string) error {
	delete(f.approvals, userID+"/"+otherID)
	return nil
}
func (f *fakeApprovalRepo) Exists(userID, otherID string) (bool, error) {
	return f.approvals[userID+"/"+otherID], nil
}
func (f *fakeApprovalRepo) ListByUser(string) ([]*model.ChatApproval, error) { return nil, nil }

// 相手 (bob) が過去に自分 (alice) へ送っていれば (otherApprovedMe)、bob の
// chatScope=followers でも alice→bob を許可する。
func TestCreateMessageToUser_ApprovalBypassesScope(t *testing.T) {
	svc, userRepo, _ := newScopeService(t)
	approvalRepo := newFakeApprovalRepo()
	svc.SetApprovalRepo(approvalRepo)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", ChatScope: "followers"}

	// approval 無し: alice は follower でないので reject。
	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "x", "")
	require.ErrorIs(t, err, corechat.ErrChatScopeViolation)

	// bob が過去に alice へ送った = approval{bob, alice}。otherApprovedMe で bypass。
	approvalRepo.approvals["bob/alice"] = true
	_, err = svc.CreateMessageToUser(context.Background(), "alice", "bob", "ok", "")
	require.NoError(t, err)
}

// 送信成功後、自分→相手の approval が記録される。
func TestCreateMessageToUser_RecordsApprovalAfterSend(t *testing.T) {
	svc, userRepo, _ := newScopeService(t)
	approvalRepo := newFakeApprovalRepo()
	svc.SetApprovalRepo(approvalRepo)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", ChatScope: "everyone"}

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	require.NoError(t, err)
	ok, _ := approvalRepo.Exists("alice", "bob")
	assert.True(t, ok, "approval{alice, bob} must be recorded after send")
}

// 既に approval{alice, bob} があれば重複挿入しない (idempotent)。
func TestCreateMessageToUser_ApprovalIdempotent(t *testing.T) {
	svc, userRepo, _ := newScopeService(t)
	approvalRepo := newFakeApprovalRepo()
	svc.SetApprovalRepo(approvalRepo)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", ChatScope: "everyone"}
	approvalRepo.approvals["alice/bob"] = true

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	require.NoError(t, err)
	assert.Equal(t, 0, approvalRepo.createCount, "既存 approval があれば Create を呼ばない")
}

// approvalRepo 未配線なら従来どおり scope のみで判定する (regression)。
func TestCreateMessageToUser_NoApprovalRepoScopeEnforced(t *testing.T) {
	svc, userRepo, _ := newScopeService(t)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", ChatScope: "followers"}
	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "x", "")
	require.ErrorIs(t, err, corechat.ErrChatScopeViolation)
}
