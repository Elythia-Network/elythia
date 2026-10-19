package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/core/chart"
	corechartcharts "github.com/elythia-network/elythia/internal/core/chart/charts"
)

// startsBackgroundChartWrite makes the late job of f start a goroutine in the
// generation's group that counts one more inbox request after a delay, the
// way the inbox processor starts its best-effort hooks.
func startsBackgroundChartWrite(t *testing.T, f *shutdownOrderFixture, delay time.Duration) {
	t.Helper()
	f.srv.background = bgwork.New()
	finish := f.job.finish
	f.job.finish = func() {
		finish()
		require.True(t, f.srv.background.Go(func(context.Context) {
			time.Sleep(delay)
			assert.NoError(t, corechartcharts.NewApRequestChart(f.apReq).Inbox())
		}), "止まりきる前の worker が起動した goroutine を捨てた")
	}
}

// TestServer_StopBackgroundCollectsGoroutinesBeforeTheHooks checks where the
// group is stopped when the queue stops in time (#3661): after the queue and
// before the hooks and the chart, so what a goroutine of the group writes to
// the chart is saved.
func TestServer_StopBackgroundCollectsGoroutinesBeforeTheHooks(t *testing.T) {
	f := newShutdownOrderFixture(t)
	startsBackgroundChartWrite(t, f, 100*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.srv.StopBackground(ctx))
	assert.Zero(t, f.srv.background.Dropped())
	f.assertJobWritesSaved(t, 2)
}

// TestServer_StopBackgroundCollectsGoroutinesOfAWorkerStoppingAfterTheDeadline
// checks the other place (#3661): when the queue did not stop in time, the
// group is stopped only after the worker stopped, so the goroutines it starts
// are not dropped, and before the late chart save, so their writes are saved
// even though the shutdown ctx has expired.
func TestServer_StopBackgroundCollectsGoroutinesOfAWorkerStoppingAfterTheDeadline(t *testing.T) {
	f := newShutdownOrderFixture(t)
	f.job.block = make(chan struct{})
	// 停止の期限が切れた後も、lateBackgroundWait で最小 1 秒は待つ。0.7 秒かかる
	// goroutine は、切れた ctx のまま Stop で待てば (0 秒) 間に合わず、
	// lateBackgroundWait で待てば間に合う (#3661 のレビュー L-2)。
	startsBackgroundChartWrite(t, f, 700*time.Millisecond)
	f.srv.addPart("testPart", nil, func(ctx context.Context) {
		<-ctx.Done()
		close(f.job.block)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- f.srv.StopBackground(ctx) }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("StopBackground が返らない")
	}
	require.Error(t, ctx.Err(), "前提: worker が止まった時点で期限が切れている")
	assert.Zero(t, f.srv.background.Dropped())
	f.assertJobWritesSaved(t, 2)
}

func TestLateBackgroundWait(t *testing.T) {
	long, cancelLong := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelLong()
	assert.Equal(t, bgwork.MaxWait, lateBackgroundWait(long), "上限を超えて待つ")

	mid, cancelMid := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancelMid()
	assert.InDelta(t, float64(2*time.Second), float64(lateBackgroundWait(mid)), float64(100*time.Millisecond), "残りの 1/4 でない")

	expired, cancelExpired := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelExpired()
	<-expired.Done()
	assert.Equal(t, time.Second, lateBackgroundWait(expired), "期限が切れていても 1 秒は待つ")
	assert.Equal(t, bgwork.MaxWait, lateBackgroundWait(context.Background()))
}

