package deliveryhealth

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestBreaker(t *testing.T) (*Breaker, *testClock, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	b := NewBreaker(rdb)
	clk := &testClock{now: time.UnixMilli(1_790_000_000_000)}
	b.clock = clk.Now
	// ずらしは別に試す。ここでは待ち時間を正確に見たいので 0 にする。
	b.jitter = func(time.Duration) time.Duration { return 0 }
	return b, clk, mr
}

// check returns Check's decision, delay and hasState as a tuple.
func check(b *Breaker, ctx context.Context, host string) (BreakerDecision, time.Duration, bool) {
	r := b.Check(ctx, host, "")
	return r.Decision, r.Delay, r.HasState
}

// reservedAt returns job's reserved send time as an offset from the clock,
// or -1 when it has none.
func reservedAt(t *testing.T, mr *miniredis.Miniredis, clk *testClock, host, job string) time.Duration {
	t.Helper()
	v := mr.HGet(reservationKey(host), job)
	if v == "" {
		return -1
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	require.NoError(t, err)
	return time.UnixMilli(ms).Sub(clk.Now())
}

func failN(b *Breaker, host string, n int) (open bool) {
	for range n {
		open, _ = b.RecordFailure(context.Background(), host, "")
	}
	return open
}

func TestBreaker_OpensAfterConsecutiveFailures(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	ctx := context.Background()

	assert.False(t, failN(b, "down.example", BreakerThreshold-1), "below the threshold stays closed")
	d, _, state := check(b, ctx, "down.example")
	assert.Equal(t, BreakerAllow, d)
	assert.True(t, state, "counted failures are state (so a success resets them)")

	assert.True(t, failN(b, "down.example", 1))
	d, wait, _ := check(b, ctx, "down.example")
	assert.Equal(t, BreakerDelay, d)
	assert.Equal(t, BreakerInitialInterval, wait, "held until the first probe")
}

// 開いている間の、試行でない失敗 (開いた瞬間に飛んでいた配送など) では間隔を
// 倍にしない。倍にすると、瞬断で並行に失敗しただけで最初の試行が 1 時間後まで
// 延びる。
func TestBreaker_OnlyProbeFailuresDoubleTheInterval(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	ctx := context.Background()
	failN(b, "down.example", BreakerThreshold)
	for range 20 {
		open, untilProbe := b.RecordFailure(ctx, "down.example", "")
		require.True(t, open)
		require.Equal(t, BreakerInitialInterval, untilProbe)
	}
	_, wait, _ := check(b, ctx, "down.example")
	assert.Equal(t, BreakerInitialInterval, wait)
}

// 待たせる時間は 5 分で区切る。次に試す時刻まで丸ごと待たせると、試行が通った後や
// 手で閉じた後も、溜まったジョブが最大 1 時間待ったままになる。
func TestBreaker_HoldIsCapped(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	ctx := context.Background()
	failN(b, "down.example", BreakerThreshold)
	interval := BreakerInitialInterval
	for interval < breakerHoldCap*2 {
		clk.Advance(interval)
		r := b.Check(ctx, "down.example", "")
		require.Equal(t, BreakerProbe, r.Decision)
		_, interval = b.RecordFailure(ctx, "down.example", r.ProbeToken)
	}
	_, wait, _ := check(b, ctx, "down.example")
	assert.Equal(t, breakerHoldCap, wait)
	assert.Equal(t, breakerHoldCap, b.HoldFor(time.Hour))
	assert.Equal(t, time.Second, b.HoldFor(0), "a failure right at the probe time still waits a little")
}

// 開いている間は状態の期限を延ばし続ける。延ばさないと、開いてから 8 日で状態が
// 消え、自動停止より前に何事も無かったかのように撃ち始める。
func TestBreaker_OpenStateOutlivesItsTTLWhileFailing(t *testing.T) {
	b, clk, mr := newTestBreaker(t)
	ctx := context.Background()
	failN(b, "down.example", BreakerThreshold)
	for range 10 {
		mr.FastForward(breakerStateTTL / 2)
		clk.Advance(BreakerMaxInterval)
		r := b.Check(ctx, "down.example", "")
		require.Equal(t, BreakerProbe, r.Decision)
		b.RecordFailure(ctx, "down.example", r.ProbeToken)
	}
	_, _, state := check(b, ctx, "down.example")
	assert.True(t, state)
}

// 成功 (応答があった) で数え直す。失敗が「連続」でなければ開かない。
func TestBreaker_SuccessResetsTheCount(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	ctx := context.Background()

	failN(b, "flaky.example", BreakerThreshold-1)
	b.RecordSuccess(ctx, "flaky.example")
	assert.False(t, failN(b, "flaky.example", BreakerThreshold-1))
	d, _, _ := check(b, ctx, "flaky.example")
	assert.Equal(t, BreakerAllow, d)
}

func TestBreaker_HealthyHostHasNoState(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	d, wait, state := check(b, context.Background(), "ok.example")
	assert.Equal(t, BreakerAllow, d)
	assert.Zero(t, wait)
	assert.False(t, state, "a healthy host needs no RecordSuccess round trip")
}

// 半開は 1 件だけ通す。成功で閉じ、ほかのジョブも通るようになる。
func TestBreaker_HalfOpenLetsOneProbeAndClosesOnSuccess(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	ctx := context.Background()
	failN(b, "down.example", BreakerThreshold)

	clk.Advance(BreakerInitialInterval)
	d, _, _ := check(b, ctx, "down.example")
	require.Equal(t, BreakerProbe, d, "the first job after the wait is the probe")
	d2, wait, _ := check(b, ctx, "down.example")
	assert.Equal(t, BreakerDelay, d2, "only one probe at a time")
	// 試行中は短く待たせる。試行が通った後に溜まったジョブを長く待たせないため。
	assert.Equal(t, breakerProbeWait, wait)

	b.RecordSuccess(ctx, "down.example")
	d3, _, state := check(b, ctx, "down.example")
	assert.Equal(t, BreakerAllow, d3)
	assert.False(t, state)
}

// 試行が失敗したら間隔を倍にして開き直し、上限で頭打ちにする。
func TestBreaker_FailedProbeDoublesTheIntervalUpToTheCap(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	ctx := context.Background()
	failN(b, "down.example", BreakerThreshold)

	want := BreakerInitialInterval
	for range 10 {
		clk.Advance(want)
		r := b.Check(ctx, "down.example", "")
		require.Equal(t, BreakerProbe, r.Decision)
		open, untilProbe := b.RecordFailure(ctx, "down.example", r.ProbeToken)
		require.True(t, open)
		want = min(want*2, BreakerMaxInterval)
		require.Equal(t, want, untilProbe)
		d, wait, _ := check(b, ctx, "down.example")
		require.Equal(t, BreakerDelay, d)
		require.Equal(t, min(want, breakerHoldCap), wait, "a held job re-checks at most every breakerHoldCap")
	}
	assert.Equal(t, BreakerMaxInterval, want, "the loop must reach the cap")
	list, err := b.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, int(BreakerMaxInterval.Seconds()), list[0].ProbeIntervalSec)
}

