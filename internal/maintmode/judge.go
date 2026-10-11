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
	// DrainTimeout is how long after it first saw the maintenance the judge
	// waits before terminating the Elythia sessions that remain.
	DrainTimeout time.Duration
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time

	opID       string
	firstSeen  time.Time
	terminated []Blocker
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
	if st == nil {
		j.reset("")
		return NotJudging, j.Store.releaseJudge(ctx, j.ID)
	}
	ok, err := j.Store.acquireJudge(ctx, j.ID, JudgeLockTTL)
	if err != nil || !ok {
		return NotJudging, err
	}
	if st.OpID == "" {
		// opId の無い状態 (手で書いた、壊れた) は判定しない。書き込みは全部
		// opId で縛っているので、判定しても held にできず、待ちの時計も持てない。
		return Waiting, errors.New("maintmode: the maintenance state has no opId; clear it with `elythia maintenance exit -force`")
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
	j.firstSeen = j.now()
	j.terminated = nil
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
	if len(sessions) > 0 && now.Sub(j.firstSeen) >= j.DrainTimeout {
		sessions = j.terminateElythia(ctx, sessions, reports)
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
func (j *Judge) terminateElythia(ctx context.Context, sessions []Blocker, reports []ProcReport) []Blocker {
	var remain []Blocker
	for _, b := range sessions {
		if !b.SameRole || !lettingGo(ownerOf(b.Application, reports)) {
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
// PostgreSQL は application_name を 63 バイトで切り詰める。切り詰められた名前は、
// 接頭辞が 1 つの報告にだけ合うときに限ってその持ち主とみなす。
func ownerOf(app string, reports []ProcReport) *ProcReport {
	rest, ok := strings.CutPrefix(app, ElythiaApplicationPrefix)
	if !ok {
		return nil
	}
	_, id, ok := strings.Cut(rest, "/")
	if !ok || id == "" {
		return nil
	}
	var found *ProcReport
	for i := range reports {
		r := &reports[i]
		switch {
		case r.ID == id:
			return r
		case len(app) >= pgNameMax && strings.HasPrefix(r.ID, id):
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

// Run steps every interval until ctx is done, then releases the lock.
func (j *Judge) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = j.Release(rctx)
	}()
	for {
		if _, err := j.Step(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("maintenance: judge step failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
		return pgactivity.Terminate(ctx, conn, b.PID, t.Database, ElythiaApplicationPrefix)
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
	targets := []PGTarget{{Name: "primary", DSN: c.DSN(), Database: cfg.DB.DB}}
	if !cfg.DBReplications {
		return targets
	}
	// 本体はレプリカにも接続を張るので、各サーバーで同じ形に数える。
	c.DBSlaves = make([]config.DBSlaveOptions, len(cfg.DBSlaves))
	copy(c.DBSlaves, cfg.DBSlaves)
	for i, s := range cfg.DBSlaves {
		c.DBSlaves[i].DB = maint
		targets = append(targets, PGTarget{
			Name: fmt.Sprintf("replica%d", i), DSN: c.SlaveDSN(i), Database: s.DB,
		})
	}
	return targets
}
