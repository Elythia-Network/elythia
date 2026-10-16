package restoreop

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
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
	// The rollback (#3463 (6d)).
	CheckDatabaseMigrations(ctx context.Context, dbname string) (*backup.DatabaseMigrations, error)
	CheckRollback(ctx context.Context, maintenance, database, kept, keepAs string) error
	KeptDatabases(ctx context.Context, maintenance, database string) ([]backup.KeptDatabase, error)
	// MarkOperation / OperationMark keep, outside Redis, which operation is
	// switching a database out and whether it has ended
	// (backup.Restorer.MarkOperation).
	MarkOperation(ctx context.Context, maintenance, database, state, opID string) error
	OperationMark(ctx context.Context, maintenance, database string) (state, opID string, exists bool, err error)
}

// Maintenance is the maintenance state. *maintmode.Store implements it.
type Maintenance interface {
	Get(ctx context.Context) (*maintmode.State, error)
	Enter(ctx context.Context, st maintmode.State) error
	SetPhase(ctx context.Context, opID, phase string) error
	Clear(ctx context.Context, opID string) error
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
	// ErrInvalidKept is returned for a rollback to a name that is not one of
	// a database kept by a restore or a rollback (<db>_before_restore_<t>,
	// <db>_rolled_back_<t>).
	ErrInvalidKept = errors.New("restoreop: not a database kept by a restore or a rollback")
	// ErrNoSuchKept is returned for a rollback to a kept database that does
	// not exist.
	ErrNoSuchKept = errors.New("restoreop: no such kept database")
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
	// ModLog records a moderation log of an operation of kind (KindRestore
	// or KindRollback) in database. Failures are logged only.
	ModLog func(ctx context.Context, database, userID, kind string, info map[string]any) error
	// Acquire takes the service's job slot so that no scheduled take runs
	// during the operation. The returned function gives it back.
	Acquire func() (release func(), err error)
	// SlotBusy reports whether another job holds the slot now. CanStart
	// asks it so that a request is refused before its re-authentication
	// (which would spend the TOTP code). Nil means never busy.
	SlotBusy func() bool
	// Recovering is told when the recovery at start begins (true) and ends
	// (false), so that the service holds back its own start-up work until
	// then. Nil is fine.
	Recovering func(bool)
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

	// stalled is set when a step could not be written; settle recovers.
	// 枠のおかげで操作は同時に 1 つなので、Manager に 1 つでよい。
	stalled atomic.Bool

	// failStep, when set by a test, fails writing the step to.
	failStep func(to string) error
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
	if m.failStep != nil {
		if err := m.failStep(to); err != nil {
			return err
		}
	}
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

// journalLine is one journal line to write together with a step
// (setStepWithLine).
type journalLine struct {
	step, msg string
}

// say makes a journal line for setStepWithLine and recordWithLine.
func say(step, format string, args ...any) journalLine {
	return journalLine{step: step, msg: fmt.Sprintf(format, args...)}
}

// errStepHook marks an error returned by failStep: nothing was sent to
// Redis.
var errStepHook = errors.New("restoreop: the step was not written (test hook)")

// lineWrite is a journal line on its way into the journal: the entry is made
// once, so that a line whose write had an unknown outcome can be looked for
// (appendLine).
type lineWrite struct {
	opID  string
	entry JournalEntry
}

// setStepWithLine is setStep that appends l to the journal in the same
// write, when the step moves. Use it for a final step and the line that
// tells how the operation ended: the admin page stops reading once the step
// is final (Status.Final). Use it also for a step that another goroutine can
// move on from (ready, aborting from preparing), so that the line is in the
// journal before any line of the next step (#3626). It returns the line for
// appendLine.
func (m *Manager) setStepWithLine(ctx context.Context, opID, from, to string, l journalLine, extra ...any) (lineWrite, error) {
	w := lineWrite{opID: opID, entry: normalizeEntry(JournalEntry{At: m.now(), Step: l.step, Message: l.msg})}
	if m.failStep != nil {
		if err := m.failStep(to); err != nil {
			return w, fmt.Errorf("%w: %w", errStepHook, err)
		}
	}
	if err := m.o.Store.setStepJournal(context.WithoutCancel(ctx), opID, from, to, m.now(), &w.entry, extra...); err != nil {
		return w, err
	}
	// ログは書けた後に出す。書けなかった段のログを残さない (送り直しで書けて
	// いたときは wroteLine が、行だけを書き直すときは appendLine が出す)。
	m.logLine(w)
	return w, nil
}

// logLine logs the line of w as written.
func (m *Manager) logLine(w lineWrite) {
	m.log.Info("backup: restore", "opId", w.opID, "step", w.entry.Step, "message", w.entry.Message)
}

// wroteLine reports whether err from setStepWithLine came from a write that
// went through all the same: the error says the outcome is not known (a
// replay's ErrStep, a timeout) and the line of w is in the journal. It logs
// the line then, as setStepWithLine does on success.
func (m *Manager) wroteLine(ctx context.Context, w lineWrite, err error) bool {
	if err == nil || notSent(err) {
		return false
	}
	if written, _ := m.lineWritten(ctx, w); !written {
		return false
	}
	m.logLine(w)
	return true
}

// normalizeEntry returns e as it reads back from the journal (through JSON).
//
// lineWritten は読み戻した行と比べる。JSON は不正な UTF-8 を U+FFFD に置き換える
// (pg_restore の stderr はバイト単位で切るので、多バイト文字の途中で切れうる)
// ので、元の文面のままだと一致せず、書けた行を書けなかったと誤る。
func normalizeEntry(e JournalEntry) JournalEntry {
	b, err := json.Marshal(e)
	if err != nil {
		return e
	}
	var out JournalEntry
	if json.Unmarshal(b, &out) != nil {
		return e
	}
	return out
}

// recordWithLine is record with the journal line of setStepWithLine. When
// the step cannot be written, the line is still appended on its own, as
// record followed by journal did.
func (m *Manager) recordWithLine(ctx context.Context, opID, from, to string, l journalLine, extra ...any) {
	if w, err := m.setStepWithLine(ctx, opID, from, to, l, extra...); err != nil {
		m.log.Error("backup: cannot record the step of a restore", "opId", opID, "from", from, "to", to, "err", err)
		m.appendLine(ctx, w, err)
	}
}

// lineWritten reports whether the line of w is in the journal: whether a
// write that failed with an error other than ErrNotFound or errStepHook had
// in fact moved the step.
//
// go-redis は読み込みの時間切れや切断で script を自動で送り直す (MaxRetries)。
// 1 回目で段と行が書けて応答だけが失われると、2 回目は段が違うので ErrStep を
// 返す。ErrStep は「書けなかった」の印にならないので、行で確かめる。段だけで
// 確かめないのは、同じ段へ別の経路が進めた場合と見分けるため。
//
// It relies on At (Options.Now, nanoseconds in production) being unique per
// write: two writes of the same step and message with the same At cannot be
// told apart.
func (m *Manager) lineWritten(ctx context.Context, w lineWrite) (bool, error) {
	journal, err := m.o.Store.Journal(context.WithoutCancel(ctx), w.opID)
	if err != nil {
		return false, err
	}
	for _, j := range journal {
		if j.At.Equal(w.entry.At) && j.Step == w.entry.Step && j.Message == w.entry.Message {
			return true, nil
		}
	}
	return false, nil
}

// notSent reports whether err from setStepWithLine says that nothing was
// written: the record does not exist (the script returned 0, which a write
// that went through cannot make it do) or the test hook refused.
func notSent(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, errStepHook)
}

