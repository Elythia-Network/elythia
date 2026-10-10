package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
)

// fakeGenerations builds DB-less server generations whose router has a
// /healthz route, a /gen route answering the generation number and a /slow
// route that blocks until release is closed.
type fakeGenerations struct {
	t        *testing.T
	built    atomic.Int32
	closed   atomic.Int32
	release  chan struct{}
	entered  chan struct{}
	openErr  error
	buildErr error
	closeDB  func(*gorm.DB) error
}

func newFakeGenerations(t *testing.T) *fakeGenerations {
	return &fakeGenerations{t: t, release: make(chan struct{}), entered: make(chan struct{}, 16)}
}

func (f *fakeGenerations) deps(drain time.Duration) SupervisorDeps {
	return SupervisorDeps{
		OpenDB: func(*config.Config) (*gorm.DB, error) {
			if f.openErr != nil {
				return nil, f.openErr
			}
			return &gorm.DB{}, nil
		},
		NewServer: func(cfg *config.Config, _ *gorm.DB, _ *cache.RedisClients) (*Server, error) {
			if f.buildErr != nil {
				return nil, f.buildErr
			}
			n := f.built.Add(1)
			e := echo.New()
			e.GET("/healthz", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })
			e.GET("/gen", func(c echo.Context) error { return c.JSON(http.StatusOK, n) })
			e.GET("/slow", func(c echo.Context) error {
				f.entered <- struct{}{}
				<-f.release
				return c.String(http.StatusOK, "slow")
			})
			return &Server{echo: e, config: cfg, role: config.RoleServer}, nil
		},
		CloseDB: func(db *gorm.DB) error {
			f.closed.Add(1)
			if f.closeDB != nil {
				return f.closeDB(db)
			}
			return nil
		},
		DrainTimeout: drain,
	}
}

func getBody(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestNewSupervisor_RejectsIncompleteDeps(t *testing.T) {
	_, err := NewSupervisor(&config.Config{}, nil, SupervisorDeps{})
	assert.Error(t, err)
}

func TestNewSupervisor_FailuresReleaseWhatWasOpened(t *testing.T) {
	t.Run("db", func(t *testing.T) {
		f := newFakeGenerations(t)
		f.openErr = errors.New("db down")
		_, err := NewSupervisor(&config.Config{}, nil, f.deps(0))
		assert.ErrorContains(t, err, "connect to database: db down")
	})
	t.Run("server", func(t *testing.T) {
		f := newFakeGenerations(t)
		f.buildErr = errors.New("bad plugin")
		_, err := NewSupervisor(&config.Config{}, nil, f.deps(0))
		assert.ErrorContains(t, err, "construct server: bad plugin")
		assert.EqualValues(t, 1, f.closed.Load(), "Server を作れなかったのに DB のプールを閉じていない")
	})
}

// TestSupervisor_StopWaitsForInFlightRequests checks that a generation is
// stopped only after the requests it was serving finished, while requests
// arriving meanwhile already get the standby handler.
func TestSupervisor_StopWaitsForInFlightRequests(t *testing.T) {
	f := newFakeGenerations(t)
	sup, err := NewSupervisor(&config.Config{}, nil, f.deps(5*time.Second))
	require.NoError(t, err)

	slowDone := make(chan int, 1)
	go func() {
		code, _ := getBody(t, sup, "/slow")
		slowDone <- code
	}()
	<-f.entered

	stopped := make(chan StopReport, 1)
	go func() { stopped <- sup.stopGeneration(http.HandlerFunc(unavailable)) }()

	// 止めている最中に来たリクエストは、もう古い世代へは行かない。
	require.Eventually(t, func() bool {
		code, _ := getBody(t, sup, "/healthz")
		return code == http.StatusServiceUnavailable
	}, 5*time.Second, 5*time.Millisecond)
	select {
	case <-stopped:
		t.Fatal("処理中のリクエストを待たずに世代を止めた")
	case <-time.After(100 * time.Millisecond):
	}
	assert.Zero(t, f.closed.Load(), "処理中のリクエストがあるのに DB を閉じた")

	close(f.release)
	assert.Equal(t, http.StatusOK, <-slowDone)
	rep := <-stopped
	assert.Zero(t, rep.InFlight)
	assert.NoError(t, rep.Err)
	assert.EqualValues(t, 1, f.closed.Load())

	// 止まっているときにもう一度止めても何もしない。
	assert.Equal(t, StopReport{}, sup.stopGeneration(http.HandlerFunc(unavailable)))
}

// TestSupervisor_StopReportsRequestsLeftAfterDeadline checks the bounded
// drain: a request that does not finish is reported, not waited for forever.
func TestSupervisor_StopReportsRequestsLeftAfterDeadline(t *testing.T) {
	f := newFakeGenerations(t)
	sup, err := NewSupervisor(&config.Config{}, nil, f.deps(50*time.Millisecond))
	require.NoError(t, err)
	defer close(f.release)
	go func() { _, _ = getBody(t, sup, "/slow") }()
	<-f.entered
	rep := sup.stopGeneration(http.HandlerFunc(unavailable))
	assert.EqualValues(t, 1, rep.InFlight)
}

// TestSupervisor_StopReportsUnclosedPool checks that a DB pool that does not
// close in time is reported instead of blocking the stop.
func TestSupervisor_StopReportsUnclosedPool(t *testing.T) {
	f := newFakeGenerations(t)
	block := make(chan struct{})
	defer close(block)
	f.closeDB = func(*gorm.DB) error { <-block; return nil }
	sup, err := NewSupervisor(&config.Config{}, nil, f.deps(50*time.Millisecond))
	require.NoError(t, err)
	rep := sup.stopGeneration(http.HandlerFunc(unavailable))
	assert.ErrorIs(t, rep.Err, context.DeadlineExceeded)
}

// TestSupervisor_WebSocketRequestsAreNotCounted checks that a hijacked
// WebSocket, which lives as long as the connection, does not hold up the
// drain. Server.StopBackground closes those itself.
func TestSupervisor_WebSocketRequestsAreNotCounted(t *testing.T) {
	f := newFakeGenerations(t)
	sup, err := NewSupervisor(&config.Config{}, nil, f.deps(5*time.Second))
	require.NoError(t, err)
	defer close(f.release)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/slow", nil)
		req.Header.Set("Upgrade", "websocket")
		sup.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-f.entered
	assert.Zero(t, sup.current.Load().active.Load())
	assert.Zero(t, sup.stopGeneration(http.HandlerFunc(unavailable)).InFlight)
}

// TestSupervisor_RequestOnRetiredHandlerGoesToTheCurrentOne covers the race
// between loading the handler and the stop: a request that grabbed a
// handler which got retired must not be served by it.
func TestSupervisor_RequestOnRetiredHandlerGoesToTheCurrentOne(t *testing.T) {
	sup := &Supervisor{}
	var oldCalls, newCalls atomic.Int32
	old := &routedHandler{next: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { oldCalls.Add(1) })}
	old.retired.Store(true)
	sup.current.Store(old)
	done := make(chan struct{})
	go func() {
		sup.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	sup.current.Store(&routedHandler{next: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { newCalls.Add(1) })})
	<-done
	assert.Zero(t, oldCalls.Load())
	assert.EqualValues(t, 1, newCalls.Load())
	assert.Zero(t, old.active.Load())
}

