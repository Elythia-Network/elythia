package server

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/adminbackup"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/frontendutil"
	"github.com/elythia-network/elythia/internal/maintmode"
)

// Retry backoff for a generation that failed to come back after a
// maintenance.
const (
	resumeRetryMin = maintmode.PollInterval
	resumeRetryMax = 30 * time.Second
)

// maintTracker follows the maintenance state in Redis on behalf of a
// Supervisor (#3463).
//
// 状態の読み直しと世代の切り替えは、監視の goroutine 1 本だけが行う。
// pubsub の通知は取りこぼすので、maintmode.PollInterval ごとにも読み直す。
// Redis が読めないときは今の状態のまま動かない (メンテナンス中なら抜けない。
// 設計 §3.5)。
type maintTracker struct {
	sup   *Supervisor
	store *maintmode.Store
	id    string
	role  string
	// handler はメンテナンスの間 listener が返すもの。
	handler http.Handler
	// state は最後に読めたメンテナンスの状態 (nil なら無し)。
	state atomic.Pointer[maintmode.State]

	mu        sync.Mutex
	proc      string
	lastStop  StopReport
	lastErr   string
	retryAt   time.Time
	retryWait time.Duration

	cancel context.CancelFunc
	done   chan struct{}
	// hbCancel / hbDone は報告を書き続ける goroutine の分。
	hbCancel context.CancelFunc
	hbDone   chan struct{}
	// judgeCancel / judgeDone は判定役の goroutine の分 (deps.MaintenanceCounter が
	// あるときだけ)。
	judgeCancel context.CancelFunc
	judgeDone   chan struct{}
	// pollInterval は読み直しの間隔。テストで縮める。
	pollInterval time.Duration
}

func (m *maintTracker) init(s *Supervisor) {
	m.sup = s
	m.store = s.deps.Maintenance
	m.id = s.deps.ProcessID
	m.proc = maintmode.ProcRunning
	m.pollInterval = maintmode.PollInterval
	if s.deps.MaintenancePoll > 0 {
		m.pollInterval = s.deps.MaintenancePoll
	}
	if role, err := config.ResolveProcessRole(); err == nil {
		m.role = string(role)
	}
	healthRole := ""
	if m.role == string(config.RoleQueue) {
		healthRole = m.role
	}
	opts := maintmode.HandlerOptions{
		State:     m.state.Load,
		Static:    maintenanceStaticRoutes(),
		Role:      healthRole,
		QueueOnly: healthRole != "",
	}
	if s.cfg != nil {
		// 配送だけのノード (QueueOnly) は handler が API を出さないので、転送の口を付けても届かない。
		opts.Forward, opts.ForwardPaths = maintenanceRestoreForward(s.cfg)
	}
	m.handler = maintmode.NewHandler(opts)
}

// maintenanceRestoreForward forwards the restore endpoints while the process
// is in maintenance, resolving the client address the way echo does
// (trustProxy), so that the per-client failure budget of the
// re-authentication is the same as when the server runs.
func maintenanceRestoreForward(cfg *config.Config) (http.HandlerFunc, []string) {
	trusted, err := config.ParseTrustProxy(cfg.TrustProxy)
	if err != nil {
		// 起動時に同じ値で echo を組み立てて通っているので、ここには来ない。
		return nil, nil
	}
	extract := buildIPExtractor(trusted)
	fwd := newRestoreForwarder(cfg)
	return func(w http.ResponseWriter, r *http.Request) {
		op := strings.TrimPrefix(r.URL.Path, adminbackup.RestorePathPrefix)
		fwd.Serve(w, r, op, extract(r))
	}, maintenanceRestorePaths
}

// maintenanceRestorePaths are the restore endpoints forwarded during a
// maintenance.
//
// prepare は転送しない。メンテナンス中に始められる操作は無く (daemon も断る)、
// 転送すると再認証のために DB へ接続を張る要求を、メンテナンス中に誰でも送れる
// ことになるため。進み具合・確定・中止は、メンテナンスの中で続く操作に要る。
var maintenanceRestorePaths = []string{
	adminbackup.RestorePathPrefix + "status",
	adminbackup.RestorePathPrefix + "confirm",
	adminbackup.RestorePathPrefix + "abort",
}

// holdingAtStartup reads the state once before the first generation is
// built. Without a store there is never a maintenance.
func (m *maintTracker) holdingAtStartup() (bool, error) {
	if m.store == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := m.store.Get(ctx)
	if err != nil {
		return false, err
	}
	m.state.Store(st)
	warnIfStateEvictable(ctx, m.store)
	return st.Holding(), nil
}

func (m *maintTracker) setProc(proc string, rep StopReport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.proc = proc
	m.lastStop = rep
}

func (m *maintTracker) procState() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proc
}

