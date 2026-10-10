package server

import (
	"log/slog"
	"strings"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/backupadmin"
	"github.com/redis/go-redis/v9"
)

// backupServerStorageOptions picks the storage the main server uses:
// backup.server.storage when its type is set, else backup.storage. The main
// server can then be given a key limited to listing, deleting and presigning.
func backupServerStorageOptions(b *config.BackupOptions) config.BackupStorageOptions {
	if b.Server.Storage.Type != "" {
		return b.Server.Storage
	}
	return b.Storage
}

// newBackupAdminService builds the service behind admin/backup/*. A missing
// or unusable `backup:` section yields a service whose operations report
// "not configured" instead of failing the server start.
func newBackupAdminService(cfg *config.Config, rdb redis.Cmdable) *backupadmin.Service {
	o := backupadmin.Options{
		Tokens:          backupadmin.NewRedisTokens(rdb),
		DownloadURLBase: strings.TrimRight(cfg.URL, "/") + "/backup-download?token=",
	}
	b := cfg.Backup
	if b == nil {
		return backupadmin.NewService(o)
	}
	so := backupServerStorageOptions(b)
	o.StorageType = so.Type
	o.PricePerGBMonth = b.Server.PricePerGBMonth
	if so.Type != "" {
		st, err := backup.OpenStorage(so)
		if err != nil {
			slog.Warn("backup: storage for the admin page is unavailable", "type", so.Type, "err", err)
		} else {
			o.Storage = st
		}
	}
	if b.Server.ServiceURL != "" {
		o.Control = backupadmin.NewHTTPControl(b.Server.ServiceURL, b.Server.ServiceToken, nil)
	}
	return backupadmin.NewService(o)
}
