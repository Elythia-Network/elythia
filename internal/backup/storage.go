package backup

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned by Storage when the key does not exist.
var ErrNotFound = errors.New("backup: object not found")

// ObjectInfo describes one stored object.
type ObjectInfo struct {
	// Key is relative to the storage root (the configured prefix or path).
	Key     string
	Size    int64
	ModTime time.Time
}

// Storage is where generations are kept. Keys use "/" as the separator and
// are relative to the configured prefix (S3) or directory.
type Storage interface {
	// Put stores r under key. It must not leave a partial object visible
	// under key when it fails.
	Put(ctx context.Context, key string, r io.Reader) error
	// Get opens key for reading. It returns ErrNotFound when missing.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Stat returns ErrNotFound when missing.
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	// List returns every object whose key starts with prefix, in key order.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	// Delete removes key. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
}

// Presigner is implemented by storages that can hand out a time-limited
// download URL (S3). Directory storages do not implement it (#3462).
type Presigner interface {
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}
