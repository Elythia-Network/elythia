// Package serve implements "elythia serve", which runs the server, and
// "elythia dump-routes", which builds the same server and prints its routes
// without listening.
package serve

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/cli/cliflag"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/redislog"
	mksentry "github.com/elythia-network/elythia/internal/sentry"
	"github.com/elythia-network/elythia/internal/server"
)

// shutdownTimeout bounds the graceful shutdown after a signal.
const shutdownTimeout = 10 * time.Second

// httpServer is the part of *server.Server these subcommands drive.
type httpServer interface {
	Start() error
	Shutdown(ctx context.Context) error
	DumpRoutes(w io.Writer) error
}

// env carries the process-level dependencies so tests can replace them.
//
// サーバーの構築は DB / Redis / pid file / Sentry / シグナル / slog.Default と、
// プロセス全体に効くものに触る。テストからはそれぞれを差し替えて、起動の順序と
// 終了コードだけを確かめる。
type env struct {
	stdout, stderr io.Writer
	// setupLogging installs the process-wide slog logger writing to w, routes
	// go-redis's internal log into it, and returns it.
	setupLogging func(w io.Writer) *slog.Logger
	loadConfig   func(path string) (*config.Config, error)
	writePidFile func(path string) (func(), error)
	initSentry   func(cfg *config.Config) (func(), error)
	openDB       func(cfg *config.Config) (*gorm.DB, error)
	// openRedis returns the clients and the function that closes them.
	openRedis func(cfg *config.Config) (*cache.RedisClients, func(), error)
	newServer func(cfg *config.Config, db *gorm.DB, redis *cache.RedisClients) (httpServer, error)
	// signalContext is canceled when the process is asked to stop.
	signalContext func() (context.Context, context.CancelFunc)
	createFile    func(path string) (io.WriteCloser, error)
}

func defaultEnv() env {
	return env{
		stdout:       os.Stdout,
		stderr:       os.Stderr,
		setupLogging: setupLogging,
		loadConfig:   config.Load,
		writePidFile: server.WritePidFile,
		initSentry:   mksentry.Init,
		openDB:       model.NewDatabase,
		openRedis: func(cfg *config.Config) (*cache.RedisClients, func(), error) {
			c, err := cache.NewRedisClients(cfg)
			if err != nil {
				return nil, nil, err
			}
			return c, func() { _ = c.Close() }, nil
		},
		newServer: func(cfg *config.Config, db *gorm.DB, redis *cache.RedisClients) (httpServer, error) {
			return server.New(cfg, db, redis)
		},
		signalContext: func() (context.Context, context.CancelFunc) {
			return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		},
		createFile: func(path string) (io.WriteCloser, error) { return os.Create(path) },
	}
}

// setupLogging installs the text slog handler on w.
func setupLogging(w io.Writer) *slog.Logger {
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)
	// go-redis は既定で stderr に直接書くので、接続失敗が構造化ログに
	// 出ない (#2659)。slog に寄せる。slog.SetDefault の後、Redis を触る
	// どの経路よりも前に呼ぶ必要がある。
	redislog.UseSlog()
	return logger
}

// Serve implements "elythia serve": it starts the server and blocks until
// SIGINT / SIGTERM, then shuts down gracefully. It returns the process exit
// code.
func Serve(args []string) int { return serve(defaultEnv(), args) }

// DumpRoutes implements "elythia dump-routes": it constructs the server,
// writes the registered HTTP routes as JSON and exits without listening.
// tools/apicompat reads the output.
func DumpRoutes(args []string) int { return dumpRoutes(defaultEnv(), args) }

