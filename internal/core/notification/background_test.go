package notification

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/bgwork"
)

func countUnread(pub *stubMainPublisher) int {
	n := 0
	for _, c := range pub.snapshot() {
		if c.eventType == "unreadNotification" {
			n++
		}
	}
	return n
}

// TestService_DelayedUnreadRunsInTheBackgroundGroup checks #3661 review M-1:
// the delayed unreadNotification (and the Web Push sent with it) waits in the
// group of the server generation. When the generation starts stopping, it is
// sent at once, inside the wait of the stop, instead of firing after the DB
// and the queue have been closed.
func TestService_DelayedUnreadRunsInTheBackgroundGroup(t *testing.T) {
	svc := newTestSvc(t)
	svc.SetUnreadPublishDelay(time.Minute)
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)
	g := bgwork.New()
	svc.SetBackground(g)
	assert.True(t, svc.HasBackground())

	ctx := context.Background()
	_, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: TypeMention})
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)
	require.Zero(t, countUnread(pub), "前提: 遅らせる時間の前に送った")

	start := time.Now()
	require.Zero(t, g.StopWithin(10*time.Second), "遅らせている通知を世代の停止で送り切れなかった")
	assert.Less(t, time.Since(start), 5*time.Second, "世代の停止が、遅らせる時間を待った")
	assert.Equal(t, 1, countUnread(pub), "世代の停止で、遅らせていた通知を送らなかった")
	assert.Zero(t, g.Dropped())
}

// TestService_UnreadCreatedDuringTheStopIsSentNotDropped checks #3661 review
// r2 L-1: a notification created by a goroutine of the group while the stop
// is waiting (a post hook that got its slot late, for one) is sent in the
// caller's goroutine, which the stop waits for, instead of being dropped.
func TestService_UnreadCreatedDuringTheStopIsSentNotDropped(t *testing.T) {
	svc := newTestSvc(t)
	svc.SetUnreadPublishDelay(time.Minute)
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)
	g := bgwork.New()
	svc.SetBackground(g)

	ctx := context.Background()
	require.True(t, g.Go(func(context.Context) {
		<-g.Stopping()
		_, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: TypeMention})
		assert.NoError(t, err)
	}))
	left := g.StopWithin(10 * time.Second)
	assert.Zero(t, left)
	assert.Equal(t, 1, countUnread(pub), "停止の待ちの間に作った通知を送らなかった")
	assert.Zero(t, g.Dropped(), "停止の待ちの間に作った通知を捨てた")
}
