package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/shiroha-a/mk/internal/config"
)

func main() {
	configPath := flag.String("config", ".config/default.yml", "path to configuration file")
	direction := flag.String("direction", "up", "migration direction: up or down")
	steps := flag.Int("steps", 0, "number of steps (0 = all)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
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
	dbURL := cfg.DatabaseURL("pgx5")

	m, err := migrate.New("file://migration", dbURL)
	if err != nil {
		logDBError(cfg, "failed to create migrator", err)
		os.Exit(1)
	}
	defer m.Close()

	switch *direction {
	case "up":
		if *steps > 0 {
			err = m.Steps(*steps)
		} else {
			err = m.Up()
		}
	case "down":
		if *steps > 0 {
			err = m.Steps(-*steps)
		} else {
			err = m.Down()
		}
	default:
		slog.Error("invalid direction", "direction", *direction)
		os.Exit(1)
	}

	if err != nil && err != migrate.ErrNoChange {
		logDBError(cfg, "migration failed", err)
		os.Exit(1)
	}

	if err == migrate.ErrNoChange {
		slog.Info("no migration changes to apply")
	} else {
		slog.Info("migration completed", "direction", *direction)
	}
}

// logDBError logs a DB error, adding the TLS remediation hint when the
// failure was a certificate verification error.
func logDBError(cfg *config.Config, msg string, err error) {
	if hint := cfg.DBTLSErrorHint(err); hint != "" {
		slog.Error(msg, "error", err, "hint", hint)
		return
	}
	slog.Error(msg, "error", err)
}
