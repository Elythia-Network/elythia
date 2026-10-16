package restoreop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/maintmode"
	"github.com/elythia-network/elythia/internal/pgactivity"
)

const testDB = "app"

// fakeDB keeps the database names in memory, like pg_database. A database
// maps to its "content" label, so that a test can tell which one has the
// server's name after a swap.
type fakeDB struct {
	mu  sync.Mutex
	dbs map[string]string
	// marks maps a database's content (the comment follows the database
	// through renames, as in PostgreSQL) to the mark MarkOperation put on it.
	marks     map[string]opMark
	conns     map[string]int
	verified  bool
	fail      map[string]error
	block     chan struct{} // RestoreInto waits on it when set
	drops     []string
	switches  int
	switchErr func(n int) error
	// migrated lists the databases Migrate ran on; needsMigration is what
	// CheckDatabaseMigrations reports.
	migrated       []string
	needsMigration bool
	// migrateBlock, when set, holds Migrate until it is closed.
	migrateBlock chan struct{}
}

func newFakeDB() *fakeDB {
	return &fakeDB{dbs: map[string]string{testDB: "current", "postgres": "maint"}, conns: map[string]int{}, verified: true, fail: map[string]error{}}
}

func (f *fakeDB) failing(op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail[op]
}

func (f *fakeDB) Load(_ context.Context, id string) (*backup.RestorePlan, error) {
	if err := f.failing("load"); err != nil {
		return nil, err
	}
	return &backup.RestorePlan{ID: id, Verified: f.verified}, nil
}

func (f *fakeDB) CheckTarget(context.Context, backup.RestoreOptions) error {
	return f.failing("checkTarget")
}
func (f *fakeDB) CheckCompatibility(context.Context, *backup.RestorePlan, backup.RestoreOptions) error {
	return f.failing("checkCompatibility")
}

func (f *fakeDB) RestoreInto(ctx context.Context, plan *backup.RestorePlan, _ backup.RestoreOptions, dbname string) error {
	f.mu.Lock()
	if _, taken := f.dbs[dbname]; taken {
		f.mu.Unlock()
		return fmt.Errorf("%w: database %q already exists", backup.ErrRestoreNotCreated, dbname)
	}
	f.dbs[dbname] = "restored:" + plan.ID
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.failing("restoreInto")
}

func (f *fakeDB) VerifyRestored(context.Context, string, backup.Meta) error {
	return f.failing("verifyRestored")
}
func (f *fakeDB) ApplyDatabaseSettings(context.Context, string, backup.Meta) ([]string, error) {
	return nil, f.failing("settings")
}
func (f *fakeDB) Migrate(name string) ([]backup.MigrationState, error) {
	f.mu.Lock()
	block := f.migrateBlock
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if err := f.failing("migrate"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.migrated = append(f.migrated, name)
	return nil, nil
}

func (f *fakeDB) migratedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.migrated...)
}

func (f *fakeDB) CheckDatabaseMigrations(context.Context, string) (*backup.DatabaseMigrations, error) {
	f.mu.Lock()
	needs := f.needsMigration
	f.mu.Unlock()
	return &backup.DatabaseMigrations{NeedsMigration: needs}, f.failing("migrations")
}

