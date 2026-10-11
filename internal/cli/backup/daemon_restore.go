package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	backupkit "github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/backup/daemon"
	"github.com/elythia-network/elythia/internal/backup/restoreop"
	"github.com/elythia-network/elythia/internal/cli/migrate"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/core/passwordguard"
	"github.com/elythia-network/elythia/internal/core/selfcheck"
	"github.com/elythia-network/elythia/internal/core/strongauth"
	"github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/maintmode"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/redislog"
	"github.com/elythia-network/elythia/internal/repository"
)

// RestoreParts are what the daemon needs to restore from the admin page
// (#3463 (6c)).
type RestoreParts struct {
	Manager  *restoreop.Manager
	Verifier *strongauth.DBVerifier
	// Close releases the Redis clients.
	Close func()
}

// daemonID names the backup service in the judge lock and in
// application_name. 起動ごとの乱数を付ける (本体の processID と同じ理由)。
func daemonID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	if len(host) > 24 {
		host = host[:24]
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}

// heldGrace is added to maintenance.drainTimeout for how long a restore
// waits for the maintenance to be held (設計 §8-3)。
const heldGrace = 2 * time.Minute

// openRestore builds the restore with the bundled migrations of the image.
func openRestore(cfg *config.Config, d *daemon.Daemon, storage backupkit.Storage, logger *slog.Logger) (*RestoreParts, error) {
	return restoreWiring{coreDir: migrate.CoreDir, localDir: migrate.LocalDir, heldGrace: heldGrace}.open(cfg, d, storage, logger)
}

// restoreWiring are the parts of the restore that tests replace.
type restoreWiring struct {
	coreDir, localDir string
	heldGrace         time.Duration
}

// open builds the restore manager. d gives it the job slot; storage and the
// backup: section are the ones the daemon already uses.
//
// daemon の接続は、メンテナンス用の DB と、戻す先の新しい DB だけに張る。本体の
// DB 名へは、再認証とモデレーションログの短い接続だけ (どちらも名前を入れ替える
// 前後には張らない)。application_name は elythia/ で始めない。判定役が切る対象に
// しないため (切られると照合が途中で落ちる)。
func (w restoreWiring) open(cfg *config.Config, d *daemon.Daemon, storage backupkit.Storage, logger *slog.Logger) (*RestoreParts, error) {
	coreDir, localDir := w.coreDir, w.localDir
	redislog.UseSilent()
	clients, err := cache.NewRedisClients(cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to redis: %w", err)
	}
	selfID := daemonID()
	prefix := cfg.Redis.KeyPrefix()
	maint := maintmode.NewStore(clients.Default, clients.Pubsub, prefix)
	drain := cfg.Maintenance.DrainTimeout
	if drain <= 0 {
		drain = config.DefaultMaintenanceDrainTimeout
	}
	judge := &maintmode.Judge{
		Store:        maint,
		Counter:      &maintmode.PGCounter{Targets: maintmode.TargetsFromConfig(cfg, "elythia-backup/judge/"+selfID)},
		ID:           "backup-daemon/" + selfID,
		DrainTimeout: drain,
	}
	r := newRestorer(withAppName(cfg, "elythia-backup/restore/"+selfID), coreDir, "", logWriter{logger}, time.Now)
	r.LocalMigrationsDir = localDir
	if err := withBackupTools(r, cfg.Backup, storage); err != nil {
		_ = clients.Close()
		return nil, err
	}
	// 本体と同じく、設定の id が読めなければ既定の aidx で作る (router.go と同じ)。
	idGen, err := id.NewGenerator(cfg.ID)
	if err != nil {
		idGen, _ = id.NewGenerator("aidx")
	}
	manager := restoreop.New(restoreop.Options{
		Store: restoreop.NewStore(clients.Default, prefix),
		DB:    r,
		Maint: maint,
		RunJudge: func(ctx context.Context) {
			judge.Run(ctx, maintmode.PollInterval)
		},
		CleanRedis: func(ctx context.Context) error {
			_, err := backupkit.CleanRedisAfterRestore(ctx, redisCleanupTargets(cfg, clients))
			return err
		},
		Check: func(ctx context.Context) error {
			report, err := checkRestored(ctx, withAppName(cfg, "elythia-backup/check/"+selfID), coreDir, localDir)
			if err != nil {
				return err
			}
			return reportError(report)
		},
		ModLog: func(ctx context.Context, database, userID string, info map[string]any) error {
			return writeModLog(ctx, cfg, selfID, idGen, database, userID, info)
		},
		Acquire:       func() (func(), error) { return d.Acquire(daemon.JobRestore) },
		SlotBusy:      d.SlotBusy,
		Recovering:    d.SetRecovering,
		Notify:        d.NotifyRestoreStopped,
		Database:      cfg.DB.DB,
		MaintenanceDB: cfg.Maintenance.MaintenanceDB,
		Replicas:      cfg.DBReplications,
		Version:       config.MkGoVersion,
		Commit:        config.MkGoCommit,
		HeldTimeout:   drain + w.heldGrace,
		Logger:        logger,
	})
	verifier := &strongauth.DBVerifier{
		Open: func(ctx context.Context) (*gorm.DB, func(), error) {
			return openOnce(ctx, withAppName(cfg, "elythia-backup/auth/"+selfID))
		},
		IDGen:  idGen,
		Guard:  passwordguard.NewRedisGuard(clients.Default),
		Replay: twofactor.NewRedisReplayGuard(clients.Default),
	}
	if wa, err := twofactor.NewWebAuthnService(cfg.URL, config.DisplayName, clients.Default); err == nil {
		verifier.Passkeys = wa
	} else {
		logger.Warn("backup: passkeys are not available for the restore from the admin page", "err", err)
	}
	return &RestoreParts{Manager: manager, Verifier: verifier, Close: func() { _ = clients.Close() }}, nil
}

