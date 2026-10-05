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

// wedgeFixture wires a driver whose deliver queue runs a single worker
// and whose handler can be made to never return, which is what a job
// blocked on an unbounded network call looks like from the pool's side.
type wedgeFixture struct {
	drv  *mkqdriver.Driver
	srv  driver.Server
	mkqs *mkqdriver.Server

	// block gates the "wedge" task type. Closing it lets every wedged
	// handler return.
	block chan struct{}
	// entered counts handler entries for the wedge task type.
	entered atomic.Int32
	// fast counts completions of the non-blocking task type.
	fast atomic.Int32
	// chain counts completions of the short-but-not-instant task type
	// used to keep a queue saturated.
	chain atomic.Int32
}

func newWedgeFixture(t *testing.T, workers int, stuckAfter, supervise time.Duration) *wedgeFixture {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:             redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:       4,
		QueueConcurrency:  map[string]int{"deliver": workers},
		StuckWorkerAfter:  stuckAfter,
		SuperviseInterval: supervise,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	f := &wedgeFixture{drv: d, srv: d.Server(), block: make(chan struct{})}
	mkqs, ok := f.srv.(*mkqdriver.Server)
	require.True(t, ok, "mkqdriver.Driver must hand out its own Server type")
	f.mkqs = mkqs

	f.srv.Handle("wedge", func(ctx context.Context, _ driver.Task) error {
		f.entered.Add(1)
		// ctx を**見ない**。mkq の heartbeat が lock 延長に失敗して job ctx
		// をキャンセルしても戻ってこない handler、が本番で起きた状態。
		<-f.block
		return nil
	})
	f.srv.Handle("fast", func(context.Context, driver.Task) error {
		f.fast.Add(1)
		return nil
	})
	f.srv.Handle("chain", func(context.Context, driver.Task) error {
		// **長い job を連鎖させる。** handler を抜けてから次の handler に
		// 入るまでの idle は finalise の Redis 往復ぶん (1ms 未満) しかない。
		// job を長くすると、その窓が supervisor の周期に対して十分小さくなり、
		// 「たまたま idle を観測できた」で通ってしまう余地が消える。
		time.Sleep(800 * time.Millisecond)
		f.chain.Add(1)
		return nil
	})
	require.NoError(t, f.srv.Start())
	t.Cleanup(f.srv.Shutdown)
	t.Cleanup(f.release)
	return f
}

// release lets every blocked "wedge" handler return. Safe to call twice.
func (f *wedgeFixture) release() {
	select {
	case <-f.block:
	default:
		close(f.block)
	}
}

func (f *wedgeFixture) enqueue(t *testing.T, taskType string) {
	t.Helper()
	require.NoError(t, f.drv.Client().Enqueue(
		context.Background(), taskType, nil, driver.WithQueue("deliver")))
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, within time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition never held within %v: %s", within, msg)
}

// TestSupervisor_ReplacesWedgedWorkerAndKeepsDraining is the regression
// test for #2657: the single worker wedges inside its handler, and the
// queue must keep draining anyway.
func TestSupervisor_ReplacesWedgedWorkerAndKeepsDraining(t *testing.T) {
	f := newWedgeFixture(t, 1, 300*time.Millisecond, 100*time.Millisecond)

	f.enqueue(t, "wedge")
	eventually(t, 5*time.Second, "handler never entered", func() bool {
		return f.entered.Load() >= 1
	})

	// 詰まった worker は生存数に入らない。
	eventually(t, 5*time.Second, "wedged worker still counted as live", func() bool {
		return f.mkqs.QuarantinedWorkerCount("deliver") == 1
	})
	assert.Equal(t, 1, f.drv.WorkerCount("deliver"),
		"the roster is restored to its configured size by the supervisor")

	// 差し替えられた worker が仕事を続けられること。これが直前まで
	// 起きていなかったこと (marker が 65 秒 pop されない) が本番の症状。
	f.enqueue(t, "fast")
	eventually(t, 5*time.Second, "replacement worker never picked up the next job", func() bool {
		return f.fast.Load() >= 1
	})
}

