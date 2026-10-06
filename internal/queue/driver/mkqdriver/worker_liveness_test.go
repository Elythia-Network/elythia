package mkqdriver

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/mkq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock is a hand-advanced monotonic nanos source.
type fakeClock struct{ n int64 }

func (c *fakeClock) nanos() int64            { return c.n }
func (c *fakeClock) advance(d time.Duration) { c.n += int64(d) }

// handleBusyFor builds a handle that entered its handler busyFor ago,
// relative to clk. A zero busyFor produces an idle handle.
func handleBusyFor(seq uint64, clk *fakeClock, busyFor time.Duration) *workerHandle {
	h := &workerHandle{seq: seq}
	if busyFor > 0 {
		h.busySinceNanos.Store(clk.nanos() - int64(busyFor))
	}
	return h
}

// quarantined builds n real (non-nil) handles for the quarantine slice.
func quarantined(n int, clk *fakeClock) []*workerHandle {
	out := make([]*workerHandle, 0, n)
	for i := range n {
		out = append(out, handleBusyFor(uint64(200+i), clk, time.Hour))
	}
	return out
}

func seqs(hs []*workerHandle) []uint64 {
	out := make([]uint64, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.seq)
	}
	return out
}

// newTestPool builds a pool with no real mkq.Queue. Every helper it
// exercises works on the handle slices only, so the nil queue is never
// dereferenced as long as the test does not grow the roster.
func newTestPool(name string, desired int, stuckAfter time.Duration, clk *fakeClock) *workerPool {
	return &workerPool{
		name:            name,
		desired:         desired,
		peakDesired:     desired,
		baseConcurrency: desired,
		stuckAfter:      stuckAfter,
		nanos:           clk.nanos,
	}
}

func TestWorkerHandle_BusyFor(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}

	idle := handleBusyFor(0, clk, 0)
	assert.True(t, idle.idle())
	assert.Equal(t, time.Duration(0), idle.busyFor(clk.nanos()))

	busy := handleBusyFor(1, clk, 90*time.Second)
	assert.False(t, busy.idle())
	assert.Equal(t, 90*time.Second, busy.busyFor(clk.nanos()))

	// 単調時間なので巻き戻りは起きないが、念のため負値を 0 に潰す。
	assert.Equal(t, time.Duration(0), busy.busyFor(0))
}

func TestWorkerHandle_ProvenLive(t *testing.T) {
	h := &workerHandle{}
	h.completed.Store(7)
	h.completedAtQuarantine = 7
	assert.False(t, h.provenLive(), "no job finished since quarantine")

	h.completed.Add(1)
	assert.True(t, h.provenLive(), "one completion is proof the dispatcher runs")
}

func TestSplitForRemoval_IdleBeforeBusy(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	ws := []*workerHandle{
		handleBusyFor(0, clk, 10*time.Second),
		handleBusyFor(1, clk, 0),
		handleBusyFor(2, clk, 10*time.Second),
	}
	kept, removed := splitForRemoval(ws, 1, 1, true)
	assert.Equal(t, []uint64{1}, seqs(removed),
		"an idle worker is cheaper to stop than one holding a job")
	assert.Equal(t, []uint64{0, 2}, seqs(kept))
}

func TestSplitForRemoval_TrailingFirstAmongEquals(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	ws := []*workerHandle{
		handleBusyFor(0, clk, time.Second),
		handleBusyFor(1, clk, time.Second),
		handleBusyFor(2, clk, time.Second),
		handleBusyFor(3, clk, time.Second),
	}
	kept, removed := splitForRemoval(ws, 2, 2, true)
	assert.Equal(t, []uint64{2, 3}, seqs(removed))
	assert.Equal(t, []uint64{0, 1}, seqs(kept))
}

