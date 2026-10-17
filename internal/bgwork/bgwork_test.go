package bgwork

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGroup_StopWaitsForRunningGoroutines(t *testing.T) {
	g := New()
	release := make(chan struct{})
	var finished atomic.Bool
	require.True(t, g.Go(func(context.Context) {
		<-release
		finished.Store(true)
	}))
	require.EqualValues(t, 1, g.Running())

	stopped := make(chan int64, 1)
	go func() { stopped <- g.Stop(context.Background()) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a goroutine was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case left := <-stopped:
		assert.Zero(t, left)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the goroutine finished")
	}
	assert.True(t, finished.Load())
	assert.Zero(t, g.Running())
}

// Stop は待ちの終わり近く (3/4 の時点) で ctx を切る。ctx を見て戻るものは、その
// 後の残りの待ちの中で戻れる。
func TestGroup_StopCancelsTheContextNearTheEndOfTheWait(t *testing.T) {
	g := New()
	started := make(chan struct{})
	canceledAt := make(chan time.Duration, 1)
	start := time.Now()
	require.True(t, g.Go(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		canceledAt <- time.Since(start)
	}))
	<-started
	assert.Zero(t, g.StopWithin(800*time.Millisecond), "ctx を見て戻るものが、残りの待ちの中で戻れなかった")
	at := <-canceledAt
	// 3/4 は 600ms。タイマーは早く発火しないので、下限を詰めても揺れない。
	// 2/3 (533ms) で切る変異はここで落ちる。
	assert.GreaterOrEqual(t, at, 580*time.Millisecond, "待ちの 3/4 より前に ctx を切った")
	assert.Less(t, at, 2*time.Second)
	assert.Error(t, g.Context().Err())
}

// 待ちの間に終われば、ctx はその時点で切る (Context の後片付け)。
func TestGroup_StopCancelsTheContextWhenEverythingFinished(t *testing.T) {
	g := New()
	require.True(t, g.Go(func(context.Context) {}))
	assert.Zero(t, g.StopWithin(time.Minute))
	assert.Error(t, g.Context().Err())
}

func TestGroup_GoAfterStopDoesNotRun(t *testing.T) {
	g := New()
	assert.Zero(t, g.Stop(context.Background()))
	var ran atomic.Bool
	assert.False(t, g.Go(func(context.Context) { ran.Store(true) }))
	time.Sleep(20 * time.Millisecond)
	assert.False(t, ran.Load())
	assert.EqualValues(t, 1, g.Dropped())
	// 2 回目の Stop も待たずに戻る。
	assert.Zero(t, g.Stop(context.Background()))
}

func TestGroup_StoppingIsClosedWhenStopBegins(t *testing.T) {
	g := New()
	select {
	case <-g.Stopping():
		t.Fatal("Stopping is closed before Stop")
	default:
	}
	seen := make(chan time.Duration, 1)
	start := time.Now()
	require.True(t, g.Go(func(context.Context) {
		<-g.Stopping()
		seen <- time.Since(start)
	}))
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, g.StopWithin(10*time.Second))
	assert.Less(t, <-seen, time.Second, "Stopping が Stop の初めに閉じていない")
	var nilGroup *Group
	assert.Nil(t, nilGroup.Stopping())
}

func TestGroup_StopGivesUpAtAQuarterOfTheDeadline(t *testing.T) {
	g := New()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	require.True(t, g.Go(func(context.Context) { <-release }))

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	start := time.Now()
	left := g.Stop(ctx)
	elapsed := time.Since(start)
	assert.EqualValues(t, 1, left)
	// 1/4 は 1 秒。半分 (2 秒) や上限 (3 秒) まで待てば落ちる。上側は CI の負荷を
	// 見込んで広く取る。
	assert.Less(t, elapsed, 1900*time.Millisecond, "Stop waited longer than a quarter of the deadline")
	assert.GreaterOrEqual(t, elapsed, 900*time.Millisecond, "Stop did not wait a quarter of the deadline")
}

