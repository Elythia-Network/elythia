// Package serve implements "elythia serve", which runs the server, and
// "elythia dump-routes", which builds the same server and prints its routes
// without listening.
package serve

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/cli/cliflag"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	mkdb "github.com/elythia-network/elythia/internal/db"
	"github.com/elythia-network/elythia/internal/db/pgstat"
	"github.com/elythia-network/elythia/internal/maintmode"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/redislog"
	mksentry "github.com/elythia-network/elythia/internal/sentry"
	"github.com/elythia-network/elythia/internal/server"
)

// shutdownTimeout bounds the graceful shutdown after a signal or a start
// failure.
//
// docker の既定の停止猶予 (10 秒) より短くする。終了の後に Sentry の flush
// (最大 2 秒、internal/sentry の flushTimeout) が続くので、それを足しても 10 秒に
// 届かない値にする。届くと SIGKILL と重なり、最後の DB を閉じる処理やログが
// 途中で切られる (#3463)。
const shutdownTimeout = 7 * time.Second

// httpServer is the part of *server.Server dump-routes drives.
type httpServer interface {
	DumpRoutes(w io.Writer) error
}

// supervisor is the part of *server.Supervisor serve drives.
type supervisor interface {
	Start() error
	Shutdown(ctx context.Context) error
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
	// newSupervisor opens the DB and builds the first server generation under
	// a Supervisor, which owns the listener (#3463).
	newSupervisor func(cfg *config.Config, redis *cache.RedisClients) (supervisor, error)
	// signalContext is canceled when the process is asked to stop.
	signalContext func() (context.Context, context.CancelFunc)
	createFile    func(path string) (io.WriteCloser, error)
	// shutdownTimeout overrides the shutdownTimeout constant when non-zero.
	shutdownTimeout time.Duration
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
		newSupervisor: func(cfg *config.Config, redis *cache.RedisClients) (supervisor, error) {
			return server.NewSupervisor(cfg, redis, supervisorDeps(cfg, redis))
		},
		signalContext: func() (context.Context, context.CancelFunc) {
			return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		},
		createFile: func(path string) (io.WriteCloser, error) { return os.Create(path) },
	}
}

// supervisorDeps is what the Supervisor of `elythia serve` is built with.
func supervisorDeps(cfg *config.Config, redis *cache.RedisClients) server.SupervisorDeps {
	return server.SupervisorDeps{
		OpenDB:    model.NewDatabase,
		NewServer: server.New,
		CloseDB:   mkdb.Close,
		// shared_preload_libraries に pg_stat_statements があれば、拡張を
		// 作る (#3909)。世代を作るたびに確かめる。
		PrepareDB:   func(db *gorm.DB) { prepareDatabase(db, slog.Default(), ensurePgStatStatements) },
		Maintenance: maintmode.NewStore(redis.Default, redis.Pubsub, cfg.Redis.KeyPrefix()),
		// どのプロセスも判定役の候補になる。管理画面から入れたときに受けた
		// プロセスが落ちても、ほかのプロセスが判定を引き継ぐ (設計第2版 §2.1)。
		MaintenanceCounter: &maintmode.PGCounter{
			Targets: maintmode.TargetsFromConfig(cfg, "elythia-judge/"+processID()),
		},
		ProcessID: processID(),
	}
}

// ensurePgStatStatements is what supervisorDeps' PrepareDB runs. Tests replace
// it to see the wiring.
var ensurePgStatStatements = pgstat.Ensure

// prepareDatabaseTimeout bounds prepareDatabase, so a slow database delays a
// generation by at most this much.
const prepareDatabaseTimeout = 10 * time.Second

