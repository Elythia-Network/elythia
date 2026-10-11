package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/maintmode"
	"github.com/elythia-network/elythia/internal/pgactivity"
	"github.com/elythia-network/elythia/internal/testutil"
)

// fakeCounter stands in for pg_stat_activity.
type fakeCounter struct {
	mu       sync.Mutex
	sessions []maintmode.Blocker
	err      error
}

func (f *fakeCounter) set(sessions ...maintmode.Blocker) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = sessions
}

func (f *fakeCounter) Sessions(context.Context) ([]maintmode.Blocker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]maintmode.Blocker(nil), f.sessions...), f.err
}

func (f *fakeCounter) Terminate(context.Context, maintmode.Blocker) error { return nil }

func psqlSession(pid int) maintmode.Blocker {
	return maintmode.Blocker{Server: "primary", Session: pgactivity.Session{PID: pid, User: "app", Application: "psql", SameRole: true}}
}

type harness struct {
	e              env
	stdout, stderr bytes.Buffer
	store          *maintmode.Store
	mr             *miniredis.Miniredis
	counter        *fakeCounter
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = c.Close() })
	h := &harness{store: maintmode.NewStore(c, nil, "pfx:"), mr: mr, counter: &fakeCounter{}}
	h.e = env{
		stdout: &h.stdout,
		stderr: &h.stderr,
		loadConfig: func(string) (*config.Config, error) {
			return &config.Config{}, nil
		},
		openStore: func(*config.Config) (*maintmode.Store, func(), error) {
			return h.store, func() {}, nil
		},
		counter:      func(*config.Config, string) maintmode.Counter { return h.counter },
		now:          time.Now,
		pollInterval: 10 * time.Millisecond,
	}
	return h
}

func (h *harness) state(t *testing.T) *maintmode.State {
	t.Helper()
	st, err := h.store.Get(context.Background())
	require.NoError(t, err)
	return st
}

func (h *harness) judgeHolder(t *testing.T) string {
	t.Helper()
	id, err := h.store.Judge(context.Background())
	require.NoError(t, err)
	return id
}

