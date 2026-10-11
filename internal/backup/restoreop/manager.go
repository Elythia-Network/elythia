package restoreop

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/maintmode"
)

// DB is the database side of a restore. *backup.Restorer implements it.
type DB interface {
	Load(ctx context.Context, id string) (*backup.RestorePlan, error)
	CheckTarget(ctx context.Context, opts backup.RestoreOptions) error
	CheckCompatibility(ctx context.Context, plan *backup.RestorePlan, opts backup.RestoreOptions) error
	RestoreInto(ctx context.Context, plan *backup.RestorePlan, opts backup.RestoreOptions, dbname string) error
	VerifyRestored(ctx context.Context, dbname string, meta backup.Meta) error
	ApplyDatabaseSettings(ctx context.Context, dbname string, meta backup.Meta) ([]string, error)
	Migrate(dbname string) ([]backup.MigrationState, error)
	SwitchDatabase(ctx context.Context, maintenance, database, keepAs, replacement string) error
	DropDatabase(ctx context.Context, maintenance, name string) error
	ExistingDatabases(ctx context.Context, maintenance string, names []string) (map[string]bool, error)
}

// Maintenance is the maintenance state. *maintmode.Store implements it.
type Maintenance interface {
	Get(ctx context.Context) (*maintmode.State, error)
	Enter(ctx context.Context, st maintmode.State) error
	SetPhase(ctx context.Context, opID, phase string) error
	Reports(ctx context.Context) ([]maintmode.ProcReport, error)
}

// Errors returned before an operation starts.
var (
	ErrNotRunning        = errors.New("restoreop: the backup service is not running")
	ErrInvalidID         = errors.New("restoreop: not a generation id")
	ErrNotVerified       = errors.New("restoreop: the generation has not passed verification")
	ErrMaintenanceActive = errors.New("restoreop: a maintenance is already active")
	ErrReplicas          = errors.New("restoreop: restoring from the admin page is not supported with dbReplications")
	ErrVersionMismatch   = errors.New("restoreop: a process runs another version of Elythia")
	ErrTicket            = errors.New("restoreop: the ticket does not match")
)

// Options configure a Manager.
type Options struct {
	Store *Store
	DB    DB
	Maint Maintenance
	// RunJudge runs a maintenance judge until ctx is done. The service
	// competes for the judge lock like every other process (v2 §2.1).
	RunJudge func(ctx context.Context)
	// CleanRedis deletes what points at rows of the previous database.
	CleanRedis func(ctx context.Context) error
	// Check runs the checks of the database that now has the server's name.
	Check func(ctx context.Context) error
	// ModLog records a moderation log in database. Failures are logged only.
	ModLog func(ctx context.Context, database, userID string, info map[string]any) error
	// Acquire takes the service's job slot so that no scheduled take runs
	// during the operation. The returned function gives it back.
	Acquire func() (release func(), err error)
	// Notify reports a state an operator has to resolve.
	Notify func(ctx context.Context, message string)

	// Database is db.db; MaintenanceDB is maintenance.maintenanceDb.
	Database      string
	MaintenanceDB string
	// Replicas is true when dbReplications is on.
	Replicas bool
	// Version and Commit of this binary, compared with the live reports.
	Version, Commit string

	// ReadyTimeout drops a prepared database nobody confirmed (30 minutes).
	ReadyTimeout time.Duration
	// HeldTimeout is how long it waits for the processes to let go
	// (drainTimeout + 2 minutes).
	HeldTimeout time.Duration
	// ResumeTimeout is how long it waits for the processes to serve again
	// before putting the previous database back (5 minutes).
	ResumeTimeout time.Duration
	// Poll is how often it reads the maintenance state.
	Poll   time.Duration
	Logger *slog.Logger
	Now    func() time.Time
}

// Manager runs the operations. There is at most one at a time.
type Manager struct {
	o   Options
	log *slog.Logger

	mu   sync.Mutex
	base context.Context
	wg   sync.WaitGroup
	// ops holds the operations this process started and has not finished.
	ops map[string]*liveOp
}

