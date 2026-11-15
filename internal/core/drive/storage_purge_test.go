package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// verifiedFixture returns a fixture whose files ids were migrated and
// verified.
func verifiedFixture(t *testing.T, ids ...string) *migrationFixture {
	t.Helper()
	f := migratedFixture(t, ids...)
	f.verify(t)
	return f
}

func (f *migrationFixture) purge(t *testing.T) {
	t.Helper()
	require.NoError(t, f.m.StartPurge(context.Background()))
	f.drainPurge(t)
}

// drainPurge runs the queued purge jobs until none is left.
func (f *migrationFixture) drainPurge(t *testing.T) {
	t.Helper()
	for range 1000 {
		j, ok := f.enq.popPurge()
		if !ok {
			return
		}
		require.NoError(t, f.m.RunPurgeBatch(context.Background(), j.token, j.afterID))
	}
	t.Fatal("the jobs did not end")
}

// localKeys reports which of the local copies of id are still there.
func (f *migrationFixture) localKeys(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	for _, k := range []string{"ak-" + id, "tk-" + id, "wk-" + id} {
		rc, err := f.local.Get(k)
		if errors.Is(err, ErrObjectNotFound) {
			continue
		}
		require.NoError(t, err)
		rc.Close()
		out = append(out, k)
	}
	return out
}

func TestStoragePurgeDeletesOnlyVerifiedLocalCopies(t *testing.T) {
	ctx := context.Background()
	f := verifiedFixture(t, "ok", "unverified", "gone", "thumb")
	// 確かめた印が無い行は消さない。
	for i := range f.repo.files {
		if f.repo.files[i].ID == "unverified" {
			f.repo.files[i].ObjectStorageVerifiedAt = nil
		}
	}
	// 確かめた後に写しが消えた。消す直前に確かめ直して、ローカルを残す。
	delete(f.dst.objects, "ak-gone")
	// 確かめた後にサムネイルの写しが壊れた。
	f.dst.objects["tk-thumb"] = []byte("broken")
	// まだ写していないローカルの行。
	f.addLocal(t, "local")

	f.purge(t)

	assert.Empty(t, f.localKeys(t, "ok"), "the verified file kept its local copies")
	assert.NotNil(t, f.repo.file("ok").ObjectStorageLocalDeletedAt)
	for _, id := range []string{"unverified", "gone", "thumb", "local"} {
		assert.Len(t, f.localKeys(t, id), 3, id)
		assert.Nil(t, f.repo.file(id).ObjectStorageLocalDeletedAt, id)
	}
	// 消す直前に一致しなかったものは、確かめた印を消して失敗として残す。
	for _, id := range []string{"gone", "thumb"} {
		assert.Nil(t, f.repo.file(id).ObjectStorageVerifiedAt, id)
		assert.Contains(t, f.repo.failures[id+"/"+repository.DriveStorageMigrationOperationPurge].Error, "the local copy was kept", id)
	}
	assert.NotContains(t, f.repo.failures, "unverified/purge")
	// 消す処理はオブジェクトストレージに触れない。
	assert.Contains(t, f.dst.objects, "ak-ok")

	st, err := f.m.Status(ctx)
	require.NoError(t, err)
	assert.Empty(t, st.Running)
	assert.Zero(t, st.Purgeable)
	assert.Equal(t, int64(1), st.LocalDeleted)
	assert.Equal(t, int64(2), st.PurgeFailed)
	assert.Len(t, st.PurgeFailures, 2)
	assert.Empty(t, st.VerifyFailures, "purge failures leaked into the verification failures")
	held, _ := f.lock.Held(ctx)
	assert.False(t, held, "the lock was not released at the end")

	// 消し終えたら、もう消す対象は無い。
	assert.ErrorIs(t, f.m.StartPurge(ctx), ErrStorageMigrationNothingToPurge)
}