// prepareDatabase creates pg_stat_statements when the server preloads it and
// the database lacks it (#3909). The Supervisor runs it on every database it
// opens. It only logs: stopping the start for a measuring tool would turn a
// missing permission into an outage.
func prepareDatabase(db *gorm.DB, log *slog.Logger, ensure func(context.Context, *gorm.DB) (pgstat.Outcome, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), prepareDatabaseTimeout)
	defer cancel()
	outcome, err := ensure(ctx, db)
	switch {
	case errors.Is(err, pgstat.ErrCreate):
		log.Warn("pg_stat_statements is preloaded but the extension could not be created; run this as a superuser in the Elythia database",
			"sql", pgstat.CreateSQL+";", "error", err)
	case err != nil:
		log.Warn("could not check pg_stat_statements", "error", err)
	case outcome == pgstat.OutcomeCreated:
		log.Info("created the pg_stat_statements extension")
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
// code: 0 after a signal, non-zero when the server failed to start or stopped
// serving with an error, in which case it shuts down without waiting for a
// signal.
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
	log.Info("starting Elythia", "version", config.MisskeyVersion, "mkGoVersion", config.MkGoVersion)

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

	// 本体の接続に名前を付ける。メンテナンスに入ったとき、pg_stat_activity で
	// Elythia の接続が残っていないかを名前で数えるため (#3463)。
	cfg.DBApplicationName = dbApplicationName()

	// Redis を先に開く。DB と Server は Supervisor が開いて作る (#3463)。
	redisClients, closeRedis, err := e.openRedis(cfg)
	if err != nil {
		log.Error("failed to connect to Redis", "error", err)
		return 1
	}
	// Shutdown が期限で返ったときは、shutdown hook (chart の保存・バッファの
	// flush) が裏でまだ走っていて Redis を使いうる。閉じずにプロセスの終了に任せる
	// (#3463 のレビュー1周目 L-7)。
	keepRedis := false
	defer func() {
		if keepRedis {
			log.Warn("leaving Redis open: shutdown hooks may still be running")
			return
		}
		closeRedis()
	}()
	srv, err := e.newSupervisor(cfg, redisClients)
	if err != nil {
		log.Error("failed to construct server", "error", err)
		return 1
	}

	ctx, stop := e.signalContext()
	defer stop()

	// Start は listener を閉じるまで返らない。正常な停止 (Shutdown の後の
	// http.ErrServerClosed) では nil を返すので、エラーで返ったときだけ起動の失敗と
	// みなす。listen に失敗したまま生き続けると、コンテナの再起動にもヘルスチェック
	// にも任せられないので、シグナルを待たずに終了処理へ進み 0 以外で終える (#3633)。
	// ログは goroutine の中で出す。シグナルの後 (終了処理の最中) に返ったエラーは
	// 下の select では受けないので、ここで出さないと残らない。
	startErr := make(chan error, 1)
	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		if err := srv.Start(); err != nil {
			log.Error("server error", "error", err)
			startErr <- err
		}
	}()

	exitCode := 0
	select {
	case <-ctx.Done():
		log.Info("shutting down server...")
	case <-startErr:
		log.Info("shutting down server after a start failure...")
		exitCode = 1
	}

	// 0 のときは定数を使う。テストの env はこのフィールドを埋めないので、既定を
	// 0 で表す。
	timeout := e.shutdownTimeout
	if timeout == 0 {
		timeout = shutdownTimeout
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("server shutdown error", "error", err)
		keepRedis = errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	}
	// Start が終了処理の最中にエラーで返ったとき、そのログをプロセスの終了より前に
	// 出し切る。待つのは終了処理の期限までで、期限は延ばさない。
	select {
	case <-startDone:
	case <-shutdownCtx.Done():
	}

	fmt.Fprintln(e.stdout, "Elythia stopped.")
	return exitCode
}

// processID names this process in the maintenance reports, as the judge
// and in application_name: "<host>-<pid>-<random>". It is fixed for the
// life of the process.
//
// コンテナの中では PID がほぼ 1 で、hostname: を指定した scale や host network
// では host も重なる。重なると報告が上書きされ、判定役のロックの ID も同じに
// なる (#3463 のレビュー2周目 5) ので、起動ごとの乱数を付ける。
var processID = sync.OnceValue(func() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return buildProcessID(host, os.Getpid(), hex.EncodeToString(b[:]))
})

// processIDHostMax bounds the host part of processID so that
// "elythia/<longest role>/<processID>" fits in the 63 bytes PostgreSQL keeps
// of application_name: 63 - len("elythia/invalid/") - len("-<7 digits>-<8 hex>").
const processIDHostMax = 63 - len("elythia/invalid/") - len("-1234567-01234567")

// buildProcessID joins host, pid and random, shortening host so that the pid
// and the random part always survive PostgreSQL's truncation.
func buildProcessID(host string, pid int, random string) string {
	if len(host) > processIDHostMax {
		host = host[:processIDHostMax]
	}
	return fmt.Sprintf("%s-%d-%s", host, pid, random)
}

// dbApplicationName returns the application_name of this process's DB
// connections: "elythia/<role>/<processID>".
func dbApplicationName() string {
	role := "invalid"
	if r, err := config.ResolveProcessRole(); err == nil {
		role = string(r)
	}
	return "elythia/" + role + "/" + processID()
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
	log.Info("starting Elythia", "version", config.MisskeyVersion, "mkGoVersion", config.MkGoVersion)

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