func TestSplitForRemoval_Bounds(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	ws := []*workerHandle{handleBusyFor(0, clk, 0), handleBusyFor(1, clk, 0)}

	kept, removed := splitForRemoval(ws, 0, 0, true)
	assert.Equal(t, []uint64{0, 1}, seqs(kept))
	assert.Empty(t, removed)

	kept, removed = splitForRemoval(ws, 2, 2, true)
	assert.Empty(t, kept)
	assert.Equal(t, []uint64{0, 1}, seqs(removed))

	kept, removed = splitForRemoval(ws, 9, 9, true)
	assert.Empty(t, kept)
	assert.Equal(t, []uint64{0, 1}, seqs(removed))
}

func TestPool_LiveCountExcludesOverThreshold(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	p := newTestPool("inbox", 4, time.Minute, clk)
	p.workers = []*workerHandle{
		handleBusyFor(0, clk, 30*time.Minute),
		handleBusyFor(1, clk, 30*time.Minute),
		handleBusyFor(2, clk, time.Second),
		handleBusyFor(3, clk, 0),
	}
	assert.Equal(t, 2, p.liveCountLocked())

	p.stuckAfter = 0
	assert.Equal(t, 4, p.liveCountLocked(),
		"liveness tracking off means the ledger count is all we have")
}

func TestPool_QuarantineIsUnconditional(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	p := newTestPool("inbox", 1, time.Minute, clk)
	stuck := handleBusyFor(0, clk, 30*time.Minute)
	stuck.completed.Store(5)
	healthy := handleBusyFor(1, clk, time.Second)
	p.workers = []*workerHandle{stuck, healthy}

	p.quarantineStuckLocked()

	assert.Equal(t, []uint64{1}, seqs(p.workers))
	assert.Equal(t, []uint64{0}, seqs(p.quarantine))
	assert.Equal(t, uint64(5), stuck.completedAtQuarantine, "the baseline is snapshotted")
	// **停止していない**ことが要点。誤検知なら job はそのまま完走する。
	assert.False(t, stuck.idle(), "quarantine must not cancel the in-flight job")

	// roster に閾値超過が残らないので、勘定は len と一致する。
	assert.Equal(t, len(p.workers), p.liveCountLocked())
}

// TestPool_QuarantineDoesNotStopAtBudget pins the accounting invariant:
// eviction is never skipped, because a skipped eviction leaves a wedged
// worker on the roster and makes len(workers) disagree with the live
// count — which is how "asked to grow, got smaller" happened.
func TestPool_QuarantineDoesNotStopAtBudget(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	p := newTestPool("inbox", 1, time.Minute, clk)
	for i := range 20 {
		p.quarantine = append(p.quarantine, handleBusyFor(uint64(100+i), clk, time.Hour))
	}
	p.workers = []*workerHandle{
		handleBusyFor(0, clk, 30*time.Minute),
		handleBusyFor(1, clk, 30*time.Minute),
	}

	p.quarantineStuckLocked()

	assert.Empty(t, p.workers, "every over-threshold worker leaves the roster")
	assert.Len(t, p.quarantine, 22)
	assert.Equal(t, len(p.workers), p.liveCountLocked())
}

func TestPool_QuarantineNoopWhenDisabled(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	p := newTestPool("export", 2, 0, clk)
	p.workers = []*workerHandle{handleBusyFor(0, clk, 3*time.Hour)}

	p.quarantineStuckLocked()

	assert.Len(t, p.workers, 1)
	assert.Empty(t, p.quarantine)
}

func TestPool_ReinstateUsesCompletionNotIdleness(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	p := newTestPool("inbox", 2, time.Minute, clk)

	// 「隔離後に 1 件完了したが、今また別の job を処理中」= 生きている。
	// idle 判定だとこれを取りこぼし、健全な worker が永久に隔離される。
	chained := handleBusyFor(0, clk, time.Second)
	chained.completedAtQuarantine = 3
	chained.completed.Store(4)

	// 「今は idle に見えるが、隔離後に 1 件も完了していない」= 判定できない。
	// finalise 中の窓を idle と読んで停止するのを避ける。
	betweenPhases := handleBusyFor(1, clk, 0)
	betweenPhases.completedAtQuarantine = 9
	betweenPhases.completed.Store(9)

	p.quarantine = []*workerHandle{chained, betweenPhases}
	p.reinstateProvenLiveLocked()

	assert.Equal(t, []uint64{0}, seqs(p.workers), "the chained worker is proven live")
	assert.Equal(t, []uint64{1}, seqs(p.quarantine))
}

