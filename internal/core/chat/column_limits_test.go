package chat_test

import (
	"context"
	"strings"
	"testing"

	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AP 由来の値が chat の列に収まる形で書かれることを固定する (#2726)。
//
// mock repository は列制約を持たないので、ここで見るのは「service が渡す値」。
// 列長そのものは internal/repository の情報スキーマテストが固定する。

func TestEnsureRoomViaAP_TruncatesNameAndDescription(t *testing.T) {
	svc, repo := newRoomFedService(t)
	// **全角で埋める。** byte で数える実装だと 3 倍になって落ちる。
	require.NoError(t, svc.EnsureRoomViaAP(
		remoteRoomURI("room1"),
		strings.Repeat("あ", 300),
		strings.Repeat("い", 3000),
		"remoteOwner",
	))
	room, err := repo.FindRoomByURI(remoteRoomURI("room1"))
	require.NoError(t, err)
	assert.Equal(t, 256, len([]rune(room.Name)))
	assert.Equal(t, 2048, len([]rune(room.Description)))
}

func TestEnsureRoomViaAP_StripsNUL(t *testing.T) {
	svc, repo := newRoomFedService(t)
	require.NoError(t, svc.EnsureRoomViaAP(remoteRoomURI("room1"), "a\x00b", "c\x00d", "remoteOwner"))
	room, err := repo.FindRoomByURI(remoteRoomURI("room1"))
	require.NoError(t, err)
	assert.Equal(t, "ab", room.Name)
	assert.Equal(t, "cd", room.Description)
}

// **身元は URI なので、上限を見るのも URI (#2994)。** 行の `id` はこちらで採番する
// ので相手の値は入らない。切ると別の room を指すので、収まらなければ拒否する。
func TestEnsureRoomViaAP_RejectsOversizedRoomURI(t *testing.T) {
	svc, repo := newRoomFedService(t)
	const prefix = "https://remote.example/chat/rooms/"
	long := prefix + strings.Repeat("a", 513-len(prefix))
	require.Len(t, []rune(long), 513)

	err := svc.EnsureRoomViaAP(long, "General", "desc", "remoteOwner")
	require.ErrorIs(t, err, corechat.ErrInvalidTarget)
	_, ferr := repo.FindRoomByURI(long)
	assert.Error(t, ferr, "拒否した room は作られない")

	// 上限ちょうどは通す。
	fits := prefix + strings.Repeat("a", 512-len(prefix))
	require.Len(t, []rune(fits), 512)
	require.NoError(t, svc.EnsureRoomViaAP(fits, "General", "desc", "remoteOwner"))
}

// **origin の room id が 32 文字を超えていても取り込める。** 行の `id` を採番する
// ようになったので、相手の id が `chat_room.id` に収まる必要はなくなった (#2994)。
func TestEnsureRoomViaAP_AcceptsLongOriginRoomID(t *testing.T) {
	svc, repo := newRoomFedService(t)
	uri := "https://remote.example/chat/rooms/" + strings.Repeat("a", 64)
	require.NoError(t, svc.EnsureRoomViaAP(uri, "General", "desc", "remoteOwner"))
	room, err := repo.FindRoomByURI(uri)
	require.NoError(t, err)
	assert.LessOrEqual(t, len([]rune(room.ID)), 32, "採番した id が列に収まっていない")
}

func TestCreateRoomMessageViaAP_TruncatesText(t *testing.T) {
	svc, repo := newRoomFedService(t)
	sender := &model.User{ID: "remote1", Username: "remote1"}
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "R", sender.ID)))

	require.NoError(t, svc.CreateRoomMessageViaAP(
		"https://remote.example/chat/messages/m1", sender, remoteRoomURI("room1"), strings.Repeat("あ", 5000), ""))
	msgs, err := repo.ListMessagesByRoom("room1", "", "", 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.NotNil(t, msgs[0].Text)
	assert.Equal(t, 4096, len([]rune(*msgs[0].Text)))
}

func TestCreateRoomMessageViaAP_NULOnlyTextStaysNull(t *testing.T) {
	svc, repo := newRoomFedService(t)
	sender := &model.User{ID: "remote1", Username: "remote1"}
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "R", sender.ID)))

	require.NoError(t, svc.CreateRoomMessageViaAP(
		"https://remote.example/chat/messages/m1", sender, remoteRoomURI("room1"), "\x00", ""))
	msgs, err := repo.ListMessagesByRoom("room1", "", "", 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	// NUL だけの本文は空になる。空文字を入れず NULL のままにする (生値が空の
	// ときと同じ形)。
	assert.Nil(t, msgs[0].Text)
}

func TestCreateRoomMessageViaAP_RejectsOversizedURI(t *testing.T) {
	svc, repo := newRoomFedService(t)
	sender := &model.User{ID: "remote1", Username: "remote1"}
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "R", sender.ID)))

	longURI := "https://remote.example/chat/messages/" + strings.Repeat("a", 512)
	err := svc.CreateRoomMessageViaAP(longURI, sender, remoteRoomURI("room1"), "hi", "")
	// uri を捨てて行だけ作ると retry のたびに重複するので、message ごと拒否する。
	require.ErrorIs(t, err, corechat.ErrInvalidTarget)
	msgs, ferr := repo.ListMessagesByRoom("room1", "", "", 10)
	require.NoError(t, ferr)
	assert.Empty(t, msgs)
}

func TestCreateMessageViaAP_TruncatesTextAndRejectsOversizedURI(t *testing.T) {
	chatRepo := newFakeRepo()
	idGen, _ := id.NewGenerator("aidx")
	svc := corechat.NewService(chatRepo, idGen)
	sender := &model.User{ID: "remote1", Username: "remote1"}

	msg, err := svc.CreateMessageViaAP(context.Background(),
		"https://remote.example/chat-messages/1", sender, "local1", strings.Repeat("あ", 5000), "")
	require.NoError(t, err)
	require.NotNil(t, msg.Text)
	assert.Equal(t, 4096, len([]rune(*msg.Text)))

	longURI := "https://remote.example/chat-messages/" + strings.Repeat("a", 512)
	_, err = svc.CreateMessageViaAP(context.Background(), longURI, sender, "local1", "hi", "")
	require.ErrorIs(t, err, corechat.ErrInvalidTarget)
}
