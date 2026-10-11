package serve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
)

// fakeServer records how the subcommand drove it.
type fakeServer struct {
	mu          sync.Mutex
	started     chan struct{}
	startErr    error
	shutdownErr error
	dumpErr     error
	shutdown    bool
}

func newFakeServer() *fakeServer { return &fakeServer{started: make(chan struct{})} }

func (f *fakeServer) Start() error {
	close(f.started)
	return f.startErr
}

func (f *fakeServer) Shutdown(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdown = true
	return f.shutdownErr
}

func (f *fakeServer) DumpRoutes(w io.Writer) error {
	if f.dumpErr != nil {
		return f.dumpErr
	}
	_, err := io.WriteString(w, `[{"method":"GET","path":"/healthz"}]`)
	return err
}

// harness wires an env whose every process-level dependency is a fake, and
// records the order in which they were used.
type harness struct {
	e              env
	stdout, stderr bytes.Buffer
	log            bytes.Buffer
	steps          []string
	srv            *fakeServer
	files          map[string]*bytes.Buffer
	cancel         context.CancelFunc
	// appName は newSupervisor に渡った cfg の DBApplicationName。
	appName string
}

type nopCloser struct{ *bytes.Buffer }

func (nopCloser) Close() error { return nil }

func newHarness() *harness {
	h := &harness{srv: newFakeServer(), files: map[string]*bytes.Buffer{}}
	h.e = env{
		stdout: &h.stdout,
		stderr: &h.stderr,
		setupLogging: func(w io.Writer) *slog.Logger {
			switch w {
			case &h.stdout:
				h.steps = append(h.steps, "log:stdout")
			case &h.stderr:
				h.steps = append(h.steps, "log:stderr")
			}
			return slog.New(slog.NewTextHandler(&h.log, nil))
		},
		loadConfig: func(path string) (*config.Config, error) {
			h.steps = append(h.steps, "config:"+path)
			return &config.Config{PidFile: "/run/x.pid"}, nil
		},
		writePidFile: func(path string) (func(), error) {
			h.steps = append(h.steps, "pid:"+path)
			return func() { h.steps = append(h.steps, "pid-cleanup") }, nil
		},
		initSentry: func(*config.Config) (func(), error) {
			h.steps = append(h.steps, "sentry")
			return func() { h.steps = append(h.steps, "sentry-flush") }, nil
		},
		openDB: func(*config.Config) (*gorm.DB, error) {
			h.steps = append(h.steps, "db")
			return &gorm.DB{}, nil
		},
		openRedis: func(*config.Config) (*cache.RedisClients, func(), error) {
			h.steps = append(h.steps, "redis")
			return &cache.RedisClients{}, func() { h.steps = append(h.steps, "redis-close") }, nil
		},
		newServer: func(*config.Config, *gorm.DB, *cache.RedisClients) (httpServer, error) {
			h.steps = append(h.steps, "server")
			return h.srv, nil
		},
		newSupervisor: func(cfg *config.Config, _ *cache.RedisClients) (supervisor, error) {
			h.steps = append(h.steps, "supervisor")
			h.appName = cfg.DBApplicationName
			return h.srv, nil
		},
		signalContext: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			h.cancel = cancel
			return ctx, cancel
		},
		createFile: func(path string) (io.WriteCloser, error) {
			b := &bytes.Buffer{}
			h.files[path] = b
			return nopCloser{b}, nil
		},
	}
	return h
}

// runServe runs serve in the background, delivers the stop signal once the
// server has started, and returns the exit code.
func (h *harness) runServe(t *testing.T, args []string) int {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- serve(h.e, args) }()
	// 待ちには上限を付ける。変異で Start やシグナル待ちが壊れたときに、テストが
	// go test の既定の 10 分まで止まらず、すぐに落ちるようにする。
	select {
	case <-h.srv.started:
	case code := <-done:
		t.Fatalf("serve returned %d before starting the server", code)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not start the server within 5s")
	}
	h.cancel()
	select {
	case code := <-done:
		return code
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return within 5s of the stop signal")
		return -1
	}
}

func TestServe_StartsAndShutsDownOnSignal(t *testing.T) {
	h := newHarness()
	code := h.runServe(t, []string{"-config", "x.yml"})
	assert.Equal(t, 0, code)
	assert.Equal(t, []string{
		"log:stdout", "config:x.yml", "pid:/run/x.pid", "sentry", "redis", "supervisor",
		"redis-close", "sentry-flush", "pid-cleanup",
	}, h.steps)
	// DB の接続には、このプロセスを表す名前が付く (#3463)。
	assert.Regexp(t, `^elythia/both/.+-[0-9]+-[0-9a-f]{8}$`, h.appName)
	assert.True(t, h.srv.shutdown)
	assert.Contains(t, h.stdout.String(), "Elythia stopped.")
	assert.Contains(t, h.log.String(), "shutting down server")
}

