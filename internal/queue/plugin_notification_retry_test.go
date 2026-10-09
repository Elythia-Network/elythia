package queue_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/driver/mkqdriver"
	"github.com/elythia-network/elythia/internal/testutil"
)

// newPluginDriver builds a real mkq driver that also consumes plugin's queue.
func newPluginDriver(t *testing.T, plugin string) driver.Driver {
	t.Helper()
	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:       redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency: 2,
		QueueNames:  append(append([]string{}, mkqdriver.QueueNames...), queue.PluginQueueNames([]string{plugin})...),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// 本物の mkq で、失敗した通知の受け渡しが backoff の後に再実行されること
// (#3469)。オプションを見るだけのテストでは、driver が実際に再試行するかは
// 分からない (mkq の既定は再試行 0 回)。
func TestPluginNotification_RetriedByRealDriver(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	t.Parallel()
	const plugin = "notifretry"
	d := newPluginDriver(t, plugin)
	srv := queue.NewServer(d)

	var mu sync.Mutex
	var calls []time.Time
	done := make(chan struct{})
	srv.Handle(queue.PluginNotificationTaskType(plugin), func(_ context.Context, task driver.Task) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, time.Now())
		if len(calls) == 1 {
			return errors.New("handler down")
		}
		assert.JSONEq(t, `{"id":"n1"}`, string(task.Payload()))
		close(done)
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	c := queue.NewClient(d)
	require.NoError(t, c.EnqueuePluginNotification(context.Background(), plugin, []byte(`{"id":"n1"}`)))

	select {
	case <-done:
	case <-time.After(40 * time.Second):
		t.Fatal("失敗した通知が再実行されない")
	}
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, calls, 2)
	gap := calls[1].Sub(calls[0])
	assert.GreaterOrEqual(t, gap, queue.PluginNotificationBackoffBase-500*time.Millisecond,
		"backoff を待たずに再実行した (%s)", gap)
}

// ErrSkipRetry (server が plugin.ErrNoRetry から写すもの) を返したら、残りの
// 回数があっても再試行せず failed に移る (#3469)。
func TestPluginNotification_SkipRetryIsNotRetriedByRealDriver(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	t.Parallel()
	const plugin = "notifnoretry"
	d := newPluginDriver(t, plugin)
	srv := queue.NewServer(d)

	var mu sync.Mutex
	calls := 0
	srv.Handle(queue.PluginNotificationTaskType(plugin), func(context.Context, driver.Task) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return fmt.Errorf("%w: account gone", driver.ErrSkipRetry)
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	c := queue.NewClient(d)
	require.NoError(t, c.EnqueuePluginNotification(context.Background(), plugin, []byte(`{"id":"n2"}`)))

	insp := d.Inspector()
	qname := queue.PluginQueueName(plugin)
	require.Eventually(t, func() bool {
		rows, err := insp.ListFailedTasks(qname, 1, 10)
		return err == nil && len(rows) == 1
	}, 20*time.Second, 100*time.Millisecond, "failed に移らない (再試行待ちに入った?)")
	scheduled, err := insp.ListScheduledTasks(qname, 1, 10)
	require.NoError(t, err)
	assert.Empty(t, scheduled, "再試行の待ちに入れない")
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, calls)
}
