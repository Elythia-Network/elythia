package mkqdriver

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mkq"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

func testInfo() abandonInfo {
	return abandonInfo{queue: "inbox", taskType: "ap:inbox", jobID: "1"}
}

func TestRunBounded_DeadlineDisabledRunsInline(t *testing.T) {
	var ranOn context.Context
	sentinel := errors.New("boom")
	h := func(ctx context.Context, _ driver.Task) error {
		ranOn = ctx
		return sentinel
	}
	ctx := context.Background()

	err := runBounded(ctx, h, nil, 0, 0, testInfo(), nil)

	assert.ErrorIs(t, err, sentinel)
	// 期限無効なら ctx を包まない (deadline 付きの ctx を渡さない)。
	_, hasDeadline := ranOn.Deadline()
	assert.False(t, hasDeadline)
}

func TestRunBounded_FastHandlerPassesResultThrough(t *testing.T) {
	sentinel := errors.New("boom")
	var ab abandonCounters

	assert.NoError(t, runBounded(context.Background(),
		func(context.Context, driver.Task) error { return nil },
		nil, time.Minute, time.Second, testInfo(), &ab))

	err := runBounded(context.Background(),
		func(context.Context, driver.Task) error { return sentinel },
		nil, time.Minute, time.Second, testInfo(), &ab)
	assert.ErrorIs(t, err, sentinel)

	cur, total := ab.snapshot("inbox")
	assert.Zero(t, cur)
	assert.Zero(t, total, "nothing was abandoned")
}

// ctx を見る handler は期限で自分から戻るので、放棄されない。
func TestRunBounded_CtxAwareHandlerIsNotAbandoned(t *testing.T) {
	var ab abandonCounters
	h := func(ctx context.Context, _ driver.Task) error {
		<-ctx.Done()
		return ctx.Err()
	}

	err := runBounded(context.Background(), h, nil, 50*time.Millisecond, time.Second, testInfo(), &ab)

	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorIs(t, err, ErrHandlerAbandoned)
	cur, total := ab.snapshot("inbox")
	assert.Zero(t, cur)
	assert.Zero(t, total, "a handler that honours ctx must not be counted as abandoned")
}

// **本題。** ctx を無視する handler は放棄して dispatcher を返す。
func TestRunBounded_CtxIgnoringHandlerIsAbandoned(t *testing.T) {
	var ab abandonCounters
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	entered := make(chan struct{})
	h := func(context.Context, driver.Task) error {
		close(entered)
		<-release // ctx を一切見ない
		return nil
	}

	start := time.Now()
	err := runBounded(context.Background(), h, nil,
		50*time.Millisecond, 50*time.Millisecond, testInfo(), &ab)
	elapsed := time.Since(start)

	<-entered
	require.ErrorIs(t, err, ErrHandlerAbandoned)
	assert.Less(t, elapsed, 2*time.Second, "the dispatcher must not wait for the handler")
	assert.Contains(t, err.Error(), "ap:inbox", "the log/error must identify the job")

	cur, total := ab.snapshot("inbox")
	assert.Equal(t, 1, cur)
	assert.Equal(t, uint64(1), total)
}

// 放棄したあとに遅れて戻ってきたら現存数を戻す。gauge が張り付いたままだと
// 「本当に戻ってこない」のか「遅かっただけ」なのか区別できない。
func TestRunBounded_LateReturnDecrementsCurrent(t *testing.T) {
	var ab abandonCounters
	release := make(chan struct{})
	h := func(context.Context, driver.Task) error {
		<-release
		return nil
	}

	err := runBounded(context.Background(), h, nil,
		30*time.Millisecond, 30*time.Millisecond, testInfo(), &ab)
	require.ErrorIs(t, err, ErrHandlerAbandoned)
	cur, _ := ab.snapshot("inbox")
	require.Equal(t, 1, cur)

	close(release)
	require.Eventually(t, func() bool {
		cur, _ := ab.snapshot("inbox")
		return cur == 0
	}, 3*time.Second, 10*time.Millisecond, "late return must decrement the live count")

	_, total := ab.snapshot("inbox")
	assert.Equal(t, uint64(1), total, "the cumulative total must not be decremented")
}