// probe が戻らなくても (プロセスが落ちた等)、ロックが切れれば次の試行に移る。
// 「開いたまま戻らない」を作らない。
func TestBreaker_StuckProbeLockExpires(t *testing.T) {
	b, clk, mr := newTestBreaker(t)
	ctx := context.Background()
	failN(b, "down.example", BreakerThreshold)
	clk.Advance(BreakerInitialInterval)
	d, _, _ := check(b, ctx, "down.example")
	require.Equal(t, BreakerProbe, d)

	mr.FastForward(breakerProbeLockTTL)
	d, _, _ = check(b, ctx, "down.example")
	assert.Equal(t, BreakerProbe, d)
}

func TestBreaker_CloseByHand(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	ctx := context.Background()
	failN(b, "down.example", BreakerThreshold)

	list, err := b.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.True(t, list[0].Open)
	assert.Equal(t, BreakerThreshold, list[0].ConsecutiveFailures)
	require.NotNil(t, list[0].OpenedAt)
	require.NotNil(t, list[0].NextProbeAt)
	assert.Equal(t, int(BreakerInitialInterval.Seconds()), list[0].ProbeIntervalSec)

	require.NoError(t, b.Close(ctx, "down.example"))
	d, _, _ := check(b, ctx, "down.example")
	assert.Equal(t, BreakerAllow, d)
	list, err = b.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// 429 は止めずに間隔を空ける。ブレーカーは開かない。
func TestBreaker_ThrottleHoldsWithoutOpening(t *testing.T) {
	b, _, mr := newTestBreaker(t)
	ctx := context.Background()

	b.Throttle(ctx, "busy.example", 30*time.Second)
	d, wait, state := check(b, ctx, "busy.example")
	assert.Equal(t, BreakerDelay, d)
	assert.Equal(t, 30*time.Second, wait)
	assert.False(t, state, "a throttle alone is not breaker state")

	list, err := b.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.False(t, list[0].Open)
	assert.NotNil(t, list[0].ThrottledUntil)

	mr.FastForward(30 * time.Second)
	d, _, _ = check(b, ctx, "busy.example")
	assert.Equal(t, BreakerAllow, d)
}

func TestBreaker_ThrottleBounds(t *testing.T) {
	b, clk, mr := newTestBreaker(t)
	ctx := context.Background()

	b.Throttle(ctx, "a.example", 0)
	_, wait, _ := check(b, ctx, "a.example")
	assert.Equal(t, ThrottleDefault, wait, "no usable Retry-After uses the default")

	b.Throttle(ctx, "b.example", 48*time.Hour)
	r := b.Check(ctx, "b.example", "j")
	assert.Equal(t, breakerHoldCap, r.Delay, "the wait itself is capped; the job re-checks when it wakes")
	assert.Equal(t, ThrottleMax, reservedAt(t, mr, clk, "b.example", "j"), "a huge Retry-After is capped")
}

// 成功で閉じても、429 で間隔を空けている間は一覧から消さない。
func TestBreaker_SuccessKeepsAThrottledHostListed(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	ctx := context.Background()
	failN(b, "x.example", BreakerThreshold)
	b.Throttle(ctx, "x.example", time.Minute)

	b.RecordSuccess(ctx, "x.example")
	list, err := b.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.False(t, list[0].Open)
	assert.NotNil(t, list[0].ThrottledUntil)
}

// 失敗を数えているだけの hash は、失敗が止まれば時間で消える。
func TestBreaker_ForgetsStaleFailures(t *testing.T) {
	b, _, mr := newTestBreaker(t)
	ctx := context.Background()
	failN(b, "once.example", 1)
	mr.FastForward(breakerFailureWindow)
	_, _, state := check(b, ctx, "once.example")
	assert.False(t, state)
}

// 待ち時間をずらす。一斉に起きると、半開の瞬間に全ジョブが Redis を叩く。
func TestBreaker_JitterSpreadsTheWait(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	ctx := context.Background()
	var gotMax time.Duration
	b.jitter = func(max time.Duration) time.Duration { gotMax = max; return max }
	failN(b, "down.example", BreakerThreshold)
	_, wait, _ := check(b, ctx, "down.example")
	assert.Equal(t, BreakerInitialInterval/10, gotMax)
	assert.Equal(t, BreakerInitialInterval+BreakerInitialInterval/10, wait)

	// 待ちは 5 分で区切るので、ずれも最大 30 秒 (長い待ちをさらに大きく延ばさない)。
	assert.Equal(t, breakerHoldCap+breakerHoldCap/10, b.HoldFor(time.Hour))
}

func TestBreaker_NilAndEmptyHostAreHarmless(t *testing.T) {
	var b *Breaker
	ctx := context.Background()
	d, _, _ := check(b, ctx, "x")
	assert.Equal(t, BreakerAllow, d)
	open, _ := b.RecordFailure(ctx, "x", "")
	assert.False(t, open)
	assert.Equal(t, time.Minute, b.HoldFor(time.Minute))
	assert.NoError(t, b.Close(ctx, "x"))
	b.RecordSuccess(ctx, "x")
	b.Throttle(ctx, "x", time.Second)
	list, err := b.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)

	nb, _, _ := newTestBreaker(t)
	d, _, _ = check(nb, ctx, "")
	assert.Equal(t, BreakerAllow, d)
	open, _ = nb.RecordFailure(ctx, "", "")
	assert.False(t, open)
	assert.Nil(t, NewBreaker(nil))
}

