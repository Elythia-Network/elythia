package server

import (
	"context"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/backupadmin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markedBackupDir returns a directory a backup.DirStorage accepts, holding one
// object of body so that the usage shows which directory was opened.
func markedBackupDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, backup.CreateDirMarker(dir))
	st, err := backup.NewDirStorage(dir)
	require.NoError(t, err)
	require.NoError(t, st.Put(context.Background(), "object", strings.NewReader(body)))
	return dir
}

func TestNewBackupAdminService_OpensTheConfiguredStorage(t *testing.T) {
	ctx := context.Background()
	mainDir := markedBackupDir(t, "main")
	serverDir := markedBackupDir(t, "server-only")
	dirOpts := func(p string) config.BackupStorageOptions {
		return config.BackupStorageOptions{Type: backup.StorageTypeDir, Dir: config.BackupDirectoryOptions{Path: p}}
	}

	t.Run("backup.storage", func(t *testing.T) {
		svc := newBackupAdminService(&config.Config{URL: "https://example.com", Backup: &config.BackupOptions{Storage: dirOpts(mainDir)}}, nil)
		ov, err := svc.List(ctx)
		require.NoError(t, err)
		assert.Equal(t, "dir", ov.StorageType)
		assert.Equal(t, int64(len("main")), ov.Usage.TotalBytes)
	})
	t.Run("backup.server.storage wins", func(t *testing.T) {
		b := &config.BackupOptions{Storage: dirOpts(mainDir)}
		b.Server.Storage = dirOpts(serverDir)
		svc := newBackupAdminService(&config.Config{URL: "https://example.com", Backup: b}, nil)
		ov, err := svc.List(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(len("server-only")), ov.Usage.TotalBytes)
	})
	t.Run("unusable storage is reported as not configured", func(t *testing.T) {
		// 目印の無いディレクトリは開けない。起動は止めず、一覧が未設定を返す。
		svc := newBackupAdminService(&config.Config{Backup: &config.BackupOptions{Storage: dirOpts(t.TempDir())}}, nil)
		_, err := svc.List(ctx)
		assert.ErrorIs(t, err, backupadmin.ErrNotConfigured)
	})
	t.Run("no backup section", func(t *testing.T) {
		_, err := newBackupAdminService(&config.Config{}, nil).List(ctx)
		assert.ErrorIs(t, err, backupadmin.ErrNotConfigured)
	})
}
