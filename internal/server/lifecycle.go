package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/maintmode"
)

// defaultDrainTimeout bounds how long stopping a generation waits for the
// in-flight HTTP requests and for the DB pool to close.
const defaultDrainTimeout = 30 * time.Second

// SupervisorDeps are the constructors a Supervisor uses to build and tear
// down a server generation. Tests replace them.
type SupervisorDeps struct {
	// OpenDB opens the application DB pool.
	OpenDB func(cfg *config.Config) (*gorm.DB, error)
	// NewServer builds a server generation on db.
	NewServer func(cfg *config.Config, db *gorm.DB, redis *cache.RedisClients) (*Server, error)
	// CloseDB closes every pool behind db (primary and replicas).
	CloseDB func(db *gorm.DB) error
	// DrainTimeout bounds the wait for in-flight requests and for the DB pool
	// to close when a generation stops. Zero means defaultDrainTimeout.
	DrainTimeout time.Duration
	// Maintenance, when set, makes the Supervisor follow the maintenance
	// state in Redis: it stops the generation when a maintenance starts and
	// builds a new one when it ends (#3463).
	Maintenance *maintmode.Store
	// MaintenanceCounter, when set together with Maintenance, makes the
	// Supervisor a candidate judge: it takes the judge lock when nobody
	// holds it and decides from pg_stat_activity when the database has been
	// let go.
	MaintenanceCounter maintmode.Counter
	// ProcessID names this process in its maintenance report and as a judge.
	ProcessID string
	// MaintenancePoll overrides maintmode.PollInterval, and also
	// maintmode.IdlePollInterval unless MaintenanceIdlePoll is set (tests).
	MaintenancePoll time.Duration
	// MaintenanceIdlePoll overrides maintmode.IdlePollInterval (tests).
	MaintenanceIdlePoll time.Duration
}

// generation is one Server together with the DB pool it was built on.
type generation struct {
	srv *Server
	db  *gorm.DB
}

// routedHandler is what the listener currently dispatches to. It counts the
// requests it is serving so a generation can be stopped only after they are
// done.
//
// 差し替えと数えの順序: ServeHTTP は先に active を増やしてから retired を見る。
// 止める側は retired を立ててから active が 0 になるのを待つ。どちらも atomic
// (逐次一貫) なので、「古いハンドラーを掴んだが数えられていない」リクエストは
// 生じない。retired を見たリクエストは数えを戻し、今のハンドラーへ回す。
type routedHandler struct {
	next    http.Handler
	active  atomic.Int64
	retired atomic.Bool
}

// Supervisor owns the listener of a serving process and the current server
// generation, and can stop a generation and build a new one without closing
// the listener.
//
// listener と Redis のクライアントは Supervisor が世代をまたいで持つ。
// Server・DB のプール・Server が持つキャッシュや購読や定期処理は、世代ごとに
// 作り直す。メンテナンスの間に DB への接続を 0 にし、抜けるときに DB に由来する
// 状態を全部読み直すため (#3463、設計の方式B)。
type Supervisor struct {
	cfg   *config.Config
	redis *cache.RedisClients
	deps  SupervisorDeps

	current atomic.Pointer[routedHandler]

	// genMu guards gen. 世代の停止と構築は genMu を持ったまま行うので、
	// 同時に 2 つの切り替えは走らない。
	genMu sync.Mutex
	gen   *generation

	httpSrv *http.Server
	// listening は Start が listener を張り終えたら閉じる。
	listening    chan struct{}
	listenerAddr atomic.Pointer[string]

	shutdownOnce sync.Once
	shutdownErr  error

	// afterLoad, when set, runs in ServeHTTP right after a request loaded the
	// current handler (tests: to swap the handler at that exact point).
	afterLoad func()

	// maint はメンテナンスの状態を追う部分 (maintenance.go)。deps.Maintenance が
	// nil なら使わない。
	maint maintTracker
}