// Redis が読めなければ通す (観測の仕組みが全配送を止める側に倒れない)。
func TestBreaker_RedisDownAllows(t *testing.T) {
	b, _, mr := newTestBreaker(t)
	mr.Close()
	d, _, state := check(b, context.Background(), "x.example")
	assert.Equal(t, BreakerAllow, d)
	assert.False(t, state)
}

// queue ノードが複数あっても判断を共有する: 別のノードで数えた失敗で開き、
// 半開の試行はノードをまたいで 1 件だけ。
func TestBreaker_SharedAcrossNodes(t *testing.T) {
	mr := miniredis.RunT(t)
	clk := &testClock{now: time.UnixMilli(1_790_000_000_000)}
	node := func() *Breaker {
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		b := NewBreaker(rdb)
		b.clock = clk.Now
		b.jitter = func(time.Duration) time.Duration { return 0 }
		return b
	}
	a, b := node(), node()
	ctx := context.Background()

	failN(a, "down.example", BreakerThreshold)
	d, _, _ := check(b, ctx, "down.example")
	assert.Equal(t, BreakerDelay, d, "node b sees what node a counted")

	clk.Advance(BreakerInitialInterval)
	d, _, _ = check(a, ctx, "down.example")
	require.Equal(t, BreakerProbe, d)
	d, _, _ = check(b, ctx, "down.example")
	assert.Equal(t, BreakerDelay, d, "one probe across nodes")

	b.RecordSuccess(ctx, "down.example")
	d, _, _ = check(a, ctx, "down.example")
	assert.Equal(t, BreakerAllow, d, "a success on node b closes it for node a")
}