// 日時が無いのにローカルが無いファイル (消す前の世代の DB のバックアップへ戻したときなど)
// も、流せば確かめ直して (ローカルが無いものは比べない) 日時を入れる。
func TestStoragePurgeFinishesAFileWhoseLocalCopyIsAlreadyGone(t *testing.T) {
	f := verifiedFixture(t, "a1")
	require.NoError(t, f.local.Delete("ak-a1"))
	require.NoError(t, f.local.Delete("tk-a1"))
	f.purge(t)
	assert.Empty(t, f.localKeys(t, "a1"))
	assert.NotNil(t, f.repo.file("a1").ObjectStorageLocalDeletedAt)
	assert.NotContains(t, f.repo.failures, "a1/purge")
}

func TestStoragePurgeChainsBatches(t *testing.T) {
	ids := make([]string, storageMigrationBatch+3)
	for i := range ids {
		ids[i] = fmt.Sprintf("f%04d", i)
	}
	f := verifiedFixture(t, ids...)
	direct := "ak-s3"
	f.repo.files = append(f.repo.files, model.DriveFile{ID: "zz-s3", AccessKey: &direct})

	require.NoError(t, f.m.StartPurge(context.Background()))
	first, ok := f.enq.popPurge()
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(first.token, purgeTokenPrefix))
	require.NoError(t, f.m.RunPurgeBatch(context.Background(), first.token, first.afterID))
	next, ok := f.enq.popPurge()
	require.True(t, ok)
	assert.Equal(t, migrationJob{first.token, fmt.Sprintf("f%04d", storageMigrationBatch-1)}, next)
	f.enq.purgeJobs = append([]migrationJob{next}, f.enq.purgeJobs...)
	f.drainPurge(t)

	c, _ := f.repo.Counts(context.Background())
	assert.Equal(t, int64(len(ids)), c.LocalDeleted)
	assert.Zero(t, c.Purgeable)
	assert.Empty(t, f.enq.jobs)
	assert.Empty(t, f.enq.verifyJobs)
}

func TestStoragePurgeStartRefuses(t *testing.T) {
	ctx := context.Background()
	t.Run("no queue", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		m := NewStorageMigrator(f.repo, f.local, f.dst, nil, f.lock)
		assert.ErrorIs(t, m.StartPurge(ctx), ErrStorageMigrationNoQueue)
	})
	t.Run("object storage not configured", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		m := NewStorageMigrator(f.repo, f.local, f.local, f.enq, f.lock)
		assert.ErrorIs(t, m.StartPurge(ctx), ErrStorageMigrationNotConfigured)
	})
	t.Run("nothing verified", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		assert.ErrorIs(t, f.m.StartPurge(ctx), ErrStorageMigrationNothingToPurge)
	})
	t.Run("counts fail", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		m := NewStorageMigrator(&failingMigrationRepo{memMigrationRepo: f.repo, fail: "Counts"}, f.local, f.dst, f.enq, f.lock)
		assert.ErrorIs(t, m.StartPurge(ctx), errInjected)
	})
	t.Run("verification running", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		require.NoError(t, f.m.StartVerify(ctx))
		assert.ErrorIs(t, f.m.StartPurge(ctx), ErrStorageMigrationRunning)
		assert.Empty(t, f.enq.purgeJobs)
	})
	t.Run("purge running", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		f.addLocal(t, "a2")
		require.NoError(t, f.m.StartPurge(ctx))
		assert.ErrorIs(t, f.m.StartPurge(ctx), ErrStorageMigrationRunning)
		assert.ErrorIs(t, f.m.StartVerify(ctx), ErrStorageMigrationRunning)
		assert.ErrorIs(t, f.m.Start(ctx), ErrStorageMigrationRunning)
		st, err := f.m.Status(ctx)
		require.NoError(t, err)
		assert.Equal(t, StorageMigrationOperationPurge, st.Running)
		// 消す処理は写さないので、削除の経路は写しを消さなくてよい。
		running, err := f.m.Running(ctx)
		require.NoError(t, err)
		assert.False(t, running)
	})
	t.Run("enqueue fails", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		f.enq.err = errors.New("redis down")
		require.Error(t, f.m.StartPurge(ctx))
		held, _ := f.lock.Held(ctx)
		assert.False(t, held)
	})
}

func TestStoragePurgeErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("row not verified", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		file := f.repo.file("a1")
		assert.ErrorContains(t, f.m.purgeFile(ctx, f.dst, &file, "t"), "has not been verified")
		assert.Len(t, f.localKeys(t, "a1"), 3)
	})
	t.Run("local delete fails", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		m := NewStorageMigrator(f.repo, &erroringStorage{Storage: f.local, deleteFail: map[string]bool{"tk-a1": true}}, f.dst, f.enq, f.lock)
		require.NoError(t, m.StartPurge(ctx))
		j, _ := f.enq.popPurge()
		require.NoError(t, m.RunPurgeBatch(ctx, j.token, j.afterID))
		assert.Contains(t, f.repo.failures, "a1/purge")
		// 日時を先に入れるので、消しそびれたローカルは「消した」扱いで残る (戻す処理は
		// オブジェクトストレージから取り直す)。「日時が無いのにローカルが無い」にはならない。
		assert.NotNil(t, f.repo.file("a1").ObjectStorageLocalDeletedAt)
	})
	t.Run("recording the deletion fails", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		m := NewStorageMigrator(&failingMigrationRepo{memMigrationRepo: f.repo, fail: "MarkLocalDeleted"}, f.local, f.dst, f.enq, f.lock)
		require.NoError(t, m.StartPurge(ctx))
		j, _ := f.enq.popPurge()
		// ジョブごとやり直す。ファイルの失敗としては残さず、ローカルも消さない。
		assert.ErrorIs(t, m.RunPurgeBatch(ctx, j.token, j.afterID), errInjected)
		assert.NotContains(t, f.repo.failures, "a1/purge")
		assert.Len(t, f.localKeys(t, "a1"), 3, "the local copy was deleted before the deletion was recorded")
	})
	t.Run("extending the lock before the deletion fails", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		lock := &flakyExtendLock{memMigrationLock: f.lock, failAt: 2}
		m := NewStorageMigrator(f.repo, f.local, f.dst, f.enq, lock)
		require.NoError(t, m.StartPurge(ctx))
		j, _ := f.enq.popPurge()
		lock.calls = 0
		// 1 回目は runBatch の頭、2 回目は 1 件目の前、3 回目が消す直前。
		lock.failAt = 3
		assert.ErrorIs(t, m.RunPurgeBatch(ctx, j.token, j.afterID), errInjected)
		assert.Len(t, f.localKeys(t, "a1"), 3)
		assert.NotContains(t, f.repo.failures, "a1/purge")
	})
	t.Run("object storage read fails", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		bad := &erroringStorage{Storage: f.dst, fail: map[string]bool{"ak-a1": true}}
		m := NewStorageMigrator(f.repo, f.local, bad, f.enq, f.lock)
		require.NoError(t, m.StartPurge(ctx))
		j, _ := f.enq.popPurge()
		require.NoError(t, m.RunPurgeBatch(ctx, j.token, j.afterID))
		assert.Len(t, f.localKeys(t, "a1"), 3)
		assert.Nil(t, f.repo.file("a1").ObjectStorageVerifiedAt)
		assert.Contains(t, f.repo.failures, "a1/purge")
	})
	t.Run("probe put fails", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		m := NewStorageMigrator(f.repo, &putFailingStorage{Storage: f.local}, f.dst, f.enq, f.lock)
		require.NoError(t, m.StartPurge(ctx))
		j, _ := f.enq.popPurge()
		assert.ErrorIs(t, m.RunPurgeBatch(ctx, j.token, j.afterID), errInjected)
		assert.Len(t, f.localKeys(t, "a1"), 3)
	})
	t.Run("probe put to the object storage fails", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		m := NewStorageMigrator(f.repo, f.local, &probeFailingStorage{Storage: f.dst, failPut: true}, f.enq, f.lock)
		require.NoError(t, m.StartPurge(ctx))
		j, _ := f.enq.popPurge()
		assert.ErrorIs(t, m.RunPurgeBatch(ctx, j.token, j.afterID), errInjected)
		assert.Len(t, f.localKeys(t, "a1"), 3)
	})
	t.Run("probe read back fails", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		m := NewStorageMigrator(f.repo, &probeFailingStorage{Storage: f.local, failGet: true}, f.dst, f.enq, f.lock)
		require.NoError(t, m.StartPurge(ctx))
		j, _ := f.enq.popPurge()
		assert.ErrorIs(t, m.RunPurgeBatch(ctx, j.token, j.afterID), errInjected)
		assert.Len(t, f.localKeys(t, "a1"), 3)
	})
	t.Run("probe read back breaks", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		m := NewStorageMigrator(f.repo, &probeFailingStorage{Storage: f.local, breakRead: true}, f.dst, f.enq, f.lock)
		require.NoError(t, m.StartPurge(ctx))
		j, _ := f.enq.popPurge()
		assert.ErrorIs(t, m.RunPurgeBatch(ctx, j.token, j.afterID), errInjected)
		assert.Len(t, f.localKeys(t, "a1"), 3)
	})
	t.Run("statfs fails", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		f.m.onFUSE = func(string) (bool, error) { return false, errInjected }
		require.NoError(t, f.m.StartPurge(ctx))
		j, _ := f.enq.popPurge()
		assert.ErrorIs(t, f.m.RunPurgeBatch(ctx, j.token, j.afterID), errInjected)
		assert.Len(t, f.localKeys(t, "a1"), 3)
	})
	t.Run("clearing the verification fails", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		delete(f.dst.objects, "ak-a1")
		m := NewStorageMigrator(&failingMigrationRepo{memMigrationRepo: f.repo, fail: "SetVerified"}, f.local, f.dst, f.enq, f.lock)
		require.NoError(t, m.StartPurge(ctx))
		j, _ := f.enq.popPurge()
		assert.ErrorIs(t, m.RunPurgeBatch(ctx, j.token, j.afterID), errInjected)
		assert.Len(t, f.localKeys(t, "a1"), 3, "the local copy was deleted although the copy was gone")
	})
	t.Run("ListFailures of the purge in Status", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		m := NewStorageMigrator(&failingPurgeFailuresRepo{memMigrationRepo: f.repo}, f.local, f.dst, f.enq, f.lock)
		_, err := m.Status(ctx)
		assert.ErrorIs(t, err, errInjected)
	})
}