// announceStarting writes the starting report and keeps it fresh. It must
// run before the state is read and the database opened: the judge does not
// move a maintenance to held while a report is starting, so a maintenance
// entered between the read and the open waits for this process. No-op
// without a store.
//
// 報告は監視とは別の goroutine で書き続ける。世代を作る・止める間 (最大で
// DrainTimeout の 3 倍) は監視の goroutine がその中にいる。報告が切れても判定は
// 誤らない (根拠は pg_stat_activity) が、starting / resuming の報告が消えると、
// DB を開く途中のプロセスを判定役が待たなくなる。
func (m *maintTracker) announceStarting() error {
	if m.store == nil {
		return nil
	}
	m.setProc(maintmode.ProcStarting, StopReport{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.store.Report(ctx, m.snapshot()); err != nil {
		return err
	}
	hbCtx, hbCancel := context.WithCancel(context.Background())
	m.hbCancel = hbCancel
	m.hbDone = make(chan struct{})
	go func() {
		defer close(m.hbDone)
		m.heartbeat(hbCtx)
	}()
	return nil
}

// start launches the watch loop, and the judge when the Supervisor has a
// counter. No-op without a store.
func (m *maintTracker) start() {
	if m.store == nil || m.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	go func() {
		defer close(m.done)
		m.watch(ctx)
	}()
	if counter := m.sup.deps.MaintenanceCounter; counter != nil {
		// 判定役は世代をまたいで動く。メンテナンス中は世代も DB のプールも無いが、
		// 判定はメンテナンス用の DB への短い接続で行うので関係しない。
		jctx, jcancel := context.WithCancel(context.Background())
		m.judgeCancel = jcancel
		m.judgeDone = make(chan struct{})
		judge := &maintmode.Judge{
			Store: m.store, Counter: counter, ID: m.id,
			DrainTimeout: m.sup.cfg.Maintenance.DrainTimeout,
		}
		if judge.DrainTimeout <= 0 {
			judge.DrainTimeout = config.DefaultMaintenanceDrainTimeout
		}
		go func() {
			defer close(m.judgeDone)
			judge.Run(jctx, m.pollInterval)
		}()
	}
}

func (m *maintTracker) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.report(ctx)
		}
	}
}

// stop ends the watch loop, the judge and the heartbeat, and removes this
// process's report. Used where no database is open (a failed start, tests).
func (m *maintTracker) stop(ctx context.Context) {
	m.stopWatch(ctx)
	m.leave()
}

// stopWatch ends the watch loop and the judge; the heartbeat keeps the
// report fresh.
func (m *maintTracker) stopWatch(ctx context.Context) {
	if m.judgeCancel != nil {
		m.judgeCancel()
		select {
		case <-m.judgeDone:
		case <-ctx.Done():
		}
	}
	if m.cancel == nil {
		return
	}
	m.cancel()
	select {
	case <-m.done:
	case <-ctx.Done():
	}
}

// leave stops the heartbeat and removes this process's report. Call it after
// the database pool is closed.
//
// 消す順序は判定に関わらない。判定役は報告が消えたことを「手放した」とは数えず、
// 根拠は pg_stat_activity に置いている (#3463 の設計第2版 §3.2)。消し損ねても、
// 報告は ProcTTL で消える。
func (m *maintTracker) leave() {
	if m.store == nil || m.hbCancel == nil {
		return
	}
	m.hbCancel()
	<-m.hbDone
	m.hbCancel = nil
	rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.store.RemoveReport(rctx, m.id); err != nil {
		slog.Warn("maintenance: failed to remove the process report; it expires by itself", "err", err)
	}
}

func (m *maintTracker) watch(ctx context.Context) {
	hints := m.store.Subscribe(ctx)
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	m.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-hints:
			if !ok {
				// 購読が切れた。読み直しの周期だけで追い、次の周で張り直す。
				hints = nil
			}
		case <-ticker.C:
			if hints == nil {
				hints = m.store.Subscribe(ctx)
			}
		}
		m.reconcile(ctx)
	}
}

// reconcile reads the state and moves this process towards it.
func (m *maintTracker) reconcile(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	st, err := m.store.Get(rctx)
	cancel()
	if err != nil {
		slog.Warn("maintenance: failed to read the state; keeping the current one", "err", err)
		m.report(ctx)
		return
	}
	m.state.Store(st)
	holding := st.Holding()
	switch proc := m.procState(); {
	case holding && proc == maintmode.ProcRunning:
		m.enter(ctx)
	case holding && proc == maintmode.ProcResumeFailed:
		// 作り直しに失敗したまま、もう一度メンテナンスに入った。止める世代は無い。
		m.setProc(maintmode.ProcDrained, StopReport{})
	case !holding && (proc == maintmode.ProcDrained || proc == maintmode.ProcResumeFailed):
		m.resume(ctx)
	}
	m.report(ctx)
}

