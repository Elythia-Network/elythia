package backup

import (
	"fmt"

	"github.com/elythia-network/elythia/internal/config"
)

// Storage types accepted in backup.storage.type.
const (
	StorageTypeS3  = "s3"
	StorageTypeDir = "dir"
)

// OpenStorage builds the Storage that o selects.
func OpenStorage(o config.BackupStorageOptions) (Storage, error) {
	switch o.Type {
	case StorageTypeS3:
		return NewS3Storage(o.S3)
	case StorageTypeDir:
		return NewDirStorage(o.Dir.Path)
	case "":
		return nil, fmt.Errorf("backup: storage.type is empty (want %q or %q)", StorageTypeS3, StorageTypeDir)
	default:
		return nil, fmt.Errorf("backup: unknown storage.type %q (want %q or %q)", o.Type, StorageTypeS3, StorageTypeDir)
	}
}