// 確かめている間に別の実行が lock を取ったら (別の処理が始まった)、消さずに止める。
func TestStoragePurgeStopsWhenTheLockIsLostWhileComparing(t *testing.T) {
	ctx := context.Background()
	f := verifiedFixture(t, "a1")
	dst := &hookStorage{Storage: f.dst, onGet: func(key string) {
		if key == "ak-a1" {
			f.lock.expire()
			ok, _ := f.lock.Acquire(ctx, "other", time.Minute)
			require.True(t, ok)
		}
	}}
	m := NewStorageMigrator(f.repo, f.local, dst, f.enq, f.lock)
	require.NoError(t, m.StartPurge(ctx))
	j, _ := f.enq.popPurge()
	require.NoError(t, m.RunPurgeBatch(ctx, j.token, j.afterID))
	assert.Len(t, f.localKeys(t, "a1"), 3, "the local copy was deleted after the lock was lost")
	assert.Nil(t, f.repo.file("a1").ObjectStorageLocalDeletedAt)
	assert.NotContains(t, f.repo.failures, "a1/purge")
}

// 確かめている間に lock の期限が切れただけ (誰も持っていない) なら、取り直して消す。
// 読むのに期限を超える大きなファイルで、毎回止まって先へ進めなくならない。
func TestStoragePurgeTakesTheLockAgainWhenItExpiredWhileComparing(t *testing.T) {
	ctx := context.Background()
	f := verifiedFixture(t, "a1")
	dst := &hookStorage{Storage: f.dst, onGet: func(key string) {
		if key == "ak-a1" {
			f.lock.expire()
		}
	}}
	m := NewStorageMigrator(f.repo, f.local, dst, f.enq, f.lock)
	require.NoError(t, m.StartPurge(ctx))
	j, _ := f.enq.popPurge()
	require.NoError(t, m.RunPurgeBatch(ctx, j.token, j.afterID))
	assert.Empty(t, f.localKeys(t, "a1"))
	assert.NotNil(t, f.repo.file("a1").ObjectStorageLocalDeletedAt)
}