// **panic を別 goroutine で取りこぼすとプロセスが落ちる。** mkq の runHandler の
// recover は別 goroutine を守らない。
func TestRunBounded_RecoversHandlerPanic(t *testing.T) {
	var ab abandonCounters
	h := func(context.Context, driver.Task) error { panic("kaboom") }

	err := runBounded(context.Background(), h, nil, time.Minute, time.Second, testInfo(), &ab)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kaboom")
	assert.Contains(t, err.Error(), "handler panic")
	cur, _ := ab.snapshot("inbox")
	assert.Zero(t, cur, "a panicking handler returned, so it is not abandoned")
}

// **親 ctx のキャンセルでは諦めない。** mk-go の deliver / inbox /
// relationship processor は `Handle(_ context.Context, ...)` で ctx を捨てて
// いるので、cancel で諦めると job は failed -> retry に回る一方で元の実行は
// 完走する = **scale-down のたびに配送が二重になる**。親キャンセル側の頭打ちは
// mkq の Worker.Stop (stopWorkerTimeout) が持っており、#2658 以前と変わらない。
func TestRunBounded_ParentCancelDoesNotAbandonEarly(t *testing.T) {
	var ab abandonCounters
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	// ctx を無視する handler (本番の deliver / inbox と同じ形)。
	h := func(context.Context, driver.Task) error {
		<-release
		close(handlerDone)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	returned := make(chan error, 1)
	go func() {
		returned <- runBounded(ctx, h, nil, 5*time.Second, 30*time.Millisecond, testInfo(), &ab)
	}()

	// cancel 済みでも、期限まではまだ諦めない。
	select {
	case err := <-returned:
		t.Fatalf("returned too early (%v); a cancelled parent must not abandon a running handler", err)
	case <-time.After(300 * time.Millisecond):
	}
	cur, total := ab.snapshot("inbox")
	assert.Zero(t, cur)
	assert.Zero(t, total)

	// handler が戻れば、その結果がそのまま返る (job は成功扱い)。
	close(release)
	<-handlerDone
	select {
	case err := <-returned:
		assert.NoError(t, err, "the handler completed, so the job must not be failed")
	case <-time.After(3 * time.Second):
		t.Fatal("runBounded did not return after the handler finished")
	}
	cur, _ = ab.snapshot("inbox")
	assert.Zero(t, cur, "a handler that finished is never abandoned")
}

// ctx を見る handler は親キャンセルで自分から戻るので、期限を待たずに返る。
func TestRunBounded_ParentCancelReturnsPromptlyForCtxAwareHandlers(t *testing.T) {
	var ab abandonCounters
	h := func(ctx context.Context, _ driver.Task) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := runBounded(ctx, h, nil, time.Hour, time.Second, testInfo(), &ab)

	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 2*time.Second,
		"a ctx-aware handler must not be held until the deadline")
	cur, _ := ab.snapshot("inbox")
	assert.Zero(t, cur)
}

