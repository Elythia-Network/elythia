package maintmode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/pgactivity"
)

/*
 * 判定役 (#3463 の設計第2版 §2)。
 *
 * 「DB を手放し終えた」の根拠は pg_stat_activity の実測だけにする。各プロセスの
 * 報告は「まだ待つ理由」を足す方向にだけ使い、報告が消えても欠けても held へは
 * 近づかない。以前は報告 (drained / left) を集めて判定していたため、報告をどの
 * 順序で書くかが正しさを左右し、レビューのたびに順序の穴が出た。
 *
 * 判定役は Redis のロックを取った 1 つ。本体の各プロセスの Supervisor、
 * `elythia maintenance` CLI、(6c) のバックアップのサービスがなれる。
 */

// JudgeLockTTL is how long the judge lock lives without being extended. The
// judge extends it on every step.
const JudgeLockTTL = 10 * time.Second

// ElythiaApplicationPrefix starts the application_name of every connection
// the Elythia processes open (config.Config.DBApplicationName,
// "elythia/<role>/<process ID>"). Only those are terminated after the drain
// timeout, and only for a process that reports it is letting go.
const ElythiaApplicationPrefix = "elythia/"

// maxTerminatedRecorded bounds State.Terminated.
const maxTerminatedRecorded = 100

// pgNameMax is the length PostgreSQL truncates application_name to
// (NAMEDATALEN - 1).
const pgNameMax = 63

// Blocker is a session on the database the maintenance waits for.
type Blocker struct {
	// Server names the PostgreSQL server: "primary", or "replica<N>".
	Server string `json:"server"`
	pgactivity.Session
}

// String formats the blocker for logs and the CLI.
func (b Blocker) String() string { return b.Server + " " + b.Session.String() }

// Counter lists the sessions on the database and terminates them.
type Counter interface {
	// Sessions lists the sessions on the database on every server, except
	// the counter's own and the autovacuum workers.
	Sessions(ctx context.Context) ([]Blocker, error)
	// Terminate ends one session listed by Sessions, if it is still an
	// Elythia session of the counter's role on the database. It reports
	// whether it ended one.
	Terminate(ctx context.Context, b Blocker) (bool, error)
}

// Judge decides, for the maintenance in Store, when every process has let go
// of the database, and when every process serves again after it.
type Judge struct {
	Store   *Store
	Counter Counter
	// ID names this judge in the lock.
	ID string
	// DrainTimeout is how long after the judge first saw a process
	// draining or drained it waits before terminating that process's
	// Elythia sessions that remain.
	DrainTimeout time.Duration
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
	// IdleInterval is how long Run waits between steps while no maintenance
	// is active. Zero means the interval given to Run.
	IdleInterval time.Duration
	// Wake, when set, makes Run step at once while it waits IdleInterval.
	// The Supervisor sends to it when its watch loop reads an active state,
	// so the judge notices a maintenance as soon as the process does.
	Wake <-chan struct{}

	// idle is set by Step when it saw no maintenance.
	idle bool
	// notHolding is set while this judge knows it does not hold the lock, so
	// an idle step does not release it again.
	notHolding bool

	opID       string
	terminated []Blocker
	// lettingGo holds the IDs of the processes seen draining or drained
	// during this opId, with the time this judge first saw them so. A
	// process's sessions may be terminated DrainTimeout after that time,
	// even after its report is gone (the process died while letting go).
	lettingGo map[string]time.Time
}

// Outcome is what one Step did.
type Outcome int

const (
	// NotJudging means another judge holds the lock, or nothing is active.
	NotJudging Outcome = iota
	// Waiting means the judge looked and the maintenance did not move.
	Waiting
	// MovedToHeld means the judge moved the maintenance to held.
	MovedToHeld
	// Cleared means every process serves again and the judge ended the
	// maintenance.
	Cleared
)

func (j *Judge) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