func serve(e env, args []string) int {
	fs := cliflag.New("serve", e.stderr)
	configPath := fs.String("config", ".config/default.yml", "path to configuration file")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}

	// 通常起動のログは stdout に出す (以前の misskey バイナリと同じ)。
	log := e.setupLogging(e.stdout)
	log.Info("starting Misskey (Go)", "version", config.MisskeyVersion, "mkGoVersion", config.MkGoVersion)

	cfg, err := e.loadConfig(*configPath)
	if err != nil {
		log.Error("failed to load config", "error", err)
		return 1
	}

	// pidFile が設定されていれば PID を書き込み、終了時に削除する。
	// 既存ファイルが生存中の他プロセスを指す場合は ErrAlreadyRunning で
	// 起動を拒否して二重起動を防ぐ (#497)。
	cleanupPid, err := e.writePidFile(cfg.PidFile)
	if err != nil {
		log.Error("failed to write pid file", "error", err)
		return 1
	}
	defer cleanupPid()

	// Sentry init は他のサービスより前に走らせ、以降の起動エラーも捕捉対象にする。
	flushSentry, err := e.initSentry(cfg)
	if err != nil {
		log.Error("failed to init sentry", "error", err)
		return 1
	}
	defer flushSentry()

	srv, closeRedis, code := build(e, log, cfg)
	if code != 0 {
		return code
	}
	defer closeRedis()

	ctx, stop := e.signalContext()
	defer stop()

	go func() {
		if err := srv.Start(); err != nil {
			log.Error("server error", "error", err)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down server...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("server shutdown error", "error", err)
	}

	fmt.Fprintln(e.stdout, "Misskey stopped.")
	return 0
}

func dumpRoutes(e env, args []string) int {
	fs := cliflag.New("dump-routes", e.stderr)
	configPath := fs.String("config", ".config/default.yml", "path to configuration file")
	out := fs.String("dump-routes-out", "", "path to write the routes JSON to; defaults to stdout. "+
		"recommended to use a file so gorm/slog noise on stdout/stderr doesn't pollute the JSON consumer")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}

	// stdout に JSON だけを出したいので、log は stderr に向ける。
	log := e.setupLogging(e.stderr)
	log.Info("starting Misskey (Go)", "version", config.MisskeyVersion, "mkGoVersion", config.MkGoVersion)

	cfg, err := e.loadConfig(*configPath)
	if err != nil {
		log.Error("failed to load config", "error", err)
		return 1
	}

	// pid file と Sentry は初期化しない。同じ config を共有する稼働中の
	// インスタンスと pid file が衝突するうえ、dump したらすぐ終わるので
	// graceful shutdown 系の配線も要らない。
	srv, closeRedis, code := build(e, log, cfg)
	if code != 0 {
		return code
	}
	defer closeRedis()

	// listener を bind する前に、echo に登録済みの route 一覧を JSON で流して
	// 終わる。tools/apicompat が Misskey TS の api.json と突き合わせるための
	// 入力。DB/Redis 接続は handler 構築 (auth middleware の repo の配線など) で
	// 必須なので、ここまで実行してから dump する。
	w := e.stdout
	if *out != "" {
		f, err := e.createFile(*out)
		if err != nil {
			log.Error("failed to open dump-routes output", "path", *out, "error", err)
			return 1
		}
		defer f.Close()
		w = f
	}
	if err := srv.DumpRoutes(w); err != nil {
		log.Error("failed to dump routes", "error", err)
		return 1
	}
	return 0
}

// build connects to PostgreSQL and Redis and constructs the server. On
// failure it logs the cause, releases what it opened and returns a nil server
// with the exit code.
func build(e env, log *slog.Logger, cfg *config.Config) (httpServer, func(), int) {
	db, err := e.openDB(cfg)
	if err != nil {
		log.Error("failed to connect to database", "error", err)
		return nil, nil, 1
	}
	redisClients, closeRedis, err := e.openRedis(cfg)
	if err != nil {
		log.Error("failed to connect to Redis", "error", err)
		return nil, nil, 1
	}
	srv, err := e.newServer(cfg, db, redisClients)
	if err != nil {
		log.Error("failed to construct server", "error", err)
		closeRedis()
		return nil, nil, 1
	}
	return srv, closeRedis, 0
}