func TestHandlerDeadlineFor(t *testing.T) {
	// **値も固定する。** 「既定と等しい」だけだと defaultHandlerDeadline を
	// 1 秒にしても通ってしまい、docs が謳う 3600 と食い違う。
	assert.Equal(t, time.Hour, defaultHandlerDeadline)
	assert.Equal(t, time.Hour, handlerDeadlineFor(queue.TaskTypeInbox, 0))
	// chart 系は短いので対象。resync は tick と同じ処理を呼ぶだけ。
	assert.Equal(t, time.Hour, handlerDeadlineFor(queue.TaskTypeChartResync, 0))
	assert.Equal(t, time.Hour, handlerDeadlineFor(queue.TaskTypeChartClean, 0))
	assert.Equal(t, defaultHandlerDeadline, handlerDeadlineFor(queue.TaskTypeDeliver, 0))
	assert.Equal(t, defaultHandlerDeadline, handlerDeadlineFor("plugin:whatever", 0))

	// batch 系は期限を持たない。短い期限を当てると job が retry に回り、
	// 永久に完了しなくなる。
	for _, tt := range []string{
		queue.TaskTypeCleanRemoteFiles, queue.TaskTypeDeleteAccount,
		queue.TaskTypeExport, queue.TaskTypeImport,
	} {
		assert.Zero(t, handlerDeadlineFor(tt, 0), "task type %q must be exempt", tt)
		assert.Zero(t, handlerDeadlineFor(tt, time.Minute), "%q stays exempt under an explicit value", tt)
	}

	// 明示値は対象外の task type にだけ効く。
	assert.Equal(t, time.Minute, handlerDeadlineFor(queue.TaskTypeInbox, time.Minute))
	// 負値で機能ごと無効。
	assert.Zero(t, handlerDeadlineFor(queue.TaskTypeInbox, -1))
}

func TestGraceFor(t *testing.T) {
	assert.Equal(t, 5*time.Second, abandonGrace)
	assert.Equal(t, 5*time.Second, graceFor(time.Hour))
	// 短い期限のほうが猶予より短いなら、猶予もそこまで詰める。
	assert.Equal(t, 100*time.Millisecond, graceFor(100*time.Millisecond))
}

func TestAbandonCounters_ZeroValueAndFloor(t *testing.T) {
	var ab abandonCounters
	cur, total := ab.snapshot("inbox")
	assert.Zero(t, cur)
	assert.Zero(t, total)

	// 放棄していないのに late return が来ても負にしない。
	ab.handlerReturnedLate("inbox")
	cur, _ = ab.snapshot("inbox")
	assert.Zero(t, cur)

	ab.handlerAbandoned("inbox")
	ab.handlerAbandoned("inbox")
	ab.handlerReturnedLate("inbox")
	cur, total = ab.snapshot("inbox")
	assert.Equal(t, 1, cur)
	assert.Equal(t, uint64(2), total)
}

func TestServer_AbandonedHandlerCount(t *testing.T) {
	s := &Server{}
	cur, total := s.AbandonedHandlerCount("inbox")
	assert.Zero(t, cur)
	assert.Zero(t, total)

	s.abandoned.handlerAbandoned("inbox")
	cur, total = s.AbandonedHandlerCount("inbox")
	assert.Equal(t, 1, cur)
	assert.Equal(t, uint64(1), total)
}

// dispatchProbe runs one job through newDispatchHandler and reports the ctx
// the handler saw.
func dispatchProbe(t *testing.T, taskType string, configured time.Duration, ab abandonSink) context.Context {
	t.Helper()
	var seen context.Context
	d := newDispatchHandler(map[string]driver.HandlerFunc{
		taskType: func(ctx context.Context, _ driver.Task) error {
			seen = ctx
			return nil
		},
	}, "inbox", nil, configured, ab)
	_, err := d(context.Background(), &mkq.Job[framedPayload]{
		Data: framedPayload{Type: taskType},
	})
	require.NoError(t, err)
	require.NotNil(t, seen)
	return seen
}

