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
}

// NewSupervisor builds the first server generation (opening the DB pool) and
// returns a Supervisor ready to Start.
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

	gen, err := s.buildGeneration()
	if err != nil {
		return nil, err
	}
	s.gen = gen
	s.setHandler(gen.srv.ServeHandler())
	return s, nil
}

// unavailable answers while no generation is serving.
func unavailable(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Retry-After", "5")
	http.Error(w, "service unavailable", http.StatusServiceUnavailable)
}

// buildGeneration opens a DB pool and constructs a Server on it. On failure
// it closes the pool it opened.
func (s *Supervisor) buildGeneration() (*generation, error) {
	db, err := s.deps.OpenDB(s.cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	srv, err := s.deps.NewServer(s.cfg, db, s.redis)
	if err != nil {
		if cerr := s.deps.CloseDB(db); cerr != nil {
			slog.Warn("failed to close database after a failed server construction", "err", cerr)
		}
		return nil, fmt.Errorf("construct server: %w", err)
	}
	return &generation{srv: srv, db: db}, nil
}

// setHandler routes new requests to h and returns the handler it replaced.
func (s *Supervisor) setHandler(h http.Handler) *routedHandler {
	return s.current.Swap(&routedHandler{next: h})
}

// ServeHTTP dispatches to the current handler.
func (s *Supervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for {
		rh := s.current.Load()
		// WebSocket は hijack されて長く残るので数えない。止めるときは
		// Server.StopBackground が close frame を送って閉じる。
		if isWebSocketUpgrade(r) {
			rh.next.ServeHTTP(w, r)
			return
		}
		rh.active.Add(1)
		if rh.retired.Load() {
			rh.active.Add(-1)
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
	if rep.InFlight = retire(drainCtx, old); rep.InFlight > 0 {
		slog.Warn("in-flight requests did not finish before the drain deadline", "inFlight", rep.InFlight)
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), s.deps.DrainTimeout)
	defer cancelStop()
	stopErr := gen.srv.StopBackground(stopCtx)
	closeCtx, cancelClose := context.WithTimeout(context.Background(), s.deps.DrainTimeout)
	defer cancelClose()
	var closeErr error
	rep.OpenConns, closeErr = s.closeDB(closeCtx, gen.db)
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
func (s *Supervisor) startGeneration() error {
	s.genMu.Lock()
	defer s.genMu.Unlock()
	return s.startGenerationLocked()
}

func (s *Supervisor) startGenerationLocked() error {
	if s.gen != nil {
		return nil
	}
	gen, err := s.buildGeneration()
	if err != nil {
		return err
	}
	if err := gen.srv.StartBackground(); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), s.deps.DrainTimeout)
		defer cancel()
		_ = gen.srv.StopBackground(ctx)
		_, _ = s.closeDB(ctx, gen.db)
		return err
	}
	s.gen = gen
	s.setHandler(gen.srv.ServeHandler())
	return nil
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

// Shutdown stops the current generation, closes its DB pool, then stops the
// listener and removes the UNIX socket file. Safe to call more than once.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		var errs []error
		s.genMu.Lock()
		if gen := s.gen; gen != nil {
			errs = append(errs, gen.srv.StopBackground(ctx))
			_, err := s.closeDB(ctx, gen.db)
			errs = append(errs, err)
			s.gen = nil
		}
		s.genMu.Unlock()
		errs = append(errs, s.httpSrv.Shutdown(ctx))
		// UDS listen していた場合、Shutdown で net.Listener.Close() は呼ばれる
		// が、ソケットファイル自体は残るので明示的に unlink しておく。
		if rmErr := config.RemoveUnixSocket(s.cfg.Socket); rmErr != nil {
			slog.Warn("failed to remove socket file", "socket", s.cfg.Socket, "err", rmErr)
		}
		s.shutdownErr = errors.Join(errs...)
	})
	return s.shutdownErr
}
