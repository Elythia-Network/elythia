// Package diag implements the elythia subcommands that inspect an instance
// without starting the server: doctor, fsck, config-dump and healthcheck.
package diag

import (
	"context"
	"fmt"
	"io"
	"os"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/elythia-network/elythia/internal/cli/cliflag"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/fsck"
	"github.com/elythia-network/elythia/internal/redislog"
)

// defaultConfigPath is the -config default these subcommands have always used.
const defaultConfigPath = ".config/default.yml"

// env carries the process-level dependencies so tests can replace them.
//
// DB への接続と go-redis のロガー差し替えはプロセス全体に効くので、テストからは
// 差し替えて触らないようにする。
type env struct {
	stdout, stderr io.Writer
	openDB         func(*config.Config) (*gorm.DB, error)
	closeDB        func(*gorm.DB)
	silenceRedis   func()
	migrationsDir  string
	// runFsck runs the counter check; fsck.Run outside tests.
	runFsck func(ctx context.Context, db *gorm.DB, opts fsck.Options) (fsck.Report, error)
}

func defaultEnv() env {
	return env{
		stdout:        os.Stdout,
		stderr:        os.Stderr,
		openDB:        openDB,
		closeDB:       closeDB,
		silenceRedis:  redislog.UseSilent,
		migrationsDir: migrationsDir,
		runFsck:       fsck.Run,
	}
}

// configFlag registers the -config flag shared by every subcommand here.
func configFlag(name string, w io.Writer) (*string, func([]string) (int, bool)) {
	fs := cliflag.New(name, w)
	path := fs.String("config", defaultConfigPath, "path to configuration file")
	return path, func(args []string) (int, bool) { return cliflag.Parse(fs, args) }
}

// openDB dials PostgreSQL with the same settings the server uses.
// 失敗しても doctor は検査を続ける (理由が DBErr 経由で DB の検査結果に載る)。
func openDB(cfg *config.Config) (*gorm.DB, error) {
	// 本体と同じ config.DSN() を使う。以前は sslmode=disable を直書きしており、
	// db.extra.ssl を設定した環境では doctor だけが平文で繋いでいた。
	return gorm.Open(postgres.Open(cfg.DSN()), &gorm.Config{
		// doctor の出力に gorm のログを混ぜない。読むのは検査結果の表だけ。
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
}

func closeDB(db *gorm.DB) {
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// loadConfig loads the configuration and reports a failure in the
// subcommand's own words.
func loadConfig(e env, name, path string) (*config.Config, bool) {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(e.stderr, "%s: 設定を読めない: %v\n", name, err)
		return nil, false
	}
	return cfg, true
}