// Step takes (or keeps) the judge lock and looks at the maintenance once.
func (j *Judge) Step(ctx context.Context) (Outcome, error) {
	st, err := j.Store.Get(ctx)
	if err != nil {
		return NotJudging, err
	}
	j.idle = st == nil
	if st == nil {
		j.reset("")
		// 持っていないと分かっているロックは解きに行かない。待機中の判定のたびに
		// Lua を 1 回撃っていた。
		if j.notHolding {
			return NotJudging, nil
		}
		if err := j.Store.releaseJudge(ctx, j.ID); err != nil {
			return NotJudging, err
		}
		j.notHolding = true
		return NotJudging, nil
	}
	ok, err := j.Store.acquireJudge(ctx, j.ID, JudgeLockTTL)
	// 失敗したときは、前の延長で持ったままかもしれないので、持っている側に倒す。
	j.notHolding = err == nil && !ok
	if err != nil || !ok {
		return NotJudging, err
	}
	if st.OpID == "" || st.phaseMissing {
		// opId か phase の無い状態 (手で書いた、壊れた) は判定しない。書き込みは
		// opId と phase で縛っているので、判定しても held にできず、上限の後に
		// 接続を切り続けるだけになる (#3463 のレビュー2周目 8)。
		return Waiting, errors.New("maintmode: the maintenance state has no opId or no phase; clear it with `elythia maintenance exit -force`")
	}
	if st.OpID != j.opID {
		j.reset(st.OpID)
	}
	switch st.Phase {
	case PhaseEntering:
		return j.entering(ctx, st)
	case PhaseHeld:
		return j.held(ctx, st)
	case PhaseResuming:
		return j.resuming(ctx, st)
	}
	return Waiting, nil
}

func (j *Judge) reset(opID string) {
	j.opID = opID
	j.terminated = nil
	j.lettingGo = map[string]time.Time{}
}

// remember records which processes said they let go, from when, and forgets
// those that serve again.
func (j *Judge) remember(reports []ProcReport, now time.Time) {
	if j.lettingGo == nil {
		j.lettingGo = map[string]time.Time{}
	}
	for _, r := range reports {
		switch r.State {
		case ProcDraining, ProcDrained:
			if _, ok := j.lettingGo[r.ID]; !ok {
				j.lettingGo[r.ID] = now
			}
		case ProcStarting, ProcRunning, ProcResuming:
			delete(j.lettingGo, r.ID)
		}
	}
}

// withRemembered returns reports plus a drained entry for every process
// remembered as letting go whose report is gone.
func (j *Judge) withRemembered(reports []ProcReport) []ProcReport {
	live := make(map[string]bool, len(reports))
	for _, r := range reports {
		live[r.ID] = true
	}
	out := append([]ProcReport(nil), reports...)
	for id := range j.lettingGo {
		if !live[id] {
			out = append(out, ProcReport{ID: id, State: ProcDrained})
		}
	}
	return out
}

// waitingFor lists the live reports that are not drained.
func waitingFor(reports []ProcReport) []string {
	var out []string
	for _, r := range reports {
		if r.State != ProcDrained {
			out = append(out, r.ID+"("+r.State+")")
		}
	}
	return out
}

