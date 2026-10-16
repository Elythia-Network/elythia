package serve

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
)

// runServeWithoutSignal runs serve and returns its exit code without ever
// delivering the stop signal. It fails the test when serve keeps waiting.
func runServeWithoutSignal(t *testing.T, h *harness) int {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- serve(h.e, nil) }()
	select {
	case code := <-done:
		return code
	case <-time.After(5 * time.Second):
		// 後片付けのためにシグナルを送ってから落とす。
		if h.cancel != nil {
			h.cancel()
		}
		t.Fatal("serve kept waiting for a signal after the server failed to start")
		return -1
	}
}

// TestServe_StartFailureExitsWithoutSignal checks #3633: when Start returns
// an error (the listener could not be bound, the background work failed to
// start), serve shuts down without waiting for a signal and exits non-zero.
func TestServe_StartFailureExitsWithoutSignal(t *testing.T) {
	h := newHarness()
	h.srv.startErr = errors.New("listen tcp :3000: bind: address already in use")
	code := runServeWithoutSignal(t, h)
	assert.Equal(t, 1, code)
	assert.True(t, h.srv.shutdown, "Shutdown was not called after the start failure")
	assert.Equal(t, []string{
		"log:stdout", "config:.config/default.yml", "pid:/run/x.pid", "sentry", "redis", "supervisor",
		"redis-close", "sentry-flush", "pid-cleanup",
	}, h.steps)
	assert.Contains(t, h.log.String(), "server error")
	assert.Contains(t, h.log.String(), "address already in use")
	assert.Contains(t, h.stdout.String(), "Elythia stopped.")
}

// TestServe_StartFailureKeepsShutdownRules checks that the shutdown after a
// start failure follows the same rules as after a signal: a Shutdown that hit
// its deadline leaves Redis open, other errors still close it.
func TestServe_StartFailureKeepsShutdownRules(t *testing.T) {
	for _, tt := range []struct {
		name          string
		shutdownErr   error
		wantRedisOpen bool
	}{
		{name: "deadline", shutdownErr: fmt.Errorf("wrapped: %w", context.DeadlineExceeded), wantRedisOpen: true},
		{name: "other error", shutdownErr: errors.New("still busy")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness()
			h.srv.startErr = errors.New("bind failed")
			h.srv.shutdownErr = tt.shutdownErr
			assert.Equal(t, 1, runServeWithoutSignal(t, h))
			assert.Contains(t, h.log.String(), "server shutdown error")
			if tt.wantRedisOpen {
				assert.NotContains(t, h.steps, "redis-close")
				assert.Contains(t, h.log.String(), "leaving Redis open")
			} else {
				assert.Contains(t, h.steps, "redis-close")
			}
		})
	}
}

// TestServe_StartFailureShutdownHasDeadline checks that the Shutdown after a
// start failure gets the same bounded context as after a signal.
func TestServe_StartFailureShutdownHasDeadline(t *testing.T) {
	h := newHarness()
	var deadline time.Time
	var hasDeadline bool
	srv := &blockingServer{started: make(chan struct{}), stop: make(chan struct{}), startErr: errors.New("bind failed")}
	srv.onShutdown = func(ctx context.Context) { deadline, hasDeadline = ctx.Deadline() }
	h.e.newSupervisor = func(*config.Config, *cache.RedisClients) (supervisor, error) { return srv, nil }
	before := time.Now()
	assert.Equal(t, 1, runServeWithoutSignal(t, h))
	require.True(t, hasDeadline, "Shutdown got a context without a deadline")
	assert.WithinDuration(t, before.Add(shutdownTimeout), deadline, 2*time.Second)
}

// blockingServer is a supervisor whose Start blocks like the real one: it
// returns startErr at once when set, otherwise stopErr (nil by default) once
// Shutdown is called.
type blockingServer struct {
	started    chan struct{}
	stop       chan struct{}
	startErr   error
	stopErr    error
	onShutdown func(ctx context.Context)
	shutdowns  int
}

func (b *blockingServer) Start() error {
	close(b.started)
	if b.startErr != nil {
		return b.startErr
	}
	<-b.stop
	// 本物の Supervisor.Start は、Shutdown の後の http.ErrServerClosed を nil に
	// して返す。stopErr は、終了処理の最中に起動の残り (StartBackground や listen)
	// が失敗した場合を表す。
	return b.stopErr
}

func (b *blockingServer) Shutdown(ctx context.Context) error {
	if b.onShutdown != nil {
		b.onShutdown(ctx)
	}
	b.shutdowns++
	close(b.stop)
	return nil
}

