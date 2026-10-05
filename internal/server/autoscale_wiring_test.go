package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/queue/driver"
	queuemetrics "github.com/elythia-network/elythia/internal/queue/metrics"
	"github.com/elythia-network/elythia/internal/queue/runtimestats"
)

// scriptableDriver is a driver.Driver fake whose WorkerCount /
// Inspector.GetQueueInfo return canned values; Resize records calls
// and updates the worker map atomically so the controller's view
// stays consistent. Other Driver methods panic; tests should not
// exercise them.
type scriptableDriver struct {
	mu        sync.Mutex
	workers   map[string]int
	pending   map[string]int
	resizeErr error // if non-nil, Resize returns this for every call
	resizes   atomic.Int32

	insp *scriptableInspector

	// 呼び出し経路の検証用。autoscaler は集計 API (GetQueueInfo) ではなく
	// DispatchableCount を使うべき (#2605)。
	queueInfoCalls         atomic.Int64
	dispatchableCountCalls atomic.Int64
	// resizeCeiling caps what Resize actually applies (0 = uncapped).
	resizeCeiling int
}

func newScriptableDriver(initial map[string]int) *scriptableDriver {
	workers := map[string]int{}
	for k, v := range initial {
		workers[k] = v
	}
	d := &scriptableDriver{
		workers: workers,
		pending: map[string]int{},
	}
	d.insp = &scriptableInspector{parent: d}
	return d
}

func (d *scriptableDriver) Client() driver.Client {
	panic("scriptableDriver: Client() not implemented")
}
func (d *scriptableDriver) Server() driver.Server {
	panic("scriptableDriver: Server() not implemented")
}
func (d *scriptableDriver) Inspector() driver.Inspector { return d.insp }
func (d *scriptableDriver) Scheduler() driver.Scheduler {
	panic("scriptableDriver: Scheduler() not implemented")
}
func (d *scriptableDriver) Close() error { return nil }
func (d *scriptableDriver) WorkerCount(qname string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.workers[qname]
}
func (d *scriptableDriver) Resize(qname string, n int) error {
	d.resizes.Add(1)
	if d.resizeErr != nil {
		return d.resizeErr
	}
	d.mu.Lock()
	// mkq driver は総 worker 数の上限に当たると要求より少ない本数しか
	// 立てずに nil を返す (#2657)。その挙動を再現できるようにしておく。
	if d.resizeCeiling > 0 && n > d.resizeCeiling {
		n = d.resizeCeiling
	}
	d.workers[qname] = n
	d.mu.Unlock()
	return nil
}
func (d *scriptableDriver) setPending(qname string, n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending[qname] = n
}

type scriptableInspector struct {
	parent *scriptableDriver
}

func (i *scriptableInspector) Queues() ([]string, error) { return nil, nil }
func (i *scriptableInspector) GetQueueInfo(qname string) (*driver.InspectorInfo, error) {
	i.parent.mu.Lock()
	defer i.parent.mu.Unlock()
	i.parent.queueInfoCalls.Add(1)
	return &driver.InspectorInfo{Queue: qname, Pending: i.parent.pending[qname]}, nil
}

// DispatchableCount serves the same scripted depth as GetQueueInfo. **同じ値を
// 返させる。** autoscaler が読む経路はこちらに変わったので、ここがずれると
// 制御ループのテストが何も検証しなくなる。
func (i *scriptableInspector) DispatchableCount(qname string) (int, error) {
	i.parent.mu.Lock()
	defer i.parent.mu.Unlock()
	// 集計 API を経由していないことを見えるようにする。
	i.parent.dispatchableCountCalls.Add(1)
	return i.parent.pending[qname], nil
}

func (i *scriptableInspector) DeleteTask(qname, taskID string) error { return nil }
func (i *scriptableInspector) DeleteAllPendingTasks(qname string) (int, error) {
	return 0, nil
}
func (i *scriptableInspector) RunTask(qname, taskID string) error { return nil }
func (i *scriptableInspector) PauseQueue(qname string) error      { return nil }
func (i *scriptableInspector) UnpauseQueue(qname string) error    { return nil }
func (i *scriptableInspector) ListPendingTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *scriptableInspector) ListActiveTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *scriptableInspector) ListCompletedTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *scriptableInspector) ListFailedTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *scriptableInspector) ListScheduledTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *scriptableInspector) ListRetryTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *scriptableInspector) ListDelayedTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *scriptableInspector) GetTaskInfo(qname, taskID string) (*driver.TaskSummary, error) {
	return nil, nil
}
func (i *scriptableInspector) GetTaskLogs(string, string, int64, int64) ([]string, int64, error) {
	return nil, 0, nil
}

