package maintmode

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/pgactivity"
)

// fakeCounter stands in for pg_stat_activity.
type fakeCounter struct {
	mu         sync.Mutex
	sessions   []Blocker
	err        error
	terminated []int
	termErr    error
}

func (f *fakeCounter) set(sessions ...Blocker) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = sessions
}

func (f *fakeCounter) Sessions(context.Context) ([]Blocker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Blocker(nil), f.sessions...), f.err
}

func (f *fakeCounter) Terminate(_ context.Context, b Blocker) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.termErr != nil {
		return f.termErr
	}
	f.terminated = append(f.terminated, b.PID)
	var rest []Blocker
	for _, s := range f.sessions {
		if s.PID != b.PID {
			rest = append(rest, s)
		}
	}
	f.sessions = rest
	return nil
}

func elythiaSession(pid int) Blocker {
	return Blocker{Server: "primary", Session: pgactivity.Session{
		PID: pid, User: "app", Application: "elythia/server/p1", SameRole: true,
	}}
}

func operatorSession(pid int) Blocker {
	return Blocker{Server: "primary", Session: pgactivity.Session{
		PID: pid, User: "app", Application: "psql", SameRole: true,
	}}
}

// clock is a manual clock for the judge.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock               { return &clock{t: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)} }
func (c *clock) judge(s *Store, f Counter, id string) *Judge {
	return &Judge{Store: s, Counter: f, ID: id, DrainTimeout: 30 * time.Second, Now: c.now}
}

func phase(t *testing.T, s *Store) string {
	t.Helper()
	st, err := s.Get(context.Background())
	require.NoError(t, err)
	if st == nil {
		return ""
	}
	return st.Phase
}

// P1 (単体): 接続が残っている間は held にしない。閉じたら held。
func TestJudge_HeldOnlyWhenNoSession(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &fakeCounter{}
	f.set(operatorSession(10))
	j := c.judge(s, f, "j")
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcDrained}))

	out, err := j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, Waiting, out)
	assert.Equal(t, PhaseEntering, phase(t, s))
	st, _ := s.Get(ctx)
	require.Len(t, st.Blockers, 1)
	assert.Equal(t, 10, st.Blockers[0].PID)

	f.set()
	out, err = j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, MovedToHeld, out)
	assert.Equal(t, PhaseHeld, phase(t, s))
}

// P2: 報告が消えても、接続が残っていれば held にしない。以前の実装は報告だけで
// 判定していたので、TTL 切れを手放し済みと読んだ (レビュー1周目の H)。
func TestJudge_VanishedReportDoesNotMeanLetGo(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &fakeCounter{}
	f.set(elythiaSession(11))
	j := c.judge(s, f, "j")
	require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcRunning}))
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	require.NoError(t, s.RemoveReport(ctx, "p1"))
	mr.FastForward(ProcTTL + time.Second)

	out, err := j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, Waiting, out)
	assert.Equal(t, PhaseEntering, phase(t, s), "報告が無いだけで held にした")
}

// P3: 起動中のプロセス (starting) は、まだ DB を開いていなくても待つ。
func TestJudge_WaitsForStartingProcess(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &fakeCounter{}
	j := c.judge(s, f, "j")
	require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcStarting}))
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))

	out, err := j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, Waiting, out)
	assert.Equal(t, PhaseEntering, phase(t, s), "起動中のプロセスを待たずに held にした")
	st, _ := s.Get(ctx)
	assert.Equal(t, []string{"p1(starting)"}, st.WaitingFor)

	require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcDrained}))
	out, err = j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, MovedToHeld, out)
}

// P4 (単体): 上限を過ぎたら、Elythia の接続を切って held へ進む。
func TestJudge_TerminatesElythiaSessionsAfterTheDrainTimeout(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &fakeCounter{}
	f.set(elythiaSession(12))
	j := c.judge(s, f, "j")
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))

	_, err := j.Step(ctx)
	require.NoError(t, err)
	c.add(29 * time.Second)
	_, err = j.Step(ctx)
	require.NoError(t, err)
	assert.Empty(t, f.terminated, "上限の前に切った")

	c.add(time.Second)
	_, err = j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, []int{12}, f.terminated)
	out, err := j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, MovedToHeld, out)
	st, _ := s.Get(ctx)
	require.Len(t, st.Terminated, 1, "切った接続を記録していない")
	assert.Equal(t, 12, st.Terminated[0].PID)
}

