package maintmode

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntervals fixes the relation between the intervals (#3592): the
// report must outlive the longest gap between two writes (twice the idle
// interval, when the watch loop is busy and the heartbeat writes).
func TestIntervals(t *testing.T) {
	assert.Greater(t, ProcTTL, 2*IdlePollInterval)
	assert.GreaterOrEqual(t, IdlePollInterval, PollInterval)
	assert.Greater(t, JudgeLockTTL, PollInterval)
}

func runJudge(t *testing.T, j *Judge, interval time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { j.Run(ctx, interval); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// TestJudge_IdleRunWaitsForTheIdleIntervalOrAWake checks that while no
// maintenance is active Run steps only every IdleInterval, and steps at once
// when Wake fires.
func TestJudge_IdleRunWaitsForTheIdleIntervalOrAWake(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	s := NewStore(c, nil, "pfx:")
	log := &cmdLog{}
	c.AddHook(log)
	wake := make(chan struct{}, 1)
	j := &Judge{Store: s, Counter: &fakeCounter{}, ID: "j", DrainTimeout: time.Second,
		IdleInterval: time.Hour, Wake: wake}
	runJudge(t, j, 10*time.Millisecond)
	// 最初の判定 (状態なし) の最後のロック解除が終わるまで、回数で待つ。sleep で
	// 待つと、重い CI では最初の判定より先に状態を書き、待機の周期を試せない。
	steps := func() (reads, scripts int) {
		log.mu.Lock()
		defer log.mu.Unlock()
		for _, name := range log.cmds {
			switch name {
			case "hgetall":
				reads++
			case "evalsha", "eval":
				scripts++
			}
		}
		return
	}
	require.Eventually(t, func() bool { _, n := steps(); return n >= 1 }, 5*time.Second, time.Millisecond)
	reads, _ := steps()
	require.NoError(t, s.Enter(context.Background(), State{OpID: "op"}))
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, PhaseEntering, phase(t, s), "an idle judge stepped at the short interval")
	// phase() の読みの 1 回を除いて、判定役は読んでいない。
	now, _ := steps()
	assert.Equal(t, reads+1, now, "an idle judge read the state at the short interval")

	wake <- struct{}{}
	require.Eventually(t, func() bool { return phase(t, s) == PhaseHeld }, 5*time.Second, 10*time.Millisecond,
		"Wake did not make the idle judge step")
}

// TestJudge_KeepsPollingWhileAnotherJudgeHolds checks that a judge that is
// not the lock holder is not idle: it steps at the short interval and takes
// over once the holder's lock expires.
func TestJudge_KeepsPollingWhileAnotherJudgeHolds(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	ok, err := s.acquireJudge(ctx, "other", JudgeLockTTL)
	require.NoError(t, err)
	require.True(t, ok)
	j := &Judge{Store: s, Counter: &fakeCounter{}, ID: "j", DrainTimeout: time.Second, IdleInterval: time.Hour}
	runJudge(t, j, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, PhaseEntering, phase(t, s))
	mr.FastForward(JudgeLockTTL + time.Second)
	require.Eventually(t, func() bool { return phase(t, s) == PhaseHeld }, 5*time.Second, 10*time.Millisecond,
		"the judge waited the idle interval while another judge held the lock")
}

// TestJudge_IdleStepReleasesTheLockOnce checks that idle steps do not send
// the release script again once the judge knows it does not hold the lock
// (#3592), and that a judge that held the lock still releases it.
func TestJudge_IdleStepReleasesTheLockOnce(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	s := NewStore(c, nil, "pfx:")
	log := &cmdLog{}
	c.AddHook(log)
	ctx := context.Background()
	scripts := func() int {
		log.mu.Lock()
		defer log.mu.Unlock()
		n := 0
		for _, name := range log.cmds {
			if name == "evalsha" { // 初回だけ NOSCRIPT の後に eval が続く
				n++
			}
		}
		log.cmds = nil
		return n
	}
	j := &Judge{Store: s, Counter: &fakeCounter{}, ID: "j", DrainTimeout: time.Second}
	for range 5 {
		_, err := j.Step(ctx)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, scripts(), "idle steps keep releasing a lock the judge does not hold")

	// ロックを持った後に状態が消えたら、1 回だけ解く。
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	_, err := j.Step(ctx)
	require.NoError(t, err)
	holder, err := s.Judge(ctx)
	require.NoError(t, err)
	require.Equal(t, "j", holder)
	require.NoError(t, s.Clear(ctx, ""))
	scripts()
	for range 3 {
		_, err := j.Step(ctx)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, scripts())
	holder, err = s.Judge(ctx)
	require.NoError(t, err)
	assert.Empty(t, holder, "the judge kept the lock after the maintenance ended")

	// 他の判定役が持っている間は、自分のものではないので解きに行かない。
	require.NoError(t, s.Enter(ctx, State{OpID: "op2"}))
	ok, err := s.acquireJudge(ctx, "other", JudgeLockTTL)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = j.Step(ctx)
	require.NoError(t, err)
	require.NoError(t, s.Clear(ctx, ""))
	scripts()
	_, err = j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, scripts())
	holder, err = s.Judge(ctx)
	require.NoError(t, err)
	assert.Equal(t, "other", holder)
}

// failJudgeLock makes the judge lock script fail while on is set.
type failJudgeLock struct {
	key string
	on  atomic.Bool
}

func (h *failJudgeLock) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *failJudgeLock) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.on.Load() && (cmd.Name() == "evalsha" || cmd.Name() == "eval") {
			for _, a := range cmd.Args() {
				if a == h.key {
					cmd.SetErr(errors.New("injected failure"))
					return cmd.Err()
				}
			}
		}
		return next(ctx, cmd)
	}
}
func (h *failJudgeLock) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// TestJudge_FailedExtensionStillReleasesTheLock checks that a judge that
// failed to extend the lock it held still releases it once the maintenance
// ends: the failure does not prove it lost the lock.
func TestJudge_FailedExtensionStillReleasesTheLock(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = c.Close() })
	s := NewStore(c, nil, "pfx:")
	hook := &failJudgeLock{key: s.JudgeKey()}
	c.AddHook(hook)
	ctx := context.Background()
	j := &Judge{Store: s, Counter: &fakeCounter{}, ID: "j", DrainTimeout: time.Hour}
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	_, err := j.Step(ctx)
	require.NoError(t, err)
	holder, err := s.Judge(ctx)
	require.NoError(t, err)
	require.Equal(t, "j", holder)

	hook.on.Store(true)
	_, err = j.Step(ctx)
	require.Error(t, err)
	hook.on.Store(false)

	require.NoError(t, s.Clear(ctx, ""))
	_, err = j.Step(ctx)
	require.NoError(t, err)
	holder, err = s.Judge(ctx)
	require.NoError(t, err)
	assert.Empty(t, holder, "the judge kept the lock after a failed extension")
}

// TestJudge_DrainTimeoutCountsFromWhenTheProcessLetsGo checks #3592 review
// L-2: a process that starts letting go late (it missed the hint) gets the
// whole drain timeout from then, not from when the judge saw the
// maintenance.
func TestJudge_DrainTimeoutCountsFromWhenTheProcessLetsGo(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &fakeCounter{}
	f.set(elythiaSession(12))
	require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcRunning}))
	j := c.judge(s, f, "j")
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	_, err := j.Step(ctx)
	require.NoError(t, err)

	// 10 秒後に通知に気付いて手放し始めた。
	c.add(10 * time.Second)
	require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcDraining}))
	_, err = j.Step(ctx)
	require.NoError(t, err)
	c.add(25 * time.Second)
	_, err = j.Step(ctx)
	require.NoError(t, err)
	assert.Empty(t, f.terminated, "terminated 25s after the process started letting go (timeout 30s)")

	c.add(5 * time.Second)
	_, err = j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, []int{12}, f.terminated)
}
