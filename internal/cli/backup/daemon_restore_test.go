package backup

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/backup/restoreop"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/core/selfcheck"
	"github.com/elythia-network/elythia/internal/testutil"
)

func TestReportError(t *testing.T) {
	assert.NoError(t, reportError(selfcheck.Report{OK: true}))
	err := reportError(selfcheck.Report{Results: []selfcheck.Result{
		{Name: "db", Status: selfcheck.StatusOK},
		{Name: "root", Status: selfcheck.StatusFail, Detail: "no root user"},
	}})
	require.Error(t, err)
	assert.Equal(t, "the restored database failed the checks: root: no root user", err.Error())
}

func TestModLogType(t *testing.T) {
	assert.Equal(t, moderationlog.LogRollbackBackup, modLogType(restoreop.KindRollback))
	assert.Equal(t, moderationlog.LogRestoreBackup, modLogType(restoreop.KindRestore))
	assert.Equal(t, moderationlog.LogRestoreBackup, modLogType(""))
}

func TestOpenRestoreFailures(t *testing.T) {
	cfg := cfgWith(&config.BackupOptions{})
	cfg.Redis.Host = "127.0.0.1"
	cfg.Redis.Port = testutil.DeadPort(t)
	_, err := openRestore(cfg, nil, emptyStorage{}, slog.Default())
	assert.ErrorContains(t, err, "connect to redis")

	cfg.DB.Host = "127.0.0.1"
	cfg.DB.Port = testutil.DeadPort(t)
	cfg.DB.DB = "elythia"
	_, _, err = openOnce(context.Background(), cfg)
	assert.Error(t, err)
}

func TestDaemonID(t *testing.T) {
	a, b := daemonID(), daemonID()
	assert.NotEqual(t, a, b, "each start gets its own random part")
	assert.LessOrEqual(t, len(strings.SplitN(a, "-", 2)[0]), 24)
}