func (i *scriptableInspector) QueueMetrics(qname, kind string) (*driver.MetricsResult, error) {
	return nil, nil
}
func (i *scriptableInspector) Close() error { return nil }

// TestStartAutoScale_DisabledReturnsNil covers the no-op path: auto-scale
// flag false → runner nil, no goroutine, no Resize call.
func TestStartAutoScale_DisabledReturnsNil(t *testing.T) {
	cfg := &config.Config{JobQueueAutoScale: false}
	d := newScriptableDriver(nil)

	runner, err := startAutoScale(context.Background(), cfg, d, queuemetrics.New(), nil)
	require.NoError(t, err)
	assert.Nil(t, runner, "runner should be nil when auto-scale is off")
	assert.Equal(t, int32(0), d.resizes.Load(), "no Resize call when auto-scale is off")
}

// TestStartAutoScale_DeliverInboxKnobsOnly_OthersStillManaged verifies
// that when deliver / inbox have explicit JobConcurrency knobs set,
// they are excluded from controller management (per ADR §3.6 priority)
// while export / push / webhook continue to be managed (those queues
// have no per-queue knob in the current config schema, so they are
// always managed when jobQueueAutoScale=true).
//
// 注: 現状の config schema では「全 queue を knob で覆う」は不可能なため、
// "all knobs → nil runner" の経路は存在しない。将来 export/push/webhook
// に per-queue knob を生やしたら本テストを延長 / 別 test 化する。
func TestStartAutoScale_DeliverInboxKnobsOnly_OthersStillManaged(t *testing.T) {
	dc := 16
	ic := 8
	cfg := &config.Config{
		JobQueueAutoScale:     true,
		DeliverJobConcurrency: &dc,
		InboxJobConcurrency:   &ic,
	}
	d := newScriptableDriver(map[string]int{
		"deliver": dc, "inbox": ic, "export": 0, "push": 0, "webhook": 0,
	})

	runner, err := startAutoScale(context.Background(), cfg, d, queuemetrics.New(), nil)
	require.NoError(t, err)
	require.NotNil(t, runner, "runner should start for unmanaged queues (export/push/webhook)")
	t.Cleanup(func() { runner.Stop(context.Background()) })

	// export / push / webhook の 3 queue が controller 管理対象 → 初期
	// Resize で minWorkers=4 にされる。deliver / inbox は touch されない。
	deadline := time.After(2 * time.Second)
	for d.WorkerCount("export") != 4 {
		select {
		case <-deadline:
			t.Fatalf("export never reached min=4 (got %d)", d.WorkerCount("export"))
		case <-time.After(10 * time.Millisecond):
		}
	}
	assert.Equal(t, 4, d.WorkerCount("export"))
	assert.Equal(t, 4, d.WorkerCount("push"))
	assert.Equal(t, 4, d.WorkerCount("webhook"))
	assert.Equal(t, dc, d.WorkerCount("deliver"), "deliver should retain explicit knob value")
	assert.Equal(t, ic, d.WorkerCount("inbox"), "inbox should retain explicit knob value")
}