type liveOp struct {
	cancelPrepare context.CancelFunc
	releaseOnce   sync.Once
	release       func()
}

func (l *liveOp) done() {
	l.releaseOnce.Do(func() {
		if l.release != nil {
			l.release()
		}
	})
}

// New returns a Manager. Call Start before using it.
func New(o Options) *Manager {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = 30 * time.Minute
	}
	if o.HeldTimeout <= 0 {
		o.HeldTimeout = 150 * time.Second
	}
	if o.ResumeTimeout <= 0 {
		o.ResumeTimeout = 5 * time.Minute
	}
	if o.Poll <= 0 {
		o.Poll = time.Second
	}
	if o.Acquire == nil {
		o.Acquire = func() (func(), error) { return func() {}, nil }
	}
	return &Manager{o: o, log: o.Logger, ops: map[string]*liveOp{}}
}

// Start sets the context the operations run under (the service's lifetime)
// and recovers an operation an earlier run of the service left unfinished.
// It returns once the recovery has been started; Wait waits for it.
func (m *Manager) Start(ctx context.Context) {
	m.StartHolding(ctx, nil)
}

// StartHolding is Start with the job slot already taken (reserved before the
// service started its own jobs). The recovery uses it and gives it back.
func (m *Manager) StartHolding(ctx context.Context, release func()) {
	m.mu.Lock()
	m.base = ctx
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.recoverOnStart(ctx, release)
	}()
}

// Wait waits for the running operations to return (after the context of
// Start is done).
func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) now() time.Time { return m.o.Now() }

// journal records one step. 書けなくても操作は止めない (記録は表示と調査のため)。
func (m *Manager) journal(ctx context.Context, opID, step, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	m.log.Info("backup: restore", "opId", opID, "step", step, "message", msg)
	if err := m.o.Store.appendJournal(context.WithoutCancel(ctx), opID, JournalEntry{At: m.now(), Step: step, Message: msg}); err != nil {
		m.log.Warn("backup: cannot write the restore journal", "opId", opID, "err", err)
	}
}

// setStep writes the step. 状態は「書いてから進める」。書けなければ進めない。
func (m *Manager) setStep(ctx context.Context, opID, from, to string, extra ...any) error {
	return m.o.Store.setStep(context.WithoutCancel(ctx), opID, from, to, m.now(), extra...)
}

// record is setStep for the places that go on when the record cannot be
// written: the actions after it are safe to repeat, and a restart decides
// from the database names when the record lags behind.
func (m *Manager) record(ctx context.Context, opID, from, to string, extra ...any) {
	if err := m.setStep(ctx, opID, from, to, extra...); err != nil {
		m.log.Error("backup: cannot record the step of a restore", "opId", opID, "from", from, "to", to, "err", err)
	}
}

// PrepareRequest asks for a restore of one generation.
type PrepareRequest struct {
	GenerationID string
	UserID       string
	// Token is the administrator's session token. Only its hash is kept,
	// to tie the ticket to the session.
	Token string
}

// Prepare checks the request and starts restoring into a new database while
// the server keeps running. It returns the record and the ticket the
// administrator presents to see the progress, confirm or abort.
func (m *Manager) Prepare(ctx context.Context, req PrepareRequest) (*Record, string, error) {
	m.mu.Lock()
	base := m.base
	m.mu.Unlock()
	if base == nil || base.Err() != nil {
		return nil, "", ErrNotRunning
	}
	if !backup.ValidID(req.GenerationID) {
		return nil, "", ErrInvalidID
	}
	release, err := m.o.Acquire()
	if err != nil {
		return nil, "", err
	}
	rec, ticket, plan, opts, err := m.prepareChecks(ctx, req)
	if err != nil {
		release()
		return nil, "", err
	}
	pctx, cancel := context.WithCancel(base)
	live := &liveOp{cancelPrepare: cancel, release: release}
	m.mu.Lock()
	m.ops[rec.OpID] = live
	m.mu.Unlock()
	m.journal(ctx, rec.OpID, StepPreparing, "restoring generation %s into %s", rec.GenerationID, rec.Target)
	m.modlog(ctx, m.o.Database, rec, "prepare")
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		m.prepare(pctx, rec, plan, opts, live)
	}()
	return rec, ticket, nil
}