// P5: Elythia 以外の接続と、別の role の接続は切らない。
func TestJudge_LeavesOtherSessionsAlone(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &fakeCounter{}
	otherRole := elythiaSession(14)
	otherRole.SameRole = false
	f.set(operatorSession(13), otherRole)
	j := c.judge(s, f, "j")
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	_, _ = j.Step(ctx)
	c.add(time.Hour)
	_, err := j.Step(ctx)
	require.NoError(t, err)
	assert.Empty(t, f.terminated)
	assert.Equal(t, PhaseEntering, phase(t, s))
	st, _ := s.Get(ctx)
	assert.Len(t, st.Blockers, 2)
}

func TestJudge_TerminateFailureKeepsTheSession(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &fakeCounter{termErr: errors.New("denied")}
	f.set(elythiaSession(15))
	j := c.judge(s, f, "j")
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	_, _ = j.Step(ctx)
	c.add(time.Minute)
	_, err := j.Step(ctx)
	require.NoError(t, err)
	st, _ := s.Get(ctx)
	assert.Len(t, st.Blockers, 1)
	assert.Empty(t, st.Terminated)
	assert.Equal(t, PhaseEntering, st.Phase)
}

// P6: 判定役は 1 つ。期限が切れたら、もう片方が引き継ぐ。
func TestJudge_OnlyTheLockHolderJudges(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &fakeCounter{}
	a := c.judge(s, f, "a")
	b := c.judge(s, f, "b")
	f.set(operatorSession(16))
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	out, err := a.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, Waiting, out)

	f.set()
	out, err = b.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, NotJudging, out, "ロックを持たない判定役が判定した")
	assert.Equal(t, PhaseEntering, phase(t, s))

	// a が消えた (延長しなくなった)。
	mr.FastForward(JudgeLockTTL + time.Second)
	out, err = b.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, MovedToHeld, out)
}

// P7: 判定の途中で exit → enter し直したら、古い判定は新しい opId に書かない。
// 新しい opId では、待ちの時計も切った記録もやり直す。
func TestJudge_NewOpIDRestartsTheJudgement(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &fakeCounter{}
	f.set(elythiaSession(17))
	j := c.judge(s, f, "j")
	require.NoError(t, s.Enter(ctx, State{OpID: "op1"}))
	_, _ = j.Step(ctx)
	c.add(time.Minute)
	_, _ = j.Step(ctx)
	require.Equal(t, []int{17}, f.terminated)

	require.NoError(t, s.Clear(ctx, ""))
	require.NoError(t, s.Enter(ctx, State{OpID: "op2"}))
	f.set(elythiaSession(18))
	_, err := j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, []int{17}, f.terminated, "新しい opId で、待たずに切った")
	st, _ := s.Get(ctx)
	assert.Empty(t, st.Terminated, "前の opId で切った記録が残っている")
}

// held の間も数え続け、後から現れた接続を Blockers に出す (設計 §2.5)。
func TestJudge_HeldKeepsListingSessions(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &fakeCounter{}
	j := c.judge(s, f, "j")
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	out, _ := j.Step(ctx)
	require.Equal(t, MovedToHeld, out)

	f.set(operatorSession(19))
	out, err := j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, Waiting, out)
	st, _ := s.Get(ctx)
	assert.Equal(t, PhaseHeld, st.Phase, "held を entering に戻した")
	require.Len(t, st.Blockers, 1)

	f.err = errors.New("pg down")
	_, err = j.Step(ctx)
	require.Error(t, err)
	st, _ = s.Get(ctx)
	assert.Equal(t, "pg down", st.JudgeError)
}

func TestJudge_CountFailureStaysEntering(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &fakeCounter{err: errors.New("pg down")}
	j := c.judge(s, f, "j")
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	_, err := j.Step(ctx)
	require.Error(t, err)
	st, _ := s.Get(ctx)
	assert.Equal(t, PhaseEntering, st.Phase)
	assert.Equal(t, "pg down", st.JudgeError)
}

