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

// TestHandlerDeadline_WorkerSurvivesAHandlerThatNeverReturns is the
// regression test for #2658.
//
// mkq の dispatchLoop は handler を同期で呼ぶので、戻らなければその worker は
// 二度と awaitMarker に到達しない。本番の inbox はこれで 4 本すべてを失った。
// 期限を切って dispatcher を返せば、**同じ worker が次の job を処理できる**。
func TestHandlerDeadline_WorkerSurvivesAHandlerThatNeverReturns(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:            redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:      4,
		QueueConcurrency: map[string]int{"deliver": 1},
		HandlerDeadline:  300 * time.Millisecond,
		// 詰まり検出 (#2657) は切っておく。ここで見たいのは期限だけで、
		// 隔離が先に走ると「差し替えられた worker が処理した」のか
		// 「同じ worker が復帰した」のか区別できない。
		StuckWorkerAfter: -1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	var wedged, fast atomic.Int32

	// ctx を一切見ない handler。#2657 の本番で起きていた状態。
	srv.Handle("wedge", func(context.Context, driver.Task) error {
		wedged.Add(1)
		<-block
		return nil
	})
	srv.Handle("fast", func(context.Context, driver.Task) error {
		fast.Add(1)
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)

	require.NoError(t, d.Client().Enqueue(context.Background(), "wedge", nil,
		driver.WithQueue("deliver")))
	eventually(t, 5*time.Second, "wedge handler never started", func() bool {
		return wedged.Load() >= 1
	})

	// **Driver 経由で読む。** metrics collector が使うのはこちらで、
	// Server の同名メソッドだけを叩いていると Driver 側の委譲が落ちても
	// 気付けない (gauge が常時 0 になる)。
	eventually(t, 5*time.Second, "the dispatcher never gave up on the handler", func() bool {
		cur, total := d.AbandonedHandlerCount("deliver")
		return cur == 1 && total == 1
	})

	// **worker は 1 本しか無い。** 隔離も切ってあるので、これが通るのは
	// 詰まった handler を放棄した同じ worker が戻ってきた場合だけ。
	require.NoError(t, d.Client().Enqueue(context.Background(), "fast", nil,
		driver.WithQueue("deliver")))
	eventually(t, 5*time.Second, "the worker was lost to the wedged handler", func() bool {
		return fast.Load() >= 1
	})
	assert.Equal(t, 1, d.WorkerCount("deliver"))
}

// 期限を無効にすると従来どおり worker が失われる。**期限が効いていることの
// 対照**で、上のテストが「たまたま通っている」のではないことを示す。
func TestHandlerDeadline_DisabledLosesTheWorker(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:            redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:      4,
		QueueConcurrency: map[string]int{"deliver": 1},
		HandlerDeadline:  -1,
		StuckWorkerAfter: -1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	block := make(chan struct{})
	var wedged, fast atomic.Int32
	srv.Handle("wedge", func(context.Context, driver.Task) error {
		wedged.Add(1)
		<-block
		return nil
	})
	srv.Handle("fast", func(context.Context, driver.Task) error {
		fast.Add(1)
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)
	// Cleanup は LIFO。Shutdown より先に handler を解放しないと
	// Worker.Stop が stopWorkerTimeout まで待つ。
	t.Cleanup(func() { close(block) })

	require.NoError(t, d.Client().Enqueue(context.Background(), "wedge", nil,
		driver.WithQueue("deliver")))
	eventually(t, 5*time.Second, "wedge handler never started", func() bool {
		return wedged.Load() >= 1
	})

	require.NoError(t, d.Client().Enqueue(context.Background(), "fast", nil,
		driver.WithQueue("deliver")))
	time.Sleep(time.Second)
	assert.Zero(t, fast.Load(), "with no deadline the only worker stays lost")

	cur, total := d.AbandonedHandlerCount("deliver")
	assert.Zero(t, cur)
	assert.Zero(t, total)
}

// TestHandlerDeadline_LeakWindsDownUnderASystemicWedge measures the whole
// loop against real mkq: quarantine -> deadline -> abandon -> reinstate.
//
// **数えるだけでは止まらない。** 放棄ぶんを予算に足しても、復帰した worker が
// 庇われて victim 候補から外れるため roster が縮まず、goroutine が deadline ごとに
// 増え続ける。ここでは「放棄の累計が頭打ちになる」ことを見る。
func TestHandlerDeadline_LeakWindsDownUnderASystemicWedge(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:             redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:       4,
		QueueConcurrency:  map[string]int{"deliver": 2},
		HandlerDeadline:   200 * time.Millisecond,
		StuckWorkerAfter:  150 * time.Millisecond,
		SuperviseInterval: 50 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	block := make(chan struct{})
	srv.Handle("wedge", func(context.Context, driver.Task) error {
		<-block // ctx を見ない = 本番の deliver / inbox と同じ形
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)
	t.Cleanup(func() { close(block) })

	// 全部が詰まる job を流し続ける。
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = d.Client().Enqueue(context.Background(), "wedge", nil, driver.WithQueue("deliver"))
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// 予算 = desired 2 + max(2,4) 4 = 6。累計はそこで頭打ちになるはず。
	deadline := time.After(12 * time.Second)
	var last uint64
	stable := 0
	for stable < 8 {
		select {
		case <-deadline:
			_, total := d.AbandonedHandlerCount("deliver")
			t.Fatalf("abandonment never stopped growing (total=%d)", total)
		case <-time.After(300 * time.Millisecond):
		}
		_, total := d.AbandonedHandlerCount("deliver")
		if total == last {
			stable++
			continue
		}
		stable, last = 0, total
	}

	// **頭打ちになったこと自体が検証したい性質。** 上のループが 8 回連続で
	// 変化なしを確認している。絶対値は「予算 6」ぴったりにはならない —
	// 隔離と放棄が同時に進み、reconcile の合間に spawn した worker も
	// それぞれ 1 回は放棄されるため。上限が効いていない場合は毎秒数件の
	// ペースで増え続ける (計測: 20 秒で 108) ので、この幅で十分区別できる。
	assert.LessOrEqual(t, last, uint64(40),
		"abandonment must plateau at the leak budget, not grow without bound (got %d)", last)
	assert.Zero(t, d.WorkerCount("deliver"),
		"the pool must wind down rather than keep leaking")
}

// TestHandlerDeadline_LeakBoundedWithQuarantineDisabled covers the hole the
// budget's early return left: 隔離が無効なキュー (maintenance / export /
// objectStorage は既定でそう、queueStuckWorkerSeconds: -1 でも同じ) では
// 放棄だけが起きるので、予算がそこに効かないと漏れが無制限になる。
func TestHandlerDeadline_LeakBoundedWithQuarantineDisabled(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushRedis(t)

	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:             redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency:       4,
		QueueConcurrency:  map[string]int{"deliver": 2},
		HandlerDeadline:   200 * time.Millisecond,
		StuckWorkerAfter:  -1, // 隔離は無効
		SuperviseInterval: 50 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	srv := d.Server()
	block := make(chan struct{})
	srv.Handle("wedge", func(context.Context, driver.Task) error { <-block; return nil })
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)
	t.Cleanup(func() { close(block) })

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = d.Client().Enqueue(context.Background(), "wedge", nil, driver.WithQueue("deliver"))
			time.Sleep(20 * time.Millisecond)
		}
	}()

	deadline := time.After(12 * time.Second)
	var last uint64
	stable := 0
	for stable < 8 {
		select {
		case <-deadline:
			_, total := d.AbandonedHandlerCount("deliver")
			t.Fatalf("abandonment never stopped growing with quarantine off (total=%d)", total)
		case <-time.After(300 * time.Millisecond):
		}
		_, total := d.AbandonedHandlerCount("deliver")
		if total == last {
			stable++
			continue
		}
		stable, last = 0, total
	}
	assert.LessOrEqual(t, last, uint64(40),
		"the budget must apply even with quarantine disabled (got %d)", last)
	assert.Zero(t, d.WorkerCount("deliver"))
}