func TestServe_DefaultConfigPath(t *testing.T) {
	h := newHarness()
	assert.Equal(t, 0, h.runServe(t, nil))
	assert.Contains(t, h.steps, "config:.config/default.yml")
}

func TestServe_LogsStartAndShutdownErrors(t *testing.T) {
	h := newHarness()
	h.srv.startErr = errors.New("bind failed")
	h.srv.shutdownErr = errors.New("still busy")
	assert.Equal(t, 0, h.runServe(t, nil))
	assert.Contains(t, h.log.String(), "server shutdown error")
}

// TestServe_KeepsRedisOpenWhileHooksMayRun checks #3463 review L-7: when
// Shutdown returned at its deadline, the shutdown hooks may still be running
// in the background, so Redis is left open for the process exit to close.
func TestServe_KeepsRedisOpenWhileHooksMayRun(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, fmt.Errorf("wrapped: %w", context.Canceled)} {
		h := newHarness()
		h.srv.shutdownErr = err
		assert.Equal(t, 0, h.runServe(t, nil))
		assert.NotContains(t, h.steps, "redis-close", "hook が走っているかもしれないのに Redis を閉じた")
		assert.Contains(t, h.log.String(), "leaving Redis open")
	}
	// それ以外の失敗では、今までどおり閉じる。
	h := newHarness()
	h.srv.shutdownErr = errors.New("still busy")
	assert.Equal(t, 0, h.runServe(t, nil))
	assert.Contains(t, h.steps, "redis-close")
}

func TestServe_StartupFailures(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name    string
		break_  func(h *harness)
		wantLog string
		// wantSteps is the exact sequence, proving that nothing after the
		// failing step ran and what was opened got released.
		wantSteps []string
	}{
		{
			name:      "config",
			break_:    func(h *harness) { h.e.loadConfig = func(string) (*config.Config, error) { return nil, boom } },
			wantLog:   "failed to load config",
			wantSteps: []string{"log:stdout"},
		},
		{
			name: "pid file",
			break_: func(h *harness) {
				h.e.writePidFile = func(string) (func(), error) { return nil, boom }
			},
			wantLog:   "failed to write pid file",
			wantSteps: []string{"log:stdout", "config:.config/default.yml"},
		},
		{
			name:      "sentry",
			break_:    func(h *harness) { h.e.initSentry = func(*config.Config) (func(), error) { return nil, boom } },
			wantLog:   "failed to init sentry",
			wantSteps: []string{"log:stdout", "config:.config/default.yml", "pid:/run/x.pid", "pid-cleanup"},
		},
		{
			name: "redis",
			break_: func(h *harness) {
				h.e.openRedis = func(*config.Config) (*cache.RedisClients, func(), error) { return nil, nil, boom }
			},
			wantLog: "failed to connect to Redis",
			wantSteps: []string{"log:stdout", "config:.config/default.yml", "pid:/run/x.pid", "sentry",
				"sentry-flush", "pid-cleanup"},
		},
		{
			// DB に繋がらない場合も、Server を作れない場合も、Supervisor の構築の
			// 失敗として返る。
			name: "supervisor",
			break_: func(h *harness) {
				h.e.newSupervisor = func(*config.Config, *cache.RedisClients) (supervisor, error) { return nil, boom }
			},
			wantLog: "failed to construct server",
			wantSteps: []string{"log:stdout", "config:.config/default.yml", "pid:/run/x.pid", "sentry", "redis",
				"redis-close", "sentry-flush", "pid-cleanup"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness()
			tt.break_(h)
			assert.Equal(t, 1, serve(h.e, nil))
			assert.Contains(t, h.log.String(), tt.wantLog)
			assert.Equal(t, tt.wantSteps, h.steps)
		})
	}
}

func TestServe_Flags(t *testing.T) {
	h := newHarness()
	assert.Equal(t, 0, serve(h.e, []string{"-h"}))
	assert.Contains(t, h.stderr.String(), "Usage: elythia serve")
	// 以前の misskey バイナリのモード切替 flag は serve では受けない。
	// 受けると `elythia serve -healthcheck` がサーバーを起動してしまう。
	for _, old := range []string{"-healthcheck", "-doctor", "-fsck", "-config-dump", "-dump-routes"} {
		h := newHarness()
		assert.Equal(t, 2, serve(h.e, []string{old}), old)
		assert.Empty(t, h.steps, "%s must not start anything", old)
	}
}