// appendLine appends the line of w on its own after setStepWithLine failed
// with err, unless the failed write did append it.
//
// **送っていないと分かる誤り (notSent) のとき以外は、journal を読み直して同じ行が
// 無いときだけ足す。** 結果の分からない誤り (応答の前の時間切れ・切断) や、送り
// 直しが返した ErrStep では、行が既に入っていることがあり、そのまま足すと結末の
// 行が2つ並ぶ。読めなければ足す (欠けるより、重なる方を選ぶ)。
func (m *Manager) appendLine(ctx context.Context, w lineWrite, err error) {
	ctx = context.WithoutCancel(ctx)
	m.logLine(w)
	if !notSent(err) {
		if written, werr := m.lineWritten(ctx, w); werr == nil && written {
			return
		}
	}
	if aerr := m.o.Store.appendJournal(ctx, w.opID, w.entry); aerr != nil {
		m.log.Warn("backup: cannot write the restore journal", "opId", w.opID, "err", aerr)
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
		m.prepare(pctx, rec, func(ctx context.Context) error { return m.restoreInto(ctx, rec, plan, opts) }, live)
	}()
	return rec, ticket, nil
}

// Database is the name of the server's database (db.db).
func (m *Manager) Database() string { return m.o.Database }

// CanStart reports why an operation cannot start now (a job holding the
// slot, an unfinished one, an active maintenance, replicas). It reads Redis only, so the control API
// calls it before it spends a re-authentication, which opens the database.
func (m *Manager) CanStart(ctx context.Context) error {
	// 枠は Prepare が取るので、ここでは今空いているかだけを見る (Prepare の中の確かめ
	// では見ない。自分が取った枠で断ってしまう)。
	if m.o.SlotBusy != nil && m.o.SlotBusy() {
		return ErrBusy
	}
	return m.canStart(ctx)
}

