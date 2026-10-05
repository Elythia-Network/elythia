package processors

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeChunkedReclaimer struct {
	calls int
	limit int
	n     int
	err   error
}

func (f *fakeChunkedReclaimer) GCChunkedUploads(_ context.Context, _ time.Time, limit int) (int, error) {
	f.calls++
	f.limit = limit
	return f.n, f.err
}

func TestChunkedUploadGCProcessor_Handle(t *testing.T) {
	svc := &fakeChunkedReclaimer{n: 3}
	p := NewChunkedUploadGCProcessor(svc)

	require.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
	assert.Equal(t, 1, svc.calls)
	// 1 回の実行で処理する件数を bound しないと、大量のバックログで maintenance
	// worker を占有しうる。
	assert.Equal(t, chunkedUploadGCBatch, svc.limit)
}

func TestChunkedUploadGCProcessor_Error(t *testing.T) {
	svc := &fakeChunkedReclaimer{err: errors.New("db down")}
	p := NewChunkedUploadGCProcessor(svc)
	assert.Error(t, p.Handle(context.Background(), driver.RawTask{}))
}

// 未配線なら no-op。他の maintenance processor と同じ扱い。
func TestChunkedUploadGCProcessor_NilServiceIsNoop(t *testing.T) {
	p := NewChunkedUploadGCProcessor(nil)
	assert.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
}