// TestStartAutoScale_ResizeNotSupportedDoesNotPreventStart pins that a
// driver which cannot resize no longer blocks startup (#2985)。
//
// 以前は ErrResizeNotSupported だけを起動エラーにしていたが、**その判定は
// production では一度も発火しない**。mkq driver があれを返すのは
// `Driver.Server()` を一度も呼んでいないときだけで、`newServer` は構築時に
// `queue.NewServer(queueDriver)` 経由で呼ぶため (実測)。asynq を消して
// 「resize できない driver」自体が無くなったので判定ごと外した。
//
// **述語を `err != nil` へ広げる形は採れない** — すぐ下の
// TestStartAutoScale_InitialResizeFailureDoesNotPreventStart が固定している
// とおり、Redis の瞬断で起動できなくなる。
func TestStartAutoScale_ResizeNotSupportedDoesNotPreventStart(t *testing.T) {
	cfg := &config.Config{JobQueueAutoScale: true}
	d := newScriptableDriver(nil)
	d.resizeErr = driver.ErrResizeNotSupported

	runner, err := startAutoScale(context.Background(), cfg, d, queuemetrics.New(), nil)
	require.NoError(t, err)
	require.NotNil(t, runner)
	t.Cleanup(func() { runner.Stop(context.Background()) })
}

