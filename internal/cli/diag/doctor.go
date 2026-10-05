package diag

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/core/dbhealth"
	"github.com/shiroha-a/mk/internal/core/selfcheck"
)

// doctorTimeout bounds the whole run so a hung dependency does not leave the
// operator staring at a blank terminal.
const doctorTimeout = 60 * time.Second

// migrationsDir mirrors what "elythia migrate" passes to golang-migrate
// (`file://migration`). 同じ場所を数えないと「適用漏れ」の判定がずれる。
const migrationsDir = "migration"

// Doctor implements "elythia doctor": it runs the configuration / dependency /
// federation self-checks, prints a report and returns 0 (ok) or 1 (failures).
func Doctor(args []string) int { return doctor(defaultEnv(), args) }

// doctor is Doctor with its dependencies passed in.
//
// **サーバーが起動していなくても回せる**ことが要点。config / DB / Redis の検査は
// 単体で成立し、連合の検査だけが「公開 URL に届くか」に依存する。新規構築時は
// まずここまでで詰まりを潰せる (#2463)。
func doctor(e env, args []string) int {
	path, parse := configFlag("doctor", e.stderr)
	if code, ok := parse(args); !ok {
		return code
	}
	cfg, ok := loadConfig(e, "doctor", *path)
	if !ok {
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()

	// go-redis は接続失敗を内部 logger で stderr に吐く。検査結果の表が
	// 埋もれるので黙らせる (失敗は Result 側で報告する)。
	e.silenceRedis()

	deps := selfcheck.LocalDeps{MigrationCount: countMigrations(e.migrationsDir)}
	db, dbErr := e.openDB(cfg)
	if dbErr != nil {
		deps.DBErr = dbErr
	} else {
		deps.DB = db
		deps.DBHealth = dbhealth.NewService(db, cfg.DBReplications && len(cfg.DBSlaves) > 0).Report
		defer e.closeDB(db)
	}
	if rdb := openDoctorRedis(cfg); rdb != nil {
		deps.Redis = rdb
		defer func() { _ = rdb.Close() }()
	}

	report := selfcheck.Run(ctx, selfcheck.NewChecker(cfg.URL), deps)
	printReport(e.stdout, report)
	if !report.OK {
		return 1
	}
	return 0
}

// countMigrations counts the bundled up migrations. 数えられなければ 0 を返し、
// 「適用漏れ」の比較だけを飛ばす (接続や dirty の判定は残る)。
func countMigrations(dir string) int {
	matches, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		return 0
	}
	return len(matches)
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
func printReport(w io.Writer, r selfcheck.Report) {
	fmt.Fprintln(w)
	for _, res := range r.Results {
		fmt.Fprintf(w, "  %s  %-12s %s\n", statusMark(res.Status), res.Name, res.Detail)
		// hint は失敗したときだけ出す。全部出すと読むべき行が埋もれる。
		if res.Hint != "" && (res.Status == selfcheck.StatusFail || res.Status == selfcheck.StatusWarn) {
			fmt.Fprintf(w, "        %s\n", res.Hint)
		}
	}
	fmt.Fprintln(w)
	if r.OK {
		fmt.Fprintln(w, "  問題は見つかりませんでした。")
	} else {
		fmt.Fprintln(w, "  FAIL の項目があります。上のヒントを参照してください。")
	}
	fmt.Fprintln(w)
}