// NewSupervisor builds the first server generation (opening the DB pool) and
// returns a Supervisor ready to Start. When deps.Maintenance reports an
// active maintenance, it opens nothing and starts with the maintenance
// handler instead; the generation is built once the maintenance is over.
func NewSupervisor(cfg *config.Config, redis *cache.RedisClients, deps SupervisorDeps) (*Supervisor, error) {
	if deps.OpenDB == nil || deps.NewServer == nil || deps.CloseDB == nil {
		return nil, errors.New("server: supervisor deps are incomplete")
	}
	if deps.DrainTimeout <= 0 {
		deps.DrainTimeout = defaultDrainTimeout
	}
	s := &Supervisor{
		cfg:       cfg,
		redis:     redis,
		deps:      deps,
		listening: make(chan struct{}),
	}
	s.httpSrv = &http.Server{Handler: s}
	applyServerTimeouts(s.httpSrv)
	s.setHandler(http.HandlerFunc(unavailable))
	s.maint.init(s)

	// 状態を読む前に「起動中」を報告する。判定役は starting の報告がある間は
	// held にしないので、読んだ直後に入ったメンテナンスはこのプロセスを待つ。
	// 順序が逆だと、DB を開く前のこのプロセスは pg_stat_activity にも報告にも
	// 出ず、判定役が held にした後で DB を開きうる (設計第2版 §2.3)。
	if err := s.maint.announceStarting(); err != nil {
		return nil, fmt.Errorf("maintenance: write the starting report: %w", err)
	}
	// メンテナンス中に起動したら DB を開かない。抜けるのを待ってから、
	// 監視の流れの中で世代を作る (設計 §3.4)。
	holding, err := s.maint.holdingAtStartup()
	if err != nil {
		s.maint.stop(context.Background())
		return nil, err
	}
	if holding {
		slog.Info("maintenance is active; starting without opening the database")
		s.setHandler(s.maint.handler)
		s.maint.setProc(maintmode.ProcDrained, StopReport{})
		s.maint.report(context.Background())
		return s, nil
	}

	gen, rep, err := s.buildGeneration()
	if err != nil {
		s.maint.stop(context.Background())
		return nil, err
	}
	slog.Info("server generation built", "openDB", rep.OpenDB, "construct", rep.Construct)
	s.gen = gen
	s.setHandler(gen.srv.ServeHandler())
	s.maint.setProc(maintmode.ProcRunning, StopReport{})
	s.maint.report(context.Background())
	return s, nil
}

// unavailable answers while no generation is serving.
func unavailable(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Retry-After", "5")
	http.Error(w, "service unavailable", http.StatusServiceUnavailable)
}

// buildGeneration opens a DB pool and constructs a Server on it. On failure
// it closes the pool it opened.
func (s *Supervisor) buildGeneration() (*generation, StartReport, error) {
	var rep StartReport
	began := time.Now()
	db, err := s.deps.OpenDB(s.cfg)
	rep.OpenDB = time.Since(began)
	if err != nil {
		return nil, rep, fmt.Errorf("connect to database: %w", err)
	}
	began = time.Now()
	srv, err := s.deps.NewServer(s.cfg, db, s.redis)
	rep.Construct = time.Since(began)
	if err != nil {
		if cerr := s.deps.CloseDB(db); cerr != nil {
			slog.Warn("failed to close database after a failed server construction", "err", cerr)
		}
		return nil, rep, fmt.Errorf("construct server: %w", err)
	}
	return &generation{srv: srv, db: db}, rep, nil
}

// setHandler routes new requests to h and returns the handler it replaced.
func (s *Supervisor) setHandler(h http.Handler) *routedHandler {
	return s.current.Swap(&routedHandler{next: h})
}

// ServeHTTP dispatches to the current handler.
func (s *Supervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for {
		rh := s.current.Load()
		if s.afterLoad != nil {
			s.afterLoad()
		}
		// WebSocket は hijack されて長く残るので数えない。止めるときは
		// Server.StopBackground が close frame を送って閉じる。
		if isWebSocketUpgrade(r) {
			rh.next.ServeHTTP(w, r)
			return
		}
		rh.active.Add(1)
		if rh.retired.Load() {
			rh.active.Add(-1)
			if s.current.Load() == rh {
				// 止めたハンドラーが差し替えられないまま残っている。読み直し続けると
				// 抜けられないので、503 で返す (#3463 のレビュー2周目 1)。
				unavailable(w, r)
				return
			}
			continue
		}
		func() {
			defer rh.active.Add(-1)
			rh.next.ServeHTTP(w, r)
		}()
		return
	}
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// retire marks rh as replaced and waits until the requests it is serving
// finish or ctx is done. It returns how many were still running.
func retire(ctx context.Context, rh *routedHandler) int64 {
	if rh == nil {
		return 0
	}
	rh.retired.Store(true)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		n := rh.active.Load()
		if n == 0 {
			return 0
		}
		select {
		case <-ctx.Done():
			return n
		case <-ticker.C:
		}
	}
}