func TestPool_AllowedRosterCapsAtPeakPlusHeadroom(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	p := newTestPool("inbox", 4, time.Minute, clk)

	assert.Equal(t, 4, p.allowedRosterLocked(), "nothing quarantined")

	p.quarantine = quarantined(4, clk)
	assert.Equal(t, 4, p.allowedRosterLocked(),
		"one full roster's worth of wedges is still fully replaceable")

	// desired < headroom の側も見る。desired そのものを幅にすると
	// scale-down で幅まで縮み、詰まり 2 件で 1 本も動かせなくなる。
	small := newTestPool("export", 2, time.Minute, clk)
	small.quarantine = quarantined(2, clk)
	assert.Equal(t, 2, small.allowedRosterLocked(),
		"a 2-worker pool tolerates the headroom floor, not 2x itself")

	// desired を下げても幅 (peak + headroom) は動かない。
	small.desired = 1
	assert.Equal(t, 1, small.allowedRosterLocked())

	p.quarantine = quarantined(5, clk)
	assert.Equal(t, 3, p.allowedRosterLocked())

	p.quarantine = quarantined(8, clk)
	assert.Equal(t, 0, p.allowedRosterLocked())

	p.quarantine = quarantined(99, clk)
	assert.Equal(t, 0, p.allowedRosterLocked(), "never negative")

	// 隔離を切ると quarantine には何も入らないので、実質 desired に戻る。
	p.stuckAfter = 0
	p.quarantine = nil
	assert.Equal(t, 4, p.allowedRosterLocked())

	// **ただし放棄ぶんは隔離の有無に関わらず効く。** 期限 (#2658) は隔離とは
	// 独立に動くので、maintenance / export のような隔離対象外のキューでも
	// 放棄は起きる。ここで効かないと、そのキューの漏れが無制限になる。
	var ab abandonCounters
	p.abandoned = &ab
	for range 6 {
		ab.handlerAbandoned("inbox")
	}
	assert.Equal(t, 2, p.allowedRosterLocked(),
		"abandoned handlers consume the budget even with quarantine disabled")
}

func TestPool_CapLogIsThrottled(t *testing.T) {
	// 単調時間 0 (= プロセス起動直後) を「まだ出していない」と取り違えない
	// ことも見る。
	clk := &fakeClock{n: 0}
	p := newTestPool("inbox", 4, time.Minute, clk)
	p.quarantine = []*workerHandle{handleBusyFor(9, clk, time.Hour)}

	p.reportCapLocked(0)
	first := p.lastCapLogNanos
	require.True(t, p.capLogged)
	require.Equal(t, clk.nanos(), first)

	clk.advance(reportCapEvery - time.Second)
	p.reportCapLocked(0)
	assert.Equal(t, first, p.lastCapLogNanos, "throttled inside the window")

	clk.advance(2 * time.Second)
	p.reportCapLocked(0)
	assert.Equal(t, clk.nanos(), p.lastCapLogNanos)
}

func TestPool_AdjustShrinksToAllowedRoster(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	p := newTestPool("inbox", 2, time.Minute, clk)
	// allowed = peak + max(base, 4) - len(quarantine) = 2 + 4 - 5 = 1
	p.quarantine = quarantined(5, clk)
	p.workers = []*workerHandle{
		handleBusyFor(0, clk, time.Second),
		handleBusyFor(1, clk, 0),
	}

	// h.w == nil なので stopHandles は実 Stop を撃たずに飛ばす。
	require.NoError(t, p.adjustLocked())

	assert.Equal(t, []uint64{0}, seqs(p.workers), "the idle worker is the one dropped")
}

