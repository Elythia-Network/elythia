package server

import (
	"context"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corenotification "github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/misc/id"
)

// recordingMainPublisher captures PublishMainEvent calls for assertions.
type recordingMainPublisher struct {
	mu    sync.Mutex
	types []string
}

func (p *recordingMainPublisher) PublishMainEvent(_ string, eventType string, _ any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.types = append(p.types, eventType)
}

func (p *recordingMainPublisher) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.types...)
}

// TestNotifReaderAdapter_ReadAll_DoesNotForce pins that the WebSocket
// `readNotification` message stays an implicit mark-as-read (#2831).
//
// upstream Connection.onReadNotification は force を渡さない。frontend は通知が
// 届くたびにこれを送る (MkStreamingNotificationsTimeline / common.vue) ので、
// force を立てると毎回 readAllNotifications が飛び、保留中の
// unreadNotification を潰してバッジが点かなくなる。復帰用の force を持つのは
// 明示操作の POST /api/notifications/mark-all-as-read だけ。
func TestNotifReaderAdapter_ReadAll_DoesNotForce(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	svc := corenotification.NewService(client, idGen, "")
	// 2 秒の AfterFunc を待たずに済ませる。
	svc.SetUnreadPublishDelay(0)

	_, err = svc.Create(context.Background(), corenotification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: corenotification.TypeFollow,
	})
	require.NoError(t, err)

	// Create 由来の unreadNotification を拾わないよう、通知を作ってから wire する。
	pub := &recordingMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	adapter := &notifReaderAdapter{svc: svc}
	require.NoError(t, adapter.ReadAll("alice"))
	require.Equal(t, []string{"readAllNotifications"}, pub.snapshot())

	// 2 回目は read marker が動かないので publish されない。
	require.NoError(t, adapter.ReadAll("alice"))
	assert.Equal(t, []string{"readAllNotifications"}, pub.snapshot(),
		"WebSocket readNotification must not force a re-publish")
}
