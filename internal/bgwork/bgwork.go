// Package bgwork tracks the fire-and-forget goroutines of one server
// generation, so stopping the generation can wait for them (and cancel them
// near the end of the wait) before the DB pool they use is closed (#3661).
//
// A Group is created with the generation and stopped with it. Code that used
// to write `go fn()` for best-effort work after a response (hooks, audit
// logs, mail) starts it with Group.Go instead. A nil *Group is usable and
// starts plain goroutines with context.Background(), so components built
// without a generation (unit tests, tools) keep working.
package bgwork

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// Group counts the goroutines started through it and owns the context they
// receive. The zero value is not usable; use New.
type Group struct {
	ctx    context.Context
	cancel context.CancelFunc

	// mu は closed と wg.Add を一緒に守る。Stop が closed を立ててから Wait する
	// ので、Wait の後に Add が走ること (WaitGroup の誤用) は起きない。
	mu     sync.Mutex
	closed bool
	// stopping は Stop が始まったら閉じる。待たずに今すぐ済ませたいもの (遅らせて
	// 送る通知など) がこれを見る。
	stopping chan struct{}
	// timers は AfterFunc で待っている仕事。mu で守る。Stop が取り出して今すぐ
	// 走らせるか、発火した側が取り出して走らせるかの、どちらか一方だけが走らせる
	// (集合から取り出せた側が走らせる)。
	timers  map[*pendingTimer]struct{}
	wg      sync.WaitGroup
	running atomic.Int64
	dropped atomic.Int64
}

// New returns a Group whose context is live until Stop.
func New() *Group {
	ctx, cancel := context.WithCancel(context.Background())
	return &Group{ctx: ctx, cancel: cancel, stopping: make(chan struct{}), timers: map[*pendingTimer]struct{}{}}
}

// Context returns the context the goroutines of g receive. Stop cancels it
// near the end of its wait (see Stop). A nil Group returns
// context.Background().
func (g *Group) Context() context.Context {
	if g == nil {
		return context.Background()
	}
	return g.ctx
}

// Go runs fn in a new goroutine with the context of g, recovering panics.
// It reports whether fn was started: after Stop it does not start fn, logs a
// warning with the number of goroutines dropped so far and returns false,
// because the generation fn would use is already going away.
func (g *Group) Go(fn func(ctx context.Context)) bool { return g.GoAs("", fn) }

// GoAs is Go with a label for the warning logged when fn is not started.
func (g *Group) GoAs(what string, fn func(ctx context.Context)) bool {
	if g == nil {
		go run(context.Background(), fn)
		return true
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		// 黙って捨てない。止まった後に起動しようとしたもの (止めきれなかった queue の
		// worker が起動する hook など) は、通知のように冪等でないものもあり、再配送
		// されても戻らない。何件捨てたかを残す。
		g.RecordDropped(what, "the server generation has stopped")
		return false
	}
	g.startLocked(fn)
	g.mu.Unlock()
	return true
}

type pendingTimer struct {
	t  *time.Timer
	fn func()
}