func TestPool_AdjustAtZeroDesiredStopsQuarantineToo(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	p := newTestPool("inbox", 0, time.Minute, clk)
	p.workers = []*workerHandle{handleBusyFor(0, clk, 0)}
	p.quarantine = []*workerHandle{handleBusyFor(1, clk, time.Hour)}

	require.NoError(t, p.adjustLocked())

	assert.Empty(t, p.workers)
	assert.Empty(t, p.quarantine, "pausing a queue must not leave workers behind")
}

func TestPool_ReconcileAfterShutdown(t *testing.T) {
	p := &workerPool{name: "deliver", shutdown: true}
	assert.ErrorIs(t, p.reconcileLocked(), ErrResizeAfterShutdown)
	assert.ErrorIs(t, p.resizeLocked(2), ErrResizeAfterShutdown)
}

func TestPool_TrackedHandlerMarksBusyAndCounts(t *testing.T) {
	// プロセス起動直後の 0 も busy と読めること。0 は idle の番兵なので、
	// ここを取りこぼすと詰まり検出が丸ごと空振りする。
	clk := &fakeClock{n: 0}
	h := &workerHandle{}
	var sawBusy bool
	p := newTestPool("deliver", 1, time.Minute, clk)
	p.handler = func(context.Context, *mkq.Job[framedPayload]) (any, error) {
		sawBusy = !h.idle()
		return nil, nil
	}

	_, err := p.trackedHandler(h)(context.Background(), &mkq.Job[framedPayload]{})
	require.NoError(t, err)
	assert.True(t, sawBusy, "the handle must read as busy while the handler runs")
	assert.True(t, h.idle(), "and as idle once it returns")
	assert.Equal(t, uint64(1), h.completed.Load())
}

func TestServer_ResolvedStuckAfter(t *testing.T) {
	s := &Server{}
	assert.Equal(t, defaultStuckAfter, s.resolvedStuckAfter("inbox"))
	assert.Equal(t, defaultStuckAfter, s.resolvedStuckAfter("deliver"))
	assert.Equal(t, time.Duration(0), s.resolvedStuckAfter("export"),
		"batch queues legitimately run past any threshold")
	assert.Equal(t, time.Duration(0), s.resolvedStuckAfter("objectStorage"))
	assert.Equal(t, time.Duration(0), s.resolvedStuckAfter("maintenance"))
	assert.Equal(t, time.Duration(0), s.resolvedStuckAfter("operator-defined"))

	over := &Server{stuckAfter: 90 * time.Second}
	assert.Equal(t, 90*time.Second, over.resolvedStuckAfter("inbox"))
	assert.Equal(t, 90*time.Second, over.resolvedStuckAfter("export"),
		"an explicit value applies to every queue")

	off := &Server{stuckAfter: -1}
	assert.Equal(t, time.Duration(0), off.resolvedStuckAfter("inbox"))
}

func TestDefaultStuckAfterByQueue_CoversTheBoundedQueues(t *testing.T) {
	// 表に載せるかどうかは「job の長さに上限があるか」で決める。追加した
	// キューをどちらかに分類し忘れると、既定で追跡対象外に落ちる。
	tracked := map[string]bool{
		"inbox": true, "deliver": true, "relationship": true,
		"push": true, "webhook": true,
		"export": false, "objectStorage": false, "maintenance": false,
	}
	// **QueueNames から回す。** 手書きの一覧だと、キューを足したときに
	// どちらにも分類されないまま既定 (追跡しない) に落ちて気付けない。
	for _, q := range QueueNames {
		want, ok := tracked[q]
		require.True(t, ok, "queue %q is not classified in the stuck-worker table", q)
		if want {
			assert.Positive(t, defaultStuckAfterForQueue(q), "queue %q should be tracked", q)
			continue
		}
		assert.Zero(t, defaultStuckAfterForQueue(q), "queue %q must not be tracked", q)
	}
	assert.Len(t, tracked, len(QueueNames), "the classification must cover exactly the driver's queues")
}