func (j *Judge) entering(ctx context.Context, st *State) (Outcome, error) {
	now := j.now()
	// 報告を先に、接続を後に読む。どちらが先でも条件は崩れないが (DB を開いた
	// プロセスは手放し終えるまで drained 以外を報告し、drained から戻るのは
	// resuming のときだけ)、推論を単純にするために固定する。
	reports, err := j.Store.Reports(ctx)
	if err != nil {
		return Waiting, err
	}
	waiting := waitingFor(reports)
	j.remember(reports, now)
	sessions, err := j.Counter.Sessions(ctx)
	if err != nil {
		_ = j.Store.progress(ctx, st.OpID, now, waiting, st.Blockers, j.terminated, err.Error())
		return Waiting, err
	}
	if len(waiting) == 0 && len(sessions) == 0 {
		moved, err := j.Store.markHeld(ctx, st.OpID, now, j.terminated)
		if err != nil || !moved {
			return Waiting, err
		}
		return MovedToHeld, nil
	}
	if len(sessions) > 0 {
		// 手放す途中で落ちたプロセスは報告が消える。一度 draining / drained と
		// 見たプロセスの接続は、報告が消えた後も切ってよいとする。覚えずにいると、
		// その接続を二度と自動で切らず、held に届かない (#3463 のレビュー2周目 3)。
		sessions = j.terminateElythia(ctx, now, sessions, j.withRemembered(reports))
	}
	return Waiting, j.Store.progress(ctx, st.OpID, now, waiting, sessions, j.terminated, "")
}

// terminateElythia ends the sessions that an Elythia process which reports
// it is letting go (drained, or still draining past the drain timeout) left
// behind: an abandoned queue handler, a stop that ran out of time. It
// returns the sessions that remain.
//
// 切るのは、持ち主のプロセスが手放すと言っている接続だけにする。報告の無い
// プロセス (Redis に繋がらないまま running で、メンテナンスに気付いていない
// かもしれない) や running のプロセスの接続を切ると、そのプロセスはすぐに
// 繋ぎ直し、held の後に接続が現れる。2 秒ごとに切り続けることにもなる
// (#3463 のレビュー1周目 M-1)。運営者の psql など、Elythia 以外の接続も切らない。
//
// 上限はプロセスごとに、判定役がそのプロセスの draining / drained を初めて
// 見た時刻から数える。判定役がメンテナンスを見た時刻から数えると、通知を
// 取りこぼして手放し始めるのが遅れたプロセス (待機中は最大
// IdlePollInterval) の猶予がそのぶん縮み、drainTimeout をその遅れ以下に
// した構成では手放し始めた直後に切ることになる (#3592 のレビュー L-2)。
func (j *Judge) terminateElythia(ctx context.Context, now time.Time, sessions []Blocker, reports []ProcReport) []Blocker {
	var remain []Blocker
	for _, b := range sessions {
		owner := ownerOf(b.Application, reports)
		if !b.SameRole || !lettingGo(owner) {
			remain = append(remain, b)
			continue
		}
		if since, ok := j.lettingGo[owner.ID]; !ok || now.Sub(since) < j.DrainTimeout {
			remain = append(remain, b)
			continue
		}
		ended, err := j.Counter.Terminate(ctx, b)
		if err != nil {
			slog.Warn("maintenance: failed to terminate an Elythia session", "session", b.String(), "err", err)
			remain = append(remain, b)
			continue
		}
		if !ended {
			// 数えた後に自分で閉じたか、pid が別の接続に替わった。次の判定で数え直す。
			continue
		}
		slog.Warn("maintenance: terminated an Elythia session that outlived the drain timeout", "session", b.String())
		j.recordTerminated(b)
	}
	return remain
}

// recordTerminated adds b to the terminated list once, up to
// maxTerminatedRecorded entries.
func (j *Judge) recordTerminated(b Blocker) {
	for _, t := range j.terminated {
		if t.Server == b.Server && t.PID == b.PID {
			return
		}
	}
	if len(j.terminated) < maxTerminatedRecorded {
		j.terminated = append(j.terminated, b)
	}
}

// lettingGo reports whether r says its process is letting go of the
// database.
func lettingGo(r *ProcReport) bool {
	return r != nil && (r.State == ProcDrained || r.State == ProcDraining)
}