// **dispatch 経路が免除表を引いていることを固定する。** handlerDeadlineFor を
// 通さずに configured をそのまま使っても runBounded 単体のテストは通るので、
// ここで配線を見る。既定 (configured=0) では export だけが期限を持たない。
func TestNewDispatchHandler_ConsultsTheExemptionTable(t *testing.T) {
	bounded := dispatchProbe(t, queue.TaskTypeInbox, 0, nil)
	_, hasDeadline := bounded.Deadline()
	assert.True(t, hasDeadline, "inbox must be bounded by the default")

	exempt := dispatchProbe(t, queue.TaskTypeExport, 0, nil)
	_, hasDeadline = exempt.Deadline()
	assert.False(t, hasDeadline, "export must stay exempt on the dispatch path")

	// 明示値でも免除は維持される。
	exempt = dispatchProbe(t, queue.TaskTypeExport, time.Minute, nil)
	_, hasDeadline = exempt.Deadline()
	assert.False(t, hasDeadline)

	// 負値は全体無効。
	off := dispatchProbe(t, queue.TaskTypeInbox, -1, nil)
	_, hasDeadline = off.Deadline()
	assert.False(t, hasDeadline)
}

// **猶予が dispatch 経路に渡っていることを固定する。** 0 にすると ctx を見る
// handler まで放棄扱いになり、gauge の意味が崩れる。
func TestNewDispatchHandler_PassesTheUnwindGrace(t *testing.T) {
	var ab abandonCounters
	// 期限 50ms、cancel 後 20ms かけて戻る = ctx を見る handler。
	d := newDispatchHandler(map[string]driver.HandlerFunc{
		queue.TaskTypeInbox: func(ctx context.Context, _ driver.Task) error {
			<-ctx.Done()
			time.Sleep(20 * time.Millisecond)
			return ctx.Err()
		},
	}, "inbox", nil, 50*time.Millisecond, &ab)

	_, err := d(context.Background(), &mkq.Job[framedPayload]{
		Data: framedPayload{Type: queue.TaskTypeInbox},
	})

	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorIs(t, err, ErrHandlerAbandoned)
	cur, total := ab.snapshot("inbox")
	assert.Zero(t, cur)
	assert.Zero(t, total, "a handler that unwinds within the grace is not abandoned")
}

// 放棄した handler は隔離した Worker と同じ予算を食う。数えないと
// 「隔離 -> 期限で放棄 -> 復帰 -> また隔離」で上限が永久に効かない。
func TestPool_AbandonedHandlersCountAgainstTheBudget(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	var ab abandonCounters
	p := newTestPool("inbox", 4, time.Minute, clk)
	p.abandoned = &ab

	require.Equal(t, 4, p.allowedRosterLocked())

	// 隔離ゼロでも、放棄が積もれば上限は下がる (4 + 4 - 6 = 2)。
	for range 6 {
		ab.handlerAbandoned("inbox")
	}
	assert.Equal(t, 2, p.allowedRosterLocked())
	assert.Equal(t, 6, p.leakedLocked())

	// 遅れて戻ってきたら予算も戻る。
	ab.handlerReturnedLate("inbox")
	assert.Equal(t, 3, p.allowedRosterLocked())

	// 別キューの放棄はこのキューの予算に影響しない。
	ab.handlerAbandoned("deliver")
	assert.Equal(t, 3, p.allowedRosterLocked())
}

