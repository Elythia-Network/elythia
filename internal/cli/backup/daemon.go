package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	backupkit "github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/backup/daemon"
	"github.com/elythia-network/elythia/internal/cli/cliflag"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/safehttp"
)

// Deps are the parts of the daemon that other stages provide.
type Deps struct {
	Storage  backupkit.Storage
	Taker    daemon.Taker
	Verifier daemon.Verifier
}

// daemonEnv carries the process-level dependencies so tests can replace them.
type daemonEnv struct {
	stderr     io.Writer
	loadConfig func(string) (*config.Config, error)
	// openDeps builds the storage, taker and verifier from the config.
	openDeps func(*config.Config, *slog.Logger) (Deps, error)
	// signalContext is canceled by SIGINT / SIGTERM.
	signalContext func() (context.Context, context.CancelFunc)
	listen        func(addr string) (net.Listener, error)
	location      *time.Location
}

func defaultDaemonEnv() daemonEnv {
	return daemonEnv{
		stderr:     os.Stderr,
		loadConfig: config.Load,
		openDeps:   wireDeps(defaultEnv(), defaultVerifyEnv()),
		signalContext: func() (context.Context, context.CancelFunc) {
			return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		},
		listen:   func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) },
		location: time.Local,
	}
}

// Daemon implements "elythia backup daemon": it takes, verifies and prunes
// backups on backup.schedule, reports problems to backup.notify and serves
// the control API on backup.schedule.listen. It returns the exit code.
//
//	elythia backup daemon -config .config/default.yml
func Daemon(args []string) int { return runDaemon(defaultDaemonEnv(), args) }

func runDaemon(e daemonEnv, args []string) int {
	fs := cliflag.New("backup daemon", e.stderr)
	cfgPath := fs.String("config", defaultConfigPath, "path to configuration file")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	logger := slog.New(slog.NewTextHandler(e.stderr, nil))
	cfg, err := e.loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(e.stderr, "backup daemon: cannot read the config: %v\n", err)
		return 1
	}
	d, handler, err := build(e, cfg, logger)
	if err != nil {
		fmt.Fprintf(e.stderr, "backup daemon: %v\n", err)
		return 1
	}

	ctx, cancel := e.signalContext()
	defer cancel()
	var ln net.Listener
	if handler != nil {
		ln, err = e.listen(cfg.Backup.Schedule.Listen)
		if err != nil {
			fmt.Fprintf(e.stderr, "backup daemon: cannot listen on %s: %v\n", cfg.Backup.Schedule.Listen, err)
			return 1
		}
		logger.Info("backup: control API listening", "addr", ln.Addr().String())
	}

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	apiErr := make(chan error, 1)
	if ln != nil {
		go func() {
			err := daemon.Serve(ctx, ln, handler, logger)
			// 制御 API が止まったら daemon も止める。管理画面から頼めない状態で
			// 動き続けると、運営者が気付かない。
			stop()
			apiErr <- err
		}()
	}
	d.Run(ctx)
	if ln != nil {
		if err := <-apiErr; err != nil {
			fmt.Fprintf(e.stderr, "backup daemon: control API: %v\n", err)
			return 1
		}
	}
	return 0
}

// build validates the config and assembles the daemon and its control API
// handler (nil when backup.schedule.listen is empty).
func build(e daemonEnv, cfg *config.Config, logger *slog.Logger) (*daemon.Daemon, http.Handler, error) {
	if cfg.Backup == nil {
		return nil, nil, errors.New("the config has no backup: section")
	}
	b := cfg.Backup
	sched, err := daemon.ParseSchedule(b.Schedule, e.location)
	if err != nil {
		return nil, nil, err
	}
	if sched == nil && b.Schedule.Listen == "" {
		return nil, nil, errors.New("nothing to do: set backup.schedule.interval or backup.schedule.listen")
	}
	var notifier daemon.Notifier
	if b.Notify.WebhookURL != "" {
		transport := safehttp.NewSSRFSafeTransport(cfg.AllowedPrivateNetworks,
			safehttp.WithProxy(cfg.Proxy, cfg.ProxyBypassHosts),
			safehttp.WithOutgoingAddress(cfg.OutgoingAddress),
			safehttp.WithAddressFamily(cfg.OutgoingAddressFamily),
		)
		wh, err := daemon.NewWebhook(b.Notify.WebhookURL, b.Notify.Format, daemon.NewWebhookClient(transport))
		if err != nil {
			return nil, nil, err
		}
		notifier = wh
	} else {
		logger.Warn("backup: backup.notify.webhookUrl is empty; failures are only logged")
	}
	deps, err := e.openDeps(cfg, logger)
	if err != nil {
		return nil, nil, err
	}
	d := daemon.New(daemon.Options{
		Schedule: sched, Storage: deps.Storage, Taker: deps.Taker, Verifier: deps.Verifier,
		Notifier: notifier, Instance: cfg.URL, Logger: logger,
	})
	if b.Schedule.Listen == "" {
		return d, nil, nil
	}
	h, err := daemon.Handler(d, b.Server.ServiceToken)
	if err != nil {
		return nil, nil, err
	}
	return d, h, nil
}

