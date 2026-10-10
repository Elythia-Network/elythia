package backup

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	bkp "github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/cli/cliflag"
	"github.com/elythia-network/elythia/internal/cli/migrate"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/core/selfcheck"
	"github.com/elythia-network/elythia/internal/redislog"
)

// restoreEnv carries the dependencies of "backup restore" so tests can
// replace them.
type restoreEnv struct {
	stdout, stderr io.Writer
	openStorage    func(config.BackupStorageOptions) (bkp.Storage, error)
	restore        func(ctx context.Context, r *bkp.Restorer, opts bkp.RestoreOptions) (*bkp.RestoreResult, error)
	replace        func(ctx context.Context, r *bkp.Restorer, maintenance, database, keepAs, replacement string) error
	cleanRedis     func(ctx context.Context, cfg *config.Config) (bkp.RedisCleanupResult, error)
	check          func(ctx context.Context, cfg *config.Config, coreDir, localDir string) (selfcheck.Report, error)
	now            func() time.Time
}

func defaultRestoreEnv() restoreEnv {
	return restoreEnv{
		stdout:      os.Stdout,
		stderr:      os.Stderr,
		openStorage: bkp.OpenStorage,
		restore: func(ctx context.Context, r *bkp.Restorer, o bkp.RestoreOptions) (*bkp.RestoreResult, error) {
			return r.Restore(ctx, o)
		},
		replace: func(ctx context.Context, r *bkp.Restorer, m, db, keepAs, repl string) error {
			return r.SwitchDatabase(ctx, m, db, keepAs, repl)
		},
		cleanRedis: cleanRedis,
		check:      checkRestored,
		now:        time.Now,
	}
}

// Restore implements "elythia backup restore" and returns the exit code.
func Restore(args []string) int { return restore(defaultRestoreEnv(), args) }

func restore(e restoreEnv, args []string) int {
	fs := cliflag.New("backup restore", e.stderr)
	configPath := fs.String("config", ".config/default.yml", "path to configuration file")
	id := fs.String("id", "", `generation to restore, or "latest" for the newest verified one`)
	mode := fs.String("mode", "", "swap: restore into a new database on the same server and swap names (the current one is kept); "+
		"empty: restore into the existing empty database (a new server)")
	confirm := fs.String("confirm", "", "the database name (db.db) again, to confirm the target")
	rollback := fs.String("rollback", "", "instead of restoring, put back a database kept by an earlier swap (<db>_before_restore_<time>)")
	maint := fs.String("maintenance-db", "postgres", "database to connect to while creating and renaming databases")
	migrations := fs.String("migrations", migrate.CoreDir, "directory of the bundled migrations (its local/ subdirectory is the fork track)")
	tmpDir := fs.String("tmp-dir", "", "directory for the downloaded dump (default: the system temporary directory)")
	redisMode := fs.String("redis", redisAuto, "after switching databases: clean (delete timelines, caches and pending deliveries), keep, "+
		"or auto (clean for -mode swap and -rollback; keep for -mode empty when restoring the newest generation, otherwise ask)")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	msg := validateRestoreFlags(set, *mode)
	if msg == "" && *redisMode != redisAuto && *redisMode != redisClean && *redisMode != redisKeep {
		msg = "-redis takes auto, clean or keep"
	}
	if msg != "" {
		fmt.Fprintf(e.stderr, "elythia backup restore: %s\n", msg)
		fs.Usage()
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia backup restore: load config: %v\n", err)
		return 1
	}
	// 中断されたら ctx を取り消して、後始末 (作った DB の削除、復号した一時ファイルの
	// 削除) を走らせる。既定のシグナルの扱いのままだと、defer が走らずに終わる。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := &bkp.Restorer{
		Conn:               connFor(cfg),
		CoreMigrationsDir:  *migrations,
		LocalMigrationsDir: filepath.Join(*migrations, filepath.Base(migrate.LocalDir)),
		TempDir:            *tmpDir,
		Out:                e.stdout,
		Now:                e.now,
	}

	if *rollback != "" {
		return e.rollback(ctx, r, cfg, *rollback, *confirm, *maint, cleanRedisFor(*redisMode, bkp.RestoreSwap))
	}

	if cfg.Backup == nil {
		fmt.Fprintln(e.stderr, "elythia backup restore: the config file has no backup: section")
		return 1
	}
	storage, err := e.openStorage(cfg.Backup.Storage)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia backup restore: open storage: %v\n", err)
		return 1
	}
	r.Storage = storage
	r.PgRestore = cfg.Backup.Tools.PgRestore
	if f := cfg.Backup.Encryption.IdentityFile; f != "" {
		if r.Identities, err = bkp.LoadIdentities(f); err != nil {
			fmt.Fprintf(e.stderr, "elythia backup restore: backup.encryption.identityFile: %v\n", err)
			return 1
		}
	}

	if *redisMode == redisAuto && bkp.RestoreMode(*mode) == bkp.RestoreEmpty {
		if code := e.checkNewest(ctx, r, *id); code != 0 {
			return code
		}
	}
	res, err := e.restore(ctx, r, bkp.RestoreOptions{
		ID: *id, Mode: bkp.RestoreMode(*mode), Database: cfg.DB.DB, Confirm: *confirm, MaintenanceDB: *maint,
	})
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia backup restore: %v\n", err)
		return 1
	}
	fmt.Fprintf(e.stdout, "restored generation %s (Elythia %s, PostgreSQL %s) into %s\n",
		res.ID, res.Meta.ElythiaVersion, res.Meta.PostgresVersion, res.Database)
	code := e.afterSwitch(ctx, cfg, *migrations, cleanRedisFor(*redisMode, res.Mode))
	if res.BeforeRestore != "" {
		fmt.Fprintf(e.stdout, "the previous database is kept as %s; it is not deleted automatically.\n", res.BeforeRestore)
		fmt.Fprintf(e.stdout, "to undo: stop the server and run `elythia backup restore -rollback %s -confirm %s`\n", res.BeforeRestore, res.Database)
	}
	return code
}

