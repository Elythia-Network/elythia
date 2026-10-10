package backup

import (
	"context"
	"errors"
	"io"
)

// ErrExists is returned by NewPutter.PutNew when the key already exists.
var ErrExists = errors.New("backup: object already exists")

// NewPutter is implemented by storages that can store an object only when the
// key does not exist yet. Take uses it so that two backups started in the
// same second (the same generation ID) cannot overwrite each other's files.
type NewPutter interface {
	PutNew(ctx context.Context, key string, r io.Reader) error
}

// putNew stores r with PutNew when st supports it, and with Put otherwise.
func putNew(ctx context.Context, st Storage, key string, r io.Reader) error {
	if np, ok := st.(NewPutter); ok {
		return np.PutNew(ctx, key, r)
	}
	return st.Put(ctx, key, r)
}
