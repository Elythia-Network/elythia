package mkqdriver_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/shiroha-a/mk/internal/queue/driver/mkqdriver"
	"github.com/shiroha-a/mk/internal/testutil"
)

var testRedis *testutil.TestRedis

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	testRedis, err = testutil.SetupRedis(ctx)
	if err != nil {
		log.Fatalf("setup redis: %v", err)
	}
	code := m.Run()
	testRedis.Teardown(ctx)
	os.Exit(code)
}

func newDriver(t *testing.T) *mkqdriver.Driver {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	flushRedis(t)
	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:       redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency: 4,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func flushRedis(t *testing.T) {
	t.Helper()
	if err := testRedis.Client.FlushAll(context.Background()).Err(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func waitGroupTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// TestEndToEnd_EnqueueProcess submits a single job, runs the worker
// against the live Redis, and verifies the dispatch handler observed
// the original task type and payload.
func TestEndToEnd_EnqueueProcess(t *testing.T) {
	d := newDriver(t)

	srv := d.Server()
	var (
		wg       sync.WaitGroup
		received int32
		gotType  string
		gotBody  []byte
		mu       sync.Mutex
	)
	wg.Add(1)
	srv.Handle("test:ok", func(_ context.Context, task driver.Task) error {
		defer wg.Done()
		atomic.AddInt32(&received, 1)
		mu.Lock()
		gotType = task.Type()
		gotBody = task.Payload()
		mu.Unlock()
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	require.NoError(t, d.Client().Enqueue(context.Background(), "test:ok", []byte(`{"a":1}`),
		driver.WithQueue("deliver")))

	if !waitGroupTimeout(&wg, 5*time.Second) {
		t.Fatal("handler not invoked within timeout")
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&received))
	mu.Lock()
	assert.Equal(t, "test:ok", gotType)
	assert.Equal(t, []byte(`{"a":1}`), gotBody)
	mu.Unlock()
}

// #495: policy MaxAttempts → driver.WithMaxRetry(N) → mkq.WithAttempts(N+1)
// → BullMQ HASH `opts.attempts` の wire を end-to-end で検証する。queue
// package を import するためテストファイル末尾の helper 経由で側 redis
// (testRedis.Client) を使って HASH を直接覗く。
func TestEnqueue_MaxRetryAppliedToBullMQHash(t *testing.T) {
	d := newDriver(t)

	require.NoError(t, d.Client().Enqueue(context.Background(),
		"deliver:test", []byte(`{"x":1}`),
		driver.WithQueue("deliver"),
		driver.WithMaxRetry(7),
	))

	// BullMQ の HASH key 形式: `bull:<queue>:<jobID>`。最新 job を ID
	// counter (`bull:<queue>:id`) から逆引きする。
	ctx := context.Background()
	idStr, err := testRedis.Client.Get(ctx, "bull:deliver:id").Result()
	require.NoError(t, err)
	require.NotEmpty(t, idStr)

	hashKey := "bull:deliver:" + idStr
	opts, err := testRedis.Client.HGet(ctx, hashKey, "opts").Result()
	require.NoError(t, err)

	// opts は JSON 文字列。`attempts:8` (MaxRetry 7 + 初回 1 = 8) を含む
	// ことを確認すれば、queue.Client → mkqdriver.Client → mkq の
	// 全経路が wire されていることが verifiable。
	assert.Contains(t, opts, `"attempts":8`,
		"MaxRetry=7 should map to attempts=8 in BullMQ HASH (got %q)", opts)
}

// #495: mkqdriver.Config.QueueRateLimits = {"deliver": N} を渡すと
// mkq.WithRateLimit(N, time.Second) が worker に積まれて dispatch が
// tasks/sec に back-pressure される。flaky 防止のため緩い閾値で elapsed
// を assert する。
func TestServer_RateLimit_BackPressuresDispatch(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:           redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:     8,
		QueueRateLimits: map[string]int{"deliver": 2},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	var (
		wg       sync.WaitGroup
		received int32
	)
	wg.Add(5)
	srv.Handle("rl:tick", func(_ context.Context, _ driver.Task) error {
		defer wg.Done()
		atomic.AddInt32(&received, 1)
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	start := time.Now()
	for range 5 {
		require.NoError(t, d.Client().Enqueue(context.Background(), "rl:tick", nil,
			driver.WithQueue("deliver")))
	}
	if !waitGroupTimeout(&wg, 10*time.Second) {
		t.Fatal("rate-limited handler did not complete within timeout")
	}
	elapsed := time.Since(start)
	assert.Equal(t, int32(5), atomic.LoadInt32(&received))
	assert.GreaterOrEqual(t, elapsed, 1*time.Second,
		"mkq rate limit should pace dispatch (got elapsed=%s)", elapsed)
}

// QueueConcurrency = {"deliver": 2} で起動 → 5 task を concurrent に
// 詰めて、handler 内の sync.barrier で peak 並列数を観測する。peak <= 2
// かつ少なくとも一度は >= 2 同時実行されたことを assert する。
func TestServer_PerQueueConcurrency_CapsParallelHandlers(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	const cap = 2
	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:            redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:      8,
		QueueConcurrency: map[string]int{"deliver": cap},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()

	var (
		wg       sync.WaitGroup
		inFlight atomic.Int32
		peak     atomic.Int32
		release  = make(chan struct{})
	)
	wg.Add(5)
	srv.Handle("conc:hold", func(ctx context.Context, _ driver.Task) error {
		defer wg.Done()
		now := inFlight.Add(1)
		// peak の monotonic update。Add は atomic だが peak は CAS が必要。
		for {
			cur := peak.Load()
			if now <= cur || peak.CompareAndSwap(cur, now) {
				break
			}
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		inFlight.Add(-1)
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	for range 5 {
		require.NoError(t, d.Client().Enqueue(context.Background(), "conc:hold", nil,
			driver.WithQueue("deliver")))
	}

	// 並列数が cap に達するまで待つ (確実に同時 cap 個動いた状態を観測)。
	deadline := time.After(5 * time.Second)
	for peak.Load() < int32(cap) {
		select {
		case <-deadline:
			t.Fatalf("peak concurrent handlers never reached cap=%d (got %d)", cap, peak.Load())
		case <-time.After(20 * time.Millisecond):
		}
	}
	// 余分な handler が cap を超えて起動していないことを 200ms 観測。
	time.Sleep(200 * time.Millisecond)
	assert.LessOrEqual(t, peak.Load(), int32(cap),
		"per-queue concurrency cap=%d should bound peak parallel handlers", cap)

	close(release)
	if !waitGroupTimeout(&wg, 5*time.Second) {
		t.Fatal("handlers did not drain after release")
	}
}

// TestServer_DefaultPerQueueConcurrency_AppliesHotTunedPools locks the
// end-to-end wiring (New -> Server -> Start -> per-queue worker pool) for the
// hot-tuned defaults. The pure-function unit tests cover resolveQueueConcurrency
// in isolation, but only this asserts that a driver built with no per-queue
// override actually spawns the hot-tuned pool sizes — catching a regression if
// Server() ever stops feeding resolveQueueConcurrency into the pools (#1374).
func TestServer_DefaultPerQueueConcurrency_AppliesHotTunedPools(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	// QueueConcurrency / Concurrency を指定しない = コード default を使う。
	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis: redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	srv.Handle("noop", func(context.Context, driver.Task) error { return nil })
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	// 旧均等割り (16/6 = 2) ではなく hot-tuned default が実 pool に反映される。
	want := map[string]int{
		"inbox":       16,
		"deliver":     16,
		"webhook":     4,
		"push":        4,
		"export":      2,
		"maintenance": 2,
	}
	for q, n := range want {
		assert.Equal(t, n, d.WorkerCount(q), "queue %q default pool size", q)
	}
}

// TestEnqueue_UnknownQueueRejects ensures the driver refuses to fall
// back to a default queue for callers that forget WithQueue.
func TestEnqueue_UnknownQueueRejects(t *testing.T) {
	d := newDriver(t)
	err := d.Client().Enqueue(context.Background(), "x", nil,
		driver.WithQueue("not-a-queue"))
	require.Error(t, err)

	err = d.Client().Enqueue(context.Background(), "x", nil)
	require.Error(t, err)
}

// TestEnqueue_DuplicateUniqueDropsSilently confirms WithUnique TTL
// 重複は黙って捨てる (unique TTL の窓の中なので成功扱い)。
func TestEnqueue_DuplicateUniqueDropsSilently(t *testing.T) {
	d := newDriver(t)
	for i := 0; i < 3; i++ {
		require.NoError(t, d.Client().Enqueue(context.Background(),
			"unique:test", nil,
			driver.WithQueue("deliver"),
			driver.WithUnique(time.Minute),
		))
	}
	// Counts should be 1 wait job. Inspector exercises Counts().
	info, err := d.Inspector().GetQueueInfo("deliver")
	require.NoError(t, err)
	assert.Equal(t, 1, info.Pending)
}

// TestServer_HandleSkipRetryConvertsToUnrecoverable ensures the
// driver-level ErrSkipRetry sentinel reaches mkq as ErrUnrecoverable
// (which mkq surfaces as a permanent failure rather than a retry).
func TestServer_HandleSkipRetryConvertsToUnrecoverable(t *testing.T) {
	d := newDriver(t)
	srv := d.Server()
	var wg sync.WaitGroup
	wg.Add(1)
	srv.Handle("test:skip", func(_ context.Context, _ driver.Task) error {
		defer wg.Done()
		return fmt.Errorf("decode boom: %w", driver.ErrSkipRetry)
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	require.NoError(t, d.Client().Enqueue(context.Background(), "test:skip", nil,
		driver.WithQueue("deliver")))
	if !waitGroupTimeout(&wg, 5*time.Second) {
		t.Fatal("handler not invoked")
	}

	// Wait for finalisation. mkq processes the result asynchronously;
	// the failed counter takes a moment to update after the handler
	// returns.
	require.Eventually(t, func() bool {
		info, err := d.Inspector().GetQueueInfo("deliver")
		if err != nil {
			return false
		}
		return info.Failed >= 1
	}, 5*time.Second, 50*time.Millisecond, "expected job to land in failed bucket")
}

// driver.Delay は実際の mkq を通しても試行回数を消費しない (#3048)。retry の枠が
// 1 回しか無い (MaxRetry 0) job でも、遅延を何度返しても失敗にならず、最後に
// 処理されて完了する。変換 (nativeError) が抜けると、1 回目の遅延で failed に
// 落ちる。
func TestServer_HandleDelayDoesNotConsumeAttempts(t *testing.T) {
	d := newDriver(t)
	srv := d.Server()
	var runs atomic.Int64
	done := make(chan struct{})
	srv.Handle("test:delay", func(_ context.Context, _ driver.Task) error {
		if runs.Add(1) <= 3 {
			return fmt.Errorf("held back: %w", driver.Delay(20*time.Millisecond))
		}
		close(done)
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	require.NoError(t, d.Client().Enqueue(context.Background(), "test:delay", nil,
		driver.WithQueue("deliver"), driver.WithMaxRetry(0)))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("job did not come back after its delays (ran %d times)", runs.Load())
	}
	require.Eventually(t, func() bool {
		info, err := d.Inspector().GetQueueInfo("deliver")
		return err == nil && info.Completed >= 1 && info.Failed == 0
	}, 5*time.Second, 50*time.Millisecond, "the job must complete, not fail")

	ctx := context.Background()
	idStr, err := testRedis.Client.Get(ctx, "bull:deliver:id").Result()
	require.NoError(t, err)
	// 完了時に BullMQ は最後の試行を 1 回と数える。遅延も数えていれば 4 になる。
	atm, _ := testRedis.Client.HGet(ctx, "bull:deliver:"+idStr, "atm").Result()
	assert.Equal(t, "1", atm, "only the run that completed counts as an attempt")
}

// TestServer_HandleNoRegisteredHandler verifies an unknown job name
// surfaces as a permanent failure (no infinite retry loop).
func TestServer_HandleNoRegisteredHandler(t *testing.T) {
	d := newDriver(t)
	srv := d.Server()
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	require.NoError(t, d.Client().Enqueue(context.Background(), "not-registered", nil,
		driver.WithQueue("deliver")))

	require.Eventually(t, func() bool {
		info, err := d.Inspector().GetQueueInfo("deliver")
		if err != nil {
			return false
		}
		return info.Failed >= 1
	}, 5*time.Second, 50*time.Millisecond)
}

// TestInspector_FullSurface walks every Inspector entry point against
// the live Redis. Some methods (DrainPending) drop counts to zero;
// each case asserts the post-condition rather than the exact return
// value.
func TestInspector_FullSurface(t *testing.T) {
	d := newDriver(t)

	// Seed the deliver queue with two pending tasks (one ad-hoc, one
	// that we'll later inspect by ID).
	require.NoError(t, d.Client().Enqueue(context.Background(), "ins:a", []byte("a"),
		driver.WithQueue("deliver")))
	require.NoError(t, d.Client().Enqueue(context.Background(), "ins:b", []byte("b"),
		driver.WithQueue("deliver")))

	ins := d.Inspector()

	queues, err := ins.Queues()
	require.NoError(t, err)
	assert.Contains(t, queues, "deliver")

	info, err := ins.GetQueueInfo("deliver")
	require.NoError(t, err)
	assert.Equal(t, 2, info.Pending)

	// Listing
	pending, err := ins.ListPendingTasks("deliver", 1, 30)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	taskID := pending[0].ID
	assert.NotEmpty(t, pending[0].Type)

	got, err := ins.GetTaskInfo("deliver", taskID)
	require.NoError(t, err)
	assert.Equal(t, taskID, got.ID)

	_, err = ins.ListActiveTasks("deliver", 1, 30)
	require.NoError(t, err)
	_, err = ins.ListScheduledTasks("deliver", 1, 30)
	require.NoError(t, err)
	_, err = ins.ListRetryTasks("deliver", 1, 30)
	require.NoError(t, err)

	// Page / pageSize clamp paths.
	_, err = ins.ListPendingTasks("deliver", 0, 0)
	require.NoError(t, err)
	_, err = ins.ListPendingTasks("deliver", -1, 999)
	require.NoError(t, err)

	// Delete one specific task.
	require.NoError(t, ins.DeleteTask("deliver", taskID))
	infoAfterDelete, err := ins.GetQueueInfo("deliver")
	require.NoError(t, err)
	assert.Equal(t, 1, infoAfterDelete.Pending)

	// Drain remaining pending.
	_, err = ins.DeleteAllPendingTasks("deliver")
	require.NoError(t, err)
	infoAfterDrain, err := ins.GetQueueInfo("deliver")
	require.NoError(t, err)
	assert.Equal(t, 0, infoAfterDrain.Pending)

	// Unknown queue returns error.
	_, err = ins.GetQueueInfo("missing")
	require.Error(t, err)
	require.Error(t, ins.DeleteTask("missing", "x"))
	_, err = ins.DeleteAllPendingTasks("missing")
	require.Error(t, err)
	require.Error(t, ins.RunTask("missing", "x"))
	_, err = ins.GetTaskInfo("missing", "x")
	require.Error(t, err)
	_, err = ins.ListPendingTasks("missing", 1, 10)
	require.Error(t, err)
}

// TestInspector_QueueMetrics_PopulatesAfterFinalize spins up a worker
// with WithJobMetrics implicitly enabled (default Driver config), runs
// jobs to completion, and asserts the BullMQ-spec metrics keys are
// reflected in Inspector.QueueMetrics. The minute-bucket gating means
// Data may stay empty inside the first minute, so we only assert
// Count > 0 and Data being non-nil; the LIST roll is exercised in
// mkq's own integration tests.
func TestInspector_QueueMetrics_PopulatesAfterFinalize(t *testing.T) {
	d := newDriver(t)
	srv := d.Server()

	var wg sync.WaitGroup
	wg.Add(2)
	srv.Handle("metrics:ok", func(_ context.Context, _ driver.Task) error {
		defer wg.Done()
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	require.NoError(t, d.Client().Enqueue(context.Background(), "metrics:ok", nil,
		driver.WithQueue("deliver")))
	require.NoError(t, d.Client().Enqueue(context.Background(), "metrics:ok", nil,
		driver.WithQueue("deliver")))
	if !waitGroupTimeout(&wg, 5*time.Second) {
		t.Fatal("handlers did not run")
	}

	// Wait for finalisation — Lua collectMetrics runs inside the
	// finalize transaction, so Count should observe the cumulative
	// total once both jobs roll into the completed bucket.
	require.Eventually(t, func() bool {
		m, err := d.Inspector().QueueMetrics("deliver", driver.MetricsKindCompleted)
		return err == nil && m != nil && m.Count >= 2
	}, 5*time.Second, 50*time.Millisecond)

	// Failed bucket: no failures in this scenario, but the call must
	// still succeed and return a zero-valued result rather than an
	// error (no metrics keys yet means the lua HMGET / LRANGE return
	// empty and mkq.GetMetrics maps that to QueueMetrics{}).
	failed, err := d.Inspector().QueueMetrics("deliver", driver.MetricsKindFailed)
	require.NoError(t, err)
	require.NotNil(t, failed)
	assert.EqualValues(t, 0, failed.Count)
}

// TestInspector_QueueMetrics_InvalidArgs covers the validation paths.
func TestInspector_QueueMetrics_InvalidArgs(t *testing.T) {
	d := newDriver(t)

	_, err := d.Inspector().QueueMetrics("missing", driver.MetricsKindCompleted)
	require.Error(t, err)

	_, err = d.Inspector().QueueMetrics("deliver", "weird")
	require.Error(t, err)
}

// TestInspector_GetQueueInfo_IncludesRepeatSchedules verifies the
// mk-go-specific behaviour added for #455: registering a repeatable
// schedule via Scheduler.Register populates `bull:<queue>:repeat`,
// and Inspector.GetQueueInfo surfaces those entries through the
// Scheduled field even though mkq does not pre-allocate concrete
// delayed jobs into the `bull:<queue>:delayed` ZSET.
//
// admin/job-queue.vue maps Scheduled into its "Delayed" KPI column,
// so without this addition operators running mk-go on the mkq driver
// would see Delayed=0 even with N cron jobs registered.
func TestInspector_GetQueueInfo_IncludesRepeatSchedules(t *testing.T) {
	d := newDriver(t)
	sched := d.Scheduler()

	// Pre-condition: zero before any schedule is registered.
	infoBefore, err := d.Inspector().GetQueueInfo("maintenance")
	require.NoError(t, err)
	assert.Equal(t, 0, infoBefore.Scheduled)

	require.NoError(t, sched.Register("0 0 * * *", "task:daily", nil,
		driver.WithQueue("maintenance"),
	))
	require.NoError(t, sched.Register("*/5 * * * *", "task:every5", nil,
		driver.WithQueue("maintenance"),
	))

	infoAfter, err := d.Inspector().GetQueueInfo("maintenance")
	require.NoError(t, err)
	// addJobScheduler-11.lua は repeat ZSET と delayed ZSET の両方に
	// 書き込むので、Scheduled には少なくとも 2 (repeat ZCARD 由来) と、
	// fresh state では加えて concrete delayed の 2 が足される。本番の
	// steady-state では concrete delayed が即 promote されて 0 になり、
	// repeat ZCARD だけが残る。assertion は「register した数以上が
	// 見える」という性質に留め、過剰カウント側は許容する。
	assert.GreaterOrEqual(t, infoAfter.Scheduled, 2,
		"Scheduled should reflect at least the repeat ZSET cardinality")
	assert.GreaterOrEqual(t, infoAfter.Size, 2)
}

// TestInspector_RunTaskPromotesDelayed verifies that RunTask pulls a
// scheduled task back to wait (admin の「今すぐ実行」)。
func TestInspector_RunTaskPromotesDelayed(t *testing.T) {
	d := newDriver(t)

	require.NoError(t, d.Client().Enqueue(context.Background(),
		"ins:run", nil,
		driver.WithQueue("deliver"),
		driver.WithProcessIn(time.Hour),
	))

	ins := d.Inspector()
	scheduled, err := ins.ListScheduledTasks("deliver", 1, 30)
	require.NoError(t, err)
	require.NotEmpty(t, scheduled)

	require.NoError(t, ins.RunTask("deliver", scheduled[0].ID))
	// After promotion the task should sit in pending.
	require.Eventually(t, func() bool {
		info, err := ins.GetQueueInfo("deliver")
		return err == nil && info.Pending >= 1
	}, 3*time.Second, 50*time.Millisecond)
}

// TestInspector_GetQueueInfo_FailedReportedAsFailedNotRetry verifies the
// post-#1187 semantic: ErrSkipRetry / permanent failures land in the failed
// bucket and are reported via `Failed`. They no longer leak into `Retry`
// — Retry is reserved for delayed bucket entries with `atm > 0`.
//
// 旧 #1181 fix では mkq の bucket 構造を「retry-backoff も failed bucket
// に居る」と誤解して `Retry = counts.Failed` にしていたが、mkq#64 close
// 時の upstream comment で訂正された (delayed bucket が両者を混在させて
// いるのが正しい)。本 test は新 semantic を pin する (#1187)。
func TestInspector_GetQueueInfo_FailedReportedAsFailedNotRetry(t *testing.T) {
	d := newDriver(t)
	srv := d.Server()
	var wg sync.WaitGroup
	wg.Add(1)
	srv.Handle("ins:permanent-fail", func(_ context.Context, _ driver.Task) error {
		defer wg.Done()
		// ErrSkipRetry sentinel = immediate failure into the failed bucket
		// (mkq's ErrUnrecoverable). retry-backoff 経路を回さない fast
		// path だが、行き着くのは failed bucket (= permanent failure)。
		return fmt.Errorf("intentional fail: %w", driver.ErrSkipRetry)
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	require.NoError(t, d.Client().Enqueue(context.Background(),
		"ins:permanent-fail", nil,
		driver.WithQueue("deliver")))
	if !waitGroupTimeout(&wg, 5*time.Second) {
		t.Fatal("handler not invoked")
	}

	ins := d.Inspector()
	require.Eventually(t, func() bool {
		info, err := ins.GetQueueInfo("deliver")
		return err == nil && info.Failed >= 1 && info.Retry == 0
	}, 5*time.Second, 50*time.Millisecond,
		"permanent failure should land in Failed, not Retry (#1187)")
}

// TestInspector_GetQueueInfo_UnknownQueue: queueFor が nil を返す未知 queue
// で `mkqdriver: unknown queue` error を返す (= error path / fallback の
// 入口を pin)。
func TestInspector_GetQueueInfo_UnknownQueue(t *testing.T) {
	d := newDriver(t)
	_, err := d.Inspector().GetQueueInfo("does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown queue")
}

// TestInspector_ListRetryTasks_UnknownQueue: listDelayedFiltered 経路の
// queueFor==nil error path を pin (= ListScheduledTasks も同 helper を
// 通るので 1 test で両方 cover)。
func TestInspector_ListRetryTasks_UnknownQueue(t *testing.T) {
	d := newDriver(t)
	_, err := d.Inspector().ListRetryTasks("does-not-exist", 1, 30)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown queue")
}

// TestInspector_ListScheduledTasks_BoundaryPaging verifies the
// listDelayedFiltered pagination guards: page < 1 / pageSize <= 0 /
// pageSize > 100 はすべて default に倒れる、start >= len(filtered) は
// 空 slice を返す (= ある page 番号より先には何も無い)。
func TestInspector_ListScheduledTasks_BoundaryPaging(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()

	require.NoError(t, d.Client().Enqueue(ctx,
		"ins:paging", nil,
		driver.WithQueue("deliver"),
		driver.WithProcessIn(time.Hour),
	))

	ins := d.Inspector()
	// page=0 / pageSize=0 → default (page=1, pageSize=30) で 1 件返る。
	rows, err := ins.ListScheduledTasks("deliver", 0, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	// pageSize > 100 → default pageSize 30 に倒れる。同 fixture で 1 件。
	rows, err = ins.ListScheduledTasks("deliver", 1, 200)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	// page=2 (= start=30) は 1 件しかない fixture で空 slice。
	rows, err = ins.ListScheduledTasks("deliver", 2, 30)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// TestInspector_GetQueueInfo_RetryReflectsDelayedAtmPositive verifies the
// new semantic for the retry-backoff path: a delayed bucket entry with
// `atm > 0` (= 過去に attempt 失敗して backoff 待ち) is surfaced as
// `Retry` (= NOT `Scheduled`, NOT `Failed`) (#1187, mkq#64 close).
//
// driver level に backoff option が無いので、natural な retry cycle を
// 回す代わりに WithProcessIn で初回 delayed に積んだ後 HSET で `atm` を
// 直接 1 に書き換えて simulate する (white-box; mkq の HASH key 構造を
// 既存 `TestEnqueue_MaxRetryAppliedToBullMQHash` と同じ手法で利用)。
func TestInspector_GetQueueInfo_RetryReflectsDelayedAtmPositive(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()

	require.NoError(t, d.Client().Enqueue(ctx,
		"ins:retry-pending", nil,
		driver.WithQueue("deliver"),
		driver.WithProcessIn(time.Hour),
	))

	// 最新 job ID を `bull:<queue>:id` カウンタから取り、HASH の atm を
	// 1 に上書きする (= mkq が retry-backoff で 1 度 attempt 済の状態を
	// simulate)。
	idStr, err := testRedis.Client.Get(ctx, "bull:deliver:id").Result()
	require.NoError(t, err)
	require.NoError(t, testRedis.Client.HSet(ctx, "bull:deliver:"+idStr, "atm", 1).Err())

	ins := d.Inspector()
	info, err := ins.GetQueueInfo("deliver")
	require.NoError(t, err)
	assert.Equal(t, 1, info.Retry, "atm>0 delayed entry should be classified as Retry (#1187)")
	assert.Equal(t, 0, info.Scheduled, "scheduled should not include atm>0 entries")
	assert.Equal(t, 0, info.Failed, "failed bucket must remain untouched by retry-backoff entries")
}

// TestInspector_ListRetryTasks_FiltersDelayedAtmPositive verifies that
// ListRetryTasks returns only delayed bucket entries with `atm > 0`
// (= retry-backoff waiters), not the entire failed bucket. (#1187)
func TestInspector_ListRetryTasks_FiltersDelayedAtmPositive(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()

	// 2 件 enqueue (= 同じ atm=0 delayed)、片方だけ atm=1 に書き換え。
	require.NoError(t, d.Client().Enqueue(ctx,
		"ins:retry-A", nil,
		driver.WithQueue("deliver"),
		driver.WithProcessIn(time.Hour),
	))
	require.NoError(t, d.Client().Enqueue(ctx,
		"ins:retry-B", nil,
		driver.WithQueue("deliver"),
		driver.WithProcessIn(time.Hour),
	))

	// `bull:deliver:id` は最新 ID を返すので、これが atm>0 にする方
	// (= retry-B 想定)、もう片方 (= retry-A) は atm=0 のままで scheduled
	// 扱い。
	idB, err := testRedis.Client.Get(ctx, "bull:deliver:id").Result()
	require.NoError(t, err)
	require.NoError(t, testRedis.Client.HSet(ctx, "bull:deliver:"+idB, "atm", 1).Err())

	ins := d.Inspector()
	retry, err := ins.ListRetryTasks("deliver", 1, 30)
	require.NoError(t, err)
	require.Len(t, retry, 1, "only atm>0 entries should appear in ListRetryTasks")

	scheduled, err := ins.ListScheduledTasks("deliver", 1, 30)
	require.NoError(t, err)
	require.Len(t, scheduled, 1, "atm=0 entry should be in ListScheduledTasks")
}

// TestInspector_ListScheduledTasks_FiltersDelayedAtmZero verifies that
// ListScheduledTasks returns only delayed bucket entries with `atm == 0`
// (= first-time scheduled, cron / WithProcessIn 等)、retry-backoff
// (atm > 0) は ListRetryTasks 側に出る (#1187)。
func TestInspector_ListScheduledTasks_FiltersDelayedAtmZero(t *testing.T) {
	d := newDriver(t)

	// WithProcessIn で delayed bucket に直接 enqueue (= 初回処理待ち、atm=0)。
	require.NoError(t, d.Client().Enqueue(context.Background(),
		"ins:list-scheduled", nil,
		driver.WithQueue("deliver"),
		driver.WithProcessIn(time.Hour),
	))

	ins := d.Inspector()
	scheduled, err := ins.ListScheduledTasks("deliver", 1, 30)
	require.NoError(t, err)
	require.Len(t, scheduled, 1, "first-time scheduled (atm=0) entry should appear")

	// 同時に retry path には居ないこと。
	retry, err := ins.ListRetryTasks("deliver", 1, 30)
	require.NoError(t, err)
	assert.Empty(t, retry, "retry list must not contain atm=0 entries")
}

// TestClient_Enqueue_WithKeepFailed_BoundsFailedBucket verifies the
// `driver.WithKeepFailed(n)` option translates to `mkq.WithKeepFailed(n)`
// so the failed bucket size is bounded at n entries even when N+M
// permanent-fail jobs are enqueued. Without this the bucket would grow
// unbounded and admin observability degrades (#1184).
func TestClient_Enqueue_WithKeepFailed_BoundsFailedBucket(t *testing.T) {
	d := newDriver(t)
	srv := d.Server()
	var wg sync.WaitGroup
	const cap = 3
	const total = 7 // cap=3, つまり最後 3 件だけ残るはず
	wg.Add(total)
	srv.Handle("keepfailed:burst", func(_ context.Context, _ driver.Task) error {
		defer wg.Done()
		return fmt.Errorf("intentional fail: %w", driver.ErrSkipRetry)
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	for i := 0; i < total; i++ {
		require.NoError(t, d.Client().Enqueue(context.Background(),
			"keepfailed:burst", []byte(fmt.Sprintf(`{"i":%d}`, i)),
			driver.WithQueue("deliver"),
			driver.WithKeepFailed(cap),
		))
	}
	if !waitGroupTimeout(&wg, 10*time.Second) {
		t.Fatal("not all handlers invoked")
	}

	ins := d.Inspector()
	// mkq の retention 削除は finalise の同期 path で動く (= worker が
	// 失敗 ack を返した瞬間に古い entry を ZREMRANGEBYRANK で prune)。
	// 全 7 件 handle 完了後、ある程度待って failed bucket が cap 件以下
	// に絞られていることを確認する (= 上限 retention 動作)。
	require.Eventually(t, func() bool {
		info, err := ins.GetQueueInfo("deliver")
		return err == nil && info.Failed <= cap && info.Failed >= 1
	}, 5*time.Second, 50*time.Millisecond,
		"failed bucket should be bounded at WithKeepFailed(%d) (=%d max)", cap, cap)
}

// TestInspector_RunTask_FallsBackToRetryJobForFailedBucket verifies that
// RunTask succeeds against a job in the failed bucket, by falling back
// from PromoteJob (delayed-only) to RetryJob. admin の RunTask
// transparently handles both buckets; this test pins the equivalent
// behaviour for the mkq driver (#1181).
//
// Without this fallback the admin panel's "Retry all queues now" button
// is a no-op on retry-pending jobs and operators cannot recover stuck
// delivery from the UI.
func TestInspector_RunTask_FallsBackToRetryJobForFailedBucket(t *testing.T) {
	d := newDriver(t)
	srv := d.Server()
	var wg sync.WaitGroup
	wg.Add(1)
	// fail-once-then-succeed: 初回は ErrSkipRetry で failed bucket に落とし、RunTask
	// 後の再処理では成功させる。常に再失敗する handler だと RunTask が failed から
	// 出した job が wait→active→再失敗で failed に戻り、「failed を出た瞬間」を
	// 観測する後段の Eventually が CI 負荷下で毎 poll「まだ failed」と見えて flaky
	// に timeout していた (#1290)。
	var calls atomic.Int32
	srv.Handle("ins:fail-then-retry", func(_ context.Context, _ driver.Task) error {
		if calls.Add(1) == 1 {
			wg.Done()
			return fmt.Errorf("intentional fail: %w", driver.ErrSkipRetry)
		}
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	require.NoError(t, d.Client().Enqueue(context.Background(),
		"ins:fail-then-retry", nil,
		driver.WithQueue("deliver")))
	if !waitGroupTimeout(&wg, 5*time.Second) {
		t.Fatal("handler not invoked")
	}

	ins := d.Inspector()
	ctx := context.Background()
	// Wait until the job has landed in the failed bucket. failed bucket は
	// 公開 list API には出ないので (#1187 で ListRetryTasks が delayed 専用
	// に変わった)、raw Redis ZRANGE で task ID を取り出す。
	var failedTaskID string
	require.Eventually(t, func() bool {
		ids, err := testRedis.Client.ZRange(ctx, "bull:deliver:failed", 0, -1).Result()
		if err != nil || len(ids) == 0 {
			return false
		}
		failedTaskID = ids[0]
		return true
	}, 5*time.Second, 50*time.Millisecond,
		"job should have landed in the failed bucket")

	// RunTask must succeed via RetryJob fallback even though the job is
	// not in mkq's delayed bucket. 呼び出し側の契約: callers only
	// pass a task ID and the driver routes to the right move primitive.
	require.NoError(t, ins.RunTask("deliver", failedTaskID),
		"RunTask should fall back to RetryJob for failed bucket jobs")

	// After the fallback the failed bucket loses this job (= ZRANGE no
	// longer contains it). We assert at the bucket level rather than
	// chasing the job through wait→active→failed again (the handler is
	// still registered and will refail it).
	require.Eventually(t, func() bool {
		ids, err := testRedis.Client.ZRange(ctx, "bull:deliver:failed", 0, -1).Result()
		if err != nil {
			return false
		}
		for _, id := range ids {
			if id == failedTaskID {
				return false // still in failed bucket
			}
		}
		return true
	}, 5*time.Second, 50*time.Millisecond,
		"job should have left the failed bucket after RetryJob fallback")
}

// TestScheduler_RegisterRoundtrip exercises the cron Register API and
// the validator branches Scheduler exposes.
func TestScheduler_RegisterRoundtrip(t *testing.T) {
	d := newDriver(t)
	sched := d.Scheduler()
	require.NoError(t, sched.Register("0 0 * * *", "task:daily", nil,
		driver.WithQueue("maintenance"),
		driver.WithMaxRetry(0),
	))
	require.NoError(t, sched.Start())
	sched.Shutdown()

	// Unknown queue → error.
	require.Error(t, sched.Register("0 0 * * *", "task:x", nil,
		driver.WithQueue("missing")))

	// Missing queue → error.
	require.Error(t, sched.Register("0 0 * * *", "task:y", nil))
}

// TestServer_StartTwiceRejects ensures the server cannot be started
// repeatedly — Process holds Redis connections per worker slot and
// re-Start would leak them.
func TestServer_StartTwiceRejects(t *testing.T) {
	d := newDriver(t)
	srv := d.Server()
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	require.Error(t, srv.Start(), "second Start must be rejected")
}

// TestServer_HandleAfterStartPanics validates the documented contract:
// callers must register every handler before Start.
func TestServer_HandleAfterStartPanics(t *testing.T) {
	d := newDriver(t)
	srv := d.Server()
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Handle after Start must panic")
		}
	}()
	srv.Handle("late", func(context.Context, driver.Task) error { return nil })
}

// TestDriver_CloseWithoutStart confirms a freshly-built driver cleans
// up cleanly when no sub-services were exercised.
func TestDriver_CloseWithoutStart(t *testing.T) {
	d := newDriver(t)
	// reset Cleanup so we can call Close manually.
	require.NoError(t, d.Close())

	// Close after Close — second call should still succeed (idempotent
	// on the worker side; the underlying redis client returns nil for
	// repeated Close).
	require.NoError(t, d.Close())
}

// TestNewDriver_BadAddressFails ensures the constructor surfaces
// connect failures rather than returning a half-built Driver.
func TestNewDriver_BadAddressFails(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := mkqdriver.New(ctx, mkqdriver.Config{
		Redis: redis.UniversalOptions{Addrs: []string{"127.0.0.1:1"}},
	})
	require.Error(t, err)
	if !errors.Is(err, context.DeadlineExceeded) && err == nil {
		t.Fatalf("expected non-nil error, got %v", err)
	}
}

// TestDriver_Resize_BeforeStartReturnsNotSupported verifies the
// pre-Server() contract — Resize called before Driver.Server() returns
// ErrResizeNotSupported (there is no Server object to resize through).
//
// **`Server()` 済みで `Start()` 前は別のエラーになる** — pool map が空なので
// Server.Resize の `unknown queue` 枝に落ちる。そちらは
// TestServer_Resize_UnknownQueueReturnsError が固定している。この違いを
// 取り違えると「Start 前は ErrResizeNotSupported」という誤った前提が
// 配線側へ伝播する (#2985 で実際に踏んだ)。
func TestDriver_Resize_BeforeStartReturnsNotSupported(t *testing.T) {
	d := newDriver(t)
	// No call to d.Server() yet.
	err := d.Resize("deliver", 8)
	require.ErrorIs(t, err, driver.ErrResizeNotSupported)
}

// TestServer_Resize_UnknownQueueReturnsError verifies that Resize on
// a queue not in the driver's known set returns a descriptive error
// (not a silent no-op that would hide controller bugs).
func TestServer_Resize_UnknownQueueReturnsError(t *testing.T) {
	d := newDriver(t)
	srv := d.Server()
	srv.Handle("noop", func(ctx context.Context, _ driver.Task) error { return nil })
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	err := d.Resize("does-not-exist", 4)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist")
}

// TestServer_Resize_NegativeNReturnsError verifies that callers cannot
// pass invalid negative pool sizes (controller bug guard).
func TestServer_Resize_NegativeNReturnsError(t *testing.T) {
	d := newDriver(t)
	srv := d.Server()
	srv.Handle("noop", func(ctx context.Context, _ driver.Task) error { return nil })
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	err := d.Resize("deliver", -1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be >= 0")
}

// TestServer_ResizeUp_ProcessesMoreInParallel verifies that Resize-up
// actually increases the parallel handler count observable via the
// in-flight gauge pattern (= ADR §7.2 the "double parallelism" check).
// Strategy: start with N=2, observe peak=2; Resize to N=8, observe peak
// climbs to 8.
func TestServer_ResizeUp_ProcessesMoreInParallel(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:            redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:      32,
		QueueConcurrency: map[string]int{"deliver": 2},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	var (
		inFlight atomic.Int32
		peak     atomic.Int32
		release  = make(chan struct{})
	)
	srv.Handle("resize:hold", func(ctx context.Context, _ driver.Task) error {
		now := inFlight.Add(1)
		for {
			cur := peak.Load()
			if now <= cur || peak.CompareAndSwap(cur, now) {
				break
			}
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		inFlight.Add(-1)
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	// Phase 1: pre-resize. Enqueue 4 jobs, peak should reach 2 (= initial).
	for range 4 {
		require.NoError(t, d.Client().Enqueue(context.Background(), "resize:hold", nil,
			driver.WithQueue("deliver")))
	}
	deadline := time.After(5 * time.Second)
	for peak.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("phase 1 peak never reached 2 (got %d)", peak.Load())
		case <-time.After(20 * time.Millisecond):
		}
	}
	// 2 を超えていないことを 100ms 観測。
	time.Sleep(100 * time.Millisecond)
	require.LessOrEqual(t, peak.Load(), int32(2),
		"pre-resize peak should be capped at initial concurrency=2")
	assert.Equal(t, 2, d.WorkerCount("deliver"), "WorkerCount should reflect initial pool size")

	// Phase 2: Resize-up to 8.
	require.NoError(t, d.Resize("deliver", 8))
	assert.Equal(t, 8, d.WorkerCount("deliver"), "WorkerCount should reflect post-resize pool size")

	// Phase 2 peak observation: enqueue more jobs, peak should now climb.
	for range 8 {
		require.NoError(t, d.Client().Enqueue(context.Background(), "resize:hold", nil,
			driver.WithQueue("deliver")))
	}
	deadline = time.After(5 * time.Second)
	for peak.Load() < 8 {
		select {
		case <-deadline:
			t.Fatalf("phase 2 peak never reached 8 after Resize (got %d)", peak.Load())
		case <-time.After(20 * time.Millisecond):
		}
	}

	close(release)
}

// TestServer_ResizeDown_CancelsInFlight verifies the actual mkq semantics
// of Worker.Stop: it cancels the run context of in-flight handlers
// (handler ctx.Done fires) and waits for the goroutines to exit cleanly,
// rather than blocking until natural job completion.
//
// 注: ADR §7.2 で「graceful drain」と書いたが、これは "goroutine leak しない"
// の意味で、in-flight job は cancel 経路で中断される (next pickup で retry
// される前提)。slow remote inbox 等で scale-down が分単位 block する事態を
// 避ける mkq library の合理的選択。
func TestServer_ResizeDown_CancelsInFlight(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:            redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:      32,
		QueueConcurrency: map[string]int{"deliver": 4},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	var (
		inFlight  atomic.Int32
		completed atomic.Int32
		cancelled atomic.Int32
	)
	srv.Handle("resize:slow", func(ctx context.Context, _ driver.Task) error {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		// 長 job: 5 秒 sleep。Resize で cancel された場合は ctx.Done で
		// 即座に return、completed しない。
		select {
		case <-time.After(5 * time.Second):
			completed.Add(1)
			return nil
		case <-ctx.Done():
			cancelled.Add(1)
			return ctx.Err()
		}
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	// 4 jobs enqueue; each Worker picks one up immediately.
	for range 4 {
		require.NoError(t, d.Client().Enqueue(context.Background(), "resize:slow", nil,
			driver.WithQueue("deliver")))
	}

	// Poll until 全 4 Worker が job を pick up 済を確認。
	deadline := time.After(3 * time.Second)
	for inFlight.Load() < 4 {
		select {
		case <-deadline:
			t.Fatalf("inFlight never reached 4 (got %d)", inFlight.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Resize down: should NOT block 5s — Worker.Stop cancels in-flight.
	start := time.Now()
	require.NoError(t, d.Resize("deliver", 1))
	elapsed := time.Since(start)

	// 3 Worker × Stop ≈ ms オーダー (cancel + goroutine join のみ)、決して
	// 5 秒 (job 完了時間) はかからない。
	assert.Less(t, elapsed, 2*time.Second,
		"Resize-down should not block for natural job completion (elapsed=%v)", elapsed)

	// 完了 (completed) ではなく cancel (cancelled) として終わったことを確認。
	// 最低 3 個は cancel されているはず (kept=1 はそのまま動き続ける)。
	assert.GreaterOrEqual(t, cancelled.Load(), int32(3),
		"at least 3 of 4 in-flight jobs should be cancelled by Resize-down (got %d)", cancelled.Load())
	assert.Equal(t, int32(0), completed.Load(),
		"no job should naturally complete in this short test window")
	assert.Equal(t, 1, d.WorkerCount("deliver"))
}

// TestServer_ResizeRace verifies that overlapping Resize calls on the
// same queue serialise safely (no goroutine leak, no negative pool
// size, no panic). The final state matches the last Resize call.
func TestServer_ResizeRace(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:            redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:      32,
		QueueConcurrency: map[string]int{"deliver": 4},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	srv.Handle("noop", func(ctx context.Context, _ driver.Task) error { return nil })
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	// 10 concurrent Resize calls with alternating sizes. Each call
	// should complete without error; final state must equal the last
	// committed value (= we test the sequential-consistency property
	// that the lock provides).
	var wg sync.WaitGroup
	sizes := []int{8, 2, 6, 3, 5, 1, 4, 7, 2, 8}
	for _, n := range sizes {
		wg.Add(1)
		go func(target int) {
			defer wg.Done()
			err := d.Resize("deliver", target)
			assert.NoError(t, err)
		}(n)
	}
	wg.Wait()

	// 最終 worker 数は 1..8 のいずれか (最後に commit された Resize の値)
	// で、いずれにせよ [1, 8] の範囲内。
	finalCount := d.WorkerCount("deliver")
	assert.GreaterOrEqual(t, finalCount, 1)
	assert.LessOrEqual(t, finalCount, 8)
}

// TestServer_Resize_AfterShutdownReturnsError verifies the post-shutdown
// race guard: a Resize call that captured a pool pointer just before
// Server.Shutdown ran must not spawn leaked Workers on the orphan pool.
//
// We simulate the race by calling Shutdown immediately before Resize on
// the same Server. The Resize is expected to fail with either "unknown
// queue" (s.pools is now nil) or ErrResizeAfterShutdown (the captured
// pool flag is checked) — both are valid no-spawn outcomes.
func TestServer_Resize_AfterShutdownReturnsError(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:            redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:      32,
		QueueConcurrency: map[string]int{"deliver": 2},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	srv.Handle("noop", func(ctx context.Context, _ driver.Task) error { return nil })
	require.NoError(t, srv.Start())

	// Shutdown を呼んでから Resize を試みる (= 後追い caller の最悪
	// シナリオの sequential 化、本物の race window と同等の効果)。
	srv.Shutdown()

	err = d.Resize("deliver", 8)
	require.Error(t, err, "Resize after Shutdown must not silently spawn workers")
	// Worker 数は 0 のまま、leak していない。
	assert.Equal(t, 0, d.WorkerCount("deliver"))
}

// TestServer_Resize_ToZeroStopsAll verifies that Resize(qname, 0)
// removes every Worker for the queue (= "pause queue" semantics).
// Subsequent enqueues backlog in Redis until a non-zero Resize.
func TestServer_Resize_ToZeroStopsAll(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:            redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:      32,
		QueueConcurrency: map[string]int{"deliver": 2},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	var processed atomic.Int32
	srv.Handle("noop", func(ctx context.Context, _ driver.Task) error {
		processed.Add(1)
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	// Resize to 0 → no workers should be active.
	require.NoError(t, d.Resize("deliver", 0))
	assert.Equal(t, 0, d.WorkerCount("deliver"))

	// Enqueue 3 jobs — they should accumulate in Redis, not be processed.
	for range 3 {
		require.NoError(t, d.Client().Enqueue(context.Background(), "noop", nil,
			driver.WithQueue("deliver")))
	}
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(0), processed.Load(),
		"no jobs should be processed while pool size is 0")

	// Resize back to 2 → jobs should drain.
	require.NoError(t, d.Resize("deliver", 2))
	deadline := time.After(3 * time.Second)
	for processed.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("processed never reached 3 after Resize back to 2 (got %d)", processed.Load())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// #2069 (upstream #17436): Inspector.PauseQueue/UnpauseQueue + GetQueueInfo.IsPaused の
// wiring と、pause 中 enqueue した job が resume で処理される (orphan しない) ことを検証。
func TestPauseResume_EndToEnd(t *testing.T) {
	d := newDriver(t)
	insp := d.Inspector()

	// wiring: pause → IsPaused true、resume → false。
	require.NoError(t, insp.PauseQueue("deliver"))
	info, err := insp.GetQueueInfo("deliver")
	require.NoError(t, err)
	assert.True(t, info.IsPaused, "pause 後は IsPaused=true")

	// worker を起動。pause 中は fetch されない。
	var received int32
	var wg sync.WaitGroup
	wg.Add(1)
	srv := d.Server()
	srv.Handle("test:pause", func(_ context.Context, _ driver.Task) error {
		atomic.AddInt32(&received, 1)
		wg.Done()
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	// pause 中に enqueue。
	require.NoError(t, d.Client().Enqueue(context.Background(), "test:pause", []byte(`{}`),
		driver.WithQueue("deliver")))

	// pause 中は処理されない (1s 待っても handler 未起動 = lua gate が wait を fetch しない)。
	time.Sleep(1 * time.Second)
	assert.Equal(t, int32(0), atomic.LoadInt32(&received), "pause 中は job が処理されない")

	// resume → IsPaused false + parked job が処理される (orphan しない)。
	require.NoError(t, insp.UnpauseQueue("deliver"))
	info, err = insp.GetQueueInfo("deliver")
	require.NoError(t, err)
	assert.False(t, info.IsPaused, "resume 後は IsPaused=false")
	if !waitGroupTimeout(&wg, 5*time.Second) {
		t.Fatal("resume 後に parked job が処理されなかった (orphan)")
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&received))
}

// TestServer_RelationshipFloodDoesNotStarveDeliver is the regression test for
// #2403: relationship jobs used to be enqueued onto the deliver queue, so a
// burst of them (account migration / follow import) occupied the delivery
// worker pool and stalled ActivityPub delivery itself.
//
// 分離できていることを「大量の relationship job で worker を塞いだ状態でも
// deliver が処理される」という形で直接観測する。
//
// **queue.Client 経由で enqueue するのが要点。** driver に直接 queue 名を
// 渡すと routing を迂回してしまい、enqueueRelationship を deliver に戻す
// 回帰を捕まえられない。ここでは EnqueueFollow / EnqueueDeliver という
// 実際の呼び出し口を使うので、routing と worker 分離の両方を同時に固定する。
//
// relationship の worker 数 (4) より十分多い job を投げてプールを飽和させ、
// 解放しないまま deliver が完了することを確認する。
func TestServer_RelationshipFloodDoesNotStarveDeliver(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	// flood は **deliver の worker 数 (16) を上回る**必要がある。下回ると、
	// 相乗りしていても deliver 側に空き worker が残ってしまい、回帰を
	// 検出できない (実際 flood=12 では revert しても通ってしまった)。
	const relWorkers = 4      // = defaultQueueConcurrency["relationship"]
	const deliverWorkers = 16 // = defaultQueueConcurrency["deliver"]
	const flood = deliverWorkers + 8

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis: redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	client := queue.NewClient(d)
	srv := d.Server()

	var (
		relInFlight atomic.Int32
		release     = make(chan struct{})
		delivered   = make(chan struct{}, 1)
	)
	// relationship 側は release されるまで worker を握り続ける。
	srv.Handle(queue.TaskTypeFollow, func(ctx context.Context, _ driver.Task) error {
		relInFlight.Add(1)
		defer relInFlight.Add(-1)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	srv.Handle(queue.TaskTypeDeliver, func(_ context.Context, _ driver.Task) error {
		select {
		case delivered <- struct{}{}:
		default:
		}
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	for i := range flood {
		require.NoError(t, client.EnqueueFollow(queue.FollowPayload{
			FollowerID: fmt.Sprintf("follower%d", i),
			FolloweeID: "followee",
		}))
	}

	// relationship の worker プールが飽和するまで待つ。ここまで来れば
	// 「relationship で塞がっている」状態を確実に作れている。飽和しない
	// (= relationship queue に worker が居ない) 場合もここで落ちる。
	deadline := time.After(10 * time.Second)
	for relInFlight.Load() < int32(relWorkers) {
		select {
		case <-deadline:
			t.Fatalf("relationship pool never saturated (in-flight=%d, want %d)",
				relInFlight.Load(), relWorkers)
		case <-time.After(20 * time.Millisecond):
		}
	}

	// 塞がったまま deliver を投入し、relationship を解放せずに完了させる。
	require.NoError(t, client.EnqueueDeliver(queue.DeliverPayload{
		Inbox: "https://example.com/inbox",
		Body:  []byte(`{}`),
	}))

	select {
	case <-delivered:
	case <-time.After(10 * time.Second):
		t.Fatalf("deliver job was starved by %d in-flight relationship jobs "+
			"(is enqueueRelationship back on the deliver queue?)", relInFlight.Load())
	}

	close(release)
}

// TestScheduler_RepeatedRegisterDoesNotDuplicate pins the property that makes
// `driver.WithUnique` unnecessary on the mkq scheduler (#2405).
//
// mkq は発火 job に決定的な ID (`repeat:<scheduleID>:<nextMillis>`) を振り、
// `updateJobScheduler-12.lua` が `EXISTS` で重複を弾く。したがって同じ
// scheduleID を何度 Register しても、同じ発火時刻の job が二重に積まれること
// はない。**cron の多重実行防止は option ではなくこの ID 設計で担保されている。**
//
// mkq がこの前提を崩す (job ID をランダム化する等) と cron が多重実行しうる
// ようになるので、ここで固定する。
func TestScheduler_RepeatedRegisterDoesNotDuplicate(t *testing.T) {
	d := newDriver(t)
	sched := d.Scheduler()

	const taskType = "task:dedup"
	register := func() {
		require.NoError(t, sched.Register("*/5 * * * *", taskType, nil,
			driver.WithQueue("maintenance"),
			// これらは drop されるが、渡しても壊れないことも同時に確認する。
			driver.WithMaxRetry(0),
			driver.WithUnique(5*time.Minute),
		))
	}

	register()
	infoOnce, err := d.Inspector().GetQueueInfo("maintenance")
	require.NoError(t, err)

	// 同じ scheduleID で 4 回追加登録する。
	for range 4 {
		register()
	}
	infoRepeat, err := d.Inspector().GetQueueInfo("maintenance")
	require.NoError(t, err)

	assert.Equal(t, infoOnce.Scheduled, infoRepeat.Scheduled,
		"re-registering the same scheduleID must not add more scheduled entries")

	// repeat ZSET も 1 エントリのまま (scheduleID がキー)。
	card, err := testRedis.Client.ZCard(context.Background(), "bull:maintenance:repeat").Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, card, "the repeat ZSET keys on scheduleID, so it stays at one entry")
}

// TestScheduler_DropsPerFireOptionsSilently documents that the dropped options
// no longer produce a startup warning (#2405)。実害が無いのに毎起動 11 件出て
// 本物の警告を埋めていたため落とした。Register 自体は成功する。
func TestScheduler_DropsPerFireOptionsSilently(t *testing.T) {
	d := newDriver(t)
	sched := d.Scheduler()

	require.NoError(t, sched.Register("0 3 * * *", "task:opts", nil,
		driver.WithQueue("maintenance"),
		driver.WithMaxRetry(3),
		driver.WithUnique(time.Hour),
		driver.WithProcessIn(time.Minute),
	))
}

// TestInspector_ListCompletedTasks: completed bucket の list 経路を pin する。
//
// admin UI の Completed タブが読む唯一の経路なのに test が無く、
// カバレッジ 0% のまま残っていた (#2469 で公開ラッパーを足した際に
// パッケージが閾値ちょうどに張り付き、CI の微差で落ちて発覚した)。
func TestInspector_ListCompletedTasks(t *testing.T) {
	d := newDriver(t)
	ins := d.Inspector()

	// 完了 job が無い状態でも空で返る (エラーにしない)。
	got, err := ins.ListCompletedTasks("deliver", 1, 30)
	require.NoError(t, err)
	assert.Empty(t, got)

	// 未知 queue は他の list と同じく error。
	_, err = ins.ListCompletedTasks("does-not-exist", 1, 30)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown queue")
}

// **長い idle poll interval でもジョブ取得は遅くならないこと。**
//
// worker は BZPOPMIN で marker key を待ち、ジョブが積まれた時点で Lua 側が
// marker を push するのでミリ秒で起きる。interval は「marker を取り逃した
// 場合に気づくまで」の上限でしかない。ここが崩れると、間隔を延ばした分だけ
// ジョブの処理が遅れる。
func TestIdlePollInterval_DoesNotDelayPickup(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	// 取得が interval に律速されるなら、この値がそのまま遅延になる。
	const interval = 30 * time.Second
	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:            redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:      2,
		IdlePollInterval: interval,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	got := make(chan time.Time, 1)
	srv.Handle("probe", func(_ context.Context, _ driver.Task) error {
		select {
		case got <- time.Now():
		default:
		}
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	// worker が marker 待ちに入るのを待ってから積む。
	time.Sleep(500 * time.Millisecond)

	enqueued := time.Now()
	require.NoError(t, d.Client().Enqueue(context.Background(), "probe", []byte(`{}`),
		driver.WithQueue("deliver")))

	select {
	case at := <-got:
		elapsed := at.Sub(enqueued)
		// interval に律速されていないこと。marker 経由なら通常は 10ms 未満で
		// 起きるが、CI の負荷を見込んで interval の 1/6 を上限にする。
		if elapsed > interval/6 {
			t.Fatalf("取得に %v かかった。interval (%v) に律速されている", elapsed, interval)
		}
		t.Logf("interval=%v でも %v で取得", interval, elapsed)
	case <-time.After(interval / 3):
		t.Fatal("interval 待ちになっている (marker で起きていない)")
	}
}

// Shutdown が idlePollInterval にも worker 数にも比例しないこと。
//
// **待ち時間の実体は BZPOPMIN のタイムアウト。** 発行済みの読み取りは ctx
// キャンセルで中断できない。mkq v1.0.4 で Stop が marker key を突いて
// 待機中の dispatcher を起こすようになり、1 worker あたりの待ちが消えた
// (実測: interval 2 秒で 9ms)。それ以前は 1 worker あたり最大 interval で、
// 直列に止めると worker 数だけ積み上がっていた (interval 1 秒 / 既定
// worker 数で 4.5 秒 → 並列化で 0.6 秒)。
//
// 両方の退行を見る。片方だけなら劣化は小さいが、揃うと再起動が長引く。
func TestShutdown_DoesNotScaleWithWorkerCount(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	const interval = 2 * time.Second
	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:            redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:      8, // queue あたり複数 worker を確実に持たせる
		IdlePollInterval: interval,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	srv.Handle("noop", func(context.Context, driver.Task) error { return nil })
	require.NoError(t, srv.Start())
	// 全 worker が marker 待ちに入るまで待つ。
	time.Sleep(500 * time.Millisecond)

	start := time.Now()
	srv.Shutdown()
	elapsed := time.Since(start)

	// marker で起こせていれば ms 台。CI の負荷を見込んで interval の 1/2 を
	// 上限にする (実測 9ms なので 100 倍以上の余裕がある)。
	if elapsed > interval/2 {
		t.Fatalf("shutdown に %v かかった (interval=%v)。marker で起こせていないか worker を直列に止めている", elapsed, interval)
	}
	t.Logf("interval=%v / shutdown %v", interval, elapsed.Round(time.Millisecond))
}

// **scale-down が dispatcher の待ち時間に律速されないこと。**
//
// worker を縮めるとき、止める側の dispatcher は BZPOPMIN で park している。
// mkq は Stop で worker 専用の wake key を突いて起こすが、それが届かないと
// park の上限 (30 秒) まで待つ。逐次に止めていると本数分積み上がる。
//
// 実際に本番が起動不能になった経路: オートスケーラが inbox を 16 → 4 に
// 縮める過程で Stop が返らず、Server.Start が完了せず HTTP の listen まで
// 到達しなかった。当時は stopWorker が context.Background() を渡していたので
// 待ちが無制限だった (#2602)。
//
// delayed job を 1 件積むのは、dispatcher を最初から長い待ちに入らせるため。
// アイドルのバックオフが伸びるのを待たずに同じ条件を作れる。
func TestResize_ScaleDownDoesNotHang(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:       redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency: 8,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	srv.Handle("noop", func(context.Context, driver.Task) error { return nil })
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	// 遠い delayed job を 1 件だけ積む。テスト中に due にはならない。
	require.NoError(t, d.Client().Enqueue(context.Background(), "noop", []byte(`{}`),
		driver.WithQueue("deliver"), driver.WithProcessIn(time.Hour)))

	require.NoError(t, d.Resize("deliver", 8))
	// 全 dispatcher が BZPOPMIN に入るまで待つ。
	time.Sleep(2 * time.Second)

	start := time.Now()
	require.NoError(t, d.Resize("deliver", 2))
	elapsed := time.Since(start)

	// 起こせていれば ms 台。取りこぼすと 1 本あたり最大 30 秒かかる。
	if elapsed > 5*time.Second {
		t.Fatalf("scale-down に %v かかった。dispatcher を起こせていない", elapsed)
	}
	assert.Equal(t, 2, d.WorkerCount("deliver"))
	t.Logf("8 → 2 の scale-down が %v", elapsed.Round(time.Millisecond))
}

// **DispatchableCount が (pause していなければ) GetQueueInfo.Pending と一致すること。**
//
// autoscaler の読み取りを集計 API から切り替えた (#2605)。両者がずれると
// スケール判断が変わってしまうので、同じ値であることを固定する。
func TestDispatchableCount_MatchesQueueInfo(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis: redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	ins := d.Inspector()

	// 空のとき。
	n, err := ins.DispatchableCount("deliver")
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	// worker を起動していないので wait に積まれたまま残る。
	for range 5 {
		require.NoError(t, d.Client().Enqueue(context.Background(), "noop", []byte(`{}`),
			driver.WithQueue("deliver")))
	}
	// delayed も混ぜる。Pending には数えない。
	require.NoError(t, d.Client().Enqueue(context.Background(), "noop", []byte(`{}`),
		driver.WithQueue("deliver"), driver.WithProcessIn(time.Hour)))

	info, err := ins.GetQueueInfo("deliver")
	require.NoError(t, err)
	n, err = ins.DispatchableCount("deliver")
	require.NoError(t, err)

	assert.Equal(t, 5, n)
	assert.Equal(t, info.Pending, n, "DispatchableCount が GetQueueInfo.Pending とずれている")
}

// **今回登録しなかった cron の旧スケジューラを撤去する (#3173)。** #2818 で
// プラグインの cron を専用キューへ移したとき、maintenance 側の旧スケジューラが
// 残って二重実行と handler 無しの失敗を続けていた。
//
// 1 つ目の driver が「前回の起動」、2 つ目が「今回の起動」。Scheduler は
// driver ごとに登録の記録を持つので、別の driver で再現できる。
func TestScheduler_PruneUnregistered(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)
	ctx := context.Background()
	newD := func() driver.Driver {
		d, err := mkqdriver.New(ctx, mkqdriver.Config{
			Redis: redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close() })
		return d
	}
	repeat := func(queue string) []string {
		ids, err := testRedis.Client.ZRange(ctx, "bull:"+queue+":repeat", 0, -1).Result()
		require.NoError(t, err)
		return ids
	}

	// 前回の起動: 3 本登録していた。
	prev := newD().Scheduler()
	for _, id := range []string{"kept", "moved", "flaky"} {
		require.NoError(t, prev.Register("0 * * * *", id, nil, driver.WithQueue("maintenance")))
	}
	require.NoError(t, prev.Register("0 * * * *", "other-queue", nil, driver.WithQueue("export")))

	cur := newD().Scheduler()

	// **何も登録していないうちは何も消さない。**
	removed, err := cur.PruneUnregistered()
	require.NoError(t, err)
	assert.Empty(t, removed)
	assert.ElementsMatch(t, []string{"kept", "moved", "flaky"}, repeat("maintenance"))

	// 今回の起動: kept と flaky は登録し、moved と other-queue は登録しない。
	require.NoError(t, cur.Register("0 * * * *", "kept", nil, driver.WithQueue("maintenance")))
	require.NoError(t, cur.Register("0 * * * *", "flaky", nil, driver.WithQueue("maintenance")))

	removed, err = cur.PruneUnregistered()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"maintenance/moved", "export/other-queue"}, removed)
	assert.ElementsMatch(t, []string{"kept", "flaky"}, repeat("maintenance"))
	assert.Empty(t, repeat("export"))

	exists, err := testRedis.Client.Exists(ctx, "bull:maintenance:repeat:moved").Result()
	require.NoError(t, err)
	assert.Zero(t, exists, "template の HASH も消える (次を積まない)")
}

// **登録が 1 件でも失敗したら撤去しない。** 最初の失敗で return する呼び出し側
// (`RegisterChartJobs`) では、失敗の後ろの cron が「試みなかった」ことになる。
// そのまま消すと、一時的な失敗で正当な cron が再起動まで止まる。
func TestScheduler_PruneSkippedAfterRegistrationFailure(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)
	ctx := context.Background()
	newD := func() driver.Driver {
		d, err := mkqdriver.New(ctx, mkqdriver.Config{
			Redis: redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close() })
		return d
	}
	prev := newD().Scheduler()
	for _, id := range []string{"first", "after-failure"} {
		require.NoError(t, prev.Register("0 * * * *", id, nil, driver.WithQueue("maintenance")))
	}

	// 今回の起動: first の登録に失敗し、呼び出し側がそこで打ち切った。
	cur := newD().Scheduler()
	require.Error(t, cur.Register("not a cron", "first", nil, driver.WithQueue("maintenance")))

	removed, err := cur.PruneUnregistered()
	require.Error(t, err)
	assert.Empty(t, removed)
	ids, err := testRedis.Client.ZRange(ctx, "bull:maintenance:repeat", 0, -1).Result()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"first", "after-failure"}, ids)
}

// **scheduler の retention は template に載る (mkq v1.2.0 / #3171)。** 件数と
// 期限の両方を渡したときに completed / failed を取り違えず、両方が 1 つの
// object にまとまることを見る。件数 0 は渡らない (driver では「制限なし」)。
func TestScheduler_RetentionReachesTemplate(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis: redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	require.NoError(t, d.Scheduler().Register("0 * * * *", "retained", nil,
		driver.WithQueue("maintenance"),
		driver.WithKeepCompleted(30), driver.WithKeepCompletedAge(time.Hour),
		driver.WithKeepFailed(0), driver.WithKeepFailedAge(2*time.Hour),
	))

	raw, err := testRedis.Client.HGet(context.Background(), "bull:maintenance:repeat:retained", "opts").Result()
	require.NoError(t, err)
	var tmpl map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &tmpl))
	assert.Equal(t, map[string]any{"count": float64(30), "age": float64(3600)}, tmpl["removeOnComplete"])
	assert.Equal(t, map[string]any{"age": float64(7200)}, tmpl["removeOnFail"], "件数 0 は渡らない")
}

// **一覧は新しい順で返す (#3167)。** upstream の admin/queue/jobs は
// `queue.getJobs(types, 0, 100)` (asc=false) なので、wait は最後に積んだもの、
// delayed は発火予定が遅いものが先頭に来る。古い順だと、完了済みが数万件
// 溜まったキューで何か月も前の job しか見えなかった。
func TestInspectorLists_NewestFirst(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis: redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	ctx := context.Background()
	ins := d.Inspector()

	types := func(rows []*driver.TaskSummary) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Type)
		}
		return out
	}

	// worker を起動していないので wait に積まれたまま残る。
	for _, typ := range []string{"w1", "w2", "w3"} {
		require.NoError(t, d.Client().Enqueue(ctx, typ, []byte(`{}`), driver.WithQueue("deliver")))
	}
	pending, err := ins.ListPendingTasks("deliver", 1, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"w3", "w2", "w1"}, types(pending), "wait は最後に積んだものが先頭")

	// 積んだ順と発火予定の順をずらす。
	for _, j := range []struct {
		typ string
		in  time.Duration
	}{{"d2", 2 * time.Hour}, {"d3", 3 * time.Hour}, {"d1", time.Hour}} {
		require.NoError(t, d.Client().Enqueue(ctx, j.typ, []byte(`{}`),
			driver.WithQueue("deliver"), driver.WithProcessIn(j.in)))
	}
	delayed, err := ins.ListDelayedTasks("deliver", 1, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"d3", "d2", "d1"}, types(delayed), "delayed は発火予定が遅いものが先頭")

	scheduled, err := ins.ListScheduledTasks("deliver", 1, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"d3", "d2", "d1"}, types(scheduled), "Scheduled も同じ並び")

	// page は新しい側から切る。
	page2, err := ins.ListDelayedTasks("deliver", 2, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"d1"}, types(page2))
}

// **pause 中は 0 を返す (#3166)。** BullMQ 6 では pause してもジョブは wait に
// 残るので、Pending には backlog が見えたままになる。オートスケーラがそれを
// 深さと読むと、ジョブを取れないキューの worker を最大数まで増やす。
// 再開したら元の深さに戻ることも見る (0 を返し続けると再開後に増やせない)。
func TestDispatchableCount_ZeroWhilePaused(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis: redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	ins := d.Inspector()
	for range 3 {
		require.NoError(t, d.Client().Enqueue(context.Background(), "noop", []byte(`{}`),
			driver.WithQueue("deliver")))
	}

	require.NoError(t, ins.PauseQueue("deliver"))
	info, err := ins.GetQueueInfo("deliver")
	require.NoError(t, err)
	require.Equal(t, 3, info.Pending, "前提: pause 中もジョブは wait に残る (BullMQ 6)")

	n, err := ins.DispatchableCount("deliver")
	require.NoError(t, err)
	assert.Equal(t, 0, n, "pause 中は取れるジョブが無い")

	require.NoError(t, ins.UnpauseQueue("deliver"))
	n, err = ins.DispatchableCount("deliver")
	require.NoError(t, err)
	assert.Equal(t, 3, n, "再開後は wait の長さに戻る")
}

// 未知の queue はエラーにする。**0 を返さない。** autoscaler が 0 と
// 受け取ると「捌けている」と誤認して縮めにかかる。
func TestDispatchableCount_UnknownQueue(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis: redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	_, err = d.Inspector().DispatchableCount("does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown queue")
}

// TestServer_RateLimit_IsPerQueueNotPerWorker pins the scope of
// mkq.WithRateLimit.
//
// **worker 数を変えても実効レートは変わらない。** mkq のリミッタは
// `bull:<queue>:limiter` という **queue ごとに 1 本の Redis キー**を全 Worker が
// INCR する BullMQ 互換の実装で、Worker ごとに独立したバケットではない。
//
// これを取り違えると被害が両方向に出る。「per-Worker だから worker 数で割れ」と
// 案内すると operator は狙った値の 1/N を設定してしまい、配送が N 分の 1 に絞られる
// (#2640 で実際に doc をそう書きかけた)。逆に「合計 rl × N」と信じると、設定した
// つもりの上限より N 倍緩いと誤解する。
//
// 同じ rate / 同じ job 数を worker 1 と worker 16 で流し、所要時間が同程度に
// なることで固定する。
func TestServer_RateLimit_IsPerQueueNotPerWorker(t *testing.T) {
	testutil.SkipIfNoDocker(t)

	const (
		rate = 4
		jobs = 12
	)
	elapsed := map[int]time.Duration{}
	for _, workers := range []int{1, 16} {
		flushRedis(t)

		d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
			Redis:            redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
			Concurrency:      workers,
			QueueConcurrency: map[string]int{"deliver": workers},
			QueueRateLimits:  map[string]int{"deliver": rate},
		})
		require.NoError(t, err)

		srv := d.Server()
		var wg sync.WaitGroup
		wg.Add(jobs)
		srv.Handle("rlscope:tick", func(_ context.Context, _ driver.Task) error {
			wg.Done()
			return nil
		})
		require.NoError(t, srv.Start())

		start := time.Now()
		for range jobs {
			require.NoError(t, d.Client().Enqueue(context.Background(), "rlscope:tick", nil,
				driver.WithQueue("deliver")))
		}
		if !waitGroupTimeout(&wg, 30*time.Second) {
			srv.Shutdown()
			_ = d.Close()
			t.Fatalf("worker=%d: rate-limited handler did not complete within timeout", workers)
		}
		elapsed[workers] = time.Since(start)
		srv.Shutdown()
		require.NoError(t, d.Close())
	}

	// 12 job を 4/sec なら**約 2 秒**。最初の窓の 4 件は即時に流れるので
	// (jobs/rate - 1) 秒が目安で、jobs/rate = 3 秒ではない。per-Worker なら
	// worker 16 では 64/sec になり 0.1 秒を切る (実測 20ms)。
	// **flaky を避けるため下限だけを見る。** elapsed は負荷で伸びる方向にしか
	// 動かないので、下限割れは per-Worker 化の兆候に限られる。
	floor := time.Duration(float64(jobs-rate) / float64(rate) * 0.8 * float64(time.Second))
	for workers, d := range elapsed {
		assert.GreaterOrEqual(t, d, floor,
			"worker=%d: %s で drain した。per-queue のリミッタなら worker 数によらず "+
				"%s は超えるはずで、これより速いのは per-Worker になっている兆候",
			workers, d, floor)
	}
	t.Logf("worker=1: %s / worker=16: %s (per-queue なら同程度)", elapsed[1], elapsed[16])
}
