package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
)

// ErrExists is returned by NewPutter.PutNew when the key already exists.
var ErrExists = errors.New("backup: object already exists")

// NewPutter is implemented by storages that can store a small object only
// when its key does not exist yet (S3 If-None-Match: *, a hard link in a
// directory).
type NewPutter interface {
	// PutNew stores body under key, or returns an error wrapping ErrExists
	// when key already exists.
	//
	// A transport that resends a request (the S3 SDK after a 500 or a
	// dropped connection, an NFS client retransmitting link) can make
	// PutNew report ErrExists for an object an earlier attempt of this very
	// call stored. claimKey tells the two apart.
	PutNew(ctx context.Context, key string, body []byte) error
}

// claimKey stores body under key unless someone else already did. It returns
// nil when key holds body afterwards, and an error wrapping ErrExists when it
// holds something else. Storages without NewPutter get a plain Put, which
// does not exclude anyone.
//
// 「既にある」と返っても、自分の前の試行が置いたものかもしれない (#3458 で、SDK の
// 自動の送り直しが 412 になり、正常に取れたバックアップを自分で壊した)。読み直して、
// 中身が自分の送ったものと同じなら成功として扱う。body に乱数を含めておけば、他の
// 誰かが同じ中身を置くことは無い。
func claimKey(ctx context.Context, st Storage, key string, body []byte) error {
	np, ok := st.(NewPutter)
	if !ok {
		return st.Put(ctx, key, bytes.NewReader(body))
	}
	err := np.PutNew(ctx, key, body)
	if err == nil || !errors.Is(err, ErrExists) {
		return err
	}
	if holds(ctx, st, key, body) {
		return nil
	}
	return err
}

// holds reports whether key can be read and holds exactly body.
func holds(ctx context.Context, st Storage, key string, body []byte) bool {
	r, err := st.Get(ctx, key)
	if err != nil {
		return false
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(io.LimitReader(r, int64(len(body))+1))
	return err == nil && bytes.Equal(got, body)
}

// releaseKey deletes key after a failure, but only while it still holds body,
// so a key claimed by someone else is never deleted.
func releaseKey(ctx context.Context, st Storage, logger *slog.Logger, key string, body []byte) {
	if holds(context.WithoutCancel(ctx), st, key, body) {
		deleteQuietly(ctx, st, logger, key)
	}
}