func TestServer_QuarantinedWorkerCount(t *testing.T) {
	s := &Server{}
	assert.Equal(t, 0, s.QuarantinedWorkerCount("deliver"), "no pools yet")

	s.pools = map[string]*workerPool{"deliver": {
		quarantine: []*workerHandle{{seq: 1}, {seq: 2}},
	}}
	assert.Equal(t, 2, s.QuarantinedWorkerCount("deliver"))
	assert.Equal(t, 0, s.QuarantinedWorkerCount("inbox"))
}

func TestServer_ClockDefaultsToMonotonic(t *testing.T) {
	assert.NotNil(t, (&Server{}).clock())
	assert.Positive(t, (&Server{}).clock()(), "process start is in the past")

	s := &Server{nanos: func() int64 { return 42 }}
	assert.Equal(t, int64(42), s.clock()())
}

func TestServer_StopSupervisorIsIdempotent(t *testing.T) {
	s := &Server{}
	// 起動していない状態で呼んでも安全であること (Start 失敗経路)。
	s.stopSupervisor()

	s.superviseInterval = time.Hour
	s.pools = map[string]*workerPool{"inbox": {stuckAfter: time.Minute}}
	s.startSupervisor()
	require.NotNil(t, s.superviseCancel)
	s.stopSupervisor()
	assert.Nil(t, s.superviseCancel)
	s.stopSupervisor()
}

func TestServer_StartSupervisorSkipped(t *testing.T) {
	tracked := map[string]*workerPool{"inbox": {stuckAfter: time.Minute}}

	s := &Server{superviseInterval: -1, pools: tracked}
	s.startSupervisor()
	assert.Nil(t, s.superviseCancel, "a negative interval opts out of the periodic check")

	// 隔離も期限も無効なら supervisor は不要。
	s = &Server{
		handlerDeadline: -1,
		pools:           map[string]*workerPool{"export": {stuckAfter: 0}},
	}
	s.startSupervisor()
	assert.Nil(t, s.superviseCancel, "nothing to supervise: neither mechanism is on")
	s.stopSupervisor()

	// **期限だけ有効でも supervisor は要る。** 漏れの予算を適用するのは
	// reconcileLocked で、それを回すのは supervisor だけ。隔離だけを条件に
	// すると、隔離対象外のキューで放棄が無制限に積もる。
	s = &Server{pools: map[string]*workerPool{"export": {stuckAfter: 0}}}
	s.startSupervisor()
	assert.NotNil(t, s.superviseCancel,
		"the deadline alone still needs the supervisor to apply the leak budget")
	s.stopSupervisor()

	// Shutdown が先に走った窓。pools が nil なら誰も止めない goroutine を
	// 生やさない。
	s = &Server{pools: nil}
	s.startSupervisor()
	assert.Nil(t, s.superviseCancel)
}

// TestPool_AllowedRosterDoesNotRatchetWithDesired is the regression test
// for the feedback loop between the cap and the autoscaler.
//
// autoscale の入力は allowedRosterLocked が返した生存数から来るので、幅を
// desired から計算すると「隔離が増える → 生存数が減る → 目標が下がる →
// 幅が縮む → さらに減る」で 0 に落ちる。desired を下げても幅が縮まないこと
// を固定する。
func TestPool_AllowedRosterDoesNotRatchetWithDesired(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	p := newTestPool("inbox", 16, time.Minute, clk)
	p.baseConcurrency = 16
	p.quarantine = quarantined(20, clk)

	// 設定値 16 + 幅 16 - 隔離 20 = 12。
	require.Equal(t, 12, p.allowedRosterLocked())

	// autoscale が生存数 12 から目標 15 を出して desired を下げても、
	// 幅は設定値に紐づいているので 12 のまま。
	for _, desired := range []int{15, 12, 5, 1} {
		p.desired = desired
		assert.Equal(t, min(desired, 12), p.allowedRosterLocked(),
			"desired=%d must not shrink the allowance itself", desired)
	}

	// autoscale が設定値より上へ伸ばしている間は幅が足枷にならない。
	// peakDesired は resizeLocked が desired と一緒に押し上げる。
	p.desired, p.peakDesired = 64, 64
	p.quarantine = nil
	assert.Equal(t, 64, p.allowedRosterLocked())

	// **上へ伸ばしたあとに隔離が出ても、基準は peak のまま動かない。**
	// ここが desired 依存だと autoscale との帰還ループになる。
	p.quarantine = quarantined(20, clk)
	require.Equal(t, 60, p.allowedRosterLocked()) // 64 + 16 - 20
	for _, desired := range []int{50, 40, 20} {
		p.desired = desired
		assert.Equal(t, min(desired, 60), p.allowedRosterLocked(),
			"desired=%d must not move the ceiling", desired)
	}
}

