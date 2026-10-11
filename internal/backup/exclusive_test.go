package backup

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPutMem is a memStorage with PutNew. resent makes PutNew store the
// object and then report ErrExists, like a request the transport sent twice
// whose first copy was stored. err makes PutNew fail without storing.
type newPutMem struct {
	*memStorage
	resent bool
	err    error
}

func (m *newPutMem) PutNew(ctx context.Context, key string, body []byte) error {
	if m.err != nil {
		return m.err
	}
	if _, err := m.Stat(ctx, key); err == nil {
		return ErrExists
	}
	if err := m.Put(ctx, key, bytes.NewReader(body)); err != nil {
		return err
	}
	if m.resent {
		return ErrExists
	}
	return nil
}

func TestClaimKey(t *testing.T) {
	ctx := context.Background()
	const key = "generations/20261010T040000Z/take.lock"

	t.Run("new key", func(t *testing.T) {
		st := &newPutMem{memStorage: newMemStorage()}
		require.NoError(t, claimKey(ctx, st, key, []byte("mine")))
		assert.Equal(t, []byte("mine"), st.objects[key])
	})
	t.Run("someone else holds it", func(t *testing.T) {
		st := &newPutMem{memStorage: newMemStorage()}
		st.objects[key] = []byte("theirs")
		require.ErrorIs(t, claimKey(ctx, st, key, []byte("mine")), ErrExists)
		assert.Equal(t, []byte("theirs"), st.objects[key], "the other claim is not overwritten")
	})
	t.Run("a resent request that was already stored", func(t *testing.T) {
		st := &newPutMem{memStorage: newMemStorage(), resent: true}
		require.NoError(t, claimKey(ctx, st, key, []byte("mine")))
	})
	t.Run("someone else holds a longer body that starts with mine", func(t *testing.T) {
		st := &newPutMem{memStorage: newMemStorage()}
		st.objects[key] = []byte("mine and more")
		require.ErrorIs(t, claimKey(ctx, st, key, []byte("mine")), ErrExists)
	})
	t.Run("the read back fails", func(t *testing.T) {
		st := &newPutMem{memStorage: newMemStorage(), resent: true}
		st.getErr = map[string]error{"take.lock": io.ErrUnexpectedEOF}
		require.ErrorIs(t, claimKey(ctx, st, key, []byte("mine")), ErrExists, "unknown ownership is not success")
	})
	t.Run("other errors pass through", func(t *testing.T) {
		st := &newPutMem{memStorage: newMemStorage(), err: io.ErrClosedPipe}
		err := claimKey(ctx, st, key, []byte("mine"))
		require.ErrorIs(t, err, io.ErrClosedPipe)
		require.NotErrorIs(t, err, ErrExists)
	})
	t.Run("storages without PutNew use Put", func(t *testing.T) {
		st := newMemStorage()
		st.objects[key] = []byte("theirs")
		require.NoError(t, claimKey(ctx, st, key, []byte("mine")))
		assert.Equal(t, []byte("mine"), st.objects[key])
	})
}

func TestReleaseKey(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	st := newMemStorage()
	st.objects["mine"] = []byte("a")
	st.objects["theirs"] = []byte("b")
	releaseKey(ctx, st, logger, "mine", []byte("a"))
	releaseKey(ctx, st, logger, "theirs", []byte("a"))
	releaseKey(ctx, st, logger, "missing", []byte("a"))
	assert.NotContains(t, st.objects, "mine")
	assert.Equal(t, []byte("b"), st.objects["theirs"], "a key held by someone else is kept")

	// ctx が切れた後でも、自分のものは消す。
	st.objects["mine"] = []byte("a")
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	releaseKey(cctx, st, logger, "mine", []byte("a"))
	assert.NotContains(t, st.objects, "mine")
}