// TestPool_QuarantineThenAbandonThenReinstate pins the composition of #2657
// and #2658, which is the design's central claim and was otherwise untested.
//
//	30 分: 隔離が capacity を戻す (代わりを spawn)
//	 1 時間: 期限が dispatcher を返す (handler の goroutine は放棄)
//	その後: 戻った worker が job を処理し、provenLive が roster に戻す
//
// 放棄が予算に効く (leakedLocked) ので、戻った直後は roster が縮む方向にも
// 働く。両方が同じ予算を見ていることをここで固定する。
func TestPool_QuarantineThenAbandonThenReinstate(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	var ab abandonCounters
	p := newTestPool("inbox", 1, 30*time.Minute, clk)
	p.abandoned = &ab

	// 1) handler に入ったまま 30 分超過 -> 隔離
	wedged := handleBusyFor(0, clk, 31*time.Minute)
	p.workers = []*workerHandle{wedged}
	p.quarantineStuckLocked()
	require.Equal(t, []uint64{0}, seqs(p.quarantine))
	require.Empty(t, p.workers)
	require.Equal(t, 1, p.leakedLocked(), "the quarantined worker counts as leaked")

	// (代わりは adjustLocked が spawn するが、ここでは mkq を触らないので
	//  roster を手で埋めて「差し替え済み」の状態を作る)
	p.workers = []*workerHandle{handleBusyFor(1, clk, time.Second)}

	// 2) 1 時間で期限。dispatcher が返り、handler は放棄される。
	ab.handlerAbandoned("inbox")
	assert.Equal(t, 2, p.leakedLocked(),
		"an abandoned handler is leaked on top of the quarantined worker")

	// 3) dispatcher が戻ったので job を処理できる = completed が進む。
	wedged.busySinceNanos.Store(0)
	wedged.completed.Add(1)
	p.reinstateProvenLiveLocked()

	assert.Equal(t, []uint64{1, 0}, seqs(p.workers), "the worker is back in the roster")
	assert.Empty(t, p.quarantine)
	// **隔離は解けたが放棄した goroutine は残っている。** ここを数え忘れると
	// 上限が永久に効かず、goroutine と DB 接続だけが増え続ける。
	assert.Equal(t, 1, p.leakedLocked())
}

// TestHandlerDeadlines_MatchTaskTypeConstants pins the hard-coded keys in
// handlerDeadlines against the task type constants.
//
// 本体が `internal/queue` を import しない (循環を避けるため) 代わりに、
// **テストだけが定数を参照して値の一致を保証する**。テストは循環を作らない
// ので、ここで縛るのが一番安い。
func TestHandlerDeadlines_MatchTaskTypeConstants(t *testing.T) {
	want := map[string]time.Duration{
		queue.TaskTypeCleanRemoteFiles:        0,
		queue.TaskTypeCleanRemoteNotes:        0,
		queue.TaskTypeOrphanUserCleanup:       0,
		queue.TaskTypeOrphanAttachmentCleanup: 0,
		queue.TaskTypeDeleteAccount:           0,
		queue.TaskTypeClean:                   0,
		queue.TaskTypeRetentionAggregate:      0,
		queue.TaskTypeExport:                  0,
		queue.TaskTypeImport:                  0,
		queue.TaskTypeImportCustomEmojis:      0,
	}
	assert.Equal(t, want, handlerDeadlines,
		"the hard-coded keys must stay in step with the task type constants")
}

// TestPool_CapDrivenShrinkOverridesProtection is the regression test for the
// leak that survived the first attempt at bounding it.
//
// 放棄すると dispatcher が戻り、その worker が job を処理するので provenLive が
// 隔離から roster に戻す。戻した worker は庇われ、mkq の prefetch ですぐ次の
// job を掴むので **protected かつ busy** になり、splitForRemoval の候補から
// 外れる。予算 (leakedLocked) を数えるだけでは roster を縮められず、
// 「隔離 -> 期限で放棄 -> 復帰 -> また隔離」が満員のまま回り続けて goroutine
// だけが増える。実測で allowed=0 / roster=4 のまま増え続ける状態を確認した。
func TestPool_CapDrivenShrinkOverridesProtection(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	var ab abandonCounters
	p := newTestPool("inbox", 4, time.Minute, clk)
	p.abandoned = &ab

	// 復帰直後で庇われ、かつ job を持っている worker が満員。
	for i := range 4 {
		h := handleBusyFor(uint64(i), clk, time.Second)
		h.protected = true
		p.workers = append(p.workers, h)
	}

	// 予算に収まっている縮小 (autoscale 由来) では庇いが効く。
	// **desired を下げて実際に縮小させる。** desired のままだと
	// n == current で adjustLocked が早期 return し、何も検証できない。
	p.desired = 3
	require.Equal(t, 3, p.allowedRosterLocked())
	require.NoError(t, p.adjustLocked())
	assert.Len(t, p.workers, 4,
		"an autoscale-driven shrink must not cancel a protected worker's job")
	p.desired = 4

	// 放棄が積もって予算を超えたら、庇いより予算が優先される。
	for range 8 {
		ab.handlerAbandoned("inbox")
	}
	require.Equal(t, 0, p.allowedRosterLocked())
	require.NoError(t, p.adjustLocked())
	assert.Empty(t, p.workers,
		"a cap-driven shrink must not be vetoed by protection, or the leak never stops")
}

