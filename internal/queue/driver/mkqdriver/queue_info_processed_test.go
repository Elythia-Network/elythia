package mkqdriver_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/driver/mkqdriver"
	"github.com/elythia-network/elythia/internal/testutil"
)

// runJobs enqueues n jobs with enqueue and waits until every handler ran.
func runJobs(t *testing.T, enqueue func() error, wg *sync.WaitGroup, n int) {
	t.Helper()
	wg.Add(n)
	for range n {
		require.NoError(t, enqueue())
	}
	if !waitGroupTimeout(wg, 10*time.Second) {
		t.Fatal("handlers did not run")
	}
}

// startProcessedServer starts d's server with a handler that succeeds for
// "processed:ok" and fails for "processed:fail".
func startProcessedServer(t *testing.T, d *mkqdriver.Driver, wg *sync.WaitGroup) {
	t.Helper()
	srv := d.Server()
	srv.Handle("processed:ok", func(context.Context, driver.Task) error {
		wg.Done()
		return nil
	})
	srv.Handle("processed:fail", func(context.Context, driver.Task) error {
		wg.Done()
		return errors.New("boom")
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)
}

// TestInspector_GetQueueInfo_ProcessedTotalGrowsPastRetention pins #4220: the
// completed bucket stops growing at the retention (so its per-tick
// difference reads 0), while ProcessedTotal keeps counting every finished
// job, including the ones that failed for good.
func TestInspector_GetQueueInfo_ProcessedTotalGrowsPastRetention(t *testing.T) {
	d := newDriver(t)
	var wg sync.WaitGroup
	startProcessedServer(t, d, &wg)
	ok := func() error {
		return d.Client().Enqueue(context.Background(), "processed:ok", nil,
			driver.WithQueue("deliver"), driver.WithKeepCompleted(30))
	}
	fail := func() error {
		return d.Client().Enqueue(context.Background(), "processed:fail", nil,
			driver.WithQueue("deliver"), driver.WithMaxRetry(0))
	}

	runJobs(t, ok, &wg, 30)
	var first *driver.InspectorInfo
	require.Eventually(t, func() bool {
		info, err := d.Inspector().GetQueueInfo("deliver")
		first = info
		return err == nil && info.Completed == 30 && info.ProcessedTotal == 30
	}, 5*time.Second, 20*time.Millisecond)
	assert.True(t, first.HasProcessedTotal)

	runJobs(t, ok, &wg, 15)
	runJobs(t, fail, &wg, 2)
	require.Eventually(t, func() bool {
		info, err := d.Inspector().GetQueueInfo("deliver")
		return err == nil && info.ProcessedTotal == 47
	}, 5*time.Second, 20*time.Millisecond)
	// 件数 (Counts) と累計は別の往復で読むので、最後のジョブが2つの間に
	// 終わると件数だけが古い。全部が終わった後にもう一度読む。
	second, err := d.Inspector().GetQueueInfo("deliver")
	require.NoError(t, err)
	assert.EqualValues(t, 47, second.ProcessedTotal)
	assert.Equal(t, 30, second.Completed, "the completed bucket is capped by the retention")
	assert.Equal(t, 2, second.Failed)
	assert.True(t, second.HasProcessedTotal)
}

// TestInspector_GetQueueInfo_ProcessedTotalUnknownWithoutMetrics pins that a
// driver that writes no job metrics reports ProcessedTotal as unknown rather
// than as 0 (#4220), so the caller can fall back to Completed.
func TestInspector_GetQueueInfo_ProcessedTotalUnknownWithoutMetrics(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)
	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:                redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:          4,
		MaxMetricsDataPoints: -1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	var wg sync.WaitGroup
	startProcessedServer(t, d, &wg)
	runJobs(t, func() error {
		return d.Client().Enqueue(context.Background(), "processed:ok", nil, driver.WithQueue("deliver"))
	}, &wg, 3)

	require.Eventually(t, func() bool {
		info, err := d.Inspector().GetQueueInfo("deliver")
		return err == nil && info.Completed == 3
	}, 5*time.Second, 20*time.Millisecond)
	info, err := d.Inspector().GetQueueInfo("deliver")
	require.NoError(t, err)
	assert.False(t, info.HasProcessedTotal)
	assert.Zero(t, info.ProcessedTotal)
}

// TestInspector_GetQueueInfo_ReadsPauseFlagOnce pins that one GetQueueInfo
// reads meta.paused once (#4220). Counts used to count the paused bucket with
// its own HEXISTS, and IsPaused read the same field again.
func TestInspector_GetQueueInfo_ReadsPauseFlagOnce(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	require.NoError(t, d.Inspector().PauseQueue("deliver"))
	require.NoError(t, testRedis.Client.ConfigResetStat(ctx).Err())

	info, err := d.Inspector().GetQueueInfo("deliver")
	require.NoError(t, err)
	assert.True(t, info.IsPaused)

	stats, err := testRedis.Client.Info(ctx, "commandstats").Result()
	require.NoError(t, err)
	assert.Equal(t, 1, commandCalls(stats, "hexists"), stats)
}

// commandCalls returns the calls of cmd in an INFO commandstats reply.
func commandCalls(stats, cmd string) int {
	for line := range strings.SplitSeq(stats, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "cmdstat_"+cmd+":calls=")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(strings.SplitN(rest, ",", 2)[0])
		return n
	}
	return 0
}