// StopReport describes how stopping a generation went.
type StopReport struct {
	// InFlight is the number of HTTP requests still running when the drain
	// wait gave up.
	InFlight int64
	// OpenConns is the number of DB connections the pool still held when the
	// close wait gave up (0 when the pool closed).
	OpenConns int
	// Err is the error StopBackground or CloseDB returned.
	Err error
	// Drain, StopBackground and CloseDB are how long each step took
	// (#3802). 作り直す範囲を狭めるかどうかを、段ごとの時間で決めるため。
	Drain          time.Duration
	StopBackground time.Duration
	CloseDB        time.Duration
}

// StartReport describes how long building and starting a generation took
// (#3802). A step that did not run is zero.
type StartReport struct {
	OpenDB          time.Duration
	Construct       time.Duration
	StartBackground time.Duration
}

// stopGeneration routes new requests to standby, waits for the in-flight
// requests of the current generation, stops its background work and closes
// its DB pool. It is a no-op when no generation is running.
func (s *Supervisor) stopGeneration(standby http.Handler) StopReport {
	s.genMu.Lock()
	defer s.genMu.Unlock()
	return s.stopGenerationLocked(standby)
}

func (s *Supervisor) stopGenerationLocked(standby http.Handler) StopReport {
	var rep StopReport
	old := s.setHandler(standby)
	gen := s.gen
	if gen == nil {
		return rep
	}
	// 待ちの上限は段ごとに取り直す。処理中のリクエストで上限を使い切った後に、
	// 背景処理の停止や DB を閉じる待ちが 0 秒にならないようにする。
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), s.deps.DrainTimeout)
	defer cancelDrain()
	began := time.Now()
	if rep.InFlight = retire(drainCtx, old); rep.InFlight > 0 {
		slog.Warn("in-flight requests did not finish before the drain deadline", "inFlight", rep.InFlight)
	}
	rep.Drain = time.Since(began)
	stopCtx, cancelStop := context.WithTimeout(context.Background(), s.deps.DrainTimeout)
	defer cancelStop()
	began = time.Now()
	stopErr := gen.srv.StopBackground(stopCtx)
	rep.StopBackground = time.Since(began)
	closeCtx, cancelClose := context.WithTimeout(context.Background(), s.deps.DrainTimeout)
	defer cancelClose()
	var closeErr error
	began = time.Now()
	rep.OpenConns, closeErr = s.closeDB(closeCtx, gen.db)
	rep.CloseDB = time.Since(began)
	rep.Err = errors.Join(stopErr, closeErr)
	s.gen = nil
	return rep
}

// closeDB closes db and waits until the close returns or ctx is done.
//
// sql.DB.Close は実行中のクエリが終わるまで戻らない。見捨てた queue の handler が
// 接続を掴んだままのことがある (docs/configuration.md の
// queueHandlerDeadlineSeconds) ので、待ちには上限を付け、残った接続の数を返す。
func (s *Supervisor) closeDB(ctx context.Context, db *gorm.DB) (int, error) {
	done := make(chan error, 1)
	go func() { done <- s.deps.CloseDB(db) }()
	select {
	case err := <-done:
		return 0, err
	case <-ctx.Done():
		open := 0
		if db != nil && db.Config != nil {
			if sqlDB, err := db.DB(); err == nil {
				open = sqlDB.Stats().OpenConnections
			}
		}
		slog.Warn("database pool did not close before the deadline", "openConns", open)
		return open, ctx.Err()
	}
}

// startGeneration builds a new generation, starts its background work and
// routes requests to it. On failure the previous handler keeps serving.
func (s *Supervisor) startGeneration() (StartReport, error) {
	s.genMu.Lock()
	defer s.genMu.Unlock()
	return s.startGenerationLocked()
}

func (s *Supervisor) startGenerationLocked() (StartReport, error) {
	if s.gen != nil {
		return StartReport{}, nil
	}
	gen, rep, err := s.buildGeneration()
	if err != nil {
		return rep, err
	}
	began := time.Now()
	err = gen.srv.StartBackground()
	rep.StartBackground = time.Since(began)
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), s.deps.DrainTimeout)
		defer cancel()
		_ = gen.srv.StopBackground(ctx)
		_, _ = s.closeDB(ctx, gen.db)
		return rep, err
	}
	s.gen = gen
	s.setHandler(gen.srv.ServeHandler())
	return rep, nil
}