// CanStart reports why an operation cannot start now (an unfinished one, an
// active maintenance, replicas). It reads Redis only, so the control API
// calls it before it spends a re-authentication, which opens the database.
func (m *Manager) CanStart(ctx context.Context) error {
	if cur, err := m.o.Store.Current(ctx); err != nil {
		return err
	} else if cur != nil {
		return ErrBusy
	}
	if err := m.NoMaintenance(ctx); err != nil {
		return err
	}
	if m.o.Replicas {
		return ErrReplicas
	}
	return nil
}

// NoMaintenance returns ErrMaintenanceActive while any maintenance is on.
//
// メンテナンス中は、再認証で DB へ接続を張らない。張ると、そのメンテナンスの
// 「接続が無いこと」の確認を邪魔する。
func (m *Manager) NoMaintenance(ctx context.Context) error {
	st, err := m.o.Maint.Get(ctx)
	if err != nil {
		return err
	}
	if st != nil {
		return ErrMaintenanceActive
	}
	return nil
}

func (m *Manager) prepareChecks(ctx context.Context, req PrepareRequest) (*Record, string, *backup.RestorePlan, backup.RestoreOptions, error) {
	var opts backup.RestoreOptions
	if err := m.CanStart(ctx); err != nil {
		return nil, "", nil, opts, err
	}
	if err := m.checkVersions(ctx); err != nil {
		return nil, "", nil, opts, err
	}
	plan, err := m.o.DB.Load(ctx, req.GenerationID)
	if err != nil {
		return nil, "", nil, opts, err
	}
	if !plan.Verified {
		// 画面からは確認に通った世代だけを戻す (設計 §8-1)。CLI は警告付きで名指しできる。
		return nil, "", nil, opts, ErrNotVerified
	}
	at := m.now()
	target, before, err := backup.RestoreNames(m.o.Database, at)
	if err != nil {
		return nil, "", nil, opts, err
	}
	opts = backup.RestoreOptions{
		ID: plan.ID, Mode: backup.RestoreSwap, Database: m.o.Database, Confirm: m.o.Database,
		MaintenanceDB: m.o.MaintenanceDB, SkipConnectionCheck: true, At: at,
	}
	if err := m.o.DB.CheckTarget(ctx, opts); err != nil {
		return nil, "", nil, opts, err
	}
	if err := m.o.DB.CheckCompatibility(ctx, plan, opts); err != nil {
		return nil, "", nil, opts, err
	}
	ticket, ticketHash := newTicket()
	now := m.now()
	rec := &Record{
		OpID: newOpID(), Step: StepPreparing, GenerationID: plan.ID, Database: m.o.Database,
		Target: target, Before: before, UserID: req.UserID, CreatedAt: now, UpdatedAt: now,
		ticketHash: ticketHash, tokenHash: hashString(req.Token),
	}
	if err := m.o.Store.create(ctx, rec); err != nil {
		return nil, "", nil, opts, err
	}
	return rec, ticket, plan, opts, nil
}

// checkVersions refuses when a live process runs another version: the
// service migrates the restored database with its own bundled migrations,
// which the processes must match to start on it (v1 §3.3).
func (m *Manager) checkVersions(ctx context.Context) error {
	reports, err := m.o.Maint.Reports(ctx)
	if err != nil {
		return err
	}
	for _, r := range reports {
		if r.Version != m.o.Version || (m.o.Commit != "" && r.Commit != "" && r.Commit != m.o.Commit) {
			return fmt.Errorf("%w: process %s runs %s (%s), the backup service %s (%s)",
				ErrVersionMismatch, r.ID, r.Version, r.Commit, m.o.Version, m.o.Commit)
		}
	}
	return nil
}