// ownerOf returns the report of the process that opened a connection named
// app ("elythia/<role>/<process ID>"), or nil when there is none.
//
// PostgreSQL は application_name を 63 バイトで切り詰める。切り詰められたかも
// しれない名前 (63 バイト) は、完全一致でも決めず、接頭辞が合う報告が 1 つだけの
// ときに限ってその持ち主とみなす。切り詰めた結果が別のプロセスの ID とたまたま
// 一致すると、その (running の) プロセスを持ち主と取り違えるため
// (#3463 のレビュー2周目 2)。
func ownerOf(app string, reports []ProcReport) *ProcReport {
	rest, ok := strings.CutPrefix(app, ElythiaApplicationPrefix)
	if !ok {
		return nil
	}
	_, id, ok := strings.Cut(rest, "/")
	if !ok || id == "" {
		return nil
	}
	truncated := len(app) >= pgNameMax
	var found *ProcReport
	for i := range reports {
		r := &reports[i]
		switch {
		case !truncated && r.ID == id:
			return r
		case truncated && strings.HasPrefix(r.ID, id):
			if found != nil {
				return nil
			}
			found = r
		}
	}
	return found
}

// held keeps listing the sessions: held is a fact of the moment it was
// judged, and something may connect afterwards.
func (j *Judge) held(ctx context.Context, st *State) (Outcome, error) {
	now := j.now()
	reports, err := j.Store.Reports(ctx)
	if err != nil {
		return Waiting, err
	}
	sessions, err := j.Counter.Sessions(ctx)
	if err != nil {
		_ = j.Store.progress(ctx, st.OpID, now, waitingFor(reports), st.Blockers, st.Terminated, err.Error())
		return Waiting, err
	}
	if len(sessions) > 0 && len(st.Blockers) == 0 {
		slog.Warn("maintenance: sessions appeared on the database after it was held",
			"sessions", len(sessions), "first", sessions[0].String())
	}
	return Waiting, j.Store.progress(ctx, st.OpID, now, waitingFor(reports), sessions, st.Terminated, "")
}

// resuming ends the maintenance once every live report serves again.
//
// 報告が消えたプロセス (落ちたもの) は待たない。起動し直せば、状態が resuming か
// 空ならそのまま通常の起動になる。抜けるときの判定は正しさに関わらないので、
// 報告だけで判定してよい (設計 §2.6)。
func (j *Judge) resuming(ctx context.Context, st *State) (Outcome, error) {
	reports, err := j.Store.Reports(ctx)
	if err != nil {
		return Waiting, err
	}
	for _, r := range reports {
		if r.State != ProcRunning {
			return Waiting, nil
		}
	}
	cleared, err := j.Store.clearResumed(ctx, st.OpID)
	if err != nil || !cleared {
		return Waiting, err
	}
	return Cleared, nil
}

// Release gives up the judge lock if this judge holds it.
func (j *Judge) Release(ctx context.Context) error {
	return j.Store.releaseJudge(ctx, j.ID)
}

// Run steps every interval until ctx is done, then releases the lock. While
// no maintenance is active it steps every IdleInterval instead, or at once
// when Wake fires.
//
// ロックを他の判定役が持っている間は、待機中とは数えない (interval で見て、
// その判定役が落ちたら引き継ぐ)。
func (j *Judge) Run(ctx context.Context, interval time.Duration) {
	idle := j.IdleInterval
	if idle <= 0 {
		idle = interval
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = j.Release(rctx)
	}()
	for {
		if _, err := j.Step(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("maintenance: judge step failed", "err", err)
		}
		wait, wake := interval, (<-chan struct{})(nil)
		if j.idle {
			wait, wake = idle, j.Wake
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-wake:
		}
	}
}

// PGTarget is one PostgreSQL server the counter looks at.
type PGTarget struct {
	// Name is "primary" or "replica<N>".
	Name string
	// DSN connects to the maintenance database on that server.
	DSN string
	// Database is the database whose sessions are counted.
	Database string
	// MaintenanceDB is the database DSN connects to. It must differ from
	// Database.
	MaintenanceDB string
}

