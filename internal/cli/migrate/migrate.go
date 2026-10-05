// Package migrate implements "elythia migrate", which applies or rolls back
// the SQL migrations under migration/.
package migrate

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	gomigrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/shiroha-a/mk/internal/cli/cliflag"
	"github.com/shiroha-a/mk/internal/config"
)

// sourceURL is where golang-migrate reads the migrations from, relative to the
// working directory. `elythia doctor` counts the same directory.
const sourceURL = "file://migration"

// migrator is the part of *gomigrate.Migrate this command drives.
type migrator interface {
	Up() error
	Down() error
	Steps(n int) error
	Close() (source error, database error)
}

// env carries the process-level dependencies so tests can replace them.
type env struct {
	stdout io.Writer
	// setLogger installs the process-wide slog logger. テストでは slog.Default を
	// 張り替えないように差し替える (-shuffle で後続のテストに漏れるため)。
	setLogger func(*slog.Logger)
	open      func(sourceURL, databaseURL string) (migrator, error)
}

func defaultEnv() env {
	return env{
		stdout:    os.Stdout,
		setLogger: slog.SetDefault,
		open: func(src, db string) (migrator, error) {
			return gomigrate.New(src, db)
		},
	}
}

// Run implements "elythia migrate" and returns the process exit code. The
// flags (-config, -direction, -steps) are the ones the former migrate binary
// accepted.
func Run(args []string) int { return run(defaultEnv(), os.Stderr, args) }

func run(e env, flagOut io.Writer, args []string) int {
	fs := cliflag.New("migrate", flagOut)
	configPath := fs.String("config", ".config/default.yml", "path to configuration file")
	direction := fs.String("direction", "up", "migration direction: up or down")
	steps := fs.Int("steps", 0, "number of steps (0 = all)")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	// **DB に繋ぐ前に flag の値を検査する。** 以前は不正な -direction を接続の後で
	// 弾いていた。-steps の負の値は「0 より大きくない」ので「全部」と同じ扱いになり、
	// `-direction down -steps -1` が全段の down (全テーブルが消える) になっていた。
	if *direction != "up" && *direction != "down" {
		fmt.Fprintf(flagOut, "elythia migrate: invalid -direction %q (want up or down)\n", *direction)
		fs.Usage()
		return 2
	}
	if *steps < 0 {
		fmt.Fprintf(flagOut, "elythia migrate: -steps must be 0 or greater, got %d\n", *steps)
		fs.Usage()
		return 2
	}

	logger := slog.New(slog.NewTextHandler(e.stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	e.setLogger(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		return 1
	}

	// scheme は pgx5 (golang-migrate の pgx/v5 driver)。lib/pq を使う
	// `postgres` driver は使わない (#2628: GO-2026-6173 に修正版が無く、
	// 依存を残すと govulncheck が通らない)。driver 側が接続直前に scheme を
	// `postgres` へ書き戻して `sql.Open("pgx/v5", ...)` するので、**DSN の形は
	// libpq 互換のまま**でよい。pgx の ParseConfig も libpq 互換なので UDS の
	// 書き方も変わらない。
	// DSN の組み立て (TLS 設定・資格情報のエスケープ・UDS の扱い) は本体と
	// 共通の config.DatabaseURL に任せる。以前はここで独自に組んでおり、
	// db.extra.ssl を見ずに常に sslmode=disable で繋ぎ、TCP 経路では
	// パスワードをエスケープせずに URL へ埋めていた。
	m, err := e.open(sourceURL, cfg.DatabaseURL("pgx5"))
	if err != nil {
		logDBError(logger, cfg, "failed to create migrator", err)
		return 1
	}
	defer m.Close()

	switch {
	case *direction == "up" && *steps > 0:
		err = m.Steps(*steps)
	case *direction == "up":
		err = m.Up()
	case *steps > 0:
		err = m.Steps(-*steps)
	default:
		err = m.Down()
	}

	if errors.Is(err, gomigrate.ErrNoChange) {
		logger.Info("no migration changes to apply")
		return 0
	}
	if err != nil {
		logDBError(logger, cfg, "migration failed", err)
		return 1
	}
	logger.Info("migration completed", "direction", *direction)
	return 0
}

// logDBError logs a DB error, adding the TLS remediation hint when the
// failure was a certificate verification error.
func logDBError(logger *slog.Logger, cfg *config.Config, msg string, err error) {
	if hint := cfg.DBTLSErrorHint(err); hint != "" {
		logger.Error(msg, "error", err, "hint", hint)
		return
	}
	logger.Error(msg, "error", err)
}