// withAppName returns cfg with application_name set for the connections.
func withAppName(cfg *config.Config, name string) *config.Config {
	c := *cfg
	c.DBApplicationName = name
	return &c
}

// openOnce opens a pool of at most two connections that keeps none idle,
// for one short use.
func openOnce(ctx context.Context, cfg *config.Config) (*gorm.DB, func(), error) {
	db, err := gorm.Open(postgres.Open(cfg.DSN()), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		return nil, nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	sqlDB.SetMaxOpenConns(2)
	sqlDB.SetMaxIdleConns(0)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, nil, err
	}
	return db.WithContext(ctx), func() { _ = sqlDB.Close() }, nil
}

// writeModLog records a restore in the moderation log of database. It
// writes synchronously (moderationlog.Service.Log is asynchronous) so that
// the connection is closed before the daemon goes on.
func writeModLog(ctx context.Context, cfg *config.Config, selfID string, idGen id.Generator, database, userID string, info map[string]any) error {
	c := withAppName(cfg, "elythia-backup/modlog/"+selfID)
	c.DB.DB = database
	db, closeDB, err := openOnce(ctx, c)
	if err != nil {
		return err
	}
	defer closeDB()
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return repository.NewModerationLogRepository(db).Create(&model.ModerationLog{
		ID: idGen.Generate(time.Now()), UserID: userID, Type: string(moderationlog.LogRestoreBackup), Info: datatypes.JSON(b),
	})
}

// reportError turns a failed check report into an error.
func reportError(r selfcheck.Report) error {
	if r.OK {
		return nil
	}
	var failed []string
	for _, res := range r.Results {
		if res.Status == selfcheck.StatusFail {
			failed = append(failed, res.Name+": "+res.Detail)
		}
	}
	return errors.New("the restored database failed the checks: " + strings.Join(failed, "; "))
}

// logWriter sends the restorer's progress lines to the logger.
type logWriter struct{ log *slog.Logger }

func (w logWriter) Write(p []byte) (int, error) {
	for _, line := range bytes.Split(bytes.TrimRight(p, "\n"), []byte("\n")) {
		if len(line) > 0 {
			w.log.Info("backup: restore: " + string(line))
		}
	}
	return len(p), nil
}
