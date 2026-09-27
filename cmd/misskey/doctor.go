package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/core/selfcheck"
	"github.com/shiroha-a/mk/internal/redislog"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// doctorTimeout bounds the whole run so a hung dependency does not leave the
// operator staring at a blank terminal.
const doctorTimeout = 60 * time.Second

// migrationsDir mirrors what cmd/migrate passes to golang-migrate
// (`file://migration`). 同じ場所を数えないと「適用漏れ」の判定がずれる。
const migrationsDir = "migration"

// runDoctor performs the self-check and prints a report. Returns the process
// exit code.
//
// **サーバーが起動していなくても回せる**ことが要点。config / DB / Redis の検査は
// 単体で成立し、連合の検査だけが「公開 URL に届くか」に依存する。新規構築時は
// まずここまでで詰まりを潰せる。
func runDoctor(configPath string) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: 設定を読めない: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()

	// go-redis は接続失敗を内部 logger で stderr に吐く。検査結果の表が
	// 埋もれるので黙らせる (失敗は Result 側で報告する)。
	redislog.UseSilent()

	deps := selfcheck.LocalDeps{MigrationCount: countMigrations()}
	db, dbErr := openDoctorDB(cfg)
	if dbErr != nil {
		deps.DBErr = dbErr
	} else {
		deps.DB = db
		defer closeDoctorDB(db)
	}
	if rdb := openDoctorRedis(cfg); rdb != nil {
		deps.Redis = rdb
		defer func() { _ = rdb.Close() }()
	}

	report := selfcheck.Run(ctx, selfcheck.NewChecker(cfg.URL), deps)
	printReport(report)
	if !report.OK {
		return 1
	}
	return 0
}

// countMigrations counts the bundled up migrations. 数えられなければ 0 を返し、
// 「適用漏れ」の比較だけを飛ばす (接続や dirty の判定は残る)。
func countMigrations() int {
	matches, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil {
		return 0
	}
	return len(matches)
}

// openDoctorDB dials PostgreSQL with the same settings the server uses.
// 失敗しても検査は続ける (理由が DBErr 経由で DB の検査結果に載る)。
func openDoctorDB(cfg *config.Config) (*gorm.DB, error) {
	// 本体と同じ config.DSN() を使う。以前は sslmode=disable を直書きしており、
	// db.extra.ssl を設定した環境では doctor だけが平文で繋いでいた。
	return gorm.Open(postgres.Open(cfg.DSN()), &gorm.Config{
		// doctor の出力に gorm のログを混ぜない。読むのは検査結果の表だけ。
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
}

func closeDoctorDB(db *gorm.DB) {
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// openDoctorRedis dials Redis. nil を返したら検査は skip になる。
func openDoctorRedis(cfg *config.Config) redis.UniversalClient {
	if cfg.Redis.Host == "" {
		return nil
	}
	return redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%d", cfg.Redis.Host, cfg.Redis.Port),
		Password: cfg.Redis.Pass,
		Username: cfg.Redis.Username,
		DB:       cfg.Redis.DB,
	})
}

// statusMark renders a status for a terminal. 色は付けない (ログへリダイレクト
// されたときに制御文字が混ざる)。
func statusMark(s selfcheck.Status) string {
	switch s {
	case selfcheck.StatusOK:
		return "ok  "
	case selfcheck.StatusWarn:
		return "warn"
	case selfcheck.StatusFail:
		return "FAIL"
	default:
		return "skip"
	}
}

// printReport writes the human-facing table.
func printReport(r selfcheck.Report) {
	fmt.Println()
	for _, res := range r.Results {
		fmt.Printf("  %s  %-12s %s\n", statusMark(res.Status), res.Name, res.Detail)
		// hint は失敗したときだけ出す。全部出すと読むべき行が埋もれる。
		if res.Hint != "" && (res.Status == selfcheck.StatusFail || res.Status == selfcheck.StatusWarn) {
			fmt.Printf("        %s\n", res.Hint)
		}
	}
	fmt.Println()
	if r.OK {
		fmt.Println("  問題は見つかりませんでした。")
	} else {
		fmt.Println("  FAIL の項目があります。上のヒントを参照してください。")
	}
	fmt.Println()
}