// TestSupervisor_ReinstatesWorkerThatWasMerelySlow covers the false
// positive: quarantine must not cancel the in-flight job, so a handler
// that was only slow still completes, and its worker rejoins the pool.
func TestSupervisor_ReinstatesWorkerThatWasMerelySlow(t *testing.T) {
	f := newWedgeFixture(t, 1, 300*time.Millisecond, 100*time.Millisecond)

	f.enqueue(t, "wedge")
	eventually(t, 5*time.Second, "slow worker was never quarantined", func() bool {
		return f.mkqs.QuarantinedWorkerCount("deliver") == 1
	})

	// 隔離されただけで job は生きている。handler を返せば正常完了する。
	f.release()

	eventually(t, 5*time.Second, "recovered worker was never reinstated", func() bool {
		return f.mkqs.QuarantinedWorkerCount("deliver") == 0
	})
	assert.Equal(t, 1, f.drv.WorkerCount("deliver"))

	// cancel されていないので retry に回らず、キューは空のまま。
	pending, err := f.drv.Inspector().DispatchableCount("deliver")
	require.NoError(t, err)
	assert.Equal(t, 0, pending)

	// 差し替え後の worker も普通に動く。
	f.enqueue(t, "fast")
	eventually(t, 5*time.Second, "replacement worker never picked up the next job", func() bool {
		return f.fast.Load() >= 1
	})
}