func (f *fakeDB) CheckRollback(_ context.Context, _, _, kept, keepAs string) error {
	if err := f.failing("checkRollback"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conns[kept] > 0 {
		return fmt.Errorf("%w: someone is on %q", backup.ErrRestoreConnections, kept)
	}
	if _, ok := f.dbs[keepAs]; ok {
		return fmt.Errorf("database %q already exists", keepAs)
	}
	return nil
}

func (f *fakeDB) KeptDatabases(_ context.Context, _, database string) ([]backup.KeptDatabase, error) {
	if err := f.failing("kept"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []backup.KeptDatabase
	for n := range f.dbs {
		if kind, at, ok := backup.ParseKeptName(database, n); ok {
			out = append(out, backup.KeptDatabase{Name: n, Kind: kind, At: at})
		}
	}
	return out, nil
}

func (f *fakeDB) SwitchDatabase(_ context.Context, _, database, keepAs, replacement string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.switches++
	if f.switchErr != nil {
		if err := f.switchErr(f.switches); err != nil {
			return err
		}
	}
	// PostgreSQL と同じく、接続のある DB の名前は変えられない。
	if f.conns[database] > 0 {
		return fmt.Errorf("%w: database %q is being accessed by other users", backup.ErrRestoreConnections, database)
	}
	if _, ok := f.dbs[replacement]; !ok {
		return fmt.Errorf("no database %q", replacement)
	}
	if _, ok := f.dbs[keepAs]; ok {
		return fmt.Errorf("database %q already exists", keepAs)
	}
	f.dbs[keepAs] = f.dbs[database]
	f.dbs[database] = f.dbs[replacement]
	delete(f.dbs, replacement)
	return nil
}

func (f *fakeDB) DropDatabase(_ context.Context, _, name string) error {
	if err := f.failing("drop"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drops = append(f.drops, name)
	delete(f.dbs, name)
	return nil
}

func (f *fakeDB) ExistingDatabases(_ context.Context, _ string, names []string) (map[string]bool, error) {
	if err := f.failing("existing"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for _, n := range names {
		if _, ok := f.dbs[n]; ok && n != "" {
			out[n] = true
		}
	}
	return out, nil
}

func (f *fakeDB) content(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dbs[name]
}

func (f *fakeDB) names() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.dbs {
		out[k] = v
	}
	return out
}

// fakeCounter reports the sessions the fake processes hold on the database.
type fakeCounter struct{ db *fakeDB }

func (c fakeCounter) Sessions(context.Context) ([]maintmode.Blocker, error) {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	var out []maintmode.Blocker
	for i := 0; i < c.db.conns[testDB]; i++ {
		out = append(out, maintmode.Blocker{Server: "primary", Session: pgactivity.Session{PID: 100 + i, Application: "psql"}})
	}
	return out, nil
}

func (c fakeCounter) Terminate(context.Context, maintmode.Blocker) (bool, error) { return false, nil }

// fakeProcess plays one server process: it reports, lets go of the
// database while the maintenance holds, and comes back when it resumes.
type fakeProcess struct {
	id       string
	store    *maintmode.Store
	db       *fakeDB
	mu       sync.Mutex
	failBack bool // stay at resume_failed instead of coming back
	serving  string
	// reported is closed once the first report is written.
	reported chan struct{}
}

func (p *fakeProcess) run(ctx context.Context) {
	var reportedOnce sync.Once
	for {
		st, err := p.store.Get(ctx)
		if err == nil {
			state := maintmode.ProcRunning
			p.mu.Lock()
			switch {
			case st != nil && st.Holding():
				state = maintmode.ProcDrained
			case st != nil && p.failBack:
				state = maintmode.ProcResumeFailed
			}
			if state == maintmode.ProcRunning {
				p.serving = p.db.content(testDB)
			}
			p.mu.Unlock()
			if p.store.Report(ctx, maintmode.ProcReport{ID: p.id, Role: "both", Version: "1.0", State: state}) == nil {
				reportedOnce.Do(func() { close(p.reported) })
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (p *fakeProcess) setFailBack(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failBack = v
}

type env struct {
	t       *testing.T
	mr      *miniredis.Miniredis
	rdb     *redis.Client
	store   *Store
	maint   *maintmode.Store
	db      *fakeDB
	m       *Manager
	opts    Options
	cancel  context.CancelFunc
	ctx     context.Context
	cleaned int
	checked int
	modlogs []string
	notes   []string
	slot    *slot
	mu      sync.Mutex
	// clock is how far the managers' clock (Options.Now) is ahead of the
	// wall clock, in nanoseconds (waitStepPassingTime).
	clock atomic.Int64
	// stepHook, when set, is the failStep of the managers start makes.
	stepHook func(to string) error
	// recovered is closed when the recovery at start of the manager start
	// made last has ended (Options.Recovering(false)).
	recovered chan struct{}
	// finals maps an opId to its journal as it was right after the command
	// that made its step final (finalHook).
	finalsMu sync.Mutex
	finals   map[string][]JournalEntry
	// replayTo, when set, is the step whose first script replays runs
	// twice (replayStepTo).
	replayTo string
	// intercept, when set, fails a command before it is sent
	// (interceptCmd).
	intercept func(cmd redis.Cmder) error
	// pauseTo, when set, is the step whose first script holds the goroutine
	// that sent it, right after it ran, until pauseRelease is closed
	// (pauseAfterStepTo).
	pauseTo      string
	pauseReached chan struct{}
	pauseRelease chan struct{}
}

// finalHook looks at the records after every Redis command and keeps the
// journal of each one as it is when its step first becomes final: what an
// admin page sees on the read that tells it to stop reading (Status.Final).
type finalHook struct{ e *env }

func (h finalHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h finalHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := h.e.intercepts(cmd); err != nil {
			// 送らずに誤りを返す (時間切れなど)。
			cmd.SetErr(err)
			return err
		}
		err := next(ctx, cmd)
		if err == nil && h.e.replays(cmd) {
			// go-redis の送り直しを真似る: 1 回目は書けたが応答が失われ、同じ script を
			// もう一度送った。cmd の結果は 2 回目のものになる。
			err = next(ctx, cmd)
		}
		h.e.noteFinals()
		if err == nil {
			h.e.pauses(cmd)
		}
		return err
	}
}

// pauseAfterStepTo makes the first script sent to step (whether or not it
// moved the record: a script that found another step pauses too) hold the
// goroutine that sent it, right after the script ran, until release is
// called. reached is closed when the script has run. It lets a test put
// another goroutine's commands between that script and what its sender does
// next. Call it after the manager has started (newEnv): release is
// registered with t.Cleanup, which must run before the Cleanup of start that
// waits for the manager.
func (e *env) pauseAfterStepTo(step string) (reached <-chan struct{}, release func()) {
	e.finalsMu.Lock()
	defer e.finalsMu.Unlock()
	e.pauseTo = step
	e.pauseReached = make(chan struct{})
	e.pauseRelease = make(chan struct{})
	var once sync.Once
	rel := e.pauseRelease
	release = func() { once.Do(func() { close(rel) }) }
	// 止めたまま終わると、Manager.Wait が返らずにテストが固まる。
	e.t.Cleanup(release)
	return e.pauseReached, release
}

func (e *env) pauses(cmd redis.Cmder) {
	e.finalsMu.Lock()
	if e.pauseTo == "" || !stepScriptTo(cmd, e.pauseTo) {
		e.finalsMu.Unlock()
		return
	}
	e.pauseTo = ""
	reached, release := e.pauseReached, e.pauseRelease
	e.finalsMu.Unlock()
	close(reached)
	<-release
}

// replayStepTo makes the first script that moves a record to step run twice
// (replays).
func (e *env) replayStepTo(step string) {
	e.finalsMu.Lock()
	defer e.finalsMu.Unlock()
	e.replayTo = step
}

// replayed reports whether the replay set by replayStepTo happened.
func (e *env) replayed() bool {
	e.finalsMu.Lock()
	defer e.finalsMu.Unlock()
	return e.replayTo == ""
}

// interceptCmd makes f decide, before each command is sent, whether to fail
// it without sending it (a non-nil error). It also sees every command of a
// pipeline, MULTI and EXEC included; when it fails one of them, the whole
// pipeline fails without being sent.
func (e *env) interceptCmd(f func(cmd redis.Cmder) error) {
	e.finalsMu.Lock()
	defer e.finalsMu.Unlock()
	e.intercept = f
}

func (e *env) intercepts(cmd redis.Cmder) error {
	e.finalsMu.Lock()
	f := e.intercept
	e.finalsMu.Unlock()
	if f == nil {
		return nil
	}
	return f(cmd)
}

// stepScriptTo reports whether cmd is the script that moves a record to
// step.
func stepScriptTo(cmd redis.Cmder, step string) bool {
	if cmd.Name() != "evalsha" && cmd.Name() != "eval" {
		return false
	}
	// EVALSHA sha 3 key key key from to ...
	args := cmd.Args()
	return len(args) >= 8 && fmt.Sprint(args[7]) == step
}

func (e *env) replays(cmd redis.Cmder) bool {
	e.finalsMu.Lock()
	defer e.finalsMu.Unlock()
	if e.replayTo == "" || !stepScriptTo(cmd, e.replayTo) {
		return false
	}
	e.replayTo = ""
	return true
}

func (h finalHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		// pipeline (MULTI / EXEC を含む) にも interceptCmd を効かせる。効かせないと、
		// 読み出しを pipeline に変えただけで「読めない」状態が起きなくなり、それを
		// 検査するテストが黙って別の経路を通る (#3369)。どれか 1 つでも止めるなら、
		// 全体を送らずに誤りにする。一部だけ送ると、サーバー側の接続が MULTI のまま
		// 残りうる。止めると決まった後も残りのコマンドを f に見せる。f が止めた
		// コマンドを数えるテストは、MULTI の後ろにあるコマンドも数えたい。
		var ierr error
		for _, cmd := range cmds {
			if err := h.e.intercepts(cmd); err != nil && ierr == nil {
				ierr = err
			}
		}
		if ierr != nil {
			for _, c := range cmds {
				c.SetErr(ierr)
			}
			return ierr
		}
		err := next(ctx, cmds)
		h.e.noteFinals()
		return err
	}
}

func (e *env) noteFinals() {
	const prefix = "p:elythia:maintenance:restore:"
	e.finalsMu.Lock()
	defer e.finalsMu.Unlock()
	for _, k := range e.mr.Keys() {
		opID, ok := strings.CutPrefix(k, prefix)
		if !ok || strings.Contains(opID, ":") {
			continue
		}
		if _, seen := e.finals[opID]; seen {
			continue
		}
		step := e.mr.HGet(k, "step")
		if !Final(step) {
			continue
		}
		raw, _ := e.mr.List(k + ":journal")
		journal := []JournalEntry{}
		for _, line := range raw {
			var j JournalEntry
			if json.Unmarshal([]byte(line), &j) == nil {
				journal = append(journal, j)
			}
		}
		e.finals[opID] = journal
	}
}

// finalJournal returns the journal of opID as it was when its step became
// final.
func (e *env) finalJournal(opID string) []JournalEntry {
	e.t.Helper()
	e.finalsMu.Lock()
	defer e.finalsMu.Unlock()
	j, ok := e.finals[opID]
	require.True(e.t, ok, "the step of %s has not become final", opID)
	return j
}

// slot is the job slot of the backup service.
type slot struct {
	mu   sync.Mutex
	held bool
}

func (s *slot) acquire() (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held {
		return nil, errors.New("busy")
	}
	s.held = true
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.held = false
	}, nil
}

func (s *slot) isHeld() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}

func newEnv(t *testing.T, mutate ...func(*Options)) *env {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	e := &env{t: t, mr: mr, rdb: rdb, db: newFakeDB(), finals: map[string][]JournalEntry{}}
	rdb.AddHook(finalHook{e})
	e.store = NewStore(rdb, "p:", nil)
	e.maint = maintmode.NewStore(rdb, rdb, "p:")
	e.opts = Options{
		Store: e.store, DB: e.db, Maint: e.maint,
		RunJudge: func(ctx context.Context) {
			j := &maintmode.Judge{Store: e.maint, Counter: fakeCounter{e.db}, ID: "test-judge", DrainTimeout: time.Hour}
			j.Run(ctx, 5*time.Millisecond)
		},
		CleanRedis: func(context.Context) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.cleaned++
			return e.db.failing("cleanRedis")
		},
		Check: func(context.Context) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.checked++
			return e.db.failing("check")
		},
		ModLog: func(_ context.Context, database, _, kind string, info map[string]any) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			if kind == KindRollback {
				e.modlogs = append(e.modlogs, fmt.Sprintf("rollback:%s:%s:%s:%s:%s", database, e.db.content(database), info["phase"], info["from"], info["keptAs"]))
				return nil
			}
			e.modlogs = append(e.modlogs, fmt.Sprintf("%s:%s:%s", database, e.db.content(database), info["phase"]))
			return nil
		},
		Notify: func(_ context.Context, msg string) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.notes = append(e.notes, msg)
		},
		Database: testDB, MaintenanceDB: "postgres", Version: "1.0",
		// 期限は既定では切れない長さにし、状態 (held、メンテナンスが外れる) を待って
		// 進ませる。短い実時間の期限を全部に置くと、負荷で判定役が間に合わないときに、
		// 期限を検査しないテストまで中止や運営者に渡す側へ進んで落ちる (#3369)。期限が
		// 切れることを検査するテストは、短い期限を自分で置くか、waitStepPassingTime で
		// 時計を進める。
		ReadyTimeout: time.Hour, HeldTimeout: time.Hour, ResumeTimeout: time.Hour,
		Poll:   5 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return time.Now().Add(time.Duration(e.clock.Load())) },
	}
	for _, f := range mutate {
		f(&e.opts)
	}
	e.start()
	// 起動時の回復を終えてから返す。回復は別の goroutine で Current を読むので、
	// 待たずに記録を書くテストでは、遅れて走った回復がその記録を「前の実行が残した
	// もの」として片付け、段を書き換えることがあった (#3600)。
	e.waitRecovered()
	return e
}