// 取り直すのにも失敗したら (Redis の障害)、ジョブごとやり直す。
func TestStoragePurgeRetriesWhenTheLockCannotBeTakenAgain(t *testing.T) {
	ctx := context.Background()
	f := verifiedFixture(t, "a1")
	lock := &failingAcquireLock{memMigrationLock: f.lock}
	dst := &hookStorage{Storage: f.dst, onGet: func(key string) {
		if key == "ak-a1" {
			f.lock.expire()
			lock.fail = true
		}
	}}
	m := NewStorageMigrator(f.repo, f.local, dst, f.enq, lock)
	require.NoError(t, m.StartPurge(ctx))
	j, _ := f.enq.popPurge()
	assert.ErrorIs(t, m.RunPurgeBatch(ctx, j.token, j.afterID), errInjected)
	assert.Len(t, f.localKeys(t, "a1"), 3)
}

// failingAcquireLock fails Acquire once fail is set.
type failingAcquireLock struct {
	*memMigrationLock
	fail bool
}

func (l *failingAcquireLock) Acquire(ctx context.Context, token string, ttl time.Duration) (bool, error) {
	if l.fail {
		return false, errInjected
	}
	return l.memMigrationLock.Acquire(ctx, token, ttl)
}

// drive-files が FUSE (バケットのマウント) なら、消すとバケットの実体も消えうるので止める。
func TestStoragePurgeRefusesOnFUSE(t *testing.T) {
	ctx := context.Background()
	f := verifiedFixture(t, "a1")
	var checked string
	f.m.onFUSE = func(dir string) (bool, error) {
		checked = dir
		return true, nil
	}
	repo := &listCountingRepo{memMigrationRepo: f.repo}
	f.m.repo = repo
	require.NoError(t, f.m.StartPurge(ctx))
	j, _ := f.enq.popPurge()
	require.NoError(t, f.m.RunPurgeBatch(ctx, j.token, j.afterID))
	assert.Equal(t, f.local.rootDir, checked)
	assert.Zero(t, repo.purgeLists, "the refused run went on to list the files")
	assert.Len(t, f.localKeys(t, "a1"), 3)
	held, _ := f.lock.Held(ctx)
	assert.False(t, held)
}

// 番兵はローカルにもオブジェクトストレージにも残さない。
func TestStoragePurgeLeavesNoProbe(t *testing.T) {
	f := verifiedFixture(t, "a1")
	f.purge(t)
	for k := range f.dst.objects {
		assert.False(t, strings.HasPrefix(k, "elythia-purge-probe-"), k)
	}
	entries, err := os.ReadDir(f.local.rootDir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), "elythia-purge-probe-"), e.Name())
	}
	// 実際の構成 (Linux の一時ディレクトリ) は FUSE ではない。
	fuse, err := onFUSE(f.local.rootDir)
	require.NoError(t, err)
	assert.False(t, fuse)
}

