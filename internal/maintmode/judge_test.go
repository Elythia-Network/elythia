package maintmode

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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

func (f *fakeCounter) Terminate(_ context.Context, b Blocker) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.termErr != nil {
		return false, f.termErr
	}
	var rest []Blocker
	found := false
	for _, s := range f.sessions {
		if s.PID == b.PID {
			found = true
			continue
		}
		rest = append(rest, s)
	}
	f.sessions = rest
	if !found {
		return false, nil
	}
	f.terminated = append(f.terminated, b.PID)
	return true, nil
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
	require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcDrained}))
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
	require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcDrained}))
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
	_, err = (&PGCounter{}).Terminate(context.Background(), Blocker{Server: "primary"})
	assert.Error(t, err)
	bad := &PGCounter{Targets: []PGTarget{{Name: "primary", DSN: "host=/nonexistent port=1 dbname=x connect_timeout=1"}}}
	_, err = bad.Sessions(context.Background())
	assert.Error(t, err)
	_, err = bad.Terminate(context.Background(), Blocker{Server: "primary"})
	assert.Error(t, err)
}

func TestBlockerString(t *testing.T) {
	b := operatorSession(1)
	assert.Contains(t, b.String(), "primary pid=1")
}

// TestJudge_DoesNotJudgeAStateWithoutOpID fixes #3463 review M-2: a judge
// that read a state without an opId (half written, or written by hand) had
// no wait clock and terminated sessions at once.
func TestJudge_DoesNotJudgeAStateWithoutOpID(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &fakeCounter{}
	f.set(elythiaSession(17))
	require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcDrained}))
	j := c.judge(s, f, "j")
	mr.HSet(s.StateKey(), "phase", PhaseEntering)
	for range 3 {
		_, err := j.Step(ctx)
		assert.ErrorContains(t, err, "no opId or no phase")
		c.add(time.Hour)
	}
	assert.Empty(t, f.terminated, "opId の無い状態で接続を切った")
	assert.Equal(t, PhaseEntering, phase(t, s))
}

// vanishingCounter lists a session that is gone by the time it is
// terminated (it closed by itself, or its pid now belongs to another
// connection).
type vanishingCounter struct{ fakeCounter }

func (v *vanishingCounter) Terminate(context.Context, Blocker) (bool, error) { return false, nil }

// TestJudge_SessionGoneBeforeTerminateIsNotRecorded checks #3463 review L-4:
// a session that pg_terminate_backend did not end is not recorded as
// terminated.
func TestJudge_SessionGoneBeforeTerminateIsNotRecorded(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	v := &vanishingCounter{}
	v.set(elythiaSession(30))
	require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcDrained}))
	j := c.judge(s, v, "j")
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	_, _ = j.Step(ctx)
	c.add(time.Minute)
	_, err := j.Step(ctx)
	require.NoError(t, err)
	st, _ := s.Get(ctx)
	assert.Empty(t, st.Terminated, "切れなかった接続を切ったと記録した")
}

func sessionOf(pid int, app string) Blocker {
	return Blocker{Server: "primary", Session: pgactivity.Session{PID: pid, User: "app", Application: app, SameRole: true}}
}

// TestJudge_TerminatesOnlyForProcessesLettingGo fixes #3463 review M-1: an
// Elythia session is terminated only when its process reports drained (or
// draining past the timeout). A process without a report (Redis lost while
// running) or a running one would reconnect at once, after held.
func TestJudge_TerminatesOnlyForProcessesLettingGo(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &fakeCounter{}
	f.set(
		sessionOf(40, "elythia/server/drained-1"),
		sessionOf(41, "elythia/server/draining-1"),
		sessionOf(42, "elythia/server/running-1"),
		sessionOf(43, "elythia/server/unreported-1"),
		sessionOf(44, "elythia/server/starting-1"),
	)
	for id, state := range map[string]string{
		"drained-1": ProcDrained, "draining-1": ProcDraining, "running-1": ProcRunning, "starting-1": ProcStarting,
	} {
		require.NoError(t, s.Report(ctx, ProcReport{ID: id, State: state}))
	}
	j := c.judge(s, f, "j")
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	_, _ = j.Step(ctx)
	c.add(time.Minute)
	_, err := j.Step(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []int{40, 41}, f.terminated)
	st, _ := s.Get(ctx)
	var left []int
	for _, b := range st.Blockers {
		left = append(left, b.PID)
	}
	assert.ElementsMatch(t, []int{42, 43, 44}, left, "切らなかった接続を blockers に出していない")
	assert.Equal(t, PhaseEntering, st.Phase)
}

