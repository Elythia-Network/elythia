package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// migrated returns a fixture whose files ids were migrated to object storage.
func migratedFixture(t *testing.T, ids ...string) *migrationFixture {
	t.Helper()
	f := newMigrationFixture(t)
	for _, id := range ids {
		f.addLocal(t, id)
	}
	require.NoError(t, f.m.Start(context.Background()))
	f.drain(t)
	return f
}

func (f *migrationFixture) verify(t *testing.T) {
	t.Helper()
	require.NoError(t, f.m.StartVerify(context.Background()))
	f.drainVerify(t)
}

func TestStorageVerifyMarksOnlyMatchingFiles(t *testing.T) {
	ctx := context.Background()
	f := migratedFixture(t, "ok", "md5", "gone", "size", "thumb", "nothumb", "webp", "webmd5", "nolocal")
	// 同じ大きさで中身だけ違う。
	f.dst.objects["ak-md5"] = []byte(strings.Repeat("x", len(f.dst.objects["ak-md5"])))
	delete(f.dst.objects, "ak-gone")
	f.dst.objects["ak-size"] = append(f.dst.objects["ak-size"], '!')
	// ローカルにあるサムネイルが、オブジェクトストレージに無い。
	delete(f.dst.objects, "tk-thumb")
	// サムネイルがどちらにも無い (もともと配信できていなかった)。
	delete(f.dst.objects, "tk-nothumb")
	require.NoError(t, f.local.Delete("tk-nothumb"))
	f.dst.objects["wk-webp"] = []byte("short")
	// 大きさが同じで中身だけ違う webpublic も、ローカルと比べて見つける。
	f.dst.objects["wk-webmd5"] = []byte(strings.Repeat("y", len(f.dst.objects["wk-webmd5"])))
	// ローカルの実体が無くても、オブジェクトストレージの本体が記録と一致すれば確かめられる。
	for _, k := range []string{"ak-nolocal", "tk-nolocal", "wk-nolocal"} {
		require.NoError(t, f.local.Delete(k))
	}

	f.verify(t)

	verified := map[string]bool{"ok": true, "nothumb": true, "nolocal": true}
	for _, id := range []string{"ok", "md5", "gone", "size", "thumb", "nothumb", "webp", "webmd5", "nolocal"} {
		file := f.repo.file(id)
		_, failed := f.repo.failures[id+"/"+repository.DriveStorageMigrationOperationVerify]
		if verified[id] {
			assert.NotNil(t, file.ObjectStorageVerifiedAt, id)
			assert.False(t, failed, id)
		} else {
			assert.Nil(t, file.ObjectStorageVerifiedAt, id)
			assert.True(t, failed, id)
		}
	}
	msgs := map[string]string{
		"md5":    "md5",
		"gone":   "the file is not in the object storage",
		"size":   "does not match the recorded size",
		"thumb":  "the thumbnail is not in the object storage",
		"webp":   "the size of the webpublic",
		"webmd5": "the md5 of the webpublic",
	}
	for id, want := range msgs {
		assert.Contains(t, f.repo.failures[id+"/verify"].Error, want, id)
	}
	// 確かめる処理は写さず、ローカルも消さない。
	_, err := f.local.Get("ak-gone")
	require.NoError(t, err)
	assert.NotContains(t, f.dst.objects, "ak-gone")

	st, err := f.m.Status(ctx)
	require.NoError(t, err)
	assert.Empty(t, st.Running)
	assert.Equal(t, int64(3), st.Verified)
	assert.Equal(t, int64(6), st.VerifyFailed)
	assert.Len(t, st.VerifyFailures, 6)
	assert.Empty(t, st.Failures, "verification failures leaked into the migration failures")
	held, _ := f.lock.Held(ctx)
	assert.False(t, held, "the lock was not released at the end")
}

// 確かめ直すと、壊れた写しは印を失い、直った写しは印を得て失敗の記録が消える。
func TestStorageVerifyReverifies(t *testing.T) {
	f := migratedFixture(t, "a1", "a2")
	delete(f.dst.objects, "ak-a2")
	f.verify(t)
	require.NotNil(t, f.repo.file("a1").ObjectStorageVerifiedAt)
	require.Contains(t, f.repo.failures, "a2/verify")

	f.dst.objects["ak-a1"] = []byte("broken")
	_, err := f.dst.Put("ak-a2", strings.NewReader("body of ak-a2"))
	require.NoError(t, err)
	f.verify(t)
	assert.Nil(t, f.repo.file("a1").ObjectStorageVerifiedAt)
	assert.Contains(t, f.repo.failures, "a1/verify")
	assert.NotNil(t, f.repo.file("a2").ObjectStorageVerifiedAt)
	assert.NotContains(t, f.repo.failures, "a2/verify")
}

