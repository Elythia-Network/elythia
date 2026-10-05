// Package backfill implements "elythia backfill <name>", the one-off
// maintenance batches that rewrite rows stored by older builds.
//
// どのバッチも冪等で、途中で失敗しても再実行して安全。書き込みの既定はバッチごとに
// 違う (emoji-system-file だけが既定で dry-run)。手順は docs/deployment.md にある。
package backfill

import (
	"context"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/shiroha-a/mk/internal/cli/cliflag"
	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/maintenance"
)

// defaultConfigPath is the -config default the batches have always used: the
// path inside the container image.
const defaultConfigPath = "/app/.config/default.yml"

// env carries the process-level dependencies so tests can replace them.
//
// バッチ 1 回分の SQL は maintenance パッケージが実 DB に対してテストしている。
// ここで差し替えるのは、カーソルの進め方・ログ・終了コードといったバッチを回す側の
// 振る舞いを確かめるため。
type env struct {
	// stderr receives flag errors, usage and the emoji-system-file report.
	stderr io.Writer
	// logger replaces the standard logger the former binaries wrote to.
	logger        *log.Logger
	loadConfig    func(path string) (*config.Config, error)
	openDB        func(cfg *config.Config) (*gorm.DB, error)
	sleep         func(time.Duration)
	signalContext func() (context.Context, context.CancelFunc)

	avatarBatch    func(db *gorm.DB, fromID string, batchSize int, dryRun bool) (maintenance.AvatarPublicURLBackfillResult, error)
	instanceBatch  func(db *gorm.DB, fromID string, batchSize int, dryRun bool) (maintenance.InstanceCountsBackfillResult, error)
	noteTagsBatch  func(db *gorm.DB, fromID string, batchSize int, dryRun bool) (maintenance.NoteTagsBackfillResult, error)
	hostBatch      func(db *gorm.DB, col maintenance.HostColumn, fromKey string, batchSize int, dryRun bool) (maintenance.HostBackfillResult, error)
	emojiBackfill  func(ctx context.Context, db *gorm.DB, copier maintenance.SystemFileCopier, opts maintenance.EmojiSystemFileBackfillOptions) (maintenance.EmojiSystemFileBackfillResult, error)
	hostColumnList []maintenance.HostColumn
}

func defaultEnv() env {
	return env{
		stderr: os.Stderr,
		// 以前の各バイナリは標準の log パッケージ (stderr、日時付き) に書いていた。
		logger:     log.New(os.Stderr, "", log.LstdFlags),
		loadConfig: config.Load,
		openDB: func(cfg *config.Config) (*gorm.DB, error) {
			return gorm.Open(postgres.Open(cfg.DSN()), &gorm.Config{})
		},
		sleep: time.Sleep,
		signalContext: func() (context.Context, context.CancelFunc) {
			return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		},
		avatarBatch:    maintenance.BackfillAvatarPublicURLBatch,
		instanceBatch:  maintenance.BackfillInstanceCountsBatch,
		noteTagsBatch:  maintenance.BackfillNoteTagsBatch,
		hostBatch:      maintenance.BackfillHostColumnBatch,
		emojiBackfill:  maintenance.BackfillEmojiSystemFiles,
		hostColumnList: maintenance.HostColumns,
	}
}

// newFlags returns the flag set for "elythia backfill <name>" with the -config
// flag every batch takes.
func newFlags(e env, name string) (*flag.FlagSet, *string) {
	fs := cliflag.New("backfill "+name, e.stderr)
	path := fs.String("config", defaultConfigPath, "path to configuration file")
	return fs, path
}

// open loads the configuration and connects to PostgreSQL, logging the cause
// and reporting false on failure.
func open(e env, path string) (*config.Config, *gorm.DB, bool) {
	cfg, err := e.loadConfig(path)
	if err != nil {
		e.logger.Printf("load config: %v", err)
		return nil, nil, false
	}
	db, err := e.openDB(cfg)
	if err != nil {
		e.logger.Printf("open db: %v", err)
		return nil, nil, false
	}
	return cfg, db, true
}

// pause sleeps between batches to limit the load on the database.
func pause(e env, sleepMs int) {
	if sleepMs > 0 {
		e.sleep(time.Duration(sleepMs) * time.Millisecond)
	}
}

// modeLabel names the run in the final summary line.
func modeLabel(dryRun bool, dryLabel string) string {
	if dryRun {
		return dryLabel
	}
	return "applied"
}