func TestOwnerOf(t *testing.T) {
	reports := []ProcReport{
		{ID: "host-a-1", State: ProcDrained},
		{ID: "host-b-123456789", State: ProcDrained},
		{ID: "host-b-123456780", State: ProcDrained},
	}
	assert.Equal(t, "host-a-1", ownerOf("elythia/server/host-a-1", reports).ID)
	assert.Nil(t, ownerOf("elythia/server/host-a-10", reports), "名前が前方一致するだけの別のプロセス")
	assert.Nil(t, ownerOf("psql", reports))
	assert.Nil(t, ownerOf("elythia/server", reports))
	assert.Nil(t, ownerOf("elythia/server/", reports))
	// 63 バイトで切り詰められた名前は、1 つの報告にだけ合えば持ち主とみなす。
	long := "elythia/server/" + strings.Repeat("x", 60)
	lr := []ProcReport{{ID: strings.Repeat("x", 60) + "-99", State: ProcDrained}}
	truncated := long[:pgNameMax]
	require.Len(t, truncated, pgNameMax)
	assert.Equal(t, lr[0].ID, ownerOf(truncated, lr).ID)
	two := append(lr, ProcReport{ID: strings.Repeat("x", 60) + "-98"})
	assert.Nil(t, ownerOf(truncated, two), "切り詰めた名前が 2 つに合うのに持ち主を決めた")
	// 切り詰められた名前が、別のプロセスの ID とたまたま完全一致しても、接頭辞が
	// 合う報告がほかにもあれば決めない (同じ host の pid 12 と pid 1234 など)。
	host := strings.Repeat("h", pgNameMax-len("elythia/server/")-len("-12"))
	short := ProcReport{ID: host + "-12", State: ProcDrained}
	longer := ProcReport{ID: host + "-1234", State: ProcRunning}
	name := ("elythia/server/" + longer.ID)[:pgNameMax]
	require.Equal(t, "elythia/server/"+short.ID, name, "前提: 切り詰めた名前が pid 12 の ID と一致する")
	assert.Nil(t, ownerOf(name, []ProcReport{short, longer}), "running の pid 1234 の接続を pid 12 のものと取り違えた")
	// 63 バイトより短い名前は、前方一致では決めない (合う報告が 1 つでも)。
	assert.Nil(t, ownerOf("elythia/server/host-a", reports))
	assert.Nil(t, ownerOf("elythia/server/host-b-12345678", reports))
}

// stickyCounter keeps listing a session after terminating it.
type stickyCounter struct{ fakeCounter }

func (s *stickyCounter) Terminate(context.Context, Blocker) (bool, error) { return true, nil }

// TestJudge_TerminatedIsDeduplicatedAndBounded checks that the terminated
// list does not grow on every step (#3463 review M-1).
func TestJudge_TerminatedIsDeduplicatedAndBounded(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &stickyCounter{}
	var many []Blocker
	for i := range maxTerminatedRecorded + 20 {
		many = append(many, sessionOf(1000+i, "elythia/server/p1"))
	}
	f.set(many...)
	require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcDrained}))
	j := c.judge(s, f, "j")
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	_, _ = j.Step(ctx)
	c.add(time.Minute)
	for range 3 {
		_, err := j.Step(ctx)
		require.NoError(t, err)
	}
	st, _ := s.Get(ctx)
	assert.Len(t, st.Terminated, maxTerminatedRecorded)

	f.set(sessionOf(7, "elythia/server/p1"))
	j2 := c.judge(s, f, "j")
	j2.reset("op")
	j2.firstSeen = c.now().Add(-time.Hour)
	for range 3 {
		_, _ = j2.entering(ctx, st)
	}
	assert.Len(t, j2.terminated, 1, "同じ接続を何度も記録した")
}

