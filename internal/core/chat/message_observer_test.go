package chat_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

type recordingMessageObserver struct {
	seen       []*model.ChatMessage
	recipients []*model.User
}

func (o *recordingMessageObserver) DirectMessageCreated(_ context.Context, msg *model.ChatMessage, recipient *model.User) {
	o.seen = append(o.seen, msg)
	o.recipients = append(o.recipients, recipient)
}

// 保存した 1:1 のメッセージが observer へ渡る。ローカルの送信と連合の受信の
// どちらも (#3469)。
func TestMessageObserver_SeesStoredDirectMessages(t *testing.T) {
	svc, _, _ := newSvc(t)
	obs := &recordingMessageObserver{}
	svc.SetMessageObserver(obs)

	local, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	require.NoError(t, err)
	remote, err := svc.CreateMessageViaAP(context.Background(), "https://remote.example/chat/1",
		&model.User{ID: "carol"}, "bob", "hello", "")
	require.NoError(t, err)

	require.Len(t, obs.seen, 2)
	assert.Same(t, local, obs.seen[0])
	assert.Same(t, remote, obs.seen[1])
	// 送信の検査で引いた宛先をそのまま渡す (observer が引き直さなくて済む)。
	for i, r := range obs.recipients {
		require.NotNil(t, r, "%d 件目の宛先が渡っていない", i)
		assert.Equal(t, "bob", r.ID)
	}

	// AP の再送で既存のものを返したときは、もう一度は渡さない。
	again, err := svc.CreateMessageViaAP(context.Background(), "https://remote.example/chat/1",
		&model.User{ID: "carol"}, "bob", "hello", "")
	require.NoError(t, err)
	assert.Equal(t, remote.ID, again.ID)
	assert.Len(t, obs.seen, 2, "再送は observer へ渡さない")
}

// 拒否したメッセージは observer へ渡さない (#3469)。
func TestMessageObserver_NotCalledForRejectedMessages(t *testing.T) {
	svc, _, _ := newSvc(t)
	blocks := testutil.NewMockBlockingRepository()
	require.NoError(t, blocks.Create(&model.Blocking{ID: "b1", BlockerID: "bob", BlockeeID: "alice"}))
	svc.SetBlockingRepo(blocks)
	obs := &recordingMessageObserver{}
	svc.SetMessageObserver(obs)

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	require.Error(t, err)
	_, err = svc.CreateMessageViaAP(context.Background(), "https://remote.example/chat/2",
		&model.User{ID: "alice"}, "bob", "hi", "")
	require.Error(t, err)
	assert.Empty(t, obs.seen)
}