// 抜けるときは、生きている報告が全部 running になったら消す。消えた報告は待たない。
func TestJudge_ResumingClearsWhenEveryProcessRuns(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	j := c.judge(s, &fakeCounter{}, "j")
	require.NoError(t, s.Enter(ctx, State{OpID: "op", Phase: PhaseResuming}))
	require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcResuming}))
	require.NoError(t, s.Report(ctx, ProcReport{ID: "p2", State: ProcRunning}))
	out, err := j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, Waiting, out)
	assert.Equal(t, PhaseResuming, phase(t, s))

	require.NoError(t, s.RemoveReport(ctx, "p1")) // 落ちた
	out, err = j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, Cleared, out)
	assert.Empty(t, phase(t, s))

	// 何も無ければ、ロックを手放す。
	out, err = j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, NotJudging, out)
	holder, _ := s.Judge(ctx)
	assert.Empty(t, holder)
}

func TestJudge_RunStepsAndReleases(t *testing.T) {
	s, _ := newTestStore(t)
	j := &Judge{Store: s, Counter: &fakeCounter{}, ID: "j", DrainTimeout: time.Second}
	require.NoError(t, s.Enter(context.Background(), State{OpID: "op"}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { j.Run(ctx, 10*time.Millisecond); close(done) }()
	require.Eventually(t, func() bool { return phase(t, s) == PhaseHeld }, 5*time.Second, 10*time.Millisecond)
	cancel()
	<-done
	holder, err := s.Judge(context.Background())
	require.NoError(t, err)
	assert.Empty(t, holder, "止めた判定役がロックを持ったまま")
}

func TestJudge_StoreErrorsSurface(t *testing.T) {
	s, mr := newTestStore(t)
	j := &Judge{Store: s, Counter: &fakeCounter{}, ID: "j"}
	mr.Close()
	out, err := j.Step(context.Background())
	assert.Error(t, err)
	assert.Equal(t, NotJudging, out)
}

func TestTargetsFromConfig(t *testing.T) {
	cfg := &config.Config{
		DB:          config.DBOptions{Host: "db", Port: 5432, DB: "elythia", User: "u", Pass: "p"},
		Maintenance: config.MaintenanceOptions{MaintenanceDB: "postgres"},
		DBSlaves: []config.DBSlaveOptions{
			{Host: "r0", Port: 5432, DB: "elythia_r", User: "u", Pass: "p"},
		},
	}
	targets := TargetsFromConfig(cfg, "elythia-judge/x")
	require.Len(t, targets, 1, "dbReplications が無効ならレプリカは数えない")
	assert.Equal(t, "primary", targets[0].Name)
	assert.Equal(t, "elythia", targets[0].Database)
	assert.Contains(t, targets[0].DSN, "dbname=postgres")
	assert.Contains(t, targets[0].DSN, "application_name=")
	assert.Contains(t, targets[0].DSN, "elythia-judge/x")

	cfg.DBReplications = true
	targets = TargetsFromConfig(cfg, "elythia-judge/x")
	require.Len(t, targets, 2)
	assert.Equal(t, "replica0", targets[1].Name)
	assert.Equal(t, "elythia_r", targets[1].Database)
	assert.Contains(t, targets[1].DSN, "host=r0")
	assert.Contains(t, targets[1].DSN, "dbname=postgres")
	assert.Equal(t, "elythia_r", cfg.DBSlaves[0].DB, "元の設定を書き換えた")
}

func TestPGCounter_Errors(t *testing.T) {
	_, err := (&PGCounter{}).Sessions(context.Background())
	assert.Error(t, err)
	assert.Error(t, (&PGCounter{}).Terminate(context.Background(), Blocker{Server: "primary"}))
	bad := &PGCounter{Targets: []PGTarget{{Name: "primary", DSN: "host=/nonexistent port=1 dbname=x connect_timeout=1"}}}
	_, err = bad.Sessions(context.Background())
	assert.Error(t, err)
	assert.Error(t, bad.Terminate(context.Background(), Blocker{Server: "primary"}))
}

func TestBlockerString(t *testing.T) {
	b := operatorSession(1)
	assert.Contains(t, b.String(), "primary pid=1")
}