func TestDumpRoutes_ToStdout(t *testing.T) {
	h := newHarness()
	assert.Equal(t, 0, dumpRoutes(h.e, []string{"-config", "x.yml"}))
	assert.Equal(t, `[{"method":"GET","path":"/healthz"}]`, h.stdout.String())
	// pid file と Sentry には触らない。listener も起動しない。
	assert.Equal(t, []string{"log:stderr", "config:x.yml", "db", "redis", "server", "redis-close"}, h.steps)
	select {
	case <-h.srv.started:
		t.Fatal("dump-routes must not start the listener")
	default:
	}
}

func TestDumpRoutes_ToFile(t *testing.T) {
	h := newHarness()
	assert.Equal(t, 0, dumpRoutes(h.e, []string{"-dump-routes-out", "routes.json"}))
	assert.Empty(t, h.stdout.String())
	require.Contains(t, h.files, "routes.json")
	assert.Contains(t, h.files["routes.json"].String(), "/healthz")
}

func TestDumpRoutes_Failures(t *testing.T) {
	boom := errors.New("boom")
	t.Run("config", func(t *testing.T) {
		h := newHarness()
		h.e.loadConfig = func(string) (*config.Config, error) { return nil, boom }
		assert.Equal(t, 1, dumpRoutes(h.e, nil))
		assert.Contains(t, h.log.String(), "failed to load config")
	})
	t.Run("build", func(t *testing.T) {
		h := newHarness()
		h.e.openDB = func(*config.Config) (*gorm.DB, error) { return nil, boom }
		assert.Equal(t, 1, dumpRoutes(h.e, nil))
		assert.Contains(t, h.log.String(), "failed to connect to database")
	})
	t.Run("build redis", func(t *testing.T) {
		h := newHarness()
		h.e.openRedis = func(*config.Config) (*cache.RedisClients, func(), error) { return nil, nil, boom }
		assert.Equal(t, 1, dumpRoutes(h.e, nil))
		assert.Contains(t, h.log.String(), "failed to connect to Redis")
	})
	t.Run("build server", func(t *testing.T) {
		h := newHarness()
		h.e.newServer = func(*config.Config, *gorm.DB, *cache.RedisClients) (httpServer, error) { return nil, boom }
		assert.Equal(t, 1, dumpRoutes(h.e, nil))
		assert.Contains(t, h.log.String(), "failed to construct server")
		assert.Contains(t, h.steps, "redis-close", "構築に失敗したのに Redis を閉じていない")
	})
	t.Run("output file", func(t *testing.T) {
		h := newHarness()
		h.e.createFile = func(string) (io.WriteCloser, error) { return nil, boom }
		assert.Equal(t, 1, dumpRoutes(h.e, []string{"-dump-routes-out", "x.json"}))
		assert.Contains(t, h.log.String(), "failed to open dump-routes output")
	})
	t.Run("dump", func(t *testing.T) {
		h := newHarness()
		h.srv.dumpErr = boom
		assert.Equal(t, 1, dumpRoutes(h.e, nil))
		assert.Contains(t, h.log.String(), "failed to dump routes")
	})
	t.Run("flags", func(t *testing.T) {
		h := newHarness()
		assert.Equal(t, 0, dumpRoutes(h.e, []string{"-h"}))
		assert.Contains(t, h.stderr.String(), "-dump-routes-out")
		assert.Equal(t, 2, dumpRoutes(h.e, []string{"extra"}))
	})
}