// validateRestoreFlags returns a message for an invalid combination, or "".
func validateRestoreFlags(set map[string]bool, mode string) string {
	if !set["confirm"] {
		return "-confirm <database name> is required"
	}
	if set["rollback"] {
		if set["id"] || set["mode"] {
			return "-rollback cannot be combined with -id or -mode"
		}
		return ""
	}
	if !set["id"] {
		return `-id <generation id> or -id latest is required`
	}
	// 既定の形を置かない。どちらの形でも DB を書き換えるので、打った人に選ばせる。
	if mode != string(bkp.RestoreSwap) && mode != string(bkp.RestoreEmpty) {
		return "-mode swap or -mode empty is required"
	}
	return ""
}

// rollback swaps a kept database back in.
func (e restoreEnv) rollback(ctx context.Context, r *bkp.Restorer, cfg *config.Config, kept, confirm, maint string, clean bool) int {
	db := cfg.DB.DB
	if confirm != db {
		fmt.Fprintf(e.stderr, "elythia backup restore: %v: pass -confirm %s\n", bkp.ErrRestoreNotConfirmed, db)
		return 1
	}
	// 退避した DB の名前の形だけを受ける。打ち間違えた名前で、無関係な DB を
	// <DB名> として入れ替えないため。_rolled_back_ は、ロールバックで脇へ置いた
	// DB (ロールバックの取り消し)。
	if !strings.HasPrefix(kept, db+"_before_restore_") && !strings.HasPrefix(kept, db+"_rolled_back_") {
		fmt.Fprintf(e.stderr, "elythia backup restore: -rollback takes a database kept by a swap or a rollback (%s_before_restore_<time> or %s_rolled_back_<time>), got %q\n", db, db, kept)
		return 1
	}
	// 戻した DB は消さずに別名で残す。ロールバックそのものを取り消せるようにする。
	asideName := db + "_rolled_back_" + e.now().UTC().Format("20060102150405")
	if err := e.replace(ctx, r, maint, db, asideName, kept); err != nil {
		fmt.Fprintf(e.stderr, "elythia backup restore: %v\n", err)
		return 1
	}
	fmt.Fprintf(e.stdout, "%s is back as %s; the restored database is kept as %s\n", kept, db, asideName)
	return e.afterSwitch(ctx, cfg, r.CoreMigrationsDir, clean)
}

// checkNewest lets -redis auto keep Redis in the empty mode only when the
// generation to restore is the newest one in the storage.
//
// Redis を残してよいのは、戻す DB が Redis と同じ時点のとき (止めてから取った最後の
// 世代) だけ。古い世代を戻すと、Redis の方が新しいまま残り、戻した DB に無い投稿の
// 配送や、古い数に足されるリアクション数の差分が出る。どちらか分からなければ、
// 打った人に -redis を選ばせる。
func (e restoreEnv) checkNewest(ctx context.Context, r *bkp.Restorer, id string) int {
	target, _, err := r.ResolveID(ctx, id)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia backup restore: %v\n", err)
		return 1
	}
	newest, err := bkp.NewestGeneration(ctx, r.Storage)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia backup restore: %v\n", err)
		return 1
	}
	if target != newest {
		fmt.Fprintf(e.stderr, "elythia backup restore: generation %s is not the newest generation in the storage (%s), so redis may hold newer data than the backup; "+
			"pass -redis clean to delete the timelines, caches and pending deliveries, or -redis keep if redis is as old as the backup\n", target, newest)
		return 1
	}
	return 0
}

// Values of -redis.
const (
	redisAuto  = "auto"
	redisClean = "clean"
	redisKeep  = "keep"
)

// cleanRedisFor reports whether the Redis cleanup runs for -redis value v
// and the restore mode.
//
// **空の DB へ戻す形では、既定で Redis を消さない。** この形は版を上げるときと
// 引っ越すときに使い、本体を止めてから取った最後の世代を戻すので、戻した DB は
// 止めた時点の DB そのもの (最新の世代であることは checkNewest が確かめる)。Redis に残る配送待ちの job (届いていない Delete など)、
// DB へ未反映のリアクション数 (reaction-buffer) は、戻した DB と食い違わず、消すと
// 失うだけになる。DB を失って古い世代から作り直すときは -redis clean を渡す。
func cleanRedisFor(v string, mode bkp.RestoreMode) bool {
	switch v {
	case redisClean:
		return true
	case redisKeep:
		return false
	}
	return mode != bkp.RestoreEmpty
}