func TestGroup_StopWaitsAtMostMaxWait(t *testing.T) {
	g := New()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	require.True(t, g.Go(func(context.Context) { <-release }))
	start := time.Now()
	assert.EqualValues(t, 1, g.Stop(context.Background()))
	elapsed := time.Since(start)
	// 上限は 3 秒 (stream を閉じる待ち・queue を止める待ちと揃える)。定数から
	// 導くと、定数を変えても通ってしまうので数で書く。
	assert.GreaterOrEqual(t, elapsed, 3*time.Second)
	assert.Less(t, elapsed, 4500*time.Millisecond)
}

// 期限が切れた ctx では待たない。下限で待つと、後の段 (hook) の時間を食う
// (#3661 のレビュー L-4)。期限が切れた後でも待たせたい呼び出し元は StopWithin を使う。
func TestGroup_StopDoesNotWaitWhenItsContextIsDone(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	g := New()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	require.True(t, g.Go(func(context.Context) { <-release }))
	start := time.Now()
	assert.EqualValues(t, 1, g.Stop(canceled))
	assert.Less(t, time.Since(start), 300*time.Millisecond)
	assert.Error(t, g.Context().Err())
}

// StopWithin は呼び出し元の ctx に縛られず、渡した上限まで待つ。
func TestGroup_StopWithinWaitsUpToItsLimit(t *testing.T) {
	g := New()
	require.True(t, g.Go(func(context.Context) { time.Sleep(300 * time.Millisecond) }))
	assert.Zero(t, g.StopWithin(2*time.Second), "上限の内に終わるものを待たなかった")

	g = New()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	require.True(t, g.Go(func(context.Context) { <-release }))
	start := time.Now()
	assert.EqualValues(t, 1, g.StopWithin(600*time.Millisecond))
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 550*time.Millisecond)
	assert.Less(t, elapsed, 1500*time.Millisecond)
}

// Go と Stop を並べて呼んでも、Go が true を返したものは、Stop が 0 を返した
// 時点で必ず終わっている (WaitGroup の Add が Wait の後に来ない)。
func TestGroup_GoAndStopRaceKeepsEveryStartedGoroutineCounted(t *testing.T) {
	for range 200 {
		g := New()
		var started, finished atomic.Int64
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 20 {
					if g.Go(func(context.Context) {
						time.Sleep(time.Millisecond)
						finished.Add(1)
					}) {
						started.Add(1)
					}
				}
			}()
		}
		left := g.StopWithin(5 * time.Second)
		wg.Wait()
		require.Zero(t, left)
		require.Equal(t, started.Load(), finished.Load(), "Stop が 0 を返した後に、始めた goroutine が残っていた")
	}
}

func TestGroup_RecoversPanics(t *testing.T) {
	g := New()
	require.True(t, g.Go(func(context.Context) { panic("boom") }))
	assert.Zero(t, g.Stop(context.Background()))
}

func TestNilGroup_RunsPlainGoroutines(t *testing.T) {
	var g *Group
	done := make(chan context.Context, 1)
	require.True(t, g.Go(func(ctx context.Context) { done <- ctx }))
	select {
	case ctx := <-done:
		assert.NoError(t, ctx.Err())
	case <-time.After(5 * time.Second):
		t.Fatal("a nil Group did not run fn")
	}
	assert.NoError(t, g.Context().Err())
	assert.Zero(t, g.Stop(context.Background()))
	assert.Zero(t, g.StopWithin(time.Second))
	assert.Zero(t, g.Running())
	assert.Zero(t, g.Dropped())
	g.RecordDropped("x", "y")
}

