package mkqdriver_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/driver/mkqdriver"
	"github.com/elythia-network/elythia/internal/testutil"
)

// newStalledFixture starts a driver whose deliver queue runs workers
// Workers and scans for stalled jobs every 200ms.
func newStalledFixture(t *testing.T, workers int, cfg mkqdriver.Config) (*mkqdriver.Driver, *mkqdriver.Server, *atomic.Int32) {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	flushRedis(t)
	cfg.Redis = redis.UniversalOptions{Addrs: []string{testRedis.Addr}}
	cfg.Concurrency = 4
	cfg.QueueConcurrency = map[string]int{"deliver": workers}
	cfg.StalledInterval = 200 * time.Millisecond
	d, err := mkqdriver.New(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	srv := d.Server()
	var done atomic.Int32
	srv.Handle("fast", func(context.Context, driver.Task) error {
		done.Add(1)
		return nil
	})
	block := make(chan struct{})
	srv.Handle("wedge", func(context.Context, driver.Task) error {
		<-block
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)
	// 止める前に詰まった handler を戻す (Cleanup は逆順に走る)。戻さないと
	// Shutdown が stopWorkerTimeout まで待つ。
	t.Cleanup(func() { close(block) })
	mkqs, ok := srv.(*mkqdriver.Server)
	require.True(t, ok)
	return d, mkqs, &done
}

// orphanJob enqueues a "fast" job while the deliver queue has no Worker and
// moves it to active without a lock, which is what a job held by a worker
// that died looks like. Only the stalled-job scan brings it back.
func orphanJob(t *testing.T, d *mkqdriver.Driver) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, d.Client().Enqueue(ctx, "fast", nil, driver.WithQueue("deliver")))
	id, err := testRedis.Client.LMove(ctx, "bull:deliver:wait", "bull:deliver:active", "RIGHT", "LEFT").Result()
	require.NoError(t, err, "the job did not wait in bull:deliver:wait")
	require.NotEmpty(t, id)
}

// TestStalledCheck_OneWorkerPerQueue checks that only one Worker per queue
// runs the stalled-job scan (#3592: every Worker ran it).
func TestStalledCheck_OneWorkerPerQueue(t *testing.T) {
	d, mkqs, _ := newStalledFixture(t, 4, mkqdriver.Config{})
	require.Equal(t, 4, d.WorkerCount("deliver"))
	roster, _ := mkqs.StalledCheckers("deliver")
	assert.Equal(t, 1, roster)
	// 帳簿だけでなく、実際に stalled-check の Lua を撃っている Worker が 1 本で
	// あること。数えるのは Lua の呼び出しの回数 (key の TTL が残っている間の
	// 呼び出しは何もせずに返るので、実際の走査はその約半分)。200ms ごとに 1 本
	// なら 2 秒で約 10 回、4 本とも撃てば約 40 回になる。
	n := countStalledScans(t, "deliver", 2*time.Second)
	assert.Greater(t, n, 3, "nobody scans for stalled jobs")
	assert.Less(t, n, 20, "more than one worker scans for stalled jobs")

	// 縮小しても役を持つ Worker は残る。
	require.NoError(t, d.Resize("deliver", 1))
	roster, _ = mkqs.StalledCheckers("deliver")
	assert.Equal(t, 1, roster, "the scale-down stopped the worker scanning for stalled jobs")
	require.NoError(t, d.Resize("deliver", 3))
	roster, _ = mkqs.StalledCheckers("deliver")
	assert.Equal(t, 1, roster, "a scale-up added another scanner")
}

// TestStalledCheck_RecoversAfterTheScannerIsStopped checks that once the
// Worker scanning for stalled jobs is stopped (here with every other Worker
// of the queue), the Workers started afterwards take the scan over and an
// orphaned job is recovered.
func TestStalledCheck_RecoversAfterTheScannerIsStopped(t *testing.T) {
	d, mkqs, done := newStalledFixture(t, 2, mkqdriver.Config{})
	require.NoError(t, d.Resize("deliver", 0))
	roster, _ := mkqs.StalledCheckers("deliver")
	require.Equal(t, 0, roster)
	orphanJob(t, d)

	require.NoError(t, d.Resize("deliver", 2))
	eventually(t, 10*time.Second, "the orphaned job was never recovered", func() bool {
		return done.Load() >= 1
	})
	roster, _ = mkqs.StalledCheckers("deliver")
	assert.Equal(t, 1, roster)
}

// TestStalledCheck_MovesToTheReplacementOfAQuarantinedScanner checks that
// when the Worker scanning for stalled jobs is quarantined (its handler
// overran the threshold), its replacement takes the scan over, and an
// orphaned job is recovered.
func TestStalledCheck_MovesToTheReplacementOfAQuarantinedScanner(t *testing.T) {
	d, mkqs, done := newStalledFixture(t, 1, mkqdriver.Config{
		StuckWorkerAfter:  300 * time.Millisecond,
		SuperviseInterval: 100 * time.Millisecond,
	})
	require.NoError(t, d.Client().Enqueue(context.Background(), "wedge", nil, driver.WithQueue("deliver")))
	eventually(t, 10*time.Second, "the scanner was never quarantined", func() bool {
		return mkqs.QuarantinedWorkerCount("deliver") == 1 && d.WorkerCount("deliver") == 1
	})
	roster, quarantine := mkqs.StalledCheckers("deliver")
	assert.Equal(t, 1, quarantine)
	assert.Equal(t, 1, roster, "the replacement of the quarantined scanner does not scan")

	require.NoError(t, d.Client().Enqueue(context.Background(), "fast", nil, driver.WithQueue("deliver")))
	eventually(t, 10*time.Second, "the replacement does not process jobs", func() bool {
		return done.Load() >= 1
	})
}

// countStalledScans counts the stalled-job scans sent for qname during d,
// from the Redis slow log with a zero threshold.
func countStalledScans(t *testing.T, qname string, d time.Duration) int {
	t.Helper()
	ctx := context.Background()
	rc := testRedis.Client
	prev, err := rc.ConfigGet(ctx, "slowlog-log-slower-than").Result()
	require.NoError(t, err)
	prevLen, err := rc.ConfigGet(ctx, "slowlog-max-len").Result()
	require.NoError(t, err)
	require.NoError(t, rc.ConfigSet(ctx, "slowlog-max-len", "100000").Err())
	require.NoError(t, rc.ConfigSet(ctx, "slowlog-log-slower-than", "0").Err())
	require.NoError(t, rc.SlowLogReset(ctx).Err())
	time.Sleep(d)
	require.NoError(t, rc.ConfigSet(ctx, "slowlog-log-slower-than", prev["slowlog-log-slower-than"]).Err())
	entries, err := rc.SlowLogGet(ctx, -1).Result()
	require.NoError(t, err)
	require.NoError(t, rc.ConfigSet(ctx, "slowlog-max-len", prevLen["slowlog-max-len"]).Err())
	require.NoError(t, rc.SlowLogReset(ctx).Err())
	key := "bull:" + qname + ":stalled-check"
	n := 0
	for _, e := range entries {
		if len(e.Args) == 0 || (e.Args[0] != "evalsha" && e.Args[0] != "EVALSHA") {
			continue
		}
		for _, a := range e.Args {
			if a == key {
				n++
				break
			}
		}
	}
	return n
}
