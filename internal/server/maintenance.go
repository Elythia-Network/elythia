package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/adminbackup"
	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/frontendutil"
	"github.com/elythia-network/elythia/internal/maintmode"
	"github.com/elythia-network/elythia/internal/server/middleware"
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
// pubsub の通知は取りこぼすので、周期でも読み直す。周期は、メンテナンス中か
// 通常どおり動いていないときは maintmode.PollInterval、それ以外 (待機中) は
// maintmode.IdlePollInterval。
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
	// pollInterval / idleInterval は読み直しの間隔 (メンテナンス中 / 待機中)。
	// テストで縮める。
	pollInterval time.Duration
	idleInterval time.Duration
	// judgeWake は、監視がメンテナンスの状態を読んだときに判定役を起こす。
	judgeWake chan struct{}

	// reported / reportedAt は最後に書けた報告とその時刻 (mu で守る)。報告は
	// 中身が変わったときか、前に書いてから今の周期が過ぎたときだけ書く。
	// 以前は監視の読み直しと heartbeat がそれぞれ周期ごとに書いていて、待機中は
	// 2 秒に 2 回書いていた。
	reported   maintmode.ProcReport
	reportedAt time.Time
	// reportFailed は最後の書き込みが失敗したことを示す (mu で守る)。失敗した
	// 間は待機中でも短い周期で書き直す。待機中の周期 (10 秒) のままだと、
	// ProcTTL (30 秒) の間に試せるのが 2 回ほどしかなく、報告が切れうる。
	reportFailed bool
	writeMu      sync.Mutex
	// subscribe は状態の変化の通知を購読する。テストで差し替える。
	subscribe func(context.Context) <-chan struct{}
}

func (m *maintTracker) init(s *Supervisor) {
	m.sup = s
	m.store = s.deps.Maintenance
	m.id = s.deps.ProcessID
	m.proc = maintmode.ProcRunning
	m.pollInterval = maintmode.PollInterval
	m.idleInterval = maintmode.IdlePollInterval
	if s.deps.MaintenancePoll > 0 {
		// 周期を縮めるテストは、待機中も同じ周期で動かす (待機中の周期を別に
		// 縮めたいときは MaintenanceIdlePoll)。
		m.pollInterval = s.deps.MaintenancePoll
		m.idleInterval = s.deps.MaintenancePoll
	}
	if s.deps.MaintenanceIdlePoll > 0 {
		m.idleInterval = s.deps.MaintenanceIdlePoll
	}
	m.judgeWake = make(chan struct{}, 1)
	if m.store != nil {
		m.subscribe = m.store.Subscribe
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
		opts.Forward, opts.ForwardPaths = maintenanceRestoreForward(s.cfg, maintenanceRateLimiter(s))
	}
	m.handler = maintmode.NewHandler(opts)
}

// maintenanceRestoreForward forwards the restore endpoints while the process
// is in maintenance, resolving the client address the way echo does
// (trustProxy), so that the per-client failure budget of the
// re-authentication is the same as when the server runs.
func maintenanceRestoreForward(cfg *config.Config, limiter *middleware.RateLimiter) (http.HandlerFunc, []string) {
	trusted, err := config.ParseTrustProxy(cfg.TrustProxy)
	if err != nil {
		// 起動時に同じ値で echo を組み立てて通っているので、ここには来ない。
		return nil, nil
	}
	extract := buildIPExtractor(trusted)
	fwd := newRestoreForwarder(cfg)
	return func(w http.ResponseWriter, r *http.Request) {
		op := strings.TrimPrefix(r.URL.Path, adminbackup.RestorePathPrefix)
		ip := extract(r)
		// status は本体で認証しないので、動いている間と同じ接続元の上限 (echo の
		// rate limit と同じ bucket) を、メンテナンス中も通す。
		if limiter != nil {
			if ok, retry := limiter.AllowIP(r.Context(), strings.TrimPrefix(r.URL.Path, "/api/"), ip); !ok {
				w.Header().Set("Retry-After", strconv.FormatInt(retry, 10))
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(apierr.RateLimitExceeded())
				return
			}
		}
		fwd.Serve(w, r, op, ip)
	}, maintenanceRestorePaths
}