// 読めなかったときも印を消す。S3 は権限によって、無いキーを 403 (読めない) で返すので、
// 印を残すと消えた写しに印が残り続ける。
func TestStorageVerifyClearsTheMarkWhenTheStorageCannotBeRead(t *testing.T) {
	ctx := context.Background()
	f := migratedFixture(t, "a1", "a2", "a3")
	f.verify(t)
	for _, id := range []string{"a1", "a2", "a3"} {
		require.NotNil(t, f.repo.file(id).ObjectStorageVerifiedAt, id)
	}
	bad := &erroringStorage{Storage: f.dst, fail: map[string]bool{"ak-a1": true}, readFail: map[string]bool{"tk-a2": true}}
	m := NewStorageMigrator(f.repo, &erroringStorage{Storage: f.local, fail: map[string]bool{"wk-a3": true}}, bad, f.enq, f.lock)
	require.NoError(t, m.StartVerify(ctx))
	for {
		j, ok := f.enq.popVerify()
		if !ok {
			break
		}
		require.NoError(t, m.RunVerifyBatch(ctx, j.token, j.afterID))
	}
	for _, id := range []string{"a1", "a2", "a3"} {
		assert.Nil(t, f.repo.file(id).ObjectStorageVerifiedAt, id)
		assert.Contains(t, f.repo.failures, id+"/verify", id)
	}
}

// 確かめた結果を DB に書けなかったら、失敗として残して先へ進まず、ジョブごとやり直す。
// 進むと、一致しなかった写しに前の印が残ったままになる。
func TestStorageVerifyRetriesWhenTheResultCannotBeRecorded(t *testing.T) {
	ctx := context.Background()
	f := migratedFixture(t, "a1")
	f.verify(t)
	require.NotNil(t, f.repo.file("a1").ObjectStorageVerifiedAt)
	f.dst.objects["ak-a1"] = []byte("broken")
	m := NewStorageMigrator(&failingMigrationRepo{memMigrationRepo: f.repo, fail: "SetVerified"}, f.local, f.dst, f.enq, f.lock)
	require.NoError(t, m.StartVerify(ctx))
	j, ok := f.enq.popVerify()
	require.True(t, ok)
	assert.ErrorIs(t, m.RunVerifyBatch(ctx, j.token, j.afterID), errInjected)
	assert.NotContains(t, f.repo.failures, "a1/verify", "the failure to record was recorded as the failure of the file")
	held, _ := f.lock.Held(ctx)
	assert.True(t, held, "the run stopped instead of being retried")
	assert.Empty(t, f.enq.verifyJobs)
}

// 削除の経路がオブジェクトストレージの写しも消すのは、移す処理の実行中だけ。
func TestStorageMigratorRunningCountsOnlyTheMigration(t *testing.T) {
	ctx := context.Background()
	f := migratedFixture(t, "a1")
	running, err := f.m.Running(ctx)
	require.NoError(t, err)
	assert.False(t, running)
	require.NoError(t, f.m.StartVerify(ctx))
	running, err = f.m.Running(ctx)
	require.NoError(t, err)
	assert.False(t, running, "the verification counted as a migration")
	f.lock.expire()
	f.addLocal(t, "a2")
	require.NoError(t, f.m.Start(ctx))
	running, err = f.m.Running(ctx)
	require.NoError(t, err)
	assert.True(t, running)
	_, err = NewStorageMigrator(f.repo, f.local, f.dst, f.enq, failingMigrationLock{}).Running(ctx)
	assert.ErrorIs(t, err, errInjected)
}