// Start starts the background work of the current generation, listens on the
// configured UNIX socket or TCP port and serves until Shutdown.
func (s *Supervisor) Start() error {
	s.genMu.Lock()
	if s.gen != nil {
		if err := s.gen.srv.StartBackground(); err != nil {
			s.genMu.Unlock()
			return err
		}
	}
	s.genMu.Unlock()

	ln, err := s.listen()
	if err != nil {
		return err
	}
	s.maint.start()
	addr := ln.Addr().String()
	s.listenerAddr.Store(&addr)
	close(s.listening)
	if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// listen binds the UNIX socket or the TCP port, matching what Server.Start
// did before the listener moved here.
func (s *Supervisor) listen() (net.Listener, error) {
	what := "server"
	if role, err := config.ResolveProcessRole(); err == nil && !role.RunsServer() {
		what = "queue worker"
	}
	if s.cfg.Socket != "" {
		ln, err := config.ListenUnixSocket(s.cfg.Socket, s.cfg.ChmodSocket)
		if err != nil {
			return nil, err
		}
		slog.Info("starting Elythia "+what, "socket", s.cfg.Socket, "url", s.cfg.URL)
		return ln, nil
	}
	addr := fmt.Sprintf(":%d", s.cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	slog.Info("starting Elythia "+what, "addr", addr, "url", s.cfg.URL)
	return ln, nil
}

// shutdownBudgetDefault is the budget of a Shutdown whose ctx has no
// deadline.
const shutdownBudgetDefault = 30 * time.Second

// shutdownBudget splits the time Shutdown has. The drain share is what the
// lock wait and the in-flight requests may use; the rest is kept for
// stopping the background work (shutdown hooks, chart, queue), which always
// runs, and for closing the DB and the listener.
type shutdownBudget struct {
	start, drainUntil, deadline time.Time
}

// shutdownStopMin is the least time the ctx passed to the background stop
// has, even when the ctx of Shutdown is already done. hook に期限切れの ctx を
// 渡さないためのもので、Shutdown がその間待つことは保証しない。
const shutdownStopMin = time.Second

func newShutdownBudget(ctx context.Context, now time.Time) shutdownBudget {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = now.Add(shutdownBudgetDefault)
	}
	if ctx.Err() != nil && deadline.After(now) {
		// 期限の無い ctx が取り消された。もう待たない。
		deadline = now
	}
	total := deadline.Sub(now)
	if total < 0 {
		total = 0
	}
	if total < shutdownStopMin {
		return shutdownBudget{start: now, drainUntil: now, deadline: now.Add(shutdownStopMin)}
	}
	// 待ちの段は全体の 1/3 まで。残りの 2/3 から streaming を閉じる待ち
	// (streamCloseContext、その 1/3 まで)、queue の停止の待ち (stopQueue、さらに
	// その残りの 1/3 まで)、投げっぱなしの goroutine の待ち (bgwork.Group.Stop、
	// さらにその残りの 1/4 まで) を引いても、shutdown hook には全体の 2/9 以上
	// (7 秒で約 1.56 秒) が残る。goroutine の待ちに下限は置かない (期限が切れて
	// いれば待たない) ので、残りが短いときも hook の時間を食わない。
	return shutdownBudget{start: now, drainUntil: now.Add(total / 3), deadline: deadline}
}

// Shutdown stops the current generation and closes its DB pool, then stops
// the listener and removes the UNIX socket file. Safe to call more than once.
//
// 順序と時間の配分:
//
//  1. メンテナンスの監視と判定役を止め、新しいリクエストを unavailable へ回す
//  2. 処理中の HTTP リクエストを待つ (WebSocket は数えない)。世代の切り替えが
//     走っていればその終わりも待つ。どちらも全体の 1/3 まで
//  3. 世代の背景処理を止める (StopBackground: streaming を 1012 で閉じ、queue の
//     停止・投げっぱなしの goroutine の回収・shutdown hook・chart の保存)。処理中のリクエストの待ちが上限に達しても
//     呼ぶ。渡す ctx の期限は全体の期限 (切れていればそこから shutdownStopMin)
//  4. 最後に DB を閉じ、メンテナンスの報告を消す。処理中のリクエストが残って
//     いたら、その数をログに出す
//
// 例外: 切り替えのロックが 2 の上限までに取れなければ、世代は切り替えの側に任せ、
// 3 と 4 を行わない。報告は消さず、プロセスが終わるまで書き続ける (切り替えの
// 途中の世代が DB を開きうるので、判定役に待たせる)。
//
// どの順序でも、メンテナンスの判定は誤らない。判定役は pg_stat_activity を
// 根拠にし、報告が消えたことを「手放した」とは数えない (#3463 の設計第2版)。
//
// 呼び出し元の ctx が切れても、3 を始める (または 3 を行わないと決める) 前には
// 返らない。その後は ctx が切れた時点で返り、残りは裏で続く。返った後に呼び出し元が
// プロセスを終えれば、hook は途中で切られうる。
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		budget := newShutdownBudget(ctx, time.Now())
		stopCalled := make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- s.shutdownNow(budget, stopCalled) }()
		select {
		case err := <-done:
			s.shutdownErr = err
			return
		case <-ctx.Done():
		}
		// 期限は切れたが、背景処理の停止を呼ぶまでは返らない。待ちの段は
		// drainUntil で打ち切られるので、ここで長くは止まらない。
		select {
		case err := <-done:
			s.shutdownErr = err
		case <-stopCalled:
			slog.Warn("shutdown did not finish before the deadline; the rest continues in the background")
			s.shutdownErr = ctx.Err()
		}
	})
	return s.shutdownErr
}