// TestResize_DoesNotEvictTheOnlyHealthyWorker reproduces the scale-down
// half of #2657: with wedged workers on the roster, shrinking the pool
// must not take the freshly added healthy worker.
//
// 実際に効いているのは reconcile の順序 (adjustLocked より先に quarantine
// へ退避する) と生存数の勘定で、victim 選択そのものはここには来ない
// (来る頃には roster に詰まりが残っていない)。選択順序の固定は
// TestSplitForRemoval_* が持つ。
func TestResize_DoesNotEvictTheOnlyHealthyWorker(t *testing.T) {
	// supervisor は止めておく (負の interval)。ここで見たいのは Resize
	// 単体の選択順序で、supervisor が先に回収してしまうと検証にならない。
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:             redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:       4,
		QueueConcurrency:  map[string]int{"deliver": 2},
		StuckWorkerAfter:  300 * time.Millisecond,
		SuperviseInterval: -1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	block := make(chan struct{})
	var entered, fast atomic.Int32
	srv.Handle("wedge", func(context.Context, driver.Task) error {
		entered.Add(1)
		<-block
		return nil
	})
	srv.Handle("fast", func(context.Context, driver.Task) error {
		fast.Add(1)
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)
	// Cleanup は LIFO なので、これは Shutdown より**先**に走る。詰まった
	// まま Shutdown すると Worker.Stop が stopWorkerTimeout (30 秒) 待つ。
	t.Cleanup(func() {
		select {
		case <-block:
		default:
			close(block)
		}
	})

	// 2 本とも handler に入れて詰まらせる。
	for range 2 {
		require.NoError(t, d.Client().Enqueue(context.Background(), "wedge", nil,
			driver.WithQueue("deliver")))
	}
	eventually(t, 5*time.Second, "both workers never entered the handler", func() bool {
		return entered.Load() >= 2
	})

	// 閾値を超えるまで待つ。以降 WorkerCount は生存数 0 を返す。
	eventually(t, 5*time.Second, "wedged workers still counted", func() bool {
		return d.WorkerCount("deliver") == 0
	})

	// autoscale が 3 本目を足したのと同じ状況を作る。
	require.NoError(t, d.Resize("deliver", 1))
	assert.Equal(t, 1, d.WorkerCount("deliver"))
	mkqs, ok := srv.(*mkqdriver.Server)
	require.True(t, ok)
	assert.Equal(t, 2, mkqs.QuarantinedWorkerCount("deliver"),
		"both wedged workers leave the roster instead of the healthy one")

	// 残った 1 本は健全なので仕事ができる。
	require.NoError(t, d.Client().Enqueue(context.Background(), "fast", nil,
		driver.WithQueue("deliver")))
	eventually(t, 5*time.Second, "surviving worker could not process a job", func() bool {
		return fast.Load() >= 1
	})
}

// TestSupervisor_ReinstatesWorkerOnABusyQueue is the regression test for
// the sampling trap: mkq hands a worker its next job through the
// moveToFinished prefetch path, so on a queue that always has work the
// idle window between jobs is microseconds wide. Releasing quarantine on
// "is it idle right now" would therefore only fire once the backlog
// drains, and on a queue that never drains it would never fire at all —
// healthy workers would pile up in quarantine until the pool stopped
// replacing them.
//
// **キューを本当に飽和させることがこのテストの肝。** 空になる隙がある
// キューだと idle 判定でもたまたま通ってしまい、fix を戻しても緑になる。
func TestSupervisor_ReinstatesWorkerOnABusyQueue(t *testing.T) {
	// **閾値は chain job (800ms) より長く取る。** 短いと chain job のたびに
	// 正当な隔離が起き続け、roster と quarantine が定常的に揺れて
	// 「復帰したか」を安定して観測できない (実測で 10% flake)。
	f := newWedgeFixture(t, 1, 1500*time.Millisecond, 100*time.Millisecond)

	// 1 本目で閾値を超えさせる。
	f.enqueue(t, "wedge")
	eventually(t, 5*time.Second, "slow worker was never quarantined", func() bool {
		return f.mkqs.QuarantinedWorkerCount("deliver") == 1
	})

	// 差し替え後の worker が捌ききれない量の backlog を先に積む。
	// 1 件 800ms なので worker 2 本でも 8 秒かかる。
	for range 20 {
		require.NoError(t, f.drv.Client().Enqueue(context.Background(), "chain", nil,
			driver.WithQueue("deliver")))
	}
	eventually(t, 5*time.Second, "backlog never started draining", func() bool {
		pending, err := f.drv.Inspector().DispatchableCount("deliver")
		return err == nil && pending > 0
	})

	// 解放された worker は prefetch で次の長い job を掴むので idle にならない。
	f.release()
	releasedAt := time.Now()

	var pendingAtReinstate, rosterAtReinstate int
	eventually(t, 10*time.Second, "worker stayed quarantined on a saturated queue", func() bool {
		if f.mkqs.QuarantinedWorkerCount("deliver") != 0 {
			return false
		}
		// 同じ観測点で揃えて読む。別々に読むと、間に reconcile が挟まった
		// ときに「隔離ゼロだが roster はもう畳まれた後」を見てしまう。
		pendingAtReinstate, _ = f.drv.Inspector().DispatchableCount("deliver")
		rosterAtReinstate = f.drv.WorkerCount("deliver")
		return true
	})

	// 完了カウンタで判定していれば、解放の次の tick で戻る。idle 判定だと
	// 800ms の job を挟むので、この窓には入れない。
	assert.Less(t, time.Since(releasedAt), 500*time.Millisecond,
		"reinstatement must follow the completion, not a lucky idle sample")
	assert.Positive(t, pendingAtReinstate,
		"the queue must still be backed up at the moment of reinstatement")

	// 戻した worker は原則そのまま残る (job を持っているので庇われる) が、
	// **復帰の瞬間に idle だった場合は同じ reconcile で畳まれてよい**。
	// prefetch の合間の idle は実測で平均 1ms 弱あり、100ms 周期の tick が
	// そこに当たる確率が 1% ほどある。ここを Equal(2) で固定すると PR
	// 必須シャードで低頻度に落ちるので、決定的な性質は
	// TestPool_ReinstateDoesNotCancelJobs に持たせて、ここは範囲で見る。
	assert.Contains(t, []int{1, 2}, rosterAtReinstate)

	// job を 1 件終えれば庇いが外れ、余剰は次の reconcile で畳まれる。
	eventually(t, 30*time.Second, "surplus worker was never reclaimed", func() bool {
		return f.drv.WorkerCount("deliver") == 1 &&
			f.mkqs.QuarantinedWorkerCount("deliver") == 0
	})
}

// TestSupervisor_DoesNotTrackBatchQueues pins the per-queue table: export
// pages for minutes by design, so a long handler there must not be pulled
// out of the pool (which would raise effective concurrency for as long as
// the batch runs).
func TestSupervisor_DoesNotTrackBatchQueues(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:             redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:       4,
		QueueConcurrency:  map[string]int{"export": 1},
		SuperviseInterval: 50 * time.Millisecond,
		// StuckWorkerAfter は既定 (= キューごとの表) のまま。
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	block := make(chan struct{})
	var entered atomic.Int32
	srv.Handle("slow-batch", func(context.Context, driver.Task) error {
		entered.Add(1)
		<-block
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)
	t.Cleanup(func() { close(block) })

	require.NoError(t, d.Client().Enqueue(context.Background(), "slow-batch", nil,
		driver.WithQueue("export")))
	eventually(t, 5*time.Second, "batch handler never started", func() bool {
		return entered.Load() >= 1
	})

	// inbox の閾値 (30 分) より遥かに短い時間しか待たないが、export は
	// そもそも追跡対象外なので何秒待っても隔離されない。
	time.Sleep(500 * time.Millisecond)
	mkqs, ok := srv.(*mkqdriver.Server)
	require.True(t, ok)
	assert.Equal(t, 0, mkqs.QuarantinedWorkerCount("export"))
	assert.Equal(t, 1, d.WorkerCount("export"),
		"a long batch job must not shrink the reported worker count")
}
