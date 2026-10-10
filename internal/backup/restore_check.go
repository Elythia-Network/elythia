package backup

import (
	"context"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/core/selfcheck"
)

// RestoreCheckDeps are what CheckRestored inspects.
type RestoreCheckDeps struct {
	// DB is connected to the restored database under the server's name.
	DB    *gorm.DB
	Redis redis.UniversalClient
	// CoreMigrationsDir and LocalMigrationsDir are the bundled migrations.
	CoreMigrationsDir  string
	LocalMigrationsDir string
}

// CheckRestored runs the checks of `elythia doctor` that do not need the
// server to be running: the migration state, the root user and Redis.
//
// 連合の検査 (WebFinger / NodeInfo / actor / TLS) は、公開の URL に本体が応答して
// いないと通らないので、ここでは流さない。起動した後に `elythia doctor` を流す。
func CheckRestored(ctx context.Context, deps RestoreCheckDeps) (selfcheck.Report, error) {
	_, count, err := latestMigration(deps.CoreMigrationsDir)
	if err != nil {
		return selfcheck.Report{}, err
	}
	local, _, err := latestMigration(deps.LocalMigrationsDir)
	if err != nil {
		return selfcheck.Report{}, err
	}
	ld := selfcheck.LocalDeps{DB: deps.DB, Redis: deps.Redis, MigrationCount: count, LocalMigrationLatest: int64(local)}
	results := []selfcheck.Result{
		selfcheck.CheckDatabase(ctx, ld),
		selfcheck.CheckRootUser(ctx, ld),
		selfcheck.CheckRedis(ctx, ld),
	}
	report := selfcheck.Report{Results: results, OK: true}
	for _, r := range results {
		if r.Status == selfcheck.StatusFail {
			report.OK = false
		}
	}
	return report, nil
}