// waitRecovered waits for the recovery at start of the current manager to
// end.
func (e *env) waitRecovered() {
	e.t.Helper()
	select {
	case <-e.recovered:
	case <-time.After(10 * time.Second):
		e.t.Fatal("the recovery at start did not end")
	}
}

// start starts a manager (again, after stop, for the recovery tests).
func (e *env) start() {
	// 起動し直したサービスは、新しい job の枠を持つ。
	e.slot = &slot{}
	e.opts.Acquire = e.slot.acquire
	e.ctx, e.cancel = context.WithCancel(context.Background())
	o := e.opts
	recovered := make(chan struct{})
	var once sync.Once
	told := e.opts.Recovering
	o.Recovering = func(v bool) {
		if told != nil {
			told(v)
		}
		if !v {
			once.Do(func() { close(recovered) })
		}
	}
	e.recovered = recovered
	e.m = New(o)
	e.m.failStep = e.stepHook
	e.m.Start(e.ctx)
	e.t.Cleanup(func() {
		e.cancel()
		e.m.Wait()
	})
}

// stop stops the manager as if the service were killed.
func (e *env) stop() {
	e.cancel()
	e.m.Wait()
}

// process starts a fake process and returns once it has reported.
//
// 最初の報告を待ってから返す。判定役は報告の無いプロセスを待たない (resuming の
// 判定は報告だけで決まる) ので、報告より先に再開を判定されると、戻れないはずの
// プロセスがいないものとしてメンテナンスが外れる (#3369)。
func (e *env) process(id string) *fakeProcess {
	p := &fakeProcess{id: id, store: e.maint, db: e.db, reported: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.run(ctx)
	}()
	e.t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-p.reported:
	case <-time.After(10 * time.Second):
		e.t.Fatalf("process %s did not report", id)
	}
	return p
}