// afterSwitch clears Redis (when clean) and runs the checks once the
// server's database name points at a different database.
func (e restoreEnv) afterSwitch(ctx context.Context, cfg *config.Config, migrations string, clean bool) int {
	if clean {
		if code := e.clearRedis(ctx, cfg); code != 0 {
			return code
		}
	} else {
		fmt.Fprintln(e.stdout, "kept redis as it is (-redis keep, or -mode empty); pass -redis clean if the backup is older than the data in redis")
	}
	report, err := e.check(ctx, cfg, migrations, filepath.Join(migrations, filepath.Base(migrate.LocalDir)))
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia backup restore: check: %v\n", err)
		return 1
	}
	for _, res := range report.Results {
		fmt.Fprintf(e.stdout, "  %-5s %-12s %s\n", res.Status, res.Name, res.Detail)
		if res.Hint != "" && res.Status == selfcheck.StatusFail {
			fmt.Fprintf(e.stdout, "        %s\n", res.Hint)
		}
	}
	if !report.OK {
		fmt.Fprintln(e.stderr, "elythia backup restore: the restored database failed the checks")
		return 1
	}
	fmt.Fprintln(e.stdout, "start the server, then run `elythia doctor` to check federation as well.")
	return 0
}

// clearRedis runs the Redis cleanup and returns the exit code (0 on success).
func (e restoreEnv) clearRedis(ctx context.Context, cfg *config.Config) int {
	cleaned, err := e.cleanRedis(ctx, cfg)
	if err != nil {
		// DB はもう切り替わっている。Redis が古いまま本体を起動すると、戻した DB に無い
		// 投稿をタイムラインに出すので、起動する前に消すよう案内する。
		fmt.Fprintf(e.stderr, "elythia backup restore: the database is already switched, but clearing redis failed: %v\n", err)
		fmt.Fprintln(e.stderr, "elythia backup restore: do not start the server until the keys in docs/deployment.md \"Redisの後始末\" are cleared")
		return 1
	}
	fmt.Fprintf(e.stdout, "cleared %d redis keys (timelines, caches, pending deliveries)\n", cleaned.Total())
	return 0
}

// connFor builds the connection strings for another database name on the
// configured server.
func connFor(cfg *config.Config) bkp.RestoreConn {
	with := func(db string) *config.Config {
		c := *cfg
		c.DB.DB = db
		return &c
	}
	return bkp.RestoreConn{
		SQL:     func(db string) string { return with(db).DSN() },
		Migrate: func(db string) string { return with(db).DatabaseURL("pgx5") },
		Tool:    func(db string) (string, []string) { return toolConn(with(db).DatabaseURL("postgresql")) },
	}
}

// toolConn moves the password of a libpq URL into PGPASSWORD. プロセスの
// 引数は同じホストの他の利用者から見えるので、パスワードを --dbname に入れない。
func toolConn(raw string) (string, []string) {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw, nil
	}
	pass, ok := u.User.Password()
	if !ok {
		return raw, nil
	}
	u.User = url.User(u.User.Username())
	return u.String(), []string{"PGPASSWORD=" + pass}
}

func cleanRedis(ctx context.Context, cfg *config.Config) (bkp.RedisCleanupResult, error) {
	redislog.UseSilent()
	clients, err := cache.NewRedisClients(cfg)
	if err != nil {
		return bkp.RedisCleanupResult{}, err
	}
	defer func() { _ = clients.Close() }()
	return bkp.CleanRedisAfterRestore(ctx, bkp.RedisCleanupTargets{
		Default:   bkp.RedisCleanupTarget{Client: clients.Default, Prefix: cfg.Redis.KeyPrefix()},
		Timelines: bkp.RedisCleanupTarget{Client: clients.Timelines, Prefix: cfg.RedisForTimelines.KeyPrefix()},
		JobQueue:  bkp.RedisCleanupTarget{Client: clients.JobQueue},
	})
}

func checkRestored(ctx context.Context, cfg *config.Config, coreDir, localDir string) (selfcheck.Report, error) {
	db, err := gorm.Open(postgres.Open(cfg.DSN()), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		return selfcheck.Report{}, err
	}
	if sqlDB, err := db.DB(); err == nil {
		defer sqlDB.Close()
	}
	clients, err := cache.NewRedisClients(cfg)
	if err != nil {
		return selfcheck.Report{}, errors.Join(errors.New("connect to redis"), err)
	}
	defer func() { _ = clients.Close() }()
	return bkp.CheckRestored(ctx, bkp.RestoreCheckDeps{
		DB: db, Redis: clients.Default, CoreMigrationsDir: coreDir, LocalMigrationsDir: localDir,
	})
}