// 確かめている間に行が写し直されたら (移した日時が変わる)、印を付けられないので消さない。
func TestStoragePurgeKeepsTheLocalCopyOfARowChangedWhileComparing(t *testing.T) {
	ctx := context.Background()
	f := verifiedFixture(t, "a1")
	dst := &hookStorage{Storage: f.dst, onGet: func(key string) {
		if key == "ak-a1" {
			f.repo.mu.Lock()
			later := f.repo.files[0].ObjectStorageMigratedAt.Add(1)
			f.repo.files[0].ObjectStorageMigratedAt = &later
			f.repo.mu.Unlock()
		}
	}}
	m := NewStorageMigrator(f.repo, f.local, dst, f.enq, f.lock)
	require.NoError(t, m.StartPurge(ctx))
	j, _ := f.enq.popPurge()
	require.NoError(t, m.RunPurgeBatch(ctx, j.token, j.afterID))
	assert.Len(t, f.localKeys(t, "a1"), 3)
	assert.Nil(t, f.repo.file("a1").ObjectStorageLocalDeletedAt)
}

// オブジェクトストレージがローカルのファイルをそのまま配っている構成では、ローカルを
// 消すとオブジェクトストレージの実体も消えるので、消さずに止める。
func TestStoragePurgeRefusesWhenTheObjectStorageServesTheLocalFiles(t *testing.T) {
	ctx := context.Background()
	f := verifiedFixture(t, "a1")
	// オブジェクトストレージの側にも同じ中身を置いておき、比べるだけなら一致する状態にする。
	shared := &sharedStorage{Storage: f.local}
	m := NewStorageMigrator(f.repo, f.local, shared, f.enq, f.lock)
	require.NoError(t, m.StartPurge(ctx))
	j, _ := f.enq.popPurge()
	require.NoError(t, m.RunPurgeBatch(ctx, j.token, j.afterID))
	assert.Len(t, f.localKeys(t, "a1"), 3)
	assert.Nil(t, f.repo.file("a1").ObjectStorageLocalDeletedAt)
	held, _ := f.lock.Held(ctx)
	assert.False(t, held, "the refused run kept the lock")
	assert.Empty(t, f.enq.purgeJobs)
	// 番兵のファイルは残さない。
	entries, err := os.ReadDir(f.local.rootDir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), "elythia-purge-probe-"), e.Name())
	}
}

// sharedStorage is an object storage that serves the local files. It is not a
// *LocalStorage, so StorageIsLocal reports false for it.
type sharedStorage struct{ Storage }

// hookStorage calls onGet before each Get.
type hookStorage struct {
	Storage
	onGet func(key string)
}

func (s *hookStorage) Get(key string) (io.ReadCloser, error) {
	s.onGet(key)
	return s.Storage.Get(key)
}

// putFailingStorage fails every Put.
type putFailingStorage struct{ Storage }

func (putFailingStorage) Put(string, io.Reader) (string, error) { return "", errInjected }

// probeFailingStorage fails the purge probe: Put, Get, or the read of the
// object Get returns.
type probeFailingStorage struct {
	Storage
	failPut, failGet, breakRead bool
}

func (s *probeFailingStorage) Put(key string, body io.Reader) (string, error) {
	if s.failPut && strings.HasPrefix(key, "elythia-purge-probe-") {
		return "", errInjected
	}
	return s.Storage.Put(key, body)
}

func (s *probeFailingStorage) Get(key string) (io.ReadCloser, error) {
	if strings.HasPrefix(key, "elythia-purge-probe-") {
		if s.failGet {
			return nil, errInjected
		}
		if s.breakRead {
			return io.NopCloser(failingReader{}), nil
		}
	}
	return s.Storage.Get(key)
}

// flakyExtendLock fails the failAt-th Extend.
type flakyExtendLock struct {
	*memMigrationLock
	calls  int
	failAt int
}

func (l *flakyExtendLock) Extend(ctx context.Context, token string, ttl time.Duration) (bool, error) {
	l.calls++
	if l.calls == l.failAt {
		return false, errInjected
	}
	return l.memMigrationLock.Extend(ctx, token, ttl)
}