func (e *env) waitStep(opID string, steps ...string) *Record {
	e.t.Helper()
	var rec *Record
	require.Eventually(e.t, func() bool {
		r, err := e.store.Get(context.Background(), opID)
		if err != nil {
			return false
		}
		rec = r
		for _, s := range steps {
			if r.Step == s {
				return true
			}
		}
		return false
	}, 10*time.Second, 5*time.Millisecond, "waiting for %v", steps)
	return rec
}

// waitStepPassingTime is waitStep that moves the managers' clock an hour
// ahead on every look, so that every wait with a deadline (HeldTimeout,
// ResumeTimeout) the manager is in, or enters, ends with its timeout. Use
// it only once every wait left in the operation is meant to time out, or
// give the waits that must not time out a deadline far beyond the few hours
// it may move after the step is reached.
func (e *env) waitStepPassingTime(opID string, steps ...string) *Record {
	e.t.Helper()
	var rec *Record
	require.Eventually(e.t, func() bool {
		e.clock.Add(int64(time.Hour))
		r, err := e.store.Get(context.Background(), opID)
		if err != nil {
			return false
		}
		rec = r
		for _, s := range steps {
			if r.Step == s {
				return true
			}
		}
		return false
	}, 10*time.Second, 5*time.Millisecond, "waiting for %v", steps)
	return rec
}