// TestRunBounded_AbandonmentLogIdentifiesTheJob gates 完了条件 2 of #2658:
// the warn must name the queue, the job and how long it ran, or the operator
// cannot get from "something is stuck" to "which task type".
func TestRunBounded_AbandonmentLogIdentifiesTheJob(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var ab abandonCounters
	err := runBounded(context.Background(),
		func(context.Context, driver.Task) error { <-release; return nil },
		nil, 30*time.Millisecond, 30*time.Millisecond,
		abandonInfo{queue: "inbox", taskType: "ap:inbox", jobID: "4242"}, &ab)
	require.ErrorIs(t, err, ErrHandlerAbandoned)

	logged := buf.String()
	for _, want := range []string{`queue=inbox`, `taskType=ap:inbox`, `jobID=4242`, `elapsed=`, `deadline=`} {
		assert.Contains(t, logged, want, "the abandonment warn must carry %s", want)
	}
	// 失敗理由は admin の queue UI にそのまま出るので、error 側にも識別子が要る。
	assert.Contains(t, err.Error(), "ap:inbox")
	assert.Contains(t, err.Error(), "4242")
}

// TestPool_CapLogReportsBothLeakSources gates the Error an operator acts on.
// どちらの予算 (隔離 / 放棄) を食っているかが出ていないと、
// queueStuckWorkerSeconds と queueHandlerDeadlineSeconds のどちらを見るべきか
// 判断できない。
func TestPool_CapLogReportsBothLeakSources(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	clk := &fakeClock{n: int64(time.Hour)}
	var ab abandonCounters
	p := newTestPool("inbox", 4, time.Minute, clk)
	p.abandoned = &ab
	p.quarantine = quarantined(3, clk)
	for range 5 {
		ab.handlerAbandoned("inbox")
	}

	p.reportCapLocked(p.allowedRosterLocked())

	logged := buf.String()
	assert.Contains(t, logged, "quarantined=3")
	assert.Contains(t, logged, "abandonedHandlers=5")
	assert.Contains(t, logged, "configured=4")
}

func TestDriver_AbandonedHandlerCountBeforeStart(t *testing.T) {
	// Server 未構築でも panic せず 0 を返す (metrics の scrape は起動前にも来る)。
	cur, total := (&Driver{}).AbandonedHandlerCount("inbox")
	assert.Zero(t, cur)
	assert.Zero(t, total)
}

// プラグインのジョブにも queueHandlerDeadlineSeconds が効くこと (#2818)。
//
// **個別の上限を持たないから既定へ落ちる**という形なので、handlerDeadlines に
// 名前を足すのを忘れた、という失敗の仕方をしない。
func TestHandlerDeadlineFor_PluginTaskType(t *testing.T) {
	const configured = 90 * time.Second
	assert.Equal(t, configured, handlerDeadlineFor("plugin:demo:refresh", configured))
	// 未設定 (0) なら driver の既定。
	assert.Equal(t, defaultHandlerDeadline, handlerDeadlineFor("plugin:demo:refresh", 0))
	// 負値は「無効」。
	assert.Equal(t, time.Duration(0), handlerDeadlineFor("plugin:demo:refresh", -1))
	// **本体の個別上限を持つ task type に化けていないこと。**
	for taskType := range handlerDeadlines {
		assert.NotContains(t, taskType, "plugin:")
	}
}