func TestDefaultEnv(t *testing.T) {
	e := defaultEnv()
	assert.Equal(t, os.Stdout, e.stdout)
	assert.Equal(t, os.Stderr, e.stderr)
	assert.NotNil(t, e.setupLogging)
	assert.NotNil(t, e.loadConfig)
	assert.NotNil(t, e.writePidFile)
	assert.NotNil(t, e.initSentry)
	assert.NotNil(t, e.openDB)
	assert.NotNil(t, e.newServer)
	assert.NotNil(t, e.newSupervisor)

	ctx, cancel := e.signalContext()
	cancel()
	<-ctx.Done()

	path := filepath.Join(t.TempDir(), "out.json")
	f, err := e.createFile(path)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	assert.FileExists(t, path)

	// 誰も listen していない宛先へ繋ぐと、Redis の接続確認で失敗して閉じる関数を
	// 返さない。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	cfg := &config.Config{}
	cfg.Redis.Host = "127.0.0.1"
	cfg.Redis.Port = port
	cfg.RedisForPubsub = cfg.Redis
	cfg.RedisForJobQueue = cfg.Redis
	cfg.RedisForTimelines = cfg.Redis
	cfg.RedisForReactions = cfg.Redis
	clients, closeFn, err := e.openRedis(cfg)
	assert.Error(t, err)
	assert.Nil(t, clients)
	assert.Nil(t, closeFn)

	// 繋がる宛先なら、クライアントと閉じる関数を返す。
	live := miniredis.RunT(t)
	livePort, err := strconv.Atoi(live.Port())
	require.NoError(t, err)
	liveCfg := &config.Config{}
	liveCfg.Redis = config.RedisOptions{Host: live.Host(), Port: livePort}
	liveCfg.RedisForPubsub, liveCfg.RedisForJobQueue = liveCfg.Redis, liveCfg.Redis
	liveCfg.RedisForTimelines, liveCfg.RedisForReactions = liveCfg.Redis, liveCfg.Redis
	clients, closeFn, err = e.openRedis(liveCfg)
	require.NoError(t, err)
	require.NotNil(t, clients)
	closeFn()

	// 既定の Supervisor は DB を開くところから始める。誰も listen していない
	// 宛先なら、DB の接続の失敗として返る。
	// その前にメンテナンスの状態を Redis から読む (#3463)。
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	cfg.DB = config.DBOptions{Host: "127.0.0.1", Port: port, DB: "x", User: "x", Pass: "x"}
	sup, err := e.newSupervisor(cfg, &cache.RedisClients{Default: rc, Pubsub: rc})
	assert.ErrorContains(t, err, "connect to database")
	assert.Nil(t, sup)
	// Redis が読めなければ、DB を開く前に失敗する。
	mr.Close()
	_, err = e.newSupervisor(cfg, &cache.RedisClients{Default: rc, Pubsub: rc})
	// 最初に Redis へ書くのは「起動中」の報告 (#3463 のレビュー M1')。
	assert.ErrorContains(t, err, "write the starting report")
}

func TestDBApplicationName(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "")
	assert.Regexp(t, fmt.Sprintf(`^elythia/server/.+-%d-[0-9a-f]{8}$`, os.Getpid()), dbApplicationName())
	assert.Equal(t, processID(), processID(), "同じプロセスで ID が変わった")
	// 矛盾した役割の指定は Server の構築で落ちるが、名前の段では止めない。
	t.Setenv(config.EnvOnlyQueue, "1")
	assert.Regexp(t, `^elythia/invalid/`, dbApplicationName())
}

func TestExportedEntryPoints_StopOnFlagErrors(t *testing.T) {
	// 既定の env でも、flag の段で止まる経路はプロセスの状態に触らない。
	assert.Equal(t, 2, Serve([]string{"-nope"}))
	assert.Equal(t, 2, DumpRoutes([]string{"-nope"}))
}

func TestSetupLogging_InstallsDefault(t *testing.T) {
	// slog.Default は戻す。go-redis のロガーは読み戻せないので戻さないが、
	// 入るのは slog.Default へ流す橋渡しだけで、戻した Default に書くので
	// 後続のテストの出力先は変わらない。
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var buf bytes.Buffer
	logger := setupLogging(&buf)
	assert.Same(t, logger, slog.Default())
	slog.Info("hello")
	assert.True(t, strings.Contains(buf.String(), "msg=hello"), fmt.Sprintf("got %q", buf.String()))
}

// TestBuildProcessID checks #3463 review round 2 item 5: the process ID
// carries a random part, and a long host is shortened so that the pid and
// the random part survive PostgreSQL's 63-byte truncation of
// application_name.
func TestBuildProcessID(t *testing.T) {
	assert.Equal(t, "web-1-0a1b2c3d", buildProcessID("web", 1, "0a1b2c3d"))
	long := strings.Repeat("h", 255)
	id := buildProcessID(long, 1234567, "0a1b2c3d")
	assert.True(t, strings.HasSuffix(id, "-1234567-0a1b2c3d"), id)
	for _, role := range []string{"server", "queue", "both", "invalid"} {
		app := "elythia/" + role + "/" + id
		assert.LessOrEqual(t, len(app), 63, "乱数の部分が PostgreSQL に切り詰められる: %s", app)
	}
}