func (e *env) prepare() (*Record, string) {
	e.t.Helper()
	rec, ticket, err := e.m.Prepare(context.Background(), PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "session-token-01"})
	require.NoError(e.t, err)
	e.waitStep(rec.OpID, StepReady)
	return rec, ticket
}

// waitMaintenance waits until cond holds for the maintenance state. When it
// does not within wait, the failure says why: the last state read, or the
// error that kept it from being read.
//
// 条件の中で maintenance() (require で t に触る) を使わない。条件は別の goroutine
// で動き、待ちが切れた後に t に触れると panic して元の失敗を隠す (#3369)。読めない
// 誤りも捨てずに、EventuallyWithT の失敗の文面に残す。
func (e *env) waitMaintenance(cond func(st *maintmode.State) bool, wait time.Duration, msgAndArgs ...any) {
	e.t.Helper()
	require.EventuallyWithT(e.t, func(c *assert.CollectT) {
		st, err := e.maint.Get(context.Background())
		if !assert.NoError(c, err, "read the maintenance") {
			return
		}
		assert.True(c, cond(st), "the maintenance is %+v", st)
	}, wait, 5*time.Millisecond, msgAndArgs...)
}

// waitMaintenanceGone waits until no maintenance is active.
func (e *env) waitMaintenanceGone(wait time.Duration, msgAndArgs ...any) {
	e.t.Helper()
	e.waitMaintenance(func(st *maintmode.State) bool { return st == nil }, wait, msgAndArgs...)
}