// putNewContract checks PutNew: it stores new keys, and refuses to replace an
// existing key with ErrExists, leaving it as it was and nothing else behind.
func putNewContract(t *testing.T, st Storage) {
	t.Helper()
	ctx := context.Background()
	np, ok := st.(NewPutter)
	require.True(t, ok)
	key := Key("20261010T040000Z", LockFile)
	require.NoError(t, np.PutNew(ctx, key, []byte("first")))
	err := np.PutNew(ctx, key, []byte("second"))
	require.ErrorIs(t, err, ErrExists)
	assert.Equal(t, []byte("first"), readAll(t, st, key))
	objs, err := st.List(ctx, "")
	require.NoError(t, err)
	assert.Len(t, objs, 1, "the refused write leaves nothing behind")
	// 自分の 1 回目が置いたものなら、claimKey は成功として扱う。
	require.NoError(t, claimKey(ctx, st, key, []byte("first")))
	require.ErrorIs(t, claimKey(ctx, st, key, []byte("other")), ErrExists)
}

func TestDirStorage_PutNew(t *testing.T) {
	root := markedDir(t)
	st, err := NewDirStorage(root)
	require.NoError(t, err)
	putNewContract(t, st)
	ctx := context.Background()
	assert.Error(t, st.PutNew(ctx, "../x", []byte("x")))

	fi, err := os.Stat(filepath.Join(root, "generations", "20261010T040000Z", LockFile))
	require.NoError(t, err)
	assert.Equal(t, filePerm, fi.Mode().Perm(), "the lock is readable by the group like other objects")

	require.NoError(t, os.Remove(filepath.Join(root, DirMarkerFile)))
	require.ErrorIs(t, st.PutNew(ctx, Key("20261010T050000Z", LockFile), []byte("x")), ErrNoDirMarker)
	_, err = os.Stat(filepath.Join(root, "generations", "20261010T050000Z"))
	assert.True(t, os.IsNotExist(err), "nothing is created without the marker")
}

// TestDirStorage_PutNewRetransmittedLink: an NFS client that resends link
// gets EEXIST although its first link was done. claimKey must take that as
// its own claim.
func TestDirStorage_PutNewRetransmittedLink(t *testing.T) {
	orig := linkFile
	t.Cleanup(func() { linkFile = orig })
	linkFile = func(root *os.Root, oldname, newname string) error {
		if err := root.Link(oldname, newname); err != nil {
			return err
		}
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EEXIST}
	}
	st, err := NewDirStorage(markedDir(t))
	require.NoError(t, err)
	key := Key("20261010T040000Z", LockFile)
	require.ErrorIs(t, st.PutNew(context.Background(), key, []byte("mine")), ErrExists)
	require.NoError(t, claimKey(context.Background(), st, key, []byte("mine")))
	assertNoTempFiles(t, st.Root())
}

// TestDirStorage_PutNewWithoutHardLinks: on file systems that refuse hard
// links, PutNew still refuses to replace an existing key.
func TestDirStorage_PutNewWithoutHardLinks(t *testing.T) {
	for _, errno := range []error{syscall.EPERM, syscall.ENOTSUP, syscall.ENOSYS} {
		t.Run(errno.Error(), func(t *testing.T) {
			orig := linkFile
			t.Cleanup(func() { linkFile = orig })
			linkFile = func(_ *os.Root, oldname, newname string) error {
				return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: errno}
			}
			st, err := NewDirStorage(markedDir(t))
			require.NoError(t, err)
			putNewContract(t, st)
			assertNoTempFiles(t, st.Root())
		})
	}
	t.Run("other link errors fail", func(t *testing.T) {
		orig := linkFile
		t.Cleanup(func() { linkFile = orig })
		linkFile = func(_ *os.Root, oldname, newname string) error {
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EIO}
		}
		st, err := NewDirStorage(markedDir(t))
		require.NoError(t, err)
		err = st.PutNew(context.Background(), Key("20261010T040000Z", LockFile), []byte("x"))
		require.ErrorIs(t, err, syscall.EIO)
		require.NotErrorIs(t, err, ErrExists)
		_, serr := st.Stat(context.Background(), Key("20261010T040000Z", LockFile))
		require.ErrorIs(t, serr, ErrNotFound, "an EIO does not fall back to creating the file")
		assertNoTempFiles(t, st.Root())
	})
}

func assertNoTempFiles(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		assert.False(t, strings.HasPrefix(e.Name(), dirTempPrefix), "temp file left: %s", p)
		return nil
	}))
}

func TestS3Storage_PutNew(t *testing.T) {
	putNewContract(t, newS3Storage(t))
}
