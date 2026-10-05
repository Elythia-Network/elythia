package federation_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/core/federation"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// chatRoomInviteBody renders an Invite carrying a chat room Group object.
func chatRoomInviteBody(actor, groupID string) []byte {
	return []byte(`{
		"type": "Invite",
		"actor": "` + actor + `",
		"target": "https://example.com/users/bob",
		"object": {
			"type": "Group",
			"id": "` + groupID + `",
			"name": "General",
			"summary": "desc",
			"attributedTo": "` + actor + `"
		}
	}`)
}

// newChatRoomInviteProcessor wires a processor with a local invitee (bob) and a
// recording chat room receiver.
func newChatRoomInviteProcessor(t *testing.T) (*federation.Processor, *fakeChatRoomReceiver) {
	t.Helper()
	p, repo, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	return p, recv
}

// **自分の host でない room URI を名乗る Invite は drop する。** 通すと、署名が
// 通る任意の remote actor が第三者インスタンスの room として行を作れる (room の
// 素性が偽装できる)。id の先取りそのものは host 列が無い以上ここでは塞げない。
func TestProcess_ChatRoomInvite_ForeignHostGroupIDDropped(t *testing.T) {
	p, recv := newChatRoomInviteProcessor(t)

	err := p.Process(chatRoomInviteBody(
		"https://remote.example/users/alice",
		"https://evil.invalid/chat/rooms/room1",
	))

	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
	assert.Empty(t, recv.ensureCalls, "別ホストの room は作らない")
	assert.Empty(t, recv.inviteCalls, "別ホストの room への招待は作らない")
}

// 自インスタンスの room URI を名乗る remote actor の Invite も drop する
// (ローカル room の乗っ取り / 先取り)。
func TestProcess_ChatRoomInvite_LocalHostGroupIDDropped(t *testing.T) {
	p, recv := newChatRoomInviteProcessor(t)

	err := p.Process(chatRoomInviteBody(
		"https://remote.example/users/alice",
		"https://example.com/chat/rooms/room1",
	))

	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
	assert.Empty(t, recv.ensureCalls)
	assert.Empty(t, recv.inviteCalls)
}

// サブドメイン違い (親ドメインを名乗る形) も別ホスト扱いで drop する。
func TestProcess_ChatRoomInvite_SubdomainMismatchDropped(t *testing.T) {
	p, recv := newChatRoomInviteProcessor(t)

	err := p.Process(chatRoomInviteBody(
		"https://remote.example/users/alice",
		"https://chat.remote.example/chat/rooms/room1",
	))

	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
	assert.Empty(t, recv.ensureCalls)
}

// 既定 port の明示は同一ホスト (punyHost 相当の正規化)。厳しすぎる比較に
// なっていないことを固定する。
func TestProcess_ChatRoomInvite_DefaultPortIsSameHost(t *testing.T) {
	p, recv := newChatRoomInviteProcessor(t)

	require.NoError(t, p.Process(chatRoomInviteBody(
		"https://remote.example/users/alice",
		"https://remote.example:443/chat/rooms/room1",
	)))

	require.Len(t, recv.ensureCalls, 1)
	assert.Equal(t, "https://remote.example:443/chat/rooms/room1", recv.ensureCalls[0][0])
	require.Len(t, recv.inviteCalls, 1)
}

// local actor 名義の Invite (loopback / なりすまし) は drop する。host 一致
// だけでは通ってしまう経路で、通すと「作った覚えのない room」がローカル
// 利用者を owner にして生える。
func TestProcess_ChatRoomInvite_LocalActorDropped(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	// local owner (Host == nil => IsLocal)。
	localURI := "https://example.com/users/carol"
	repo.Users["carol"] = &model.User{ID: "carol", Username: "carol", URI: &localURI}

	err := p.Process(chatRoomInviteBody(
		"https://example.com/users/carol",
		"https://example.com/chat/rooms/room1",
	))

	// **どの層で落ちてもよい。** resolver が自ホストの actor URI を拒否する
	// ようになったので、今はそちらが先に効く。chat_room_inbox 側のガードは
	// 多層防御として残してある (resolver を通らない経路が将来できたときの
	// 防波堤)。固定するのは「room も招待も作らない」ことと、retry させない
	// 種類のエラーで落ちること。
	require.Error(t, err)
	assert.True(t,
		errors.Is(err, federation.ErrUnsupportedActivity) || errors.Is(err, federation.ErrLocalActor),
		"local actor の Invite が retry される種類のエラーで落ちている: %v", err)
	assert.Empty(t, recv.ensureCalls, "local actor の Invite で room を作らない")
	assert.Empty(t, recv.inviteCalls)
}