func (m *Manager) modlog(ctx context.Context, database string, rec *Record, phase string) {
	if m.o.ModLog == nil {
		return
	}
	info := map[string]any{"generationId": rec.GenerationID, "opId": rec.OpID, "phase": phase}
	if err := m.o.ModLog(context.WithoutCancel(ctx), database, rec.UserID, info); err != nil {
		m.log.Warn("backup: cannot write the moderation log of a restore", "opId", rec.OpID, "database", database, "err", err)
	}
}

// prepare restores into the new database and moves the operation to ready.
func (m *Manager) prepare(ctx context.Context, rec *Record, plan *backup.RestorePlan, opts backup.RestoreOptions, live *liveOp) {
	err := m.restoreInto(ctx, rec, plan, opts)
	if err == nil {
		err = m.setStep(ctx, rec.OpID, StepPreparing, StepReady, "readyAt", formatTime(m.now()))
		if err == nil {
			m.journal(ctx, rec.OpID, StepReady, "ready to switch; waiting for the confirmation")
			m.waitConfirmation(ctx, rec, live)
			return
		}
	}
	// 作った DB だけを消す。今の DB には触れていない。
	m.dropTarget(ctx, rec)
	final := StepFailed
	serr := m.setStep(ctx, rec.OpID, StepPreparing, StepFailed, "error", err.Error())
	if errors.Is(serr, ErrStep) {
		// 中止 (Abort) が先に aborting にした。
		final = StepAborted
		serr = m.setStep(ctx, rec.OpID, StepAborting, StepAborted)
	}
	if serr != nil {
		m.log.Error("backup: cannot record the end of a restore", "opId", rec.OpID, "err", serr)
	}
	m.journal(ctx, rec.OpID, final, "%v", err)
	m.finishLive(rec.OpID)
}

func (m *Manager) restoreInto(ctx context.Context, rec *Record, plan *backup.RestorePlan, opts backup.RestoreOptions) error {
	if err := m.o.DB.RestoreInto(ctx, plan, opts, rec.Target); err != nil {
		return err
	}
	m.journal(ctx, rec.OpID, StepPreparing, "restored into %s", rec.Target)
	if err := m.o.DB.VerifyRestored(ctx, rec.Target, plan.Meta); err != nil {
		return err
	}
	m.journal(ctx, rec.OpID, StepPreparing, "row counts match")
	if _, err := m.o.DB.ApplyDatabaseSettings(ctx, rec.Target, plan.Meta); err != nil {
		return err
	}
	if _, err := m.o.DB.Migrate(rec.Target); err != nil {
		return err
	}
	m.journal(ctx, rec.OpID, StepPreparing, "migrations applied")
	return ctx.Err()
}

// waitConfirmation drops the prepared database when nobody confirms within
// ReadyTimeout.
func (m *Manager) waitConfirmation(ctx context.Context, rec *Record, live *liveOp) {
	timer := time.NewTimer(m.o.ReadyTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		// 中止 (Abort) かサービスの停止。中止は Abort が片付ける。停止なら、起動
		// し直したときに Recover が片付ける。
		return
	case <-timer.C:
	}
	if err := m.setStep(ctx, rec.OpID, StepReady, StepAborting, "error", "nobody confirmed within the ready window"); err != nil {
		// 確定か中止が先に進めた。
		return
	}
	m.dropTarget(ctx, rec)
	m.record(ctx, rec.OpID, StepAborting, StepExpired)
	m.journal(ctx, rec.OpID, StepExpired, "dropped %s: nobody confirmed within %s", rec.Target, m.o.ReadyTimeout)
	m.finishLive(rec.OpID)
}

func (m *Manager) dropTarget(ctx context.Context, rec *Record) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := m.o.DB.DropDatabase(dctx, m.o.MaintenanceDB, rec.Target); err != nil {
		m.journal(ctx, rec.OpID, "", "could not drop %s: %v", rec.Target, err)
	}
}