// 既定のずらしは [0, max) に収まる (待ち時間を縮めない)。
func TestBreaker_DefaultJitterStaysInRange(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	b := NewBreaker(rdb)
	assert.Zero(t, b.jitter(0))
	for range 100 {
		j := b.jitter(time.Second)
		assert.GreaterOrEqual(t, j, time.Duration(0))
		assert.Less(t, j, time.Second)
	}
}

// 状態が消えたのに索引だけ残ったホスト (TTL で消えた等) は一覧から落とし、
// 索引も掃除する。
func TestBreaker_ListDropsStaleIndexEntries(t *testing.T) {
	b, _, mr := newTestBreaker(t)
	ctx := context.Background()
	failN(b, "down.example", BreakerThreshold)
	_, err := mr.ZAdd(breakerIndexKey(), 1, "gone.example")
	require.NoError(t, err)

	list, err := b.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "down.example", list[0].Host)
	members, err := mr.ZMembers(breakerIndexKey())
	require.NoError(t, err)
	assert.Equal(t, []string{"down.example"}, members)
}

// 一覧が読めないことは隠さない (管理画面が「止めているホストは無い」と誤って
// 見せないように)。
func TestBreaker_ListReportsRedisErrors(t *testing.T) {
	b, _, mr := newTestBreaker(t)
	failN(b, "down.example", BreakerThreshold)
	mr.Close()
	_, err := b.List(context.Background())
	assert.Error(t, err)
	// 閉じられなかったことも隠さない (管理画面が「再開した」と見せないように)。
	assert.Error(t, b.Close(context.Background(), "down.example"))
	open, _ := b.RecordFailure(context.Background(), "down.example", "")
	assert.False(t, open, "an unreadable breaker never holds deliveries")
}