// block されている相手からの招待は永久に解決しないので retry させない
// (ErrUnsupportedActivity にして inbox job を dead にしない)。
func TestProcess_ChatRoomInvite_BlockedIsNotRetried(t *testing.T) {
	p, recv := newChatRoomInviteProcessor(t)
	recv.inviteErr = corechat.ErrChatBlocked

	err := p.Process(chatRoomInviteBody(
		"https://remote.example/users/alice",
		"https://remote.example/chat/rooms/room1",
	))

	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
	require.Len(t, recv.inviteCalls, 1, "招待判定自体は core に委ねる")
}

// 満室 (ErrRoomFull) も同様に non-retry。
func TestProcess_ChatRoomInvite_RoomFullIsNotRetried(t *testing.T) {
	p, recv := newChatRoomInviteProcessor(t)
	recv.inviteErr = corechat.ErrRoomFull

	err := p.Process(chatRoomInviteBody(
		"https://remote.example/users/alice",
		"https://remote.example/chat/rooms/room1",
	))

	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
}

// room 不在 (ErrNotFound) も non-retry。
func TestProcess_ChatRoomInvite_RoomMissingIsNotRetried(t *testing.T) {
	p, recv := newChatRoomInviteProcessor(t)
	recv.inviteErr = corechat.ErrNotFound

	err := p.Process(chatRoomInviteBody(
		"https://remote.example/users/alice",
		"https://remote.example/chat/rooms/room1",
	))

	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
}

// **恒久的な失敗は ack、一時的な失敗は retry (2 周目レビュー M3)。** dispatch は
// `handleChatRoomInvite` の戻り値を `isPermanentSkipError` に通さないので、生で
// 返すと queue が無駄に回り続ける。逆に何でも ack すると、相手が一時的に落ちて
// いるだけの Invite を捨てることになる。
func TestProcess_ChatRoomInvite_ActorResolutionErrorClassification(t *testing.T) {
	inviteBody := chatRoomInviteBody(
		"https://remote.example/users/alice",
		"https://remote.example/chat/rooms/room1",
	)

	t.Run("恒久的な失敗は ack する", func(t *testing.T) {
		// actor document が不正 (= ErrInvalidActor 系)。retry しても変わらない。
		p, repo, _, _ := newProcessor(t, `{"id":"https://remote.example/users/alice"}`)
		recv := &fakeChatRoomReceiver{}
		p.SetChatRoomReceiver(recv)
		bobURI := "https://example.com/users/bob"
		repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

		err := p.Process(inviteBody)
		require.Error(t, err)
		assert.ErrorIs(t, err, federation.ErrUnsupportedActivity,
			"恒久的な失敗を retry する種類のエラーで返している")
		assert.Empty(t, recv.ensureCalls)
	})

	t.Run("一時的な失敗は retry させる", func(t *testing.T) {
		// fetcher が落ちている (= ネットワーク障害)。retry すれば通りうる。
		p, repo, _, _ := newProcessorFetchErr(t, errors.New("dial tcp: i/o timeout"))
		recv := &fakeChatRoomReceiver{}
		p.SetChatRoomReceiver(recv)
		bobURI := "https://example.com/users/bob"
		repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

		err := p.Process(inviteBody)
		require.Error(t, err)
		assert.NotErrorIs(t, err, federation.ErrUnsupportedActivity,
			"一時的な失敗を ack して捨てている (Invite が失われる)")
		assert.Empty(t, recv.ensureCalls)
	})
}

// newProcessorFetchErr builds a processor whose actor fetch fails with err.
func newProcessorFetchErr(t *testing.T, err error) (*federation.Processor, *testutil.MockUserRepository, *testutil.MockFollowingRepository, *testutil.MockNoteRepository) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{err: err}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	return federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo), repo, followingRepo, noteRepo
}