// 止まった後に捨てたものは、件数をログに残す。黙って捨てると、通知のように
// 冪等でないものが消えたことに気付けない。
func TestGroup_DroppedWorkIsCountedAndLogged(t *testing.T) {
	var buf safeBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	g := New()
	g.Stop(context.Background())
	g.Go(func(context.Context) {})
	g.GoAs("plugin p", func(context.Context) {})
	g.RecordDropped("note hook", "gave up waiting for a slot")
	out := buf.String()
	assert.Contains(t, out, "dropped=1")
	assert.Contains(t, out, "dropped=2")
	assert.Contains(t, out, `what="plugin p"`)
	assert.Contains(t, out, "dropped=3")
	assert.Contains(t, out, `what="note hook"`)
	assert.EqualValues(t, 3, g.Dropped())
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestWaitLimit(t *testing.T) {
	assert.Equal(t, time.Second, waitLimit(4*time.Second), "残りの 1/4")
	assert.Equal(t, 2*time.Second, waitLimit(8*time.Second), "残りの 1/4")
	assert.Equal(t, MaxWait, waitLimit(time.Minute), "上限")
	assert.Equal(t, 3*time.Second, MaxWait)
	assert.Zero(t, waitLimit(-time.Second), "負にしない")
}

func TestGroup_AfterFuncRunsAfterTheDelay(t *testing.T) {
	g := New()
	ran := make(chan time.Duration, 1)
	start := time.Now()
	require.True(t, g.AfterFunc(100*time.Millisecond, func() { ran <- time.Since(start) }))
	select {
	case at := <-ran:
		assert.GreaterOrEqual(t, at, 100*time.Millisecond)
	case <-time.After(5 * time.Second):
		t.Fatal("AfterFunc did not run")
	}
	assert.Zero(t, g.StopWithin(time.Second))
}

// 止め始めに、待っているものを今すぐ走らせ、Stop はその終わりを待つ。発火と
// 重なっても二重には走らない。
func TestGroup_AfterFuncRunsAtOnceWhenTheStopBegins(t *testing.T) {
	g := New()
	var runs atomic.Int64
	for range 100 {
		require.True(t, g.AfterFunc(time.Minute, func() {
			time.Sleep(10 * time.Millisecond)
			runs.Add(1)
		}))
	}
	start := time.Now()
	require.Zero(t, g.StopWithin(5*time.Second), "待っていたものの終わりを待たなかった")
	assert.Less(t, time.Since(start), 3*time.Second, "遅らせる時間を待った")
	assert.EqualValues(t, 100, runs.Load())
	assert.False(t, g.AfterFunc(time.Millisecond, func() { runs.Add(1) }), "止め始めた後に予約した")
	time.Sleep(20 * time.Millisecond)
	assert.EqualValues(t, 100, runs.Load())
}

func TestGroup_AfterFuncFiringDuringTheStopRunsOnce(t *testing.T) {
	for range 200 {
		g := New()
		var runs atomic.Int64
		require.True(t, g.AfterFunc(time.Millisecond, func() { runs.Add(1) }))
		time.Sleep(time.Millisecond)
		require.Zero(t, g.StopWithin(5*time.Second))
		require.EqualValues(t, 1, runs.Load(), "発火と止め始めが重なって、二重に走ったか走らなかった")
	}
}

func TestNilGroup_AfterFunc(t *testing.T) {
	var g *Group
	ran := make(chan struct{})
	require.True(t, g.AfterFunc(time.Millisecond, func() { close(ran) }))
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("a nil Group did not run fn")
	}
}

// Stop は待っていた仕事を箱の goroutine として起動し、ロックの中では走らせない
// (#3661 のレビュー3周目 L-2)。ロックの中で走らせると、仕事が Go や AfterFunc を
// 呼んだときに止まり、待ちの上限も効かなくなる。
func TestGroup_AfterFuncWorkStartedByStopCanCallTheGroup(t *testing.T) {
	g := New()
	results := make(chan [2]bool, 1)
	require.True(t, g.AfterFunc(time.Minute, func() {
		goOK := g.Go(func(context.Context) {})
		afterOK := g.AfterFunc(time.Millisecond, func() {})
		results <- [2]bool{goOK, afterOK}
	}))
	stopped := make(chan int64, 1)
	go func() { stopped <- g.StopWithin(2 * time.Second) }()
	select {
	case left := <-stopped:
		assert.Zero(t, left)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop が戻らない (待っていた仕事をロックの中で走らせた)")
	}
	r := <-results
	assert.False(t, r[0], "止め始めた後の Go を受け付けた")
	assert.False(t, r[1], "止め始めた後の AfterFunc を受け付けた")
	assert.EqualValues(t, 1, g.Dropped(), "止め始めた後の Go を数えていない")
}