// 429 で待たせたジョブには、明けた時刻から 1 件ずつずらした送信時刻を予約させる。
// 明けた瞬間に一斉に送るとまた 429 になる。起きたジョブは待ち直さずに送る。
func TestBreaker_ThrottleReservesSpacedSlots(t *testing.T) {
	b, clk, mr := newTestBreaker(t)
	ctx := context.Background()
	b.Throttle(ctx, "busy.example", time.Minute)

	perSecond := map[int64]int{}
	for i := range 1000 {
		job := fmt.Sprintf("j%d", i)
		r := b.Check(ctx, "busy.example", job)
		require.Equal(t, BreakerDelay, r.Decision)
		at := reservedAt(t, mr, clk, "busy.example", job)
		assert.Equal(t, time.Minute+time.Duration(i)*throttleSpacing, at, "job %d", i)
		assert.Equal(t, min(at, breakerHoldCap), r.Delay, "job %d", i)
		perSecond[int64(at/time.Second)]++
	}
	for sec, n := range perSecond {
		assert.LessOrEqual(t, n, int(time.Second/throttleSpacing), "second %d", sec)
	}

	// 明けた後、自分の時刻に起きたジョブは送る (待ち直さない)。
	clk.Advance(time.Minute)
	mr.FastForward(time.Minute)
	r := b.Check(ctx, "busy.example", "j0")
	assert.Equal(t, BreakerAllow, r.Decision)
	assert.Equal(t, time.Duration(-1), reservedAt(t, mr, clk, "busy.example", "j0"), "a used reservation is dropped")
	for range 3 {
		d, _, _ := check(b, ctx, "busy.example")
		assert.Equal(t, BreakerAllow, d, "new deliveries after the hold are not paced")
	}
}

// 待ちは 5 分で区切り、起きたジョブは自分の予約まで待ち直す (列の最後尾に並び
// 直さない)。並び直すと、列が 5 分より長いときに後ろのジョブが永久に送れない。
func TestBreaker_HeldJobKeepsItsSlot(t *testing.T) {
	b, clk, mr := newTestBreaker(t)
	ctx := context.Background()
	b.Throttle(ctx, "busy.example", time.Minute)
	for i := range 1000 {
		b.Check(ctx, "busy.example", fmt.Sprintf("j%d", i))
	}
	// j800 の予約は 60s + 800 x 0.5s = 460s。
	tail, err := mr.Get(slotKey("busy.example"))
	require.NoError(t, err)

	clk.Advance(breakerHoldCap)
	mr.FastForward(breakerHoldCap)
	r := b.Check(ctx, "busy.example", "j800")
	require.Equal(t, BreakerDelay, r.Decision)
	assert.Equal(t, 160*time.Second, r.Delay, "waits for its own slot")
	after, err := mr.Get(slotKey("busy.example"))
	require.NoError(t, err)
	assert.Equal(t, tail, after, "waking does not reserve again")

	clk.Advance(160 * time.Second)
	mr.FastForward(160 * time.Second)
	assert.Equal(t, BreakerAllow, b.Check(ctx, "busy.example", "j800").Decision)
}

// 予約の時刻が来たときにまだ止めていれば (429 が再発した)、列の最後尾に並び直す。
func TestBreaker_DueJobRequeuesWhileStillHeld(t *testing.T) {
	b, clk, mr := newTestBreaker(t)
	ctx := context.Background()
	b.Throttle(ctx, "busy.example", time.Minute)
	b.Check(ctx, "busy.example", "a")
	b.Check(ctx, "busy.example", "b") // 60.5s

	clk.Advance(time.Minute)
	mr.FastForward(time.Minute)
	b.Throttle(ctx, "busy.example", time.Minute)
	r := b.Check(ctx, "busy.example", "a")
	require.Equal(t, BreakerDelay, r.Decision)
	assert.Equal(t, time.Minute, reservedAt(t, mr, clk, "busy.example", "a"))
}