// TestSupervisor_FailedRebuildKeepsStandbyAndCanRetry checks that a
// generation that cannot be built leaves the standby handler serving, and
// that a later attempt succeeds.
func TestSupervisor_FailedRebuildKeepsStandbyAndCanRetry(t *testing.T) {
	f := newFakeGenerations(t)
	sup, err := NewSupervisor(&config.Config{}, nil, f.deps(time.Second))
	require.NoError(t, err)
	sup.stopGeneration(http.HandlerFunc(unavailable))

	f.openErr = errors.New("db down")
	assert.ErrorContains(t, sup.startGeneration(), "db down")
	code, _ := getBody(t, sup, "/healthz")
	assert.Equal(t, http.StatusServiceUnavailable, code)

	f.openErr = nil
	require.NoError(t, sup.startGeneration())
	code, body := getBody(t, sup, "/gen")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "2\n", body, "作り直した世代が応答していない")
	// 動いている世代があるときは作り直さない。
	require.NoError(t, sup.startGeneration())
	assert.EqualValues(t, 2, f.built.Load())
}

// TestSupervisor_StartListensAndShutdownCleansUp starts the listener on a
// UNIX socket and on a TCP port and checks that Shutdown stops serving,
// closes the DB pool and removes the socket file.
func TestSupervisor_StartListensAndShutdownCleansUp(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "")
	t.Run("unix socket", func(t *testing.T) {
		sock := filepath.Join(t.TempDir(), "mk.sock")
		f := newFakeGenerations(t)
		sup, err := NewSupervisor(&config.Config{Socket: sock}, nil, f.deps(time.Second))
		require.NoError(t, err)
		startErr := make(chan error, 1)
		go func() { startErr <- sup.Start() }()
		<-sup.listening
		client := &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		}}
		res, err := client.Get("http://unix/healthz")
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		assert.Equal(t, http.StatusOK, res.StatusCode)

		require.NoError(t, sup.Shutdown(context.Background()))
		require.NoError(t, <-startErr)
		assert.EqualValues(t, 1, f.closed.Load())
		_, err = os.Stat(sock)
		assert.True(t, os.IsNotExist(err), "socket file was not removed")
		// 2 回目は何もしない。
		require.NoError(t, sup.Shutdown(context.Background()))
		assert.EqualValues(t, 1, f.closed.Load())
	})
	t.Run("tcp", func(t *testing.T) {
		f := newFakeGenerations(t)
		sup, err := NewSupervisor(&config.Config{Port: 0}, nil, f.deps(time.Second))
		require.NoError(t, err)
		startErr := make(chan error, 1)
		go func() { startErr <- sup.Start() }()
		<-sup.listening
		_, port, err := net.SplitHostPort(*sup.listenerAddr.Load())
		require.NoError(t, err)
		res, err := http.Get("http://127.0.0.1:" + port + "/healthz")
		require.NoError(t, err)
		_ = res.Body.Close()
		assert.Equal(t, http.StatusOK, res.StatusCode)
		require.NoError(t, sup.Shutdown(context.Background()))
		require.NoError(t, <-startErr)
	})
	t.Run("listen failure", func(t *testing.T) {
		f := newFakeGenerations(t)
		sup, err := NewSupervisor(&config.Config{Socket: filepath.Join(t.TempDir(), "missing", "mk.sock")}, nil, f.deps(time.Second))
		require.NoError(t, err)
		assert.Error(t, sup.Start())
	})
}