func (s *Supervisor) shutdownNow(b shutdownBudget, stopCalled chan<- struct{}) error {
	var errs []error
	drainCtx, cancelDrain := context.WithDeadline(context.Background(), b.drainUntil)
	defer cancelDrain()
	// 監視を先に止める。止めないと、世代を止めた直後に作り直しかねない。
	s.maint.stopWatch(drainCtx)
	old := s.setHandler(http.HandlerFunc(unavailable))

	// 世代の切り替えの途中なら、終わるのを待つ。待ちの段の上限を過ぎたら、世代は
	// 切り替えの側に任せて止めずに進む。
	if !lockBefore(&s.genMu, b.drainUntil) {
		slog.Warn("a generation switch is still running; leaving its generation to it")
		close(stopCalled)
	} else {
		// ロックを待つ間に、走っていた切り替え (メンテナンスを抜ける作り直し) が
		// 新しい世代のハンドラーを差し込んでいることがある。もう一度 unavailable に
		// 差し替え、差し込まれたハンドラーの処理中も待つ (#3463 のレビュー1周目 L-3)。
		again := s.setHandler(http.HandlerFunc(unavailable))
		if gen := s.gen; gen != nil {
			inFlight := max(retire(drainCtx, old), retire(drainCtx, again))
			// 期限が切れていても、背景処理の停止は必ず呼ぶ。渡す ctx は全体の期限。
			stopCtx, cancelStop := context.WithDeadline(context.Background(), b.deadline)
			close(stopCalled)
			errs = append(errs, gen.srv.StopBackground(stopCtx))
			cancelStop()
			if inFlight = max(inFlight, old.active.Load(), again.active.Load()); inFlight > 0 {
				slog.Warn("closing the database with requests still in flight", "inFlight", inFlight)
			}
			closeCtx, cancelClose := context.WithDeadline(context.Background(), b.deadline)
			_, err := s.closeDB(closeCtx, gen.db)
			cancelClose()
			errs = append(errs, err)
			s.gen = nil
		} else {
			close(stopCalled)
		}
		s.genMu.Unlock()
		s.maint.leave()
	}
	httpCtx, cancelHTTP := context.WithDeadline(context.Background(), b.deadline)
	errs = append(errs, s.httpSrv.Shutdown(httpCtx))
	cancelHTTP()
	// UDS で listen していた場合、ソケットファイルは httpSrv.Shutdown が listener を
	// 閉じた時点で消えている (net.Listen("unix") の UnixListener は Close で自分の
	// path を unlink する)。ここでは念のためもう一度消す。
	// listen に至らなかったときは消さない。そのときこの path にあるのは自分の
	// ソケットではなく、ListenUnixSocket が消すのを拒んだソケット以外のファイルか
	// もしれない。起動に失敗したら serve がシグナルを待たずに Shutdown を呼ぶので
	// (#3633)、ここで消すと拒んだ意味が無くなる。
	select {
	case <-s.listening:
		if rmErr := config.RemoveUnixSocket(s.cfg.Socket); rmErr != nil {
			slog.Warn("failed to remove socket file", "socket", s.cfg.Socket, "err", rmErr)
		}
	default:
	}
	return errors.Join(errs...)
}

// lockBefore takes mu, giving up at deadline. It reports whether it holds mu.
func lockBefore(mu *sync.Mutex, deadline time.Time) bool {
	for {
		if mu.TryLock() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