// 手で閉じると 429 の停止と予約も消え、遅延中のジョブは起きたときに送る。
// 予約の列は溜まった件数 x 間隔まで伸びるので、消さないと手で解けない。
func TestBreaker_CloseReleasesReservations(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	ctx := context.Background()
	b.Throttle(ctx, "busy.example", time.Minute)
	for i := range 1000 {
		b.Check(ctx, "busy.example", fmt.Sprintf("j%d", i))
	}

	require.NoError(t, b.Close(ctx, "busy.example"))
	list, err := b.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
	assert.Equal(t, BreakerAllow, b.Check(ctx, "busy.example", "j999").Decision)
}

// 自動で閉じる (成功) ときは予約に触らない。送り出し中の配送はどれも成功で
// 終わるので、消すと残りが一斉に起きる。
func TestBreaker_SuccessKeepsReservations(t *testing.T) {
	b, clk, mr := newTestBreaker(t)
	ctx := context.Background()
	b.Throttle(ctx, "busy.example", time.Minute)
	for i := range 10 {
		b.Check(ctx, "busy.example", fmt.Sprintf("j%d", i))
	}
	clk.Advance(time.Minute)
	mr.FastForward(time.Minute)
	require.Equal(t, BreakerAllow, b.Check(ctx, "busy.example", "j0").Decision)
	b.RecordSuccess(ctx, "busy.example")
	list, err := b.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1, "still listed while reservations remain")

	r := b.Check(ctx, "busy.example", "j9")
	assert.Equal(t, BreakerDelay, r.Decision)
	assert.Equal(t, 9*throttleSpacing, r.Delay)
}

// 429 の期限が過ぎても予約が残っている間は一覧に出し、最後の予約時刻を見せる。
func TestBreaker_ListShowsTheReservationTail(t *testing.T) {
	b, clk, mr := newTestBreaker(t)
	ctx := context.Background()
	b.Throttle(ctx, "busy.example", time.Minute)
	for i := range 100 {
		b.Check(ctx, "busy.example", fmt.Sprintf("j%d", i))
	}
	tail := clk.Now().Add(time.Minute + 99*throttleSpacing)

	clk.Advance(time.Minute)
	mr.FastForward(time.Minute)
	list, err := b.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Nil(t, list[0].ThrottledUntil)
	require.NotNil(t, list[0].ReservedUntil)
	assert.True(t, tail.Equal(*list[0].ReservedUntil), "%v != %v", tail, *list[0].ReservedUntil)

	clk.Advance(time.Minute)
	mr.FastForward(time.Minute)
	list, err = b.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list, "dropped once the last reservation has passed")
}

// 並行して返った 429 で予約が重ならない。数え直す形だと、明けた直後に各回の
// 先頭が一斉に起きる。
func TestBreaker_ConcurrentThrottlesKeepOneQueue(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	ctx := context.Background()
	seen := map[time.Duration]bool{}
	perSecond := map[int64]int{}
	for i := range 300 {
		if i%10 == 0 {
			b.Throttle(ctx, "busy.example", time.Minute)
		}
		d, wait, _ := check(b, ctx, "busy.example")
		require.Equal(t, BreakerDelay, d)
		require.False(t, seen[wait], "job %d got a slot that was already reserved: %v", i, wait)
		seen[wait] = true
		perSecond[int64(wait/time.Second)]++
	}
	for sec, n := range perSecond {
		assert.LessOrEqual(t, n, int(time.Second/throttleSpacing), "second %d", sec)
	}
}

// 送り出しの途中で 429 が再発しても列は 1 本のまま。新しい予約は古い予約の
// 後ろに並び、止める期限が長ければその期限から始まる。
func TestBreaker_RecurringThrottleJoinsTheQueue(t *testing.T) {
	b, clk, mr := newTestBreaker(t)
	ctx := context.Background()
	b.Throttle(ctx, "busy.example", time.Minute)
	for range 200 {
		check(b, ctx, "busy.example")
	}
	// 最後の予約は 60s + 199 x 0.5s = 159.5s。

	clk.Advance(time.Minute)
	mr.FastForward(time.Minute)
	b.Throttle(ctx, "busy.example", time.Minute)
	_, wait, _ := check(b, ctx, "busy.example")
	assert.Equal(t, 100*time.Second, wait, "queued behind the last reservation (160s from the first 429)")

	b.Throttle(ctx, "busy.example", 5*time.Minute)
	_, wait, _ = check(b, ctx, "busy.example")
	assert.Equal(t, 5*time.Minute, wait, "a longer Retry-After moves the start out")
}