// maintenanceRateLimiter is the rate limiter the forward during a
// maintenance shares with the server (the same Redis buckets and limits).
func maintenanceRateLimiter(s *Supervisor) *middleware.RateLimiter {
	if s.redis == nil || s.redis.Default == nil || s.cfg.DisableEndpointRateLimits {
		return nil
	}
	rl := middleware.NewRedisRateLimiter(s.redis.Default, s.cfg.EnableIPRateLimit, middleware.DefaultEndpointLimits)
	if s.cfg.TestMode {
		rl.Disable()
	}
	return rl
}

// maintenanceRestorePaths are the restore endpoints forwarded during a
// maintenance.
//
// **進み具合 (status) だけを転送する。** 準備・確定・中止は、daemon がメンテナンス中
// (ほかの操作のものも含む) は再認証の前に必ず断る (NoMaintenance)。転送しても通る
// ことは無く、本体を止めている間に誰でも daemon と Redis まで要求を届けられる面が
// 増えるだけだった。確定・中止は rate limit の表にも無いので、転送は上限も無かった。
var maintenanceRestorePaths = []string{
	adminbackup.RestorePathPrefix + "status",
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
			IdleInterval: m.idleInterval, Wake: m.judgeWake,
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

// heartbeat keeps the report fresh while the watch loop is busy (building
// or stopping a generation) or not started yet. When the watch loop writes
// the report on time, it writes nothing.
func (m *maintTracker) heartbeat(ctx context.Context) {
	timer := time.NewTimer(m.interval())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			m.refresh(ctx)
			timer.Reset(m.interval())
		}
	}
}

// interval is how often the state is re-read and the report refreshed:
// pollInterval while a maintenance is active or this process is not running
// normally (starting, draining, drained, resuming, resume_failed), and
// idleInterval otherwise.
//
// The last report write having failed also selects pollInterval, until a
// write succeeds.
func (m *maintTracker) interval() time.Duration {
	m.mu.Lock()
	busy := m.reportFailed || m.proc != maintmode.ProcRunning
	m.mu.Unlock()
	if busy || m.state.Load() != nil {
		return m.pollInterval
	}
	return m.idleInterval
}

// wakeJudge makes the judge of this process step at once, if it waits.
func (m *maintTracker) wakeJudge() {
	select {
	case m.judgeWake <- struct{}{}:
	default:
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
	hints := m.subscribe(ctx)
	m.reconcile(ctx)
	timer := time.NewTimer(m.interval())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-hints:
			if !ok {
				// 購読が切れた。読み直しの周期だけで追い、次の周で張り直す。
				hints = nil
			}
		case <-timer.C:
			if hints == nil {
				hints = m.subscribe(ctx)
			}
		}
		m.reconcile(ctx)
		wait := m.interval()
		if hints == nil {
			// 通知が届かない間は、待機中でも短い周期で読み直す。
			wait = m.pollInterval
		}
		timer.Reset(wait)
	}
}

// reconcile reads the state and moves this process towards it.
func (m *maintTracker) reconcile(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	st, err := m.store.Get(rctx)
	cancel()
	if err != nil {
		slog.Warn("maintenance: failed to read the state; keeping the current one", "err", err)
		m.refresh(ctx)
		return
	}
	m.state.Store(st)
	if st != nil {
		m.wakeJudge()
	}
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
	m.refresh(ctx)
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
	m.write(ctx, false)
}

// refresh writes this process's report when it changed, or when the current
// interval has passed since it was last written.
func (m *maintTracker) refresh(ctx context.Context) {
	m.write(ctx, true)
}

func (m *maintTracker) write(ctx context.Context, onlyIfDue bool) {
	if m.store == nil {
		return
	}
	// 監視と heartbeat の書き込みを 1 本ずつにする。並ぶと、先に取った古い中身が
	// 後から書かれ、次に書くまで報告が戻って見える。
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	every := m.interval()
	r := m.snapshot()
	if onlyIfDue {
		m.mu.Lock()
		due := r != m.reported || time.Since(m.reportedAt) >= every
		m.mu.Unlock()
		if !due {
			return
		}
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := m.store.Report(rctx, r); err != nil {
		if ctx.Err() == nil {
			slog.Warn("maintenance: failed to write the process report", "err", err)
		}
		m.mu.Lock()
		m.reportFailed = true
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	m.reported = r
	m.reportedAt = time.Now()
	m.reportFailed = false
	m.mu.Unlock()
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