// failingPurgeFailuresRepo fails ListFailures for the purge only.
type failingPurgeFailuresRepo struct{ *memMigrationRepo }

func (r *failingPurgeFailuresRepo) ListFailures(ctx context.Context, op string, limit int) ([]model.DriveObjectStorageMigrationFailure, error) {
	if op == repository.DriveStorageMigrationOperationPurge {
		return nil, errInjected
	}
	return r.memMigrationRepo.ListFailures(ctx, op, limit)
}

func TestOperationOfPurgeToken(t *testing.T) {
	assert.Equal(t, StorageMigrationOperationPurge, operationOfToken(purgeTokenPrefix+"abc"))
}

// probeCountingStorage counts the purge probes put to it.
type probeCountingStorage struct {
	Storage
	probes int
}

func (s *probeCountingStorage) Put(key string, body io.Reader) (string, error) {
	if strings.HasPrefix(key, "elythia-purge-probe-") {
		s.probes++
	}
	return s.Storage.Put(key, body)
}

// 番兵は実行の初め (1 本目のジョブ) に 1 回だけ、lock を確かめた後で置く。終わった実行の
// ジョブや続きのジョブは置かない。
func TestStoragePurgeProbesOncePerRunWhileHoldingTheLock(t *testing.T) {
	ctx := context.Background()
	ids := make([]string, storageMigrationBatch+3)
	for i := range ids {
		ids[i] = fmt.Sprintf("f%04d", i)
	}
	f := verifiedFixture(t, ids...)
	dst := &probeCountingStorage{Storage: f.dst}
	m := NewStorageMigrator(f.repo, f.local, dst, f.enq, f.lock)

	// lock を持たない (終わった) 実行のジョブは、番兵を置かずに止まる。
	require.NoError(t, m.RunPurgeBatch(ctx, "purge-stale", ""))
	assert.Zero(t, dst.probes)

	require.NoError(t, m.StartPurge(ctx))
	for {
		j, ok := f.enq.popPurge()
		if !ok {
			break
		}
		require.NoError(t, m.RunPurgeBatch(ctx, j.token, j.afterID))
	}
	assert.Equal(t, 1, dst.probes)
	c, _ := f.repo.Counts(ctx)
	assert.Equal(t, int64(len(ids)), c.LocalDeleted)
}

// partialPutStorage writes the probe and then fails, as a full disk would.
type partialPutStorage struct{ *LocalStorage }

func (s *partialPutStorage) Put(key string, body io.Reader) (string, error) {
	if strings.HasPrefix(key, "elythia-purge-probe-") {
		if _, err := s.LocalStorage.Put(key, strings.NewReader("part")); err != nil {
			return "", err
		}
		return "", errInjected
	}
	return s.LocalStorage.Put(key, body)
}

// ローカルへの番兵の書き込みが途中で失敗しても、作りかけのファイルを残さない。
func TestStoragePurgeRemovesAPartlyWrittenProbe(t *testing.T) {
	ctx := context.Background()
	f := verifiedFixture(t, "a1")
	m := NewStorageMigrator(f.repo, &partialPutStorage{LocalStorage: f.local}, f.dst, f.enq, f.lock)
	require.NoError(t, m.StartPurge(ctx))
	j, _ := f.enq.popPurge()
	assert.ErrorIs(t, m.RunPurgeBatch(ctx, j.token, j.afterID), errInjected)
	entries, err := os.ReadDir(f.local.rootDir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), "elythia-purge-probe-"), e.Name())
	}
}

// listCountingRepo counts ListPurgeable.
type listCountingRepo struct {
	*memMigrationRepo
	purgeLists int
}

func (r *listCountingRepo) ListPurgeable(ctx context.Context, afterID string, limit int) ([]model.DriveFile, error) {
	r.purgeLists++
	return r.memMigrationRepo.ListPurgeable(ctx, afterID, limit)
}
