package server

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/backupadmin"
	"github.com/redis/go-redis/v9"
)

// errBackupStorageUnavailable is returned by openBackupStorage until the
// storage implementations land.
var errBackupStorageUnavailable = errors.New("backup storage implementations are not available in this build")

// openBackupStorage opens the storage the admin page lists (#3462).
//
// 保存先の実装 (S3 / ディレクトリ) は #3458 が足す。マージされたら、この本体を
// `return backup.OpenStorage(o)` (#3458 の internal/backup/open.go) に差し替える。
// それまでは保存先を作れないので、管理画面の一覧などは BACKUP_NOT_CONFIGURED を
// 返す。
func openBackupStorage(o config.BackupStorageOptions) (backup.Storage, error) {
	_ = o
	return nil, errBackupStorageUnavailable
}

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
		DownloadURLBase: strings.TrimRight(cfg.URL, "/") + "/backup-download/",
	}
	b := cfg.Backup
	if b == nil {
		return backupadmin.NewService(o)
	}
	so := backupServerStorageOptions(b)
	o.StorageType = so.Type
	o.PricePerGBMonth = b.Server.PricePerGBMonth
	if so.Type != "" {
		st, err := openBackupStorage(so)
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