// TestPGCounter_RefusesTheApplicationDatabase fixes #3463 review L-1: with
// no maintenance: section the config does not check the names, so a
// database named postgres would be counted from inside itself.
func TestPGCounter_RefusesTheApplicationDatabase(t *testing.T) {
	cfg := &config.Config{
		DB:          config.DBOptions{Host: "db", Port: 5432, DB: "postgres", User: "u", Pass: "p"},
		Maintenance: config.MaintenanceOptions{MaintenanceDB: "postgres"},
	}
	connected := false
	c := &PGCounter{
		Targets: TargetsFromConfig(cfg, "elythia-judge/x"),
		Connect: func(context.Context, string) (*pgx.Conn, error) {
			connected = true
			return nil, errors.New("must not connect")
		},
	}
	_, err := c.Sessions(context.Background())
	assert.ErrorContains(t, err, "is the application database")
	assert.False(t, connected, "対象の DB に繋ごうとした")

	// 判定役は判定を拒み、judgeError に出して entering のまま待つ。
	s, _ := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	j := &Judge{Store: s, Counter: c, ID: "j", DrainTimeout: time.Second}
	_, err = j.Step(ctx)
	require.Error(t, err)
	st, _ := s.Get(ctx)
	assert.Equal(t, PhaseEntering, st.Phase)
	assert.Contains(t, st.JudgeError, "is the application database")
}

// TestJudge_TerminatesForAProcessThatDiedWhileLettingGo checks #3463 review
// round 2 item 3: a process seen draining that dies loses its report; its
// sessions are still terminated after the timeout. A process remembered so
// but serving again (same ID running) is forgotten.
func TestJudge_TerminatesForAProcessThatDiedWhileLettingGo(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &fakeCounter{}
	f.set(sessionOf(50, "elythia/server/died-1"), sessionOf(51, "elythia/server/back-1"))
	require.NoError(t, s.Report(ctx, ProcReport{ID: "died-1", State: ProcDraining}))
	require.NoError(t, s.Report(ctx, ProcReport{ID: "back-1", State: ProcDrained}))
	j := c.judge(s, f, "j")
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	_, err := j.Step(ctx)
	require.NoError(t, err)
	// died-1 は落ちて報告が消え、back-1 は (同じ ID で) また running と報告した。
	require.NoError(t, s.RemoveReport(ctx, "died-1"))
	require.NoError(t, s.Report(ctx, ProcReport{ID: "back-1", State: ProcRunning}))
	_, err = j.Step(ctx)
	require.NoError(t, err)
	require.NoError(t, s.RemoveReport(ctx, "back-1"))
	c.add(time.Minute)
	_, err = j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, []int{50}, f.terminated, "落ちたプロセスの接続だけを切る")

	// opId が変わったら、覚えた分は捨てる。
	require.NoError(t, s.Clear(ctx, ""))
	require.NoError(t, s.Enter(ctx, State{OpID: "op2"}))
	f.set(sessionOf(52, "elythia/server/died-1"))
	_, _ = j.Step(ctx)
	c.add(time.Minute)
	_, err = j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, []int{50}, f.terminated, "前の opId で覚えたプロセスの接続を切った")
}

// TestJudge_RememberedLiveProcessIsNotCountedTwice checks that a process
// both remembered and still reporting counts once: counted twice, a
// truncated application_name would match two entries and find no owner.
func TestJudge_RememberedLiveProcessIsNotCountedTwice(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	id := strings.Repeat("h", 60) + "-1-0a1b2c3d"
	f := &fakeCounter{}
	f.set(sessionOf(60, ("elythia/server/" + id)[:pgNameMax]))
	require.NoError(t, s.Report(ctx, ProcReport{ID: id, State: ProcDrained}))
	j := c.judge(s, f, "j")
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	_, _ = j.Step(ctx)
	c.add(time.Minute)
	_, err := j.Step(ctx)
	require.NoError(t, err)
	assert.Equal(t, []int{60}, f.terminated)
}

// TestJudge_DoesNotJudgeAStateWithoutPhase checks #3463 review round 2 item
// 8: a hash with an opId but no phase reads as entering, but the judge's
// writes need the phase, so judging it would only terminate sessions.
func TestJudge_DoesNotJudgeAStateWithoutPhase(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	c := newClock()
	f := &fakeCounter{}
	f.set(elythiaSession(18))
	require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcDrained}))
	mr.HSet(s.StateKey(), "opId", "op")
	j := c.judge(s, f, "j")
	for range 3 {
		_, err := j.Step(ctx)
		assert.ErrorContains(t, err, "no phase")
		c.add(time.Hour)
	}
	assert.Empty(t, f.terminated, "phase の無い状態で接続を切った")
	st, _ := s.Get(ctx)
	assert.True(t, st.Holding(), "phase の無い状態は、各プロセスにはメンテナンス中と読ませる")
}