// TestPool_ReinstateDoesNotCancelJobs pins that a Worker proven alive is
// not immediately stopped as surplus. 隔離は job を殺さないという設計なのに
// 戻した直後に Stop すると、結局その job が cancel されて retry になる。
func TestPool_ReinstateDoesNotCancelJobs(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	p := newTestPool("inbox", 2, time.Minute, clk)

	// roster は満杯で全員 job 処理中 (= 忙しいキューの定常状態)。
	p.workers = []*workerHandle{
		handleBusyFor(0, clk, time.Second),
		handleBusyFor(1, clk, time.Second),
	}
	// 隔離中の 1 本が完了を 1 件記録した = 生きている。今も次の job を処理中。
	back := handleBusyFor(2, clk, time.Second)
	back.completedAtQuarantine = 3
	back.completed.Store(4)
	p.quarantine = []*workerHandle{back}

	require.NoError(t, p.reconcileLocked())

	assert.Equal(t, []uint64{0, 1, 2}, seqs(p.workers),
		"the surplus rides to the next reconcile instead of cancelling a job")
	assert.Empty(t, p.quarantine)

	// 1 本が idle になれば、次の reconcile で job を殺さずに畳める。
	p.workers[0].busySinceNanos.Store(0)
	require.NoError(t, p.reconcileLocked())
	assert.Equal(t, []uint64{1, 2}, seqs(p.workers))
}

// TestPool_ReconcileReleasesProtectionAfterOneJob gates the *wiring* of
// clearProtectionLocked, not just the helper.
//
// **helper を直接呼ぶテストだけでは足りない。** reconcileLocked から
// 呼び出しを外しても helper のテストは通るので、庇いが永久に外れず roster が
// 目標を超えたまま張り付く状態 (#2657 round-4) が復活する。ここでは全 handle が
// job を持ったまま = idle を一度も観測できない条件で、完了 1 件だけで余剰が
// 畳まれることを見る。
func TestPool_ReconcileReleasesProtectionAfterOneJob(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	p := newTestPool("inbox", 2, time.Minute, clk)
	p.workers = []*workerHandle{
		handleBusyFor(0, clk, time.Second),
		handleBusyFor(1, clk, time.Second),
	}
	back := handleBusyFor(2, clk, time.Second)
	back.completedAtQuarantine = 3
	back.completed.Store(4)
	p.quarantine = []*workerHandle{back}

	require.NoError(t, p.reconcileLocked())
	require.Equal(t, []uint64{0, 1, 2}, seqs(p.workers), "surplus rides")
	require.True(t, back.protected)

	// 復帰時に持っていた job が終わった (すぐ次の job に移ったので idle には
	// ならない)。これで庇いが外れ、余剰が畳まれる。
	back.completed.Add(1)
	require.NoError(t, p.reconcileLocked())

	assert.False(t, back.protected)
	assert.Len(t, p.workers, 2, "the surplus must be reclaimed without ever going idle")
}