// waitReport waits until the latest report of the process id says state.
//
// 期限の待ちを時計で切らせるテストは、プロセスがメンテナンスを見て報告し直すのを
// これで待ってから時計を進める。待たずに進めると、判定役がメンテナンスの前に
// 書かれた running の報告を見て、戻れないはずのプロセスを戻ったものとして
// メンテナンスを外す (#3369)。
func (e *env) waitReport(id, state string) {
	e.t.Helper()
	require.EventuallyWithT(e.t, func(c *assert.CollectT) {
		reports, err := e.maint.Reports(context.Background())
		if !assert.NoError(c, err, "read the reports") {
			return
		}
		for _, r := range reports {
			if r.ID == id {
				assert.Equal(c, state, r.State, "the report of %s", id)
				return
			}
		}
		assert.Fail(c, "no report", "process %s has no report", id)
	}, 10*time.Second, 5*time.Millisecond)
}

func (e *env) maintenance() *maintmode.State {
	e.t.Helper()
	st, err := e.maint.Get(context.Background())
	require.NoError(e.t, err)
	return st
}

func (f *fakeDB) MarkOperation(_ context.Context, _, database, state, opID string) error {
	if err := f.failing("mark"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.dbs[database]
	if !ok {
		return errors.New("no such database " + database)
	}
	if f.marks == nil {
		f.marks = map[string]opMark{}
	}
	f.marks[content] = opMark{state: state, op: opID}
	return nil
}

func (f *fakeDB) OperationMark(_ context.Context, _, database string) (string, string, bool, error) {
	if err := f.failing("operationMark"); err != nil {
		return "", "", false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.dbs[database]
	if !ok {
		return "", "", false, nil
	}
	m := f.marks[content]
	return m.state, m.op, true, nil
}

// opMark is the comment MarkOperation put on a database.
type opMark struct{ state, op string }