// AfterFunc runs fn in a goroutine of g after d, like time.AfterFunc. When g
// starts stopping before d has passed, fn runs at once instead, inside the
// wait of Stop. It reports whether fn was scheduled: once g has started
// stopping it returns false without scheduling fn, and the caller decides
// what to do (run it now, or drop it). A nil Group uses time.AfterFunc.
//
// 遅らせているだけの仕事 (unreadNotification と Web Push) のためにある。1 件ごとに
// goroutine を待たせると、time.AfterFunc の約 30 倍のメモリを使う (#3661 の
// レビュー2周目 L-2 の実測: 1 件あたり約 3.2KB と約 0.1KB)。待つ間は timer だけを
// 持ち、走らせるときだけ箱の goroutine にする。
//
// 止め始めに、待っていたものを一斉に走らせる。待ちの間に送り切れなければ残る
// が、待ちには上限があるので停止は延びない。止める時点では DB も queue もまだ
// 開いているので、捨てるより今送るほうを選ぶ。
func (g *Group) AfterFunc(d time.Duration, fn func()) bool {
	if g == nil {
		time.AfterFunc(d, fn)
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	pt := &pendingTimer{fn: fn}
	g.timers[pt] = struct{}{}
	pt.t = time.AfterFunc(d, func() { g.fire(pt) })
	return true
}

// fire runs a timer of AfterFunc that has come due, unless Stop already took
// it.
func (g *Group) fire(pt *pendingTimer) {
	g.mu.Lock()
	if _, ok := g.timers[pt]; !ok {
		g.mu.Unlock()
		return
	}
	delete(g.timers, pt)
	// timers に残っていたなら Stop はまだ集合を取り出していない。Stop は closed を
	// 立てるのと同じロックの中で集合を空にするので、ここでは closed は false で、
	// Add は Wait より前に来る。
	g.startLocked(func(context.Context) { pt.fn() })
	g.mu.Unlock()
}

// startLocked starts fn as a goroutine of g. g.mu must be held and g must not
// be closed.
func (g *Group) startLocked(fn func(context.Context)) {
	g.wg.Add(1)
	g.running.Add(1)
	go func() {
		defer g.wg.Done()
		defer g.running.Add(-1)
		run(g.ctx, fn)
	}()
}

// RecordDropped counts one piece of work given up because g is stopping and
// logs a warning with the total so far. Goroutines of g call it when they
// give up on their context (see Stop), so the loss is not silent.
func (g *Group) RecordDropped(what, why string) {
	if g == nil {
		return
	}
	n := g.dropped.Add(1)
	attrs := []any{"dropped", n, "reason", why}
	if what != "" {
		attrs = append(attrs, "what", what)
	}
	slog.Warn("background work dropped while stopping the server generation", attrs...)
}

// Stopping returns a channel closed when Stop begins. Work that is only
// delayed (not running) can watch it to finish now, within the wait of Stop.
// A nil Group returns nil (never closed).
func (g *Group) Stopping() <-chan struct{} {
	if g == nil {
		return nil
	}
	return g.stopping
}

func run(ctx context.Context, fn func(context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("recovered panic in a background goroutine", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	fn(ctx)
}

// Running returns how many goroutines started through g have not returned.
func (g *Group) Running() int64 {
	if g == nil {
		return 0
	}
	return g.running.Load()
}

// Dropped returns how many pieces of work were given up: Go calls after Stop,
// and the RecordDropped calls of goroutines that gave up on their context.
func (g *Group) Dropped() int64 {
	if g == nil {
		return 0
	}
	return g.dropped.Load()
}

// MaxWait bounds how long Stop waits for the goroutines of a Group.
const MaxWait = 3 * time.Second

// Stop refuses new goroutines and waits for the running ones for a quarter
// of what is left of ctx, at most MaxWait (MaxWait when ctx has no deadline,
// no wait when ctx is already done). See StopWithin for how the wait goes.
// It returns how many goroutines were still running when the wait ended.
// Safe to call more than once and on a nil Group.
//
// 待ちを短く取るのは、同じ期限を後の段 (shutdown hook の buffer の flush と chart
// の保存、DB のプールを閉じる待ち) と分け合うため。箱の goroutine は 1 行の書き込み
// や 1 通のメールで、ほとんどはすぐ終わる。数秒を超えて待つのは止まらないもの
// (SMTP の応答待ちなど) で、それを待つために保存の時間を削ると失うものが大きい。
// 上限は stream を閉じる待ち・queue を止める待ちと同じ 3 秒にし、割合はそれらの
// 1/3 より小さい 1/4 にして、後の段に残りの 3/4 を残す。下限は置かない。期限が
// 短いときに下限で待つと、後の段 (hook) の時間を食うため (#3661 のレビュー L-4)。
func (g *Group) Stop(ctx context.Context) int64 {
	limit := MaxWait
	if deadline, ok := ctx.Deadline(); ok {
		limit = waitLimit(time.Until(deadline))
	}
	if ctx.Err() != nil {
		limit = 0
	}
	return g.StopWithin(limit)
}

// waitLimit is the wait of Stop for a ctx with left until its deadline: a
// quarter of it, at most MaxWait, never negative.
func waitLimit(left time.Duration) time.Duration {
	return max(min(MaxWait, left/4), 0)
}

// cancelShare is the part of the wait left after the context of the group is
// canceled.
const cancelShare = 4

// StopWithin refuses new goroutines and waits at most limit for the running
// ones. For the first 3/4 of limit it only waits, so work that has not
// started yet (a hook waiting for a slot) can still run; then it cancels the
// context of g and waits the rest, so work that watches the context returns.
// It returns how many goroutines were still running when the wait ended.
// Safe to call more than once and on a nil Group. Work waiting in AfterFunc
// is started at once when the stop begins.
//
// A goroutine that runs long and returns only when its context is done (a
// plugin's GoContext loop, a download in progress) makes every stop wait
// until the 3/4 point.
//
// 先に ctx を切らないのは、ctx を見て諦めるもの (枠を待つ投稿後の hook) を、待ちの
// 予算の中で走れるなら走らせるため。先に切ると、間に合っていたものまで捨てる
// (#3661 のレビュー M-2)。
func (g *Group) StopWithin(limit time.Duration) int64 {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	if !g.closed {
		// AfterFunc で待っているものは、閉じる前に今すぐ走らせる。閉じるのと同じ
		// ロックの中で集合を空にするので、発火した側と二重には走らない。
		for pt := range g.timers {
			pt.t.Stop()
			delete(g.timers, pt)
			g.startLocked(func(context.Context) { pt.fn() })
		}
		g.closed = true
		close(g.stopping)
	}
	g.mu.Unlock()

	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	grace := limit / cancelShare
	first := time.NewTimer(limit - grace)
	defer first.Stop()
	select {
	case <-done:
		g.cancel()
		return 0
	case <-first.C:
	}
	g.cancel()
	second := time.NewTimer(grace)
	defer second.Stop()
	select {
	case <-done:
		return 0
	case <-second.C:
	}
	left := g.running.Load()
	if left > 0 {
		slog.Warn("background goroutines did not stop before the deadline", "left", left)
	}
	return left
}