// TestServer_StopBackgroundBoundsTheLateWait checks that a goroutine of the
// group that does not stop holds the late path only for lateBackgroundWait
// (1 second with an expired ctx), not for bgwork.MaxWait (#3661 review r2
// L-6 N7). 保存の期限は待った後に取るので、保存の時間は削られない。
func TestServer_StopBackgroundBoundsTheLateWait(t *testing.T) {
	f := newShutdownOrderFixture(t)
	f.job.block = make(chan struct{})
	f.srv.background = bgwork.New()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	finish := f.job.finish
	f.job.finish = func() {
		finish()
		require.True(t, f.srv.background.Go(func(context.Context) { <-release }))
	}
	f.srv.addPart("testPart", nil, func(ctx context.Context) {
		<-ctx.Done()
		close(f.job.block)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	stopped := make(chan error, 1)
	go func() { stopped <- f.srv.StopBackground(ctx) }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("StopBackground が返らない")
	}
	elapsed := time.Since(start)
	// 期限 0.3 秒 + 箱の待ち 1 秒 + 保存。MaxWait (3 秒) まで待てば 3.3 秒を超える。
	assert.GreaterOrEqual(t, elapsed, 1200*time.Millisecond, "遅い経路で箱を待たなかった")
	assert.Less(t, elapsed, 2800*time.Millisecond, "遅い経路の箱の待ちが上限を守っていない")
	assert.EqualValues(t, 1, f.srv.background.Running())
	f.assertJobWritesSaved(t, 1)
}

// deadlineRecordingRepo records the longest time left until the deadline of
// the ctx passed to ApplyDeltas, the write of a chart save.
type deadlineRecordingRepo struct {
	chart.Repository
	mu      sync.Mutex
	maxLeft time.Duration
}

func (r *deadlineRecordingRepo) ApplyDeltas(ctx context.Context, span chart.Span, id int64, deltas map[string]int64, uniqueAppends map[string][]string, setInts map[string]int64) error {
	if deadline, ok := ctx.Deadline(); ok {
		r.mu.Lock()
		r.maxLeft = max(r.maxLeft, time.Until(deadline))
		r.mu.Unlock()
	}
	return r.Repository.ApplyDeltas(ctx, span, id, deltas, uniqueAppends, setInts)
}

// TestServer_StopBackgroundLeavesTheLateSaveItsTime checks #3661 review r3
// L-1: on the late path, the chart save gets its whole lateChartSaveMax even
// after the wait for a goroutine of the group that does not stop, because
// the deadline of the save is taken after that wait.
func TestServer_StopBackgroundLeavesTheLateSaveItsTime(t *testing.T) {
	f := newShutdownOrderFixture(t)
	f.job.block = make(chan struct{})
	f.srv.background = bgwork.New()

	// 遅い経路の保存だけを見る chart を足す。書き込みは、hook の段の保存
	// (chartMgmt.Stop) が終わった後に、箱の goroutine から足す。
	schema := corechartcharts.SchemaApRequest()
	rec := &deadlineRecordingRepo{Repository: chart.NewRepository(f.db, schema)}
	late, err := chart.New(chart.Config{
		Schema: schema, Repo: rec, Lock: chart.NewMemoryLocker(), Clock: fixedClock(time.Now().UTC()),
	})
	require.NoError(t, err)
	f.srv.chartMgmt.Stop(context.Background())
	mgmt := chart.NewManagementService([]*chart.Chart{f.apReq, late}, time.Hour)
	require.NoError(t, mgmt.Start(context.Background()))
	f.srv.setChartManagement(mgmt)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	finish := f.job.finish
	f.job.finish = func() {
		finish()
		require.True(t, f.srv.background.Go(func(context.Context) {
			time.Sleep(300 * time.Millisecond)
			assert.NoError(t, corechartcharts.NewApRequestChart(late).Inbox())
		}))
		// 止まらない goroutine。箱の待ちは上限 (期限切れなので 1 秒) まで使われる。
		require.True(t, f.srv.background.Go(func(context.Context) { <-release }))
	}
	f.srv.addPart("testPart", nil, func(ctx context.Context) {
		<-ctx.Done()
		close(f.job.block)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- f.srv.StopBackground(ctx) }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("StopBackground が返らない")
	}
	rec.mu.Lock()
	left := rec.maxLeft
	rec.mu.Unlock()
	// 保存の期限は待った後に取るので、2 秒近くが残る。待つ前に取ると、箱の待ち
	// (1 秒) の分だけ削られて約 1 秒になる。
	assert.Greater(t, left, 1700*time.Millisecond, "遅い経路の保存の時間が、箱の待ちに削られた")
}