func TestSplitForRemoval_BusyBudget(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	ws := []*workerHandle{
		handleBusyFor(0, clk, time.Second),
		handleBusyFor(1, clk, time.Second),
		handleBusyFor(2, clk, 0),
	}

	// 予算 0 では idle しか外さないので、要求 2 に対して 1 本しか返らない。
	kept, removed := splitForRemoval(ws, 2, 0, true)
	assert.Equal(t, []uint64{2}, seqs(removed))
	assert.Equal(t, []uint64{0, 1}, seqs(kept))

	// 予算があれば従来どおり要求数まで外す (idle 優先、次に末尾)。
	kept, removed = splitForRemoval(ws, 2, 2, true)
	assert.Equal(t, []uint64{1, 2}, seqs(removed))
	assert.Equal(t, []uint64{0}, seqs(kept))

	// 庇われている busy handle は予算があっても選ばれない。
	// removed は選んだ順ではなく元の並び順で返る。
	ws[1].protected = true
	kept, removed = splitForRemoval(ws, 2, 2, true)
	assert.Equal(t, []uint64{0, 2}, seqs(removed))
	assert.Equal(t, []uint64{1}, seqs(kept))
}

// TestMonotonicNanos_IsElapsedNotEpoch pins that liveness uses elapsed
// monotonic time. 壁時計の epoch 値を入れると NTP の前方ジャンプで全 Worker が
// 同時に閾値超過に見える。
func TestMonotonicNanos_IsElapsedNotEpoch(t *testing.T) {
	got := monotonicNanos()
	assert.Positive(t, got)
	assert.Less(t, got, int64(24*time.Hour),
		"must be elapsed-since-start, not a Unix epoch nanosecond count")
}

// TestPool_ResizeKeepsPeakMonotone gates the high-water mark itself.
//
// **allowedRosterLocked が peak を読むだけでは不十分。** peak が desired に
// 追随して下がると、上限がまた desired 依存になり #2657 round-3 の
// 「scale-up 要求のたびに roster が縮む」帰還ループが戻る。resizeLocked を
// 実際に通して、下げても上限が動かないことを固定する。
func TestPool_ResizeKeepsPeakMonotone(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	p := newTestPool("inbox", 16, time.Minute, clk)
	// roster は目標ぶん埋めておく (縮小のみを通すので spawn は起きない)。
	for i := range 20 {
		p.workers = append(p.workers, handleBusyFor(uint64(i), clk, 0))
	}
	p.quarantine = quarantined(20, clk)

	// 20 まで伸ばす。roster は既に 20 なので spawn は走らない。
	require.NoError(t, p.resizeLocked(20))
	require.Equal(t, 20, p.peakDesired)
	ceiling := p.allowedRosterLocked() // 20 + 16 - 20 = 16
	require.Equal(t, 16, ceiling)

	// autoscale が生存数から目標を出して desired を下げていく状況。
	// 上限は peak 由来なので動かない = min(desired, 16) に張り付く。
	for _, n := range []int{16, 12, 8, 4, 1} {
		require.NoError(t, p.resizeLocked(n))
		assert.Equal(t, 20, p.peakDesired, "peak must never decrease")
		assert.Equal(t, min(n, 16), p.allowedRosterLocked(),
			"resize(%d) must not move the ceiling", n)
	}
}

// TestPool_ProtectionClearsOnCompletionNotIdleness pins the release
// condition. idle 判定だと忙しいキューで庇いが外れず、roster が目標を
// 超えたまま張り付く。
func TestPool_ProtectionClearsOnCompletionNotIdleness(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	p := newTestPool("inbox", 2, time.Minute, clk)

	// 復帰直後: job を持っていて、まだ 1 件も完了していない。
	h := handleBusyFor(0, clk, time.Second)
	h.completed.Store(7)
	h.protected = true
	h.completedAtReinstate = 7
	p.workers = []*workerHandle{h}

	p.clearProtectionLocked()
	assert.True(t, h.protected, "still holding the job it came back with")

	// job を 1 件終えた (= 次の job に移った)。idle は一度も観測していない。
	h.completed.Store(8)
	p.clearProtectionLocked()
	assert.False(t, h.protected, "one completion is enough to release it")
}