func (m *maintTracker) enter(ctx context.Context) {
	slog.Info("maintenance: entering; stopping the server generation")
	m.setProc(maintmode.ProcDraining, StopReport{})
	m.report(ctx)
	rep := m.sup.stopGeneration(m.handler)
	m.mu.Lock()
	m.proc = maintmode.ProcDrained
	m.lastStop = rep
	m.lastErr = ""
	if rep.Err != nil {
		m.lastErr = rep.Err.Error()
	}
	m.retryAt = time.Time{}
	m.retryWait = 0
	m.mu.Unlock()
	slog.Info("maintenance: drained", "inFlight", rep.InFlight, "openConns", rep.OpenConns, "err", rep.Err)
}

func (m *maintTracker) resume(ctx context.Context) {
	m.mu.Lock()
	if !m.retryAt.IsZero() && time.Now().Before(m.retryAt) {
		m.mu.Unlock()
		return
	}
	m.proc = maintmode.ProcResuming
	m.mu.Unlock()
	m.report(ctx)
	slog.Info("maintenance: over; building a new server generation")
	err := m.sup.startGeneration()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		// 元の DB に繋ぎ直せなかった。メンテナンスのハンドラーのまま、間隔を
		// 延ばしながら作り直しを繰り返す。
		m.proc = maintmode.ProcResumeFailed
		m.lastErr = err.Error()
		if m.retryWait == 0 {
			m.retryWait = resumeRetryMin
		} else {
			m.retryWait = min(m.retryWait*2, resumeRetryMax)
		}
		m.retryAt = time.Now().Add(m.retryWait)
		slog.Error("maintenance: failed to build a new server generation; will retry", "err", err, "retryIn", m.retryWait)
		return
	}
	m.proc = maintmode.ProcRunning
	m.lastStop = StopReport{}
	m.lastErr = ""
	m.retryAt = time.Time{}
	m.retryWait = 0
	slog.Info("maintenance: serving again")
}

// report writes this process's report. Failures are logged only: the next
// poll writes it again.
func (m *maintTracker) report(ctx context.Context) {
	if m.store == nil {
		return
	}
	r := m.snapshot()
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := m.store.Report(rctx, r); err != nil && ctx.Err() == nil {
		slog.Warn("maintenance: failed to write the process report", "err", err)
	}
}

// snapshot builds this process's report from the current state.
func (m *maintTracker) snapshot() maintmode.ProcReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maintmode.ProcReport{
		ID:        m.id,
		Role:      m.role,
		Version:   config.MkGoVersion,
		Commit:    config.MkGoCommit,
		State:     m.proc,
		OpenConns: m.lastStop.OpenConns,
		InFlight:  m.lastStop.InFlight,
		Error:     m.lastErr,
	}
}

// warnIfStateEvictable logs a warning when Redis may evict the maintenance
// state (#3463 review L2). Redis が CONFIG を拒む (マネージドのサービスなど)
// ときは何も言わない。
func warnIfStateEvictable(ctx context.Context, store *maintmode.Store) {
	policy, err := store.EvictionPolicy(ctx)
	if err == nil && maintmode.EvictsState(policy) {
		slog.Warn("maintenance: Redis may evict the maintenance state under memory pressure, which would end a maintenance by itself; use maxmemory-policy noeviction or volatile-*",
			"maxmemory-policy", policy)
	}
}

// maintenanceStaticRoutes lists the files served from disk during a
// maintenance, mirroring the static routes of the router (setupRoutes).
func maintenanceStaticRoutes() []maintmode.StaticRoute {
	staticDir := frontendutil.StaticDir()
	return []maintmode.StaticRoute{
		{Prefix: "/vite/", Dirs: []string{frontendutil.FrontendDir()}},
		{Prefix: "/embed_vite/", Dirs: []string{frontendutil.FrontendEmbedDir()}},
		{Prefix: "/assets/", Dirs: []string{frontendutil.FrontendDistDir(), frontendutil.RepoAssetsDir()}},
		{Prefix: "/twemoji/", Dirs: []string{frontendutil.TwemojiDir()}},
		{Prefix: "/fluent-emoji/", Dirs: []string{frontendutil.FluentEmojiDir()}},
		{Prefix: "/client-assets/", Dirs: []string{frontendutil.ClientAssetsDir()}},
		{Prefix: "/static-assets/", Dirs: []string{staticDir}},
		{Prefix: "/favicon.ico", Dirs: []string{filepath.Join(staticDir, "favicon.ico")}},
		{Prefix: "/apple-touch-icon.png", Dirs: []string{filepath.Join(staticDir, "apple-touch-icon.png")}},
		{Prefix: "/sw.js", Dirs: []string{filepath.Join(frontendutil.SwDistDir(), "sw.js")}},
	}
}

// maintenanceStatusOff answers maintmode.StatusPath while serving normally,
// so the maintenance page knows when to reload.
func maintenanceStatusOff(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, maintmode.StatusOf(nil, false))
}