// reportAll keeps writing a report with state for each id until ctx ends,
// standing in for running processes.
func (h *harness) reportAll(ctx context.Context, state string, ids ...string) {
	for _, id := range ids {
		_ = h.store.Report(context.Background(), maintmode.ProcReport{ID: id, State: state})
	}
	go func() {
		for {
			for _, id := range ids {
				_ = h.store.Report(context.Background(), maintmode.ProcReport{ID: id, State: state})
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
}

func TestEnter_HeldOnceNoSessionRemains(t *testing.T) {
	h := newHarness(t)
	h.counter.set(psqlSession(5))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.reportAll(ctx, maintmode.ProcDrained, "a")
	go func() {
		time.Sleep(100 * time.Millisecond)
		h.counter.set()
	}()
	assert.Equal(t, 0, enter(h.e, []string{"-wait", "10s", "-reason", "重い migration"}))
	assert.Contains(t, h.stdout.String(), "the maintenance is held")
	st := h.state(t)
	assert.Equal(t, maintmode.PhaseHeld, st.Phase)
	assert.Equal(t, "重い migration", st.Reason)
	assert.Empty(t, h.judgeHolder(t), "終わった CLI が判定役のロックを持ったまま")
}

func TestEnter_TimeoutLeavesTheMaintenanceEntering(t *testing.T) {
	h := newHarness(t)
	h.counter.set(psqlSession(6))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.reportAll(ctx, maintmode.ProcDraining, "a")
	assert.Equal(t, 1, enter(h.e, []string{"-wait", "100ms"}))
	assert.Equal(t, maintmode.PhaseEntering, h.state(t).Phase)
	assert.Contains(t, h.stderr.String(), "processes not drained yet: a(draining)")
	assert.Contains(t, h.stderr.String(), "pid=6")
	assert.Contains(t, h.stderr.String(), "stays at entering")
	assert.Empty(t, h.judgeHolder(t), "待ちを諦めた CLI が判定役のロックを持ったまま")
}

func TestEnter_JudgeErrorIsShown(t *testing.T) {
	h := newHarness(t)
	h.counter.err = errors.New("pg down")
	assert.Equal(t, 1, enter(h.e, []string{"-wait", "50ms"}))
	assert.Contains(t, h.stderr.String(), "judge error: pg down")
}

func TestEnter_NoWait(t *testing.T) {
	h := newHarness(t)
	assert.Equal(t, 0, enter(h.e, []string{"-wait", "0"}))
	assert.Equal(t, maintmode.PhaseEntering, h.state(t).Phase)
	assert.Empty(t, h.judgeHolder(t))
}

// 本体のプロセスが判定役のときは、CLI は判定せずに状態を見て待つ。
func TestEnter_AnotherJudgeMovesItToHeld(t *testing.T) {
	h := newHarness(t)
	other := &maintmode.Judge{Store: h.store, Counter: &fakeCounter{}, ID: "server-1", DrainTimeout: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 本体のプロセスが先にロックを持っている。CLI の counter は接続が残っていると
	// 答えるので、held にできるのは other だけ。
	h.counter.set(psqlSession(7))
	require.NoError(t, h.mr.Set(h.store.JudgeKey(), "server-1"))
	h.mr.SetTTL(h.store.JudgeKey(), maintmode.JudgeLockTTL)
	go func() {
		for ctx.Err() == nil {
			// 状態が無いと、判定役はロックを手放す。enter が書くまで待つ。
			if st, _ := h.store.Get(ctx); st != nil {
				_, _ = other.Step(ctx)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	assert.Equal(t, 0, enter(h.e, []string{"-wait", "10s"}))
	assert.Equal(t, maintmode.PhaseHeld, h.state(t).Phase)
}

func TestEnter_StateReplacedWhileWaiting(t *testing.T) {
	h := newHarness(t)
	h.counter.set(psqlSession(8))
	calls := 0
	h.e.now = func() time.Time {
		calls++
		if calls == 3 {
			_ = h.store.Clear(context.Background(), "")
		}
		return time.Now()
	}
	assert.Equal(t, 1, enter(h.e, []string{"-wait", "10s"}))
	assert.Contains(t, h.stderr.String(), "ended or replaced")
}

func TestEnter_ResumedWhileWaiting(t *testing.T) {
	h := newHarness(t)
	h.counter.set(psqlSession(9))
	calls := 0
	h.e.now = func() time.Time {
		calls++
		if calls == 3 {
			_ = h.store.SetPhase(context.Background(), "", maintmode.PhaseResuming)
		}
		return time.Now()
	}
	assert.Equal(t, 1, enter(h.e, []string{"-wait", "10s"}))
	assert.Contains(t, h.stderr.String(), "is resuming")
}

func TestEnter_AlreadyActive(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.store.Enter(context.Background(), maintmode.State{OpID: "op"}))
	assert.Equal(t, 1, enter(h.e, []string{"-wait", "0"}))
	assert.Contains(t, h.stderr.String(), "already active")
}

func TestExit_ClearsOnceEveryProcessServes(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.store.Enter(context.Background(), maintmode.State{OpID: "op", Phase: maintmode.PhaseHeld}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.reportAll(ctx, maintmode.ProcResuming, "a")
	go func() {
		time.Sleep(100 * time.Millisecond)
		h.reportAll(ctx, maintmode.ProcRunning, "a")
	}()
	assert.Equal(t, 0, exit(h.e, []string{"-wait", "10s"}))
	assert.Nil(t, h.state(t))
	assert.Contains(t, h.stdout.String(), "the maintenance is over")
}

func TestExit_TimeoutLeavesResuming(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.store.Enter(context.Background(), maintmode.State{OpID: "op"}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.reportAll(ctx, maintmode.ProcResumeFailed, "a")
	assert.Equal(t, 1, exit(h.e, []string{"-wait", "100ms"}))
	assert.Equal(t, maintmode.PhaseResuming, h.state(t).Phase)
	assert.Contains(t, h.stderr.String(), "state=resume_failed")
	assert.Contains(t, h.stderr.String(), "stays at resuming")
}

func TestExit_NoWaitForceAndNothingActive(t *testing.T) {
	h := newHarness(t)
	assert.Equal(t, 0, exit(h.e, nil))
	assert.Contains(t, h.stdout.String(), "no maintenance is active")

	require.NoError(t, h.store.Enter(context.Background(), maintmode.State{OpID: "op"}))
	assert.Equal(t, 0, exit(h.e, []string{"-wait", "0"}))
	assert.Equal(t, maintmode.PhaseResuming, h.state(t).Phase)

	assert.Equal(t, 0, exit(h.e, []string{"-force"}))
	assert.Nil(t, h.state(t))
}

func TestWait(t *testing.T) {
	h := newHarness(t)
	assert.Equal(t, 0, wait(h.e, nil))
	assert.Contains(t, h.stdout.String(), "no maintenance is active")

	// 判定役のいない entering を、wait が判定して held にする。
	require.NoError(t, h.store.Enter(context.Background(), maintmode.State{OpID: "op"}))
	assert.Equal(t, 0, wait(h.e, []string{"-wait", "10s"}))
	assert.Equal(t, maintmode.PhaseHeld, h.state(t).Phase)
	// held なら、すぐ戻る。
	assert.Equal(t, 0, wait(h.e, []string{"-wait", "10s"}))

	require.NoError(t, h.store.SetPhase(context.Background(), "op", maintmode.PhaseResuming))
	assert.Equal(t, 0, wait(h.e, []string{"-wait", "10s"}))
	assert.Nil(t, h.state(t))
}

func TestStatus(t *testing.T) {
	h := newHarness(t)
	assert.Equal(t, 0, status(h.e, nil))
	assert.Contains(t, h.stdout.String(), "maintenance: none")
	assert.Contains(t, h.stdout.String(), "processes: none reporting")

	require.NoError(t, h.store.Enter(context.Background(), maintmode.State{OpID: "op-1", Kind: maintmode.KindManual, Reason: "r"}))
	require.NoError(t, h.store.Report(context.Background(), maintmode.ProcReport{
		ID: "a", Role: "both", State: maintmode.ProcResumeFailed, Commit: "abc1234", Error: "db\ndown",
	}))
	h.stdout.Reset()
	assert.Equal(t, 0, status(h.e, nil))
	out := h.stdout.String()
	assert.Contains(t, out, "maintenance: entering (opId op-1, kind manual")
	assert.Contains(t, out, "reason: r")
	assert.Contains(t, out, "(abc1234)")
	assert.Contains(t, out, "error=db down")

	h.stdout.Reset()
	assert.Equal(t, 0, status(h.e, []string{"-json"}))
	var got struct {
		Maintenance *maintmode.State       `json:"maintenance"`
		Processes   []maintmode.ProcReport `json:"processes"`
	}
	require.NoError(t, json.Unmarshal(h.stdout.Bytes(), &got))
	assert.Equal(t, "op-1", got.Maintenance.OpID)
	require.Len(t, got.Processes, 1)
	assert.Equal(t, "a", got.Processes[0].ID)
}

// TestStatus_JudgeView checks what status shows from the judge: who judges,
// the sessions, the processes waited for, what was terminated, and the
// warning when something connected after held.
func TestStatus_JudgeView(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	require.NoError(t, h.store.Enter(ctx, maintmode.State{OpID: "op"}))
	assert.Equal(t, 0, status(h.e, nil))
	assert.Contains(t, h.stdout.String(), "judge: none")

	h.counter.set(psqlSession(11))
	require.NoError(t, h.store.Report(ctx, maintmode.ProcReport{ID: "a", State: maintmode.ProcDraining}))
	j := &maintmode.Judge{Store: h.store, Counter: h.counter, ID: "server-1", DrainTimeout: time.Minute}
	_, err := j.Step(ctx)
	require.NoError(t, err)
	h.stdout.Reset()
	assert.Equal(t, 0, status(h.e, nil))
	out := h.stdout.String()
	assert.Contains(t, out, "judge: server-1")
	assert.Contains(t, out, "session primary pid=11")
	assert.Contains(t, out, "waiting for: a(draining)")

	require.NoError(t, h.store.Report(ctx, maintmode.ProcReport{ID: "a", State: maintmode.ProcDrained}))
	h.counter.set()
	_, err = j.Step(ctx)
	require.NoError(t, err)
	h.counter.set(psqlSession(12))
	_, err = j.Step(ctx)
	require.NoError(t, err)
	h.stdout.Reset()
	assert.Equal(t, 0, status(h.e, nil))
	out = h.stdout.String()
	assert.Contains(t, out, "held since")
	assert.Contains(t, out, "connected after the maintenance was held")
	assert.Contains(t, out, "pid=12")

	h.counter.err = errors.New("pg down")
	_, _ = j.Step(ctx)
	h.stdout.Reset()
	assert.Equal(t, 0, status(h.e, nil))
	assert.Contains(t, h.stdout.String(), "judge error: pg down")
}

func TestPrintTerminated(t *testing.T) {
	var b bytes.Buffer
	printTerminated(&b, nil)
	assert.Empty(t, b.String())
	printTerminated(&b, []maintmode.Blocker{psqlSession(3)})
	assert.Contains(t, b.String(), "terminated Elythia sessions")
	assert.Contains(t, b.String(), "pid=3")
	assert.Equal(t, "-", formatTime(time.Time{}))
}

func TestCommands_Failures(t *testing.T) {
	boom := errors.New("boom")
	for name, run := range map[string]func(env, []string) int{"enter": enter, "exit": exit, "wait": wait, "status": status} {
		t.Run(name+" config", func(t *testing.T) {
			h := newHarness(t)
			h.e.loadConfig = func(string) (*config.Config, error) { return nil, boom }
			assert.Equal(t, 1, run(h.e, nil))
			assert.Contains(t, h.stderr.String(), "failed to load config")
		})
		t.Run(name+" redis", func(t *testing.T) {
			h := newHarness(t)
			h.e.openStore = func(*config.Config) (*maintmode.Store, func(), error) { return nil, nil, boom }
			assert.Equal(t, 1, run(h.e, nil))
			assert.Contains(t, h.stderr.String(), "failed to connect to Redis")
		})
		t.Run(name+" redis down after connecting", func(t *testing.T) {
			h := newHarness(t)
			h.mr.Close()
			assert.Equal(t, 1, run(h.e, nil))
		})
		t.Run(name+" flags", func(t *testing.T) {
			h := newHarness(t)
			assert.Equal(t, 0, run(h.e, []string{"-h"}))
			assert.Equal(t, 2, run(h.e, []string{"-nope"}))
		})
	}
}

// 待っている途中で Redis が読めなくなったら、失敗として返る。
func TestWaits_RedisDownWhileWaiting(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.store.Enter(context.Background(), maintmode.State{OpID: "op"}))
	h.counter.set(psqlSession(1))
	calls := 0
	h.e.now = func() time.Time {
		calls++
		if calls == 2 {
			h.mr.Close()
		}
		return time.Now()
	}
	assert.Equal(t, 1, wait(h.e, []string{"-wait", "10s"}))
	assert.Contains(t, h.stderr.String(), "failed to read the maintenance state")

	h2 := newHarness(t)
	require.NoError(t, h2.store.Enter(context.Background(), maintmode.State{OpID: "op", Phase: maintmode.PhaseResuming}))
	require.NoError(t, h2.store.Report(context.Background(), maintmode.ProcReport{ID: "a", State: maintmode.ProcResuming}))
	calls = 0
	h2.e.now = func() time.Time {
		calls++
		if calls == 2 {
			h2.mr.Close()
		}
		return time.Now()
	}
	assert.Equal(t, 1, wait(h2.e, []string{"-wait", "10s"}))
	assert.Contains(t, h2.stderr.String(), "failed to read the maintenance state")
}

func TestStatus_ReadFailures(t *testing.T) {
	// 状態は読めるが、報告の一覧で落ちる: SCAN だけ失敗させる手段が無いので、
	// 判定役のロックのキーを壊して GET を失敗させる。
	h := newHarness(t)
	h.mr.HSet(h.store.JudgeKey(), "x", "y") // GET が WRONGTYPE になる
	assert.Equal(t, 1, status(h.e, nil))
	assert.Contains(t, h.stderr.String(), "failed to read the judge lock")
}

func TestDefaultEnv(t *testing.T) {
	e := defaultEnv()
	assert.Equal(t, os.Stdout, e.stdout)
	assert.Equal(t, os.Stderr, e.stderr)
	assert.NotNil(t, e.loadConfig)
	mr := miniredis.RunT(t)
	port, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	opts := config.RedisOptions{Host: mr.Host(), Port: port, Prefix: "pfx"}
	cfg := &config.Config{Redis: opts, RedisForPubsub: opts, RedisForJobQueue: opts, RedisForTimelines: opts, RedisForReactions: opts}
	store, closeFn, err := e.openStore(cfg)
	require.NoError(t, err)
	defer closeFn()
	assert.Equal(t, "pfx:elythia:maintenance", store.StateKey())
	mr.Close()
	_, _, err = e.openStore(cfg)
	assert.Error(t, err)
}

func TestDefaultEnv_Counter(t *testing.T) {
	c := defaultEnv().counter(&config.Config{DB: config.DBOptions{DB: "elythia"}, Maintenance: config.MaintenanceOptions{MaintenanceDB: "postgres"}}, "x")
	pg, ok := c.(*maintmode.PGCounter)
	require.True(t, ok)
	require.Len(t, pg.Targets, 1)
	assert.Equal(t, "elythia", pg.Targets[0].Database)
}

func TestExportedEntryPoints_StopOnFlagErrors(t *testing.T) {
	assert.Equal(t, 2, Enter([]string{"-nope"}))
	assert.Equal(t, 2, Exit([]string{"-nope"}))
	assert.Equal(t, 2, Wait([]string{"-nope"}))
	assert.Equal(t, 2, Status([]string{"-nope"}))
}

func TestEnter_WarnsWhenRedisMayEvictTheState(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	tr, err := testutil.SetupRedis(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { tr.Teardown(context.Background()) })
	h := newHarness(t)
	h.store = maintmode.NewStore(tr.Client, nil, "pfx:")

	require.NoError(t, tr.Client.ConfigSet(context.Background(), "maxmemory-policy", "noeviction").Err())
	assert.Equal(t, 0, enter(h.e, []string{"-wait", "0"}))
	assert.NotContains(t, h.stderr.String(), "may evict")
	require.NoError(t, h.store.Clear(context.Background(), ""))

	require.NoError(t, tr.Client.ConfigSet(context.Background(), "maxmemory-policy", "allkeys-lru").Err())
	assert.Equal(t, 0, enter(h.e, []string{"-wait", "0"}))
	assert.Contains(t, h.stderr.String(), "maxmemory-policy allkeys-lru and may evict")
}