func (m *Manager) finishLive(opID string) {
	m.mu.Lock()
	live := m.ops[opID]
	delete(m.ops, opID)
	m.mu.Unlock()
	if live != nil {
		live.done()
	}
}

// CheckTicket returns the record of opID when ticket and token are the ones
// it was prepared with.
func (m *Manager) CheckTicket(ctx context.Context, opID, ticket, token string) (*Record, error) {
	rec, err := m.o.Store.Get(ctx, opID)
	if err != nil {
		return nil, err
	}
	okTicket := subtle.ConstantTimeCompare([]byte(hashString(ticket)), []byte(rec.ticketHash)) == 1
	okToken := subtle.ConstantTimeCompare([]byte(hashString(token)), []byte(rec.tokenHash)) == 1
	if ticket == "" || !okTicket || !okToken {
		return nil, ErrTicket
	}
	return rec, nil
}

// Status is what the admin page shows.
type Status struct {
	Record      *Record          `json:"record"`
	Journal     []JournalEntry   `json:"journal"`
	Maintenance *maintmode.State `json:"maintenance"`
	Final       bool             `json:"final"`
}

// Status returns the progress of rec.
func (m *Manager) Status(ctx context.Context, rec *Record) (*Status, error) {
	journal, err := m.o.Store.Journal(ctx, rec.OpID)
	if err != nil {
		return nil, err
	}
	st, err := m.o.Maint.Get(ctx)
	if err != nil {
		return nil, err
	}
	if st != nil && st.OpID != rec.OpID {
		st = nil
	}
	return &Status{Record: rec, Journal: journal, Maintenance: st, Final: Final(rec.Step)}, nil
}

// Confirm starts the switch of a ready operation: the maintenance, the name
// swap, the cleanup and the resume.
func (m *Manager) Confirm(ctx context.Context, opID string) error {
	m.mu.Lock()
	base := m.base
	live := m.ops[opID]
	m.mu.Unlock()
	if base == nil || base.Err() != nil {
		return ErrNotRunning
	}
	if live == nil {
		// この実行のサービスが準備したものではない (起動し直した後など)。
		return ErrStep
	}
	if err := m.setStep(ctx, opID, StepReady, StepEntering); err != nil {
		return err
	}
	rec, err := m.o.Store.Get(ctx, opID)
	if err != nil {
		return err
	}
	// 確定したので、準備の待ち (期限) は止める。
	live.cancelPrepare()
	m.journal(ctx, opID, StepEntering, "confirmed; entering the maintenance")
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.finishLive(opID)
		m.switchOver(base, rec)
	}()
	return nil
}

// Abort cancels an operation that has not been confirmed.
func (m *Manager) Abort(ctx context.Context, opID string) error {
	rec, err := m.o.Store.Get(ctx, opID)
	if err != nil {
		return err
	}
	switch rec.Step {
	case StepPreparing:
		m.mu.Lock()
		live := m.ops[opID]
		m.mu.Unlock()
		if live == nil {
			return ErrStep
		}
		// 先に aborting にしてから止める。止めた後に準備が ready を書くと、誰も期限を
		// 見ない ready が残る。準備の goroutine が作った DB を消して aborted にする。
		if err := m.setStep(ctx, opID, StepPreparing, StepAborting, "error", "aborted by the administrator"); err != nil {
			return err
		}
		live.cancelPrepare()
		m.journal(ctx, opID, StepPreparing, "abort requested")
		return nil
	case StepReady:
		if err := m.setStep(ctx, opID, StepReady, StepAborting, "error", "aborted by the administrator"); err != nil {
			return err
		}
		m.mu.Lock()
		if live := m.ops[opID]; live != nil {
			live.cancelPrepare()
		}
		m.mu.Unlock()
		m.dropTarget(ctx, rec)
		if err := m.setStep(ctx, opID, StepAborting, StepAborted); err != nil {
			return err
		}
		m.journal(ctx, opID, StepAborted, "aborted; dropped %s", rec.Target)
		m.finishLive(opID)
		return nil
	}
	return ErrStep
}