// TestStartAutoScale_MinWorkersFloorExceedsGlobalCapRejected verifies the
// ADR §3.6 validation: min × len(queues) must fit inside maxWorkersGlobal.
func TestStartAutoScale_MinWorkersFloorExceedsGlobalCapRejected(t *testing.T) {
	min := 10
	cap := 30 // 5 queue × min=10 = 50 > 30
	cfg := &config.Config{
		JobQueueAutoScale: true,
		MinWorkers:        &min,
		MaxWorkersGlobal:  &cap,
	}
	d := newScriptableDriver(nil)

	_, err := startAutoScale(context.Background(), cfg, d, queuemetrics.New(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds maxWorkersGlobal")
}

// TestStartAutoScale_TickerScalesUpOnBacklog verifies the end-to-end
// ticker loop: high queue depth → controller decides scale-up → driver
// resized → metric incremented.
func TestStartAutoScale_TickerScalesUpOnBacklog(t *testing.T) {
	// minWorkers=2 / maxWorkers=64 / cooldown=1s (default) で起動。
	min := 2
	maxW := 64
	cooldown := 1
	cfg := &config.Config{
		JobQueueAutoScale:        true,
		MinWorkers:               &min,
		MaxWorkers:               &maxW,
		AutoScaleCooldownSeconds: &cooldown,
	}
	d := newScriptableDriver(nil)
	m := queuemetrics.New()

	runner, err := startAutoScale(context.Background(), cfg, d, m, nil)
	require.NoError(t, err)
	require.NotNil(t, runner)
	t.Cleanup(func() { runner.Stop(context.Background()) })

	// Initial Resize で全 queue が min=2 になっている。
	deadline := time.After(2 * time.Second)
	for d.WorkerCount("deliver") != 2 {
		select {
		case <-deadline:
			t.Fatalf("deliver never reached min=2")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Queue depth を高くする (2 × 4 = 8 越えで scale-up trigger)。
	d.setPending("deliver", 100)

	// Ticker は 1Hz、最低 1 cycle 待って scale-up を観測。
	deadline = time.After(5 * time.Second)
	for d.WorkerCount("deliver") <= 2 {
		select {
		case <-deadline:
			t.Fatalf("deliver never scaled up (got %d)", d.WorkerCount("deliver"))
		case <-time.After(50 * time.Millisecond):
		}
	}
	// scale-up event counter が立った
	assert.Greater(t, testutil.ToFloat64(m.ScaleEventsTotal.WithLabelValues("deliver", "up")), 0.0,
		"scale-up metric should be incremented")
}

// TestStartAutoScale_GlobalCapBlocksScaleUp verifies that
// maxWorkersGlobal limits aggregate worker count even when individual
// queues' upThreshold is exceeded.
func TestStartAutoScale_GlobalCapBlocksScaleUp(t *testing.T) {
	min := 2
	maxW := 64
	cooldown := 1
	cfg := &config.Config{
		JobQueueAutoScale:        true,
		MinWorkers:               &min,
		MaxWorkers:               &maxW,
		AutoScaleCooldownSeconds: &cooldown,
	}
	// cap は auto-scale 対象 queue 数から導出する。min × queue 数が floor に
	// なるので、そこに headroom 2 を足した値を cap にすると「起動はできるが
	// ほぼ伸ばせない」状態を作れる。queue を増減しても追随するよう、数を
	// ハードコードしない (#2403 で 6→7 になった際に固定値が破綻した)。
	managed, _ := autoScaledQueues(cfg)
	require.NotEmpty(t, managed)
	capacity := min*len(managed) + 2
	cfg.MaxWorkersGlobal = &capacity
	d := newScriptableDriver(nil)
	m := queuemetrics.New()

	runner, err := startAutoScale(context.Background(), cfg, d, m, nil)
	require.NoError(t, err)
	require.NotNil(t, runner)
	t.Cleanup(func() { runner.Stop(context.Background()) })

	// すべての queue を高 depth に → ticker が全 queue でスケールしたがる
	for _, q := range managed {
		d.setPending(q, 100)
	}

	// しばらく走らせて、合計 worker が cap を超えないことを assert。
	// 集計対象も managed から引く。旧実装は 5 queue 固定で objectStorage を
	// 数え落としており、合計を過小評価していた。
	time.Sleep(3 * time.Second)
	total := 0
	for _, q := range managed {
		total += d.WorkerCount(q)
	}
	assert.LessOrEqual(t, total, capacity, "total workers must stay within maxWorkersGlobal=%d (got %d)", capacity, total)
}

// TestAutoscaleRunner_Stop verifies that Stop blocks until every
// ticker goroutine has exited (= no goroutine leak).
func TestAutoscaleRunner_Stop(t *testing.T) {
	cfg := &config.Config{JobQueueAutoScale: true}
	d := newScriptableDriver(nil)

	runner, err := startAutoScale(context.Background(), cfg, d, queuemetrics.New(), nil)
	require.NoError(t, err)
	require.NotNil(t, runner)

	done := make(chan struct{})
	go func() {
		runner.Stop(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return within 3 seconds (goroutine leak suspected)")
	}

	// 二度 Stop しても panic / hang しない
	assert.NotPanics(t, func() { runner.Stop(context.Background()) })
}

// TestAutoscaleRunner_Stop_RespectsDeadline verifies that Stop returns
// when the supplied ctx expires, even if internal goroutines are slow
// to exit. Uses a 1ns-deadline ctx to force the timeout path.
func TestAutoscaleRunner_Stop_RespectsDeadline(t *testing.T) {
	cfg := &config.Config{JobQueueAutoScale: true}
	d := newScriptableDriver(nil)

	runner, err := startAutoScale(context.Background(), cfg, d, queuemetrics.New(), nil)
	require.NoError(t, err)
	require.NotNil(t, runner)

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	start := time.Now()
	runner.Stop(ctx)
	elapsed := time.Since(start)
	// 即座に return (= ctx.Done 経路) すること。goroutine join 待ち
	// (= 1Hz ticker) を待たないので 100ms 以下。
	assert.Less(t, elapsed, 100*time.Millisecond,
		"Stop should return immediately on ctx deadline (elapsed=%v)", elapsed)

	// Clean up the leaked goroutines so the test process exits cleanly.
	runner.Stop(context.Background())
}

// TestStartAutoScale_NilRunnerStopIsNoop ensures (*autoscaleRunner)(nil).Stop()
// is safe — relevant when Shutdown runs before Start finished, or when
// JobQueueAutoScale=false (returned runner is nil).
func TestStartAutoScale_NilRunnerStopIsNoop(t *testing.T) {
	var r *autoscaleRunner
	assert.NotPanics(t, func() { r.Stop(context.Background()) })
}

// panickingDriver wraps scriptableDriver and panics from Resize after
// `panicAfter` calls so we can test the tick goroutine's recover().
type panickingDriver struct {
	*scriptableDriver
	panicAfter atomic.Int32
}

func (p *panickingDriver) Resize(qname string, n int) error {
	if p.panicAfter.Add(-1) < 0 {
		panic("test-induced resize panic")
	}
	return p.scriptableDriver.Resize(qname, n)
}

// TestStartAutoScale_TickRecoversFromPanic verifies that a panic from
// drv.Resize inside the per-queue tick goroutine is recovered (logged
// at Error level) rather than crashing the process. wg.Done still runs
// via defer so Stop() does not hang waiting for the leaked goroutine.
func TestStartAutoScale_TickRecoversFromPanic(t *testing.T) {
	cfg := &config.Config{JobQueueAutoScale: true}
	inner := newScriptableDriver(nil)
	inner.setPending("deliver", 1000) // 高 depth で scale-up trigger
	d := &panickingDriver{scriptableDriver: inner}
	// 初回 + 1 tick で発火する Resize までは success、その次の Resize で panic。
	// 初期 Resize × queue 数 + 1 tick の Resize が panic 発火タイミング。
	// queue を増減しても追随するよう autoScaledQueues から導出する。
	managed, _ := autoScaledQueues(cfg)
	require.NotEmpty(t, managed)
	d.panicAfter.Store(int32(len(managed) + 1))

	runner, err := startAutoScale(context.Background(), cfg, d, queuemetrics.New(), nil)
	require.NoError(t, err)
	require.NotNil(t, runner)

	// panic で deliver の tick goroutine が死ぬまで待ち、Stop が hang
	// しない (= deferred wg.Done が recover 経由でも走る) ことを確認。
	time.Sleep(2 * time.Second)

	done := make(chan struct{})
	go func() {
		runner.Stop(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung after panic; wg.Done likely missed in recover path")
	}
}

// TestStartAutoScale_InitialResizeFailureDoesNotPreventStart verifies
// that a transient Resize failure during initial setup is logged but
// allowed (controller retries on next tick), so a temporary Redis
// hiccup at startup does not block server boot.
func TestStartAutoScale_InitialResizeFailureDoesNotPreventStart(t *testing.T) {
	cfg := &config.Config{JobQueueAutoScale: true}
	d := newScriptableDriver(nil)
	// First few Resize calls fail; later ones succeed (= transient failure).
	// scriptableDriver.resizeErr is sticky, so we test the simpler case:
	// permanent failure — wiring must still return without panicking.
	d.resizeErr = errors.New("temporary redis hiccup")
	defer func() { d.resizeErr = nil }()

	runner, err := startAutoScale(context.Background(), cfg, d, queuemetrics.New(), nil)
	// startup-time validation passes: Resize failure is logged and tolerated
	// so a Redis hiccup cannot make the process unbootable.
	require.NoError(t, err)
	require.NotNil(t, runner)
	t.Cleanup(func() { runner.Stop(context.Background()) })
}

// scale-up が起きたとき、Prometheus counter だけでなく runtimestats の履歴にも
// 記録されること (#2277)。counter は「何回起きたか」しか持たないので、
// admin UI が「いつ何本から何本へ動いたか」を出すにはこちらが要る。
func TestStartAutoScale_RecordsScaleEventsForAdminUI(t *testing.T) {
	minW := 2
	maxW := 64
	cooldown := 1
	cfg := &config.Config{
		JobQueueAutoScale:        true,
		MinWorkers:               &minW,
		MaxWorkers:               &maxW,
		AutoScaleCooldownSeconds: &cooldown,
	}
	d := newScriptableDriver(nil)
	stats := runtimestats.New()

	runner, err := startAutoScale(context.Background(), cfg, d, queuemetrics.New(), stats)
	require.NoError(t, err)
	require.NotNil(t, runner)
	t.Cleanup(func() { runner.Stop(context.Background()) })

	// 初期 Resize で min まで上がるのを待つ。
	deadline := time.After(2 * time.Second)
	for d.WorkerCount("deliver") != minW {
		select {
		case <-deadline:
			t.Fatalf("deliver never reached min=%d", minW)
		case <-time.After(10 * time.Millisecond):
		}
	}

	// depth を閾値 (workers × 4) 超えにして scale-up を誘発。
	d.setPending("deliver", 100)

	deadline = time.After(5 * time.Second)
	for len(stats.Snapshot("deliver").ScaleEvents) == 0 {
		select {
		case <-deadline:
			t.Fatalf("scale event was never recorded (workers=%d)", d.WorkerCount("deliver"))
		case <-time.After(50 * time.Millisecond):
		}
	}

	ev := stats.Snapshot("deliver").ScaleEvents[0]
	assert.Equal(t, "deliver", ev.Queue)
	assert.Equal(t, "up", ev.Direction)
	assert.Greater(t, ev.To, ev.From, "scale-up は worker 数が増える方向")
	assert.False(t, ev.At.IsZero(), "発生時刻が入っていること")

	// 他 queue は depth 0 のままなので履歴も空 (誤って全 queue に記録しない)。
	assert.Empty(t, stats.Snapshot("inbox").ScaleEvents)
}

// **オートスケーラが admin 集計 API を叩かないこと。**
//
// ticker は 1Hz x 管理キュー数で回る。`GetQueueInfo` は全カウント +
// repeat の ZCARD + delayed 全件の ZRANGE + N x HGETALL + paused 判定を
// 1 回で引くので、毎秒引くとアイドル時の Redis コマンドの大半をこれが
// 占める (本番実測で毎秒 90 前後、全体の 6 割)。使うのは Pending 1 個
// だけなので、キューあたり 1 コマンドで足りる (#2605)。
//
// delayed が federation 障害で数千件に膨らむと GetQueueInfo のコストも
// それに比例するので、常時経路から外しておく意味は平常時より大きい。
func TestStartAutoScale_UsesDispatchableCountNotQueueInfo(t *testing.T) {
	cfg := &config.Config{JobQueueAutoScale: true}
	d := newScriptableDriver(map[string]int{"export": 4})

	runner, err := startAutoScale(context.Background(), cfg, d, queuemetrics.New(), nil)
	require.NoError(t, err)
	require.NotNil(t, runner)
	t.Cleanup(func() { runner.Stop(context.Background()) })

	// tick を数回踏ませる。
	deadline := time.After(5 * time.Second)
	for d.dispatchableCountCalls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("DispatchableCount が呼ばれていない (%d 回)", d.dispatchableCountCalls.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}

	assert.Zerof(t, d.queueInfoCalls.Load(),
		"GetQueueInfo が %d 回呼ばれている。集計 API を毎秒引いている", d.queueInfoCalls.Load())
}

// TestStartAutoScale_CappedResizeCommitsTheAppliedCount pins the read-back
// after Resize (#2657).
//
// mkq driver は総 worker 数の上限に当たると要求より少ない本数しか立てずに
// nil を返す。target を台帳に書いたまま抜けると、達成できなかった値で
// maxWorkersGlobal の合計を見積もり、他 queue の scale-up を無期限に
// 詰まらせる。ログにも「0 -> 4 に resize した」と毎秒書き続けることになる。
func TestStartAutoScale_CappedResizeCommitsTheAppliedCount(t *testing.T) {
	minWorkers := 2
	maxW := 64
	cooldown := 1
	cfg := &config.Config{
		JobQueueAutoScale:        true,
		MinWorkers:               &minWorkers,
		MaxWorkers:               &maxW,
		AutoScaleCooldownSeconds: &cooldown,
	}
	d := newScriptableDriver(nil)
	d.resizeCeiling = 2 // 何を要求されても 2 本しか立たない
	m := queuemetrics.New()

	runner, err := startAutoScale(context.Background(), cfg, d, m, nil)
	require.NoError(t, err)
	require.NotNil(t, runner)
	t.Cleanup(func() { runner.Stop(context.Background()) })

	// **起動時の Resize を数えない。** startAutoScale は probe 1 回 +
	// 管理キューぶんの初期化で既に 8 回呼んでいるので、単純な下限で待つと
	// tick が 1 度も回らないうちに先へ進んでしまう。
	afterStartup := d.resizes.Load()
	require.Positive(t, afterStartup)

	d.setPending("deliver", 100) // 2 x 4 = 8 を超えるので scale-up が走る

	// tick 由来の Resize が実際に走るまで待つ。
	deadline := time.After(5 * time.Second)
	for d.resizes.Load() < afterStartup+2 {
		select {
		case <-deadline:
			t.Fatalf("ticker never issued a resize (resizes=%d, afterStartup=%d)",
				d.resizes.Load(), afterStartup)
		case <-time.After(20 * time.Millisecond):
		}
	}

	runner.mu.Lock()
	ledger := runner.workerCounts["deliver"]
	runner.mu.Unlock()
	assert.Equal(t, 2, ledger,
		"the ledger must hold what was applied, not the unachieved target")
	assert.Equal(t, 0.0,
		testutil.ToFloat64(m.ScaleEventsTotal.WithLabelValues("deliver", "up")),
		"a resize that changed nothing is not a scale event")
}