func (m *Manager) canStart(ctx context.Context) error {
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
	if err := m.canStart(ctx); err != nil {
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
		OpID: newOpID(), Kind: KindRestore, Step: StepPreparing, GenerationID: plan.ID, Database: m.o.Database,
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
	if rec.Kind == KindRollback {
		info = map[string]any{"database": rec.Database, "from": rec.Target, "keptAs": rec.Before, "opId": rec.OpID, "phase": phase}
	}
	if err := m.o.ModLog(context.WithoutCancel(ctx), database, rec.UserID, rec.Kind, info); err != nil {
		m.log.Warn("backup: cannot write the moderation log of a restore", "opId", rec.OpID, "database", database, "err", err)
	}
}

// prepare runs work (restoring into the new database, or migrating the kept
// database of a rollback) and moves the operation to ready.
func (m *Manager) prepare(ctx context.Context, rec *Record, work func(context.Context) error, live *liveOp) {
	err := work(ctx)
	if err == nil {
		// ready の段と行は 1 つの script で書く。別々に書くと、ready を見た管理者の
		// 中止・確定が先に結末の行を書き、その後に ready の行が入る (#3626)。
		var w lineWrite
		w, err = m.setStepWithLine(ctx, rec.OpID, StepPreparing, StepReady,
			say(StepReady, "ready to switch; waiting for the confirmation"), "readyAt", formatTime(m.now()))
		// 送り直しが返した ErrStep や結果の分からない誤りでも、行が入っていれば
		// ready は書けている (Abort と同じ扱い)。
		if m.wroteLine(ctx, w, err) {
			err = nil
		}
		if err == nil {
			m.waitConfirmation(ctx, rec, live)
			return
		}
	}
	// 作った DB だけを消す。今の DB には触れていない。作る前に落ちたとき (同じ名前の DB
	// を確かめた後で別の作業が作った場合を含む) は、その名前の DB は自分のものではない
	// ので消さない。ロールバックの戻す先は、dropTarget が消さない。
	if !errors.Is(err, backup.ErrRestoreNotCreated) {
		m.dropTarget(ctx, rec)
	}
	msg := err.Error()
	if rec.Kind == KindRollback {
		msg += "; " + targetFate(rec)
	}
	l := journalLine{step: StepFailed, msg: msg}
	w, serr := m.setStepWithLine(ctx, rec.OpID, StepPreparing, StepFailed, l, "error", err.Error())
	if errors.Is(serr, ErrStep) {
		written, werr := m.lineWritten(ctx, w)
		switch {
		case werr != nil:
			// 読めなければ、送り直しか中止の先行かが分からない。aborted へは進まず、
			// failed の行を appendLine に任せる (段は記録のまま残る)。
			serr = fmt.Errorf("%w (and the journal could not be read: %w)", serr, werr)
		case written:
			// 送り直しが返した ErrStep。1 回目で failed と行が書けている。
			m.logLine(w)
			serr = nil
		default:
			// 中止 (Abort) が先に aborting にした。
			l.step = StepAborted
			w, serr = m.setStepWithLine(ctx, rec.OpID, StepAborting, StepAborted, l)
		}
	}
	if serr != nil {
		m.log.Error("backup: cannot record the end of a restore", "opId", rec.OpID, "err", serr)
		m.appendLine(ctx, w, serr)
	}
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
	m.recordWithLine(ctx, rec.OpID, StepAborting, StepExpired,
		say(StepExpired, "%s: nobody confirmed within %s", targetFate(rec), m.o.ReadyTimeout))
	m.finishLive(rec.OpID)
}

// dropTarget drops the new database of a restore. It never drops the
// database of a rollback (the kept one it was going to swap back in), nor
// one that has the server's name or the name of a kept database.
//
// ロールバックの Target は退避した DB そのもので、消すと戻す先を失う。種類で
// 分けたうえで、名前でも守る (種類を取り違えた記録や、手で書き換えた記録でも
// 消さない)。
func (m *Manager) dropTarget(ctx context.Context, rec *Record) {
	if rec.Kind == KindRollback {
		// 戻す先をどうしたかは、呼び出し元が最後の行に targetFate で書く (ここでも
		// 書くと、同じ行が2つ並ぶ)。
		return
	}
	if keptName(rec.Target, rec.Database) || keptName(rec.Target, m.o.Database) || !isRestoreName(m.o.Database, rec.Target) {
		m.log.Error("backup: refusing to drop a database this restore did not create", "opId", rec.OpID, "database", rec.Target)
		m.journal(ctx, rec.OpID, "", "did not drop %s: it is not a database this restore created", rec.Target)
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := m.o.DB.DropDatabase(dctx, m.o.MaintenanceDB, rec.Target); err != nil {
		m.journal(ctx, rec.OpID, "", "could not drop %s: %v", rec.Target, err)
	}
}

// targetFate tells in the journal what became of rec's Target when the
// operation ended before the switch: a restore dropped the database it
// created, a rollback keeps the kept database (dropTarget).
func targetFate(rec *Record) string {
	if rec.Kind == KindRollback {
		return "kept " + rec.Target + " as it is"
	}
	return "dropped " + rec.Target
}

// errBadRecordNames reports a record whose database names do not have the
// shapes this service writes.
var errBadRecordNames = errors.New("the database names in the record do not have the shapes this service writes")

// checkRecordNames reports whether the database names of rec have the shapes
// this service writes for database, before they are dropped or renamed.
//
// **記録の名前を信じて DROP / RENAME しない。** 記録は Redis にあり、Redis に書ける
// 者 (passwordguard やメンテナンスの状態を書き換えられる境界) が target を任意の
// DB の名前にして daemon を起動し直させると、回復が同じクラスタの別の DB を消せた。
// 名前は時刻から機械的に作るので、形で確かめられる: Database は設定の DB、
// リストアの Target は <db>_restore_<時刻>、ロールバックの Target と Before / Aside
// は退避した DB の名前 (<db>_before_restore_<時刻> か <db>_rolled_back_<時刻>)。
func checkRecordNames(rec *Record, database string) error {
	if rec == nil || rec.Database != database || rec.integrity == integrityBad {
		return errBadRecordNames
	}
	// **Target と Before の組も、作るときの形と合っているかを見る。** リストアの
	// 組は RestoreNames が同じ時刻で作り、ロールバックの Before は RollbackName
	// (rolled_back) で作る。形の合わない記録 (壊れた記録や、別の操作の名前を
	// 混ぜた記録) で DB を入れ替えない。
	//
	// **形の検査だけでは記録の真正さは分からない。** Redis に書ける者は、残って
	// いる退避の DB と同じ時刻の組で記録を作れる。backup.integrityKeyFile を設定
	// すると記録に署名し (Store.SetIntegrityKey)、署名の合わない記録もここで断る。
	switch rec.Kind {
	case KindRollback:
		if _, _, ok := backup.ParseKeptName(database, rec.Target); !ok {
			return errBadRecordNames
		}
		if kind, _, ok := backup.ParseKeptName(database, rec.Before); !ok || kind != backup.KeptRolledBack {
			return errBadRecordNames
		}
	default:
		if !isRestoreName(database, rec.Target) {
			return errBadRecordNames
		}
		if rec.Before != database+"_"+backup.KeptBeforeRestore+"_"+strings.TrimPrefix(rec.Target, database+"_restore_") {
			return errBadRecordNames
		}
	}
	if rec.Aside != "" {
		if _, _, ok := backup.ParseKeptName(database, rec.Aside); !ok {
			return errBadRecordNames
		}
	}
	return nil
}

// isRestoreName reports whether name is the new database of a restore of
// database (backup.RestoreNames): <db>_restore_<14 digits>.
func isRestoreName(database, name string) bool {
	suffix, ok := strings.CutPrefix(name, database+"_restore_")
	if !ok || len(suffix) != 14 {
		return false
	}
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// keptName reports whether name is database itself, a database kept by a
// restore or a rollback of it, or empty.
func keptName(name, database string) bool {
	return name == "" || name == database ||
		strings.HasPrefix(name, database+"_before_restore_") || strings.HasPrefix(name, database+"_rolled_back_")
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
	if !ValidOpID(opID) {
		return nil, ErrNotFound
	}
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
	if err := checkRecordNames(rec, m.o.Database); err != nil {
		// 準備した後に記録が書き換えられた。名前を入れ替えずに運営者へ渡す。
		live.cancelPrepare()
		m.giveUp(ctx, rec, StepEntering, err)
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
		// aborting の段と「abort requested」の行は 1 つの script で書く。別々に書くと、
		// 準備の goroutine が先に aborted の結末の行を書き、その後にこの行が入る (#3626)。
		w, err := m.setStepWithLine(ctx, opID, StepPreparing, StepAborting,
			say(StepPreparing, "abort requested"), "error", "aborted by the administrator")
		if m.wroteLine(ctx, w, err) {
			err = nil
		}
		if err != nil {
			return err
		}
		live.cancelPrepare()
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
		w, err := m.setStepWithLine(ctx, opID, StepAborting, StepAborted, say(StepAborted, "aborted; %s", targetFate(rec)))
		// 送り直しが返した ErrStep や結果の分からない誤りでも、行が入っていれば
		// 書けている。
		if m.wroteLine(ctx, w, err) {
			err = nil
		}
		if err != nil {
			return err
		}
		m.finishLive(opID)
		return nil
	}
	return ErrStep
}