// wireDeps returns the openDeps that builds the storage with
// backup.OpenStorage, the taker with backup.Take and the verifier with
// backup.Verify, from the same options "backup take" and "backup verify" use.
func wireDeps(te env, ve verifyEnv) func(*config.Config, *slog.Logger) (Deps, error) {
	return func(cfg *config.Config, logger *slog.Logger) (Deps, error) {
		st, err := te.openStorage(cfg.Backup.Storage)
		if err != nil {
			return Deps{}, fmt.Errorf("cannot open the storage: %w", err)
		}
		topts, err := takeOptions(te, cfg, st, logger)
		if err != nil {
			return Deps{}, err
		}
		b := cfg.Backup
		if b.Encryption.Enabled && b.Schedule.Verify && b.Encryption.IdentityFile == "" {
			// 毎回の検証が復号できずに失敗し続け、使える世代が 1 つもできない。
			return Deps{}, errors.New("backup.schedule.verify needs backup.encryption.identityFile when encryption is enabled")
		}
		// 定期実行で確かめない設定でも、制御 API から確かめることを頼めるので、
		// 確かめる準備 (同梱の migration の番号、秘密鍵) は起動時に済ませて誤りを出す。
		vopts, err := verifyOptions(ve, b)
		if err != nil {
			return Deps{}, err
		}
		return Deps{
			Storage:  st,
			Taker:    newTaker(topts),
			Verifier: verifier{st: st, opts: vopts, verify: ve.verify},
		}, nil
	}
}

// Defaults of how long a take waits for the database to accept connections.
const (
	defaultDBWait      = 5 * time.Minute
	defaultDBRetry     = 5 * time.Second
	defaultPingTimeout = 10 * time.Second
)

// taker adapts backup.Take to daemon.Taker. Before each take it waits up to
// dbWait for the database to accept connections.
//
// compose の backup-daemon は DB の起動を depends_on で待たない (待つ形にすると、
// `up` が DB のコンテナを作り直しうる)。DB と同時に起動したときや DB の再起動中に、
// 1 回目の取る作業が繋がらずに失敗して次の枠 (既定で 1 日後) まで取らない、という
// ことが無いよう、取る前に繋がるまで待つ。待ちきれなければそのまま取りにいき、
// 失敗として知らせる。
type taker struct {
	opts          backupkit.TakeOptions
	ping          func(context.Context) error
	dbWait, retry time.Duration
	now           func() time.Time
}

func newTaker(opts backupkit.TakeOptions) taker {
	return taker{
		opts: opts,
		ping: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, defaultPingTimeout)
			defer cancel()
			conn, err := pgx.Connect(ctx, opts.DatabaseURL)
			if err != nil {
				return err
			}
			return conn.Close(ctx)
		},
		dbWait: defaultDBWait,
		retry:  defaultDBRetry,
		now:    time.Now,
	}
}

// waitForDB returns once ping succeeds, dbWait has passed or ctx is done.
func (t taker) waitForDB(ctx context.Context) {
	logger := t.opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	deadline := t.now().Add(t.dbWait)
	for {
		err := t.ping(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		if !t.now().Before(deadline) {
			logger.Error("backup: the database still does not accept connections; taking the backup anyway", "waited", t.dbWait.String(), "error", err)
			return
		}
		logger.Warn("backup: waiting for the database to accept connections", "error", err)
		timer := time.NewTimer(t.retry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (t taker) Take(ctx context.Context) (backupkit.Meta, error) {
	if t.ping != nil {
		t.waitForDB(ctx)
	}
	m, err := backupkit.Take(ctx, t.opts)
	if err != nil {
		return backupkit.Meta{}, err
	}
	return *m, nil
}

// verifier adapts backup.Verify to daemon.Verifier.
type verifier struct {
	st     backupkit.Storage
	opts   backupkit.VerifyOptions
	verify func(context.Context, backupkit.Storage, string, backupkit.VerifyOptions) (backupkit.VerifyResult, error)
}

func (v verifier) Verify(ctx context.Context, id string) (backupkit.VerifyResult, error) {
	return v.verify(ctx, v.st, id, v.opts)
}