// 予約のキーは最後の予約の後まで残る (途中で消えると列の最後尾を忘れて重なる)。
func TestBreaker_ReservationOutlivesItsSlot(t *testing.T) {
	b, _, mr := newTestBreaker(t)
	ctx := context.Background()
	b.Throttle(ctx, "busy.example", time.Minute)
	for range 10 {
		check(b, ctx, "busy.example")
	}
	ttl := mr.TTL(slotKey("busy.example"))
	assert.Greater(t, ttl, time.Minute+9*throttleSpacing)
}

// 古い試行 (ロックが切れた後に返ってきたもの) は、次の試行の枠を外さず、間隔も
// 倍にしない。
func TestBreaker_StaleProbeDoesNotTouchTheNextOne(t *testing.T) {
	b, clk, mr := newTestBreaker(t)
	ctx := context.Background()
	failN(b, "down.example", BreakerThreshold)
	clk.Advance(BreakerInitialInterval)
	stale := b.Check(ctx, "down.example", "")
	require.Equal(t, BreakerProbe, stale.Decision)
	mr.FastForward(breakerProbeLockTTL)
	current := b.Check(ctx, "down.example", "")
	require.Equal(t, BreakerProbe, current.Decision)
	require.NotEqual(t, stale.ProbeToken, current.ProbeToken)

	open, untilProbe := b.RecordFailure(ctx, "down.example", stale.ProbeToken)
	assert.True(t, open)
	assert.Zero(t, untilProbe, "a stale probe does not push the next probe out")
	d, _, _ := check(b, ctx, "down.example")
	assert.Equal(t, BreakerDelay, d, "the current probe still owns the slot")

	_, untilProbe = b.RecordFailure(ctx, "down.example", current.ProbeToken)
	assert.Equal(t, 2*BreakerInitialInterval, untilProbe, "the current probe's failure doubles")
}

// こちら側の失敗で終わった試行は枠だけ返す。返さないと 5 分間次の試行ができない。
func TestBreaker_ReleaseProbe(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	ctx := context.Background()
	failN(b, "down.example", BreakerThreshold)
	clk.Advance(BreakerInitialInterval)
	r := b.Check(ctx, "down.example", "")
	require.Equal(t, BreakerProbe, r.Decision)

	b.ReleaseProbe(ctx, "down.example", "someone-else")
	d, _, _ := check(b, ctx, "down.example")
	assert.Equal(t, BreakerDelay, d, "only the owner can release")

	b.ReleaseProbe(ctx, "down.example", r.ProbeToken)
	d, _, _ = check(b, ctx, "down.example")
	assert.Equal(t, BreakerProbe, d, "the next job becomes the probe right away")

	var nb *Breaker
	nb.ReleaseProbe(ctx, "x", "t")
	b.ReleaseProbe(ctx, "down.example", "")
}

// 索引は書くたびに、状態の期限より古い行を刈る (管理画面を開かなくても増え続けない)。
func TestBreaker_IndexIsTrimmedOnWrites(t *testing.T) {
	for name, write := range map[string]func(b *Breaker){
		"open":     func(b *Breaker) { failN(b, "down.example", BreakerThreshold) },
		"throttle": func(b *Breaker) { b.Throttle(context.Background(), "busy.example", time.Minute) },
	} {
		t.Run(name, func(t *testing.T) {
			b, clk, mr := newTestBreaker(t)
			old := clk.Now().Add(-breakerStateTTL - time.Hour).UnixMilli()
			_, err := mr.ZAdd(breakerIndexKey(), float64(old), "ancient.example")
			require.NoError(t, err)
			write(b)
			members, err := mr.ZMembers(breakerIndexKey())
			require.NoError(t, err)
			assert.NotContains(t, members, "ancient.example")
		})
	}
}
