package drive

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A local file larger than the recorded size fails the migration of the file
// without being read past the recorded size, the same bound the restore uses.
func TestStorageMigrationStopsReadingAnOversizedLocalFile(t *testing.T) {
	ctx := context.Background()
	f := newMigrationFixture(t)
	f.addLocal(t, "a1")
	size := int64(f.repo.file("a1").Size)
	_, err := f.local.Put("ak-a1", strings.NewReader(strings.Repeat("x", 1<<20)))
	require.NoError(t, err)
	local := &countingGetStorage{Storage: f.local}
	f.m = NewStorageMigrator(f.repo, local, f.dst, f.enq, f.lock)

	require.NoError(t, f.m.Start(ctx))
	f.drain(t)

	assert.True(t, f.repo.file("a1").StoredInternal)
	assert.Contains(t, f.repo.failures["a1/migrate"].Error, "larger than")
	assert.NotContains(t, f.dst.objects, "ak-a1")
	assert.LessOrEqual(t, local.read, size+1, "read past the recorded size")
}

// A file exactly of the recorded size is still copied.
func TestStorageMigrationCopiesAFileOfTheRecordedSize(t *testing.T) {
	f := newMigrationFixture(t)
	f.addLocal(t, "a1")
	require.NoError(t, f.m.Start(context.Background()))
	f.drain(t)
	assert.False(t, f.repo.file("a1").StoredInternal)
	assert.Equal(t, "body of ak-a1", string(f.dst.objects["ak-a1"]))
}

// A thumbnail or a webpublic variant larger than the limit is refused
// instead of being read whole.
func TestCopyObjectRefusesAnOversizedObject(t *testing.T) {
	src := newMemObjectStorage()
	dst := newMemObjectStorage()
	src.objects["wk"] = []byte("0123456789")
	err := copyObject(src, dst, "wk", 4)
	assert.ErrorContains(t, err, "larger than 4 bytes")
	assert.NotContains(t, dst.objects, "wk")

	require.NoError(t, copyObject(src, dst, "wk", 10))
	assert.Equal(t, "0123456789", string(dst.objects["wk"]))

	assert.ErrorIs(t, copyObject(src, dst, "missing", 10), ErrObjectNotFound)
}

// endlessProbeStorage serves a body far larger than the probe for the purge
// probe key on Get and counts the bytes read from it.
type endlessProbeStorage struct {
	Storage
	read int64
}

func (s *endlessProbeStorage) Get(key string) (io.ReadCloser, error) {
	if strings.HasPrefix(key, "elythia-purge-probe-") {
		return &countingReadCloser{ReadCloser: io.NopCloser(&tripwireReader{left: probeTripwireBytes}), n: &s.read}, nil
	}
	return s.Storage.Get(key)
}

// probeTripwireBytes is how much tripwireReader serves before it fails.
const probeTripwireBytes = 1 << 20

// tripwireReader serves 'x' bytes and fails once more than left bytes have
// been asked for. 読む量を縛らない実装に戻すと、止まり続けずにここで
// エラーになってテストがすぐ落ちる。
type tripwireReader struct{ left int64 }

func (r *tripwireReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, errors.New("tripwire: the probe was read past the bound")
	}
	n := min(int64(len(p)), r.left)
	for i := range n {
		p[i] = 'x'
	}
	r.left -= n
	return int(n), nil
}

// The purge probe reads back only a little more than the probe it put, so a
// local file replaced by something large cannot make it read without end.
func TestLocalIsObjectStorageBoundsTheProbeRead(t *testing.T) {
	f := newMigrationFixture(t)
	local := &endlessProbeStorage{Storage: newMemObjectStorage()}
	m := NewStorageMigrator(f.repo, local, f.dst, f.enq, f.lock)

	reason, err := m.localIsObjectStorage(f.dst)
	require.NoError(t, err)
	assert.Equal(t, "the object storage serves the local drive files", reason)
	assert.Less(t, local.read, int64(1<<10), "the probe read was not bounded")
}