// PGCounter counts the sessions with a short connection to the maintenance
// database of each server. 対象の DB には繋がないので、自分は数に入らない。
// 判定のたびに張って閉じ、プールを持たない。
type PGCounter struct {
	Targets []PGTarget
	// Connect opens a connection. Nil means pgx.Connect.
	Connect func(ctx context.Context, dsn string) (*pgx.Conn, error)
}

// connectTimeout bounds one connection of the counter.
const connectTimeout = 5 * time.Second

func (c *PGCounter) connect(ctx context.Context, t PGTarget) (*pgx.Conn, error) {
	connect := c.Connect
	if connect == nil {
		connect = pgx.Connect
	}
	conn, err := connect(ctx, t.DSN)
	if err != nil {
		return nil, fmt.Errorf("maintmode: connect to the maintenance database on %s: %w", t.Name, err)
	}
	return conn, nil
}

// Sessions implements Counter.
func (c *PGCounter) Sessions(ctx context.Context) ([]Blocker, error) {
	if len(c.Targets) == 0 {
		return nil, errors.New("maintmode: no database server to count sessions on")
	}
	var out []Blocker
	for _, t := range c.Targets {
		sessions, err := c.list(ctx, t)
		if err != nil {
			return nil, err
		}
		for _, s := range sessions {
			out = append(out, Blocker{Server: t.Name, Session: s})
		}
	}
	return out, nil
}

func (c *PGCounter) list(ctx context.Context, t PGTarget) ([]pgactivity.Session, error) {
	if t.MaintenanceDB == t.Database {
		// 対象の DB に繋いで数えると、判定役自身の接続のせいで 0 にならず、
		// 判定のたびに対象の DB へ繋ぐことにもなる。設定の検査は maintenance: の
		// 節があるときしか効かない (db.db が postgres で節が無い構成) ので、ここでも拒む。
		return nil, fmt.Errorf("maintmode: %s: maintenance.maintenanceDb (%q) is the application database; set another database", t.Name, t.MaintenanceDB)
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, err := c.connect(ctx, t)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	sessions, err := pgactivity.List(ctx, conn, t.Database)
	if err != nil {
		return nil, fmt.Errorf("maintmode: %s: %w", t.Name, err)
	}
	return sessions, nil
}

// Terminate implements Counter.
func (c *PGCounter) Terminate(ctx context.Context, b Blocker) (bool, error) {
	for _, t := range c.Targets {
		if t.Name != b.Server {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, connectTimeout)
		defer cancel()
		conn, err := c.connect(ctx, t)
		if err != nil {
			return false, err
		}
		defer func() { _ = conn.Close(context.Background()) }()
		return pgactivity.Terminate(ctx, conn, b.PID, t.Database, b.Application)
	}
	return false, fmt.Errorf("maintmode: unknown server %q", b.Server)
}

// TargetsFromConfig returns the servers to count on: the primary, and every
// replica when dbReplications is on. applicationName names the counter's
// own connections.
func TargetsFromConfig(cfg *config.Config, applicationName string) []PGTarget {
	maint := cfg.Maintenance.MaintenanceDB
	c := *cfg
	c.DB.DB = maint
	c.DBApplicationName = applicationName
	targets := []PGTarget{{Name: "primary", DSN: c.DSN(), Database: cfg.DB.DB, MaintenanceDB: maint}}
	if !cfg.DBReplications {
		return targets
	}
	// 本体はレプリカにも接続を張るので、各サーバーで同じ形に数える。
	c.DBSlaves = make([]config.DBSlaveOptions, len(cfg.DBSlaves))
	copy(c.DBSlaves, cfg.DBSlaves)
	for i, s := range cfg.DBSlaves {
		c.DBSlaves[i].DB = maint
		targets = append(targets, PGTarget{
			Name: fmt.Sprintf("replica%d", i), DSN: c.SlaveDSN(i), Database: s.DB, MaintenanceDB: maint,
		})
	}
	return targets
}