func TestStorageVerifyChainsBatches(t *testing.T) {
	ids := make([]string, storageMigrationBatch+3)
	for i := range ids {
		ids[i] = fmt.Sprintf("f%04d", i)
	}
	f := migratedFixture(t, ids...)
	// 初めからオブジェクトストレージにある行とローカルの行は確かめない。
	direct := "ak-s3"
	f.repo.files = append(f.repo.files, model.DriveFile{ID: "zz-s3", AccessKey: &direct})
	f.addLocal(t, "zz-local")

	require.NoError(t, f.m.StartVerify(context.Background()))
	first, ok := f.enq.popVerify()
	require.True(t, ok)
	assert.Empty(t, first.afterID)
	assert.True(t, strings.HasPrefix(first.token, verifyTokenPrefix))
	require.NoError(t, f.m.RunVerifyBatch(context.Background(), first.token, first.afterID))
	next, ok := f.enq.popVerify()
	require.True(t, ok)
	assert.Equal(t, migrationJob{first.token, fmt.Sprintf("f%04d", storageMigrationBatch-1)}, next)
	f.enq.verifyJobs = append([]migrationJob{next}, f.enq.verifyJobs...)
	f.drainVerify(t)

	c, _ := f.repo.Counts(context.Background())
	assert.Equal(t, int64(len(ids)), c.Verified)
	assert.Zero(t, c.VerifyFailed)
	assert.Empty(t, f.enq.jobs, "the verification enqueued a migration job")
}

func TestStorageVerifyStartRefuses(t *testing.T) {
	ctx := context.Background()
	t.Run("no queue", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		m := NewStorageMigrator(f.repo, f.local, f.dst, nil, f.lock)
		assert.ErrorIs(t, m.StartVerify(ctx), ErrStorageMigrationNoQueue)
	})
	t.Run("object storage not configured", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		m := NewStorageMigrator(f.repo, f.local, f.local, f.enq, f.lock)
		assert.ErrorIs(t, m.StartVerify(ctx), ErrStorageMigrationNotConfigured)
	})
	t.Run("nothing migrated", func(t *testing.T) {
		f := newMigrationFixture(t)
		f.addLocal(t, "a1")
		assert.ErrorIs(t, f.m.StartVerify(ctx), ErrStorageMigrationNothingToVerify)
	})
	t.Run("counts fail", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		m := NewStorageMigrator(&failingMigrationRepo{memMigrationRepo: f.repo, fail: "Counts"}, f.local, f.dst, f.enq, f.lock)
		assert.ErrorIs(t, m.StartVerify(ctx), errInjected)
	})
	t.Run("migration running", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		f.addLocal(t, "a2")
		require.NoError(t, f.m.Start(ctx))
		// 移す処理と確かめる処理は同時に走らない。
		assert.ErrorIs(t, f.m.StartVerify(ctx), ErrStorageMigrationRunning)
		assert.Empty(t, f.enq.verifyJobs)
	})
	t.Run("verification running", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		f.addLocal(t, "a2")
		require.NoError(t, f.m.StartVerify(ctx))
		assert.ErrorIs(t, f.m.StartVerify(ctx), ErrStorageMigrationRunning)
		assert.ErrorIs(t, f.m.Start(ctx), ErrStorageMigrationRunning)
		st, err := f.m.Status(ctx)
		require.NoError(t, err)
		assert.Equal(t, StorageMigrationOperationVerify, st.Running)
	})
	t.Run("enqueue fails", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		f.enq.err = errors.New("redis down")
		require.Error(t, f.m.StartVerify(ctx))
		held, _ := f.lock.Held(ctx)
		assert.False(t, held)
	})
}

// 確かめている間に写し直された行には、古い実体を確かめた結果を書かない。
func TestStorageVerifyDoesNotMarkARowMigratedAgain(t *testing.T) {
	f := migratedFixture(t, "a1")
	file := f.repo.file("a1")
	later := file.ObjectStorageMigratedAt.Add(time.Minute)
	f.repo.files[0].ObjectStorageMigratedAt = &later
	require.NoError(t, f.m.verifyFile(context.Background(), f.dst, &file))
	assert.Nil(t, f.repo.file("a1").ObjectStorageVerifiedAt)
}

func TestStorageVerifyErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("row not migrated", func(t *testing.T) {
		f := newMigrationFixture(t)
		f.addLocal(t, "a1")
		file := f.repo.file("a1")
		assert.ErrorContains(t, f.m.verifyFile(ctx, f.dst, &file), "has not been migrated")
	})
	t.Run("no access key", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		file := f.repo.file("a1")
		file.AccessKey = nil
		assert.ErrorContains(t, f.m.verifyFile(ctx, f.dst, &file), "no access key")
	})
	t.Run("recording the verification fails", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		m := NewStorageMigrator(&failingMigrationRepo{memMigrationRepo: f.repo, fail: "SetVerified"}, f.local, f.dst, f.enq, f.lock)
		file := f.repo.file("a1")
		err := m.verifyFile(ctx, f.dst, &file)
		assert.ErrorIs(t, err, errInjected)
		var retry *storageMigrationRetry
		assert.ErrorAs(t, err, &retry)
	})
	t.Run("object storage read fails", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		file := f.repo.file("a1")
		bad := &erroringStorage{Storage: f.dst, fail: map[string]bool{"ak-a1": true}}
		assert.ErrorIs(t, f.m.verifyFile(ctx, bad, &file), errInjected)
		bad = &erroringStorage{Storage: f.dst, fail: map[string]bool{"tk-a1": true}}
		assert.ErrorIs(t, f.m.verifyFile(ctx, bad, &file), errInjected)
		bad = &erroringStorage{Storage: f.dst, readFail: map[string]bool{"ak-a1": true}}
		assert.ErrorIs(t, f.m.verifyFile(ctx, bad, &file), errInjected)
	})
	t.Run("local read fails", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		file := f.repo.file("a1")
		m := NewStorageMigrator(f.repo, &erroringStorage{Storage: f.local, fail: map[string]bool{"wk-a1": true}}, f.dst, f.enq, f.lock)
		assert.ErrorIs(t, m.verifyFile(ctx, f.dst, &file), errInjected)
	})
	t.Run("list fails", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		require.NoError(t, f.m.StartVerify(ctx))
		j, _ := f.enq.popVerify()
		f.repo.listErr = errors.New("db down")
		require.Error(t, f.m.RunVerifyBatch(ctx, j.token, j.afterID))
	})
	t.Run("ListFailures of the verification in Status", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		m := NewStorageMigrator(&failingVerifyFailuresRepo{memMigrationRepo: f.repo}, f.local, f.dst, f.enq, f.lock)
		_, err := m.Status(ctx)
		assert.ErrorIs(t, err, errInjected)
	})
}

// failingVerifyFailuresRepo fails ListFailures for the verification only.
type failingVerifyFailuresRepo struct{ *memMigrationRepo }

func (r *failingVerifyFailuresRepo) ListFailures(ctx context.Context, op string, limit int) ([]model.DriveObjectStorageMigrationFailure, error) {
	if op == repository.DriveStorageMigrationOperationVerify {
		return nil, errInjected
	}
	return r.memMigrationRepo.ListFailures(ctx, op, limit)
}

// erroringStorage fails Get for the keys in fail, and the read of the
// objects in readFail.
type erroringStorage struct {
	Storage
	fail       map[string]bool
	readFail   map[string]bool
	deleteFail map[string]bool
}

func (s *erroringStorage) Delete(key string) error {
	if s.deleteFail[key] {
		return errInjected
	}
	return s.Storage.Delete(key)
}

func (s *erroringStorage) Get(key string) (io.ReadCloser, error) {
	if s.fail[key] {
		return nil, errInjected
	}
	if s.readFail[key] {
		return io.NopCloser(failingReader{}), nil
	}
	return s.Storage.Get(key)
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errInjected }

func TestOperationOfToken(t *testing.T) {
	assert.Equal(t, StorageMigrationOperationVerify, operationOfToken(verifyTokenPrefix+"abc"))
	// 接頭辞の無い token (移す処理、#4181 より前の版が取った lock) は移す処理。
	assert.Equal(t, StorageMigrationOperationMigrate, operationOfToken("0123abcd"))
}

func TestRedisStorageMigrationLockHolder(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	l := NewRedisStorageMigrationLock(rdb, "pfx:")

	holder, err := l.Holder(ctx)
	require.NoError(t, err)
	assert.Empty(t, holder)
	ok, err := l.Acquire(ctx, "verify-a", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	holder, err = l.Holder(ctx)
	require.NoError(t, err)
	assert.Equal(t, "verify-a", holder)
	mr.Close()
	_, err = l.Holder(ctx)
	assert.Error(t, err)
}
