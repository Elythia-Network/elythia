package restoreop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
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
	mu        sync.Mutex
	dbs       map[string]string
	conns     map[string]int
	verified  bool
	fail      map[string]error
	block     chan struct{} // RestoreInto waits on it when set
	drops     []string
	switches  int
	switchErr func(n int) error
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
func (f *fakeDB) Migrate(string) ([]backup.MigrationState, error) { return nil, f.failing("migrate") }

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
}

func (p *fakeProcess) run(ctx context.Context) {
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
			_ = p.store.Report(ctx, maintmode.ProcReport{ID: p.id, Role: "both", Version: "1.0", State: state})
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
	e := &env{t: t, mr: mr, rdb: rdb, db: newFakeDB()}
	e.store = NewStore(rdb, "p:")
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
		ModLog: func(_ context.Context, database, _ string, info map[string]any) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.modlogs = append(e.modlogs, fmt.Sprintf("%s:%s:%s", database, e.db.content(database), info["phase"]))
			return nil
		},
		Notify: func(_ context.Context, msg string) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.notes = append(e.notes, msg)
		},
		Database: testDB, MaintenanceDB: "postgres", Version: "1.0",
		ReadyTimeout: time.Hour, HeldTimeout: 500 * time.Millisecond, ResumeTimeout: 500 * time.Millisecond,
		Poll:   5 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return time.Now() },
	}
	for _, f := range mutate {
		f(&e.opts)
	}
	e.start()
	return e
}

// start starts a manager (again, after stop, for the recovery tests).
func (e *env) start() {
	// 起動し直したサービスは、新しい job の枠を持つ。
	e.slot = &slot{}
	e.opts.Acquire = e.slot.acquire
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.m = New(e.opts)
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

func (e *env) process(id string) *fakeProcess {
	p := &fakeProcess{id: id, store: e.maint, db: e.db}
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

func (e *env) prepare() (*Record, string) {
	e.t.Helper()
	rec, ticket, err := e.m.Prepare(context.Background(), PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "session-token-01"})
	require.NoError(e.t, err)
	e.waitStep(rec.OpID, StepReady)
	return rec, ticket
}

func (e *env) maintenance() *maintmode.State {
	e.t.Helper()
	st, err := e.maint.Get(context.Background())
	require.NoError(e.t, err)
	return st
}