// TestServe_WaitsForSignalWhileServing checks that a server that started
// fine keeps serving until the signal, and that the nil Start returns after
// the graceful Shutdown is not taken for a failure.
func TestServe_WaitsForSignalWhileServing(t *testing.T) {
	h := newHarness()
	srv := &blockingServer{started: make(chan struct{}), stop: make(chan struct{})}
	h.e.newSupervisor = func(*config.Config, *cache.RedisClients) (supervisor, error) { return srv, nil }
	done := make(chan int, 1)
	go func() { done <- serve(h.e, nil) }()
	select {
	case <-srv.started:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not start the server within 5s")
	}
	select {
	case code := <-done:
		t.Fatalf("serve returned %d while the server was still serving", code)
	case <-time.After(200 * time.Millisecond):
	}
	h.cancel()
	select {
	case code := <-done:
		assert.Equal(t, 0, code)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return within 5s of the stop signal")
	}
	assert.Equal(t, 1, srv.shutdowns)
	assert.NotContains(t, h.log.String(), "server error")
}

// TestServe_NilStartBeforeSignalKeepsWaiting checks that a Start returning
// nil, which is not a failure, does not end the process by itself.
func TestServe_NilStartBeforeSignalKeepsWaiting(t *testing.T) {
	h := newHarness()
	done := make(chan int, 1)
	go func() { done <- serve(h.e, nil) }()
	select {
	case <-h.srv.started:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not start the server within 5s")
	}
	select {
	case code := <-done:
		t.Fatalf("serve returned %d although Start reported no error", code)
	case <-time.After(200 * time.Millisecond):
	}
	h.cancel()
	select {
	case code := <-done:
		assert.Equal(t, 0, code)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return within 5s of the stop signal")
	}
}

// TestServe_LogsStartErrorAfterSignal checks that an error Start returns while
// serve is already shutting down after a signal is still logged before serve
// returns. The exit code stays 0: the operator asked the process to stop.
func TestServe_LogsStartErrorAfterSignal(t *testing.T) {
	h := newHarness()
	srv := &blockingServer{started: make(chan struct{}), stop: make(chan struct{}),
		stopErr: errors.New("start queue worker: redis down")}
	h.e.newSupervisor = func(*config.Config, *cache.RedisClients) (supervisor, error) { return srv, nil }
	done := make(chan int, 1)
	go func() { done <- serve(h.e, nil) }()
	select {
	case <-srv.started:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not start the server within 5s")
	}
	h.cancel()
	select {
	case code := <-done:
		assert.Equal(t, 0, code)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return within 5s of the stop signal")
	}
	assert.Contains(t, h.log.String(), "start queue worker: redis down")
}

// TestServe_LogsStartErrorRacingSignal checks that when the signal and the
// start failure arrive together, the error is logged whichever one serve
// picks, and the exit code is 0 (signal) or 1 (start failure).
func TestServe_LogsStartErrorRacingSignal(t *testing.T) {
	codes := map[int]int{}
	for i := range 200 {
		h := newHarness()
		h.srv.startErr = errors.New("bind failed")
		done := make(chan int, 1)
		go func() { done <- serve(h.e, nil) }()
		select {
		case <-h.srv.started:
		case <-time.After(5 * time.Second):
			t.Fatal("serve did not start the server within 5s")
		}
		// Start のエラーと競わせてシグナルを送る。どちらが先に select に届くかは
		// 回ごとに変わる。
		h.cancel()
		var code int
		select {
		case code = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("serve did not return within 5s")
		}
		codes[code]++
		assert.Contains(t, []int{0, 1}, code, "iteration %d", i)
		require.Contains(t, h.log.String(), "bind failed", "iteration %d: exit %d", i, code)
	}
	t.Logf("exit codes: %v", codes)
}

// TestServe_DoesNotWaitForStartPastTheDeadline checks that after Shutdown,
// serve waits for Start to return only until the shutdown deadline. Start
// can stay blocked: when it holds the generation lock in StartBackground,
// Shutdown gives up on the lock and returns without stopping it.
func TestServe_DoesNotWaitForStartPastTheDeadline(t *testing.T) {
	const timeout = time.Second
	h := newHarness()
	h.e.shutdownTimeout = timeout
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := &stuckServer{started: make(chan struct{}), release: release}
	h.e.newSupervisor = func(*config.Config, *cache.RedisClients) (supervisor, error) { return srv, nil }
	done := make(chan int, 1)
	go func() { done <- serve(h.e, nil) }()
	select {
	case <-srv.started:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not start the server within 5s")
	}
	signaled := time.Now()
	h.cancel()
	select {
	case code := <-done:
		assert.Equal(t, 0, code)
	case <-time.After(5 * time.Second):
		t.Fatal("serve kept waiting for a Start that does not return")
	}
	// Shutdown が期限まで使い切るので、その後の待ちに新しい期限を取ると 2 倍になる。
	assert.Less(t, time.Since(signaled), timeout+800*time.Millisecond,
		"serve waited for Start past the shutdown deadline")
}

// stuckServer is a supervisor whose Start never returns until release is
// closed, and whose Shutdown uses up its whole deadline.
type stuckServer struct {
	started chan struct{}
	release chan struct{}
}

func (s *stuckServer) Start() error {
	close(s.started)
	<-s.release
	return nil
}

func (s *stuckServer) Shutdown(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
