package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// locatedStorage is a memObjectStorage that tells where it puts an object,
// as S3Storage does with the bucket and the prefix. Changing location moves
// it, as changing the object storage settings does.
type locatedStorage struct {
	*memObjectStorage
	location string
}

func (s *locatedStorage) ObjectLocation(key string) string {
	return s.location + "/" + key
}

func (f *migrationFixture) cleanup(t *testing.T) {
	t.Helper()
	require.NoError(t, f.m.StartCleanup(context.Background()))
	f.drainCleanup(t)
}

// drainCleanup runs the queued cleanup jobs until none is left.
func (f *migrationFixture) drainCleanup(t *testing.T) {
	t.Helper()
	for range 1000 {
		j, ok := f.enq.popCleanup()
		if !ok {
			return
		}
		require.NoError(t, f.m.RunCleanupBatch(context.Background(), j.token, j.afterID))
	}
	t.Fatal("the jobs did not end")
}

// restoredFixture migrates the files, then moves them back with dst as the
// object storage.
func restoredFixture(t *testing.T, dst func(*memObjectStorage) Storage, ids ...string) *migrationFixture {
	t.Helper()
	f := migratedFixture(t, ids...)
	if dst != nil {
		f.m = NewStorageMigrator(f.repo, f.local, dst(f.dst), f.enq, f.lock)
	}
	f.restore(t)
	return f
}

func variantKeys(ids ...string) []string {
	var out []string
	for _, id := range ids {
		out = append(out, "ak-"+id, "tk-"+id, "wk-"+id)
	}
	return out
}

func TestStorageCleanupDeletesTheCopiesLeftByMovingBack(t *testing.T) {
	ctx := context.Background()
	f := restoredFixture(t, nil, "a", "b")
	// 初めからオブジェクトストレージへアップロードしたファイルは、戻していないので記録に無い。
	direct := "ak-s3"
	f.repo.files = append(f.repo.files, model.DriveFile{ID: "zz-s3", AccessKey: &direct, Size: 1, MD5: "x"})
	f.dst.objects["ak-s3"] = []byte("s3 only")

	for _, k := range variantKeys("a", "b") {
		c, ok := f.repo.copy(k)
		require.True(t, ok, "the copy %s was not recorded", k)
		assert.Equal(t, k, c.Location, k)
		assert.Contains(t, f.dst.objects, k)
	}
	st, err := f.m.Status(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 6, st.RestoredCopies)

	f.cleanup(t)

	for _, k := range variantKeys("a", "b") {
		assert.NotContains(t, f.dst.objects, k, "the copy %s was kept", k)
		_, ok := f.repo.copy(k)
		assert.False(t, ok, "the record of %s was kept", k)
		// ローカルはそのまま。
		assert.Equal(t, "body of "+k, f.localBody(t, k), k)
	}
	assert.Contains(t, f.dst.objects, "ak-s3", "a file uploaded to object storage was deleted")
	held, err := f.lock.Held(ctx)
	require.NoError(t, err)
	assert.False(t, held, "the run did not release the lock")
	st, err = f.m.Status(ctx)
	require.NoError(t, err)
	assert.Zero(t, st.RestoredCopies)
	assert.ErrorIs(t, f.m.StartCleanup(ctx), ErrStorageMigrationNothingToCleanUp)
}

func TestStorageCleanupChainsBatches(t *testing.T) {
	var ids []string
	for i := range 20 {
		ids = append(ids, fmt.Sprintf("f%02d", i))
	}
	f := restoredFixture(t, nil, ids...)
	require.NoError(t, f.m.StartCleanup(context.Background()))
	// 60 件の写しは 1 本のジョブ (50 件) に収まらない。
	var jobs int
	for range 10 {
		j, ok := f.enq.popCleanup()
		if !ok {
			break
		}
		jobs++
		require.NoError(t, f.m.RunCleanupBatch(context.Background(), j.token, j.afterID))
	}
	assert.Equal(t, 2, jobs)
	for _, k := range variantKeys(ids...) {
		assert.NotContains(t, f.dst.objects, k, k)
	}
}

func TestStorageCleanupKeepsACopyWhoseLocationChanged(t *testing.T) {
	ctx := context.Background()
	var loc *locatedStorage
	f := restoredFixture(t, func(s *memObjectStorage) Storage {
		loc = &locatedStorage{memObjectStorage: s, location: "bucket/old"}
		return loc
	}, "a")
	c, ok := f.repo.copy("ak-a")
	require.True(t, ok)
	assert.Equal(t, "bucket/old/ak-a", c.Location)

	// prefix を変えた後は、同じアクセスキーでも別の場所になる。消さずに失敗として残す。
	loc.location = "bucket/new"
	f.cleanup(t)
	for _, k := range variantKeys("a") {
		assert.Contains(t, f.dst.objects, k, "the copy %s at another location was deleted", k)
		c, ok := f.repo.copy(k)
		require.True(t, ok, k)
		require.NotNil(t, c.Error, k)
		assert.Contains(t, *c.Error, "settings changed", k)
	}
	st, err := f.m.Status(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 3, st.RestoredCopies)
	assert.EqualValues(t, 3, st.CleanupFailed)
	assert.Len(t, st.CleanupFailures, 3)

	// 設定を戻せば消せる。
	loc.location = "bucket/old"
	f.cleanup(t)
	for _, k := range variantKeys("a") {
		assert.NotContains(t, f.dst.objects, k, k)
	}
}

func TestStorageCleanupKeepsACopyServedAgain(t *testing.T) {
	f := restoredFixture(t, nil, "a")
	// 記録が残ったまま、行がオブジェクトストレージを指している (写す前の DB のバックアップへ
	// 戻したなど)。写しは配信中の実体なので消さず、記録だけを外す。
	for i := range f.repo.files {
		if f.repo.files[i].ID == "a" {
			f.repo.files[i].StoredInternal = false
		}
	}
	f.cleanup(t)
	for _, k := range variantKeys("a") {
		assert.Contains(t, f.dst.objects, k, "the copy %s served from object storage was deleted", k)
		_, ok := f.repo.copy(k)
		assert.False(t, ok, "the record of %s was kept", k)
	}
}

func TestStorageCleanupRecordsAFailedDeletionAndRetries(t *testing.T) {
	ctx := context.Background()
	f := restoredFixture(t, nil, "a", "b")
	f.dst.deleteErr["ak-a"] = errors.New("s3 down")
	f.cleanup(t)
	c, ok := f.repo.copy("ak-a")
	require.True(t, ok)
	require.NotNil(t, c.Error)
	assert.Contains(t, *c.Error, "s3 down")
	assert.Contains(t, f.dst.objects, "ak-a")
	// 失敗した 1 件で全体を止めない。
	for _, k := range append(variantKeys("b"), "tk-a", "wk-a") {
		assert.NotContains(t, f.dst.objects, k, k)
	}
	st, err := f.m.Status(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, st.CleanupFailed)

	delete(f.dst.deleteErr, "ak-a")
	f.cleanup(t)
	assert.NotContains(t, f.dst.objects, "ak-a")
	_, ok = f.repo.copy("ak-a")
	assert.False(t, ok)
}

func TestStorageMigrationDropsTheCopiesOfAFileMovedAgain(t *testing.T) {
	ctx := context.Background()
	f := restoredFixture(t, nil, "a")
	// 戻した後にもう一度移すと、同じキーへ写し直すので、記録の写しが配信する実体になる。
	require.NoError(t, f.m.Start(ctx))
	f.drain(t)
	require.False(t, f.repo.file("a").StoredInternal)
	for _, k := range variantKeys("a") {
		_, ok := f.repo.copy(k)
		assert.False(t, ok, "the record of %s was kept after moving the file again", k)
	}
	assert.ErrorIs(t, f.m.StartCleanup(ctx), ErrStorageMigrationNothingToCleanUp)
	for _, k := range variantKeys("a") {
		assert.Contains(t, f.dst.objects, k, k)
	}
}

func TestStorageCleanupStartRefuses(t *testing.T) {
	ctx := context.Background()
	t.Run("no queue", func(t *testing.T) {
		f := restoredFixture(t, nil, "a")
		m := NewStorageMigrator(f.repo, f.local, f.dst, nil, f.lock)
		assert.ErrorIs(t, m.StartCleanup(ctx), ErrStorageMigrationNoQueue)
	})
	t.Run("object storage not configured", func(t *testing.T) {
		f := restoredFixture(t, nil, "a")
		m := NewStorageMigrator(f.repo, f.local, f.local, f.enq, f.lock)
		assert.ErrorIs(t, m.StartCleanup(ctx), ErrStorageMigrationNotConfigured)
	})
	t.Run("nothing to clean up", func(t *testing.T) {
		f := migratedFixture(t, "a")
		assert.ErrorIs(t, f.m.StartCleanup(ctx), ErrStorageMigrationNothingToCleanUp)
	})
	t.Run("count fails", func(t *testing.T) {
		f := restoredFixture(t, nil, "a")
		m := NewStorageMigrator(&failingMigrationRepo{memMigrationRepo: f.repo, fail: "Counts"}, f.local, f.dst, f.enq, f.lock)
		assert.ErrorIs(t, m.StartCleanup(ctx), errInjected)
	})
	t.Run("already running", func(t *testing.T) {
		f := restoredFixture(t, nil, "a")
		require.NoError(t, f.m.StartCleanup(ctx))
		// 同じ lock なので、ほかの処理とも二重には走らない。
		assert.ErrorIs(t, f.m.StartCleanup(ctx), ErrStorageMigrationRunning)
		assert.ErrorIs(t, f.m.Start(ctx), ErrStorageMigrationRunning)
		st, err := f.m.Status(ctx)
		require.NoError(t, err)
		assert.Equal(t, StorageMigrationOperationCleanup, st.Running)
		running, err := f.m.Running(ctx)
		require.NoError(t, err)
		assert.False(t, running, "the cleanup was counted as a migration run")
	})
}

func TestStorageCleanupRunBatchStops(t *testing.T) {
	ctx := context.Background()
	t.Run("the lock is lost", func(t *testing.T) {
		f := restoredFixture(t, nil, "a")
		require.NoError(t, f.m.StartCleanup(ctx))
		j, ok := f.enq.popCleanup()
		require.True(t, ok)
		f.lock.expire()
		require.NoError(t, f.m.RunCleanupBatch(ctx, j.token, j.afterID))
		assert.Contains(t, f.dst.objects, "ak-a", "a job of a run without the lock deleted a copy")
	})
	t.Run("object storage was turned off", func(t *testing.T) {
		f := restoredFixture(t, nil, "a")
		require.NoError(t, f.m.StartCleanup(ctx))
		j, ok := f.enq.popCleanup()
		require.True(t, ok)
		m := NewStorageMigrator(f.repo, f.local, f.local, f.enq, f.lock)
		require.NoError(t, m.RunCleanupBatch(ctx, j.token, j.afterID))
		held, err := f.lock.Held(ctx)
		require.NoError(t, err)
		assert.False(t, held, "the run kept the lock")
		assert.Contains(t, f.dst.objects, "ak-a")
	})
}

func TestStorageCleanupRunBatchErrors(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"ListRestoredCopies", "FindDriveFile", "DeleteRestoredCopy"} {
		t.Run(name, func(t *testing.T) {
			f := restoredFixture(t, nil, "a")
			require.NoError(t, f.m.StartCleanup(ctx))
			j, ok := f.enq.popCleanup()
			require.True(t, ok)
			m := NewStorageMigrator(&failingMigrationRepo{memMigrationRepo: f.repo, fail: name}, f.local, f.dst, f.enq, f.lock)
			// DB に読み書きできないときは、失敗として残さずジョブごとやり直す。
			assert.ErrorIs(t, m.RunCleanupBatch(ctx, j.token, j.afterID), errInjected)
			c, ok := f.repo.copy("ak-a")
			require.True(t, ok)
			assert.Nil(t, c.Error)
		})
	}
	t.Run("RecordRestoredCopyFailure", func(t *testing.T) {
		f := restoredFixture(t, nil, "a")
		f.dst.deleteErr["ak-a"] = errors.New("s3 down")
		require.NoError(t, f.m.StartCleanup(ctx))
		j, ok := f.enq.popCleanup()
		require.True(t, ok)
		m := NewStorageMigrator(&failingMigrationRepo{memMigrationRepo: f.repo, fail: "RecordRestoredCopyFailure"}, f.local, f.dst, f.enq, f.lock)
		assert.ErrorIs(t, m.RunCleanupBatch(ctx, j.token, j.afterID), errInjected)
	})
	t.Run("Status lists the failures", func(t *testing.T) {
		f := restoredFixture(t, nil, "a")
		m := NewStorageMigrator(&failingMigrationRepo{memMigrationRepo: f.repo, fail: "ListRestoredCopyFailures"}, f.local, f.dst, f.enq, f.lock)
		_, err := m.Status(ctx)
		assert.ErrorIs(t, err, errInjected)
	})
}

func TestStorageMigratorReleaseRestoredCopies(t *testing.T) {
	ctx := context.Background()
	t.Run("deletes the copies of the file", func(t *testing.T) {
		f := restoredFixture(t, nil, "a", "b")
		f.m.ReleaseRestoredCopies(ctx, "a")
		for _, k := range variantKeys("a") {
			assert.NotContains(t, f.dst.objects, k, k)
			_, ok := f.repo.copy(k)
			assert.False(t, ok, k)
		}
		for _, k := range variantKeys("b") {
			assert.Contains(t, f.dst.objects, k, "a copy of another file was deleted")
		}
	})
	t.Run("keeps the records while object storage is off", func(t *testing.T) {
		f := restoredFixture(t, nil, "a")
		m := NewStorageMigrator(f.repo, f.local, f.local, f.enq, f.lock)
		m.ReleaseRestoredCopies(ctx, "a")
		for _, k := range variantKeys("a") {
			assert.Contains(t, f.dst.objects, k, k)
			c, ok := f.repo.copy(k)
			require.True(t, ok, k)
			assert.Nil(t, c.Error, k)
		}
	})
	t.Run("records a copy it cannot delete", func(t *testing.T) {
		var loc *locatedStorage
		f := restoredFixture(t, func(s *memObjectStorage) Storage {
			loc = &locatedStorage{memObjectStorage: s, location: "bucket/old"}
			return loc
		}, "a")
		loc.location = "bucket/new"
		f.m.ReleaseRestoredCopies(ctx, "a")
		c, ok := f.repo.copy("ak-a")
		require.True(t, ok)
		require.NotNil(t, c.Error)
		assert.Contains(t, f.dst.objects, "ak-a")
	})
	t.Run("a file without copies", func(t *testing.T) {
		f := migratedFixture(t, "a")
		f.m.ReleaseRestoredCopies(ctx, "a")
		assert.Contains(t, f.dst.objects, "ak-a", "a migrated file lost its copy")
	})
	t.Run("the list fails", func(t *testing.T) {
		f := restoredFixture(t, nil, "a")
		m := NewStorageMigrator(&failingMigrationRepo{memMigrationRepo: f.repo, fail: "ListRestoredCopiesOfFiles"}, f.local, f.dst, f.enq, f.lock)
		m.ReleaseRestoredCopies(ctx, "a")
		assert.Contains(t, f.dst.objects, "ak-a")
	})
}

type recordingReleaser struct {
	ids   []string
	calls int
}

func (r *recordingReleaser) ReleaseRestoredCopiesOfFiles(_ context.Context, fileIDs []string) {
	r.calls++
	r.ids = append(r.ids, fileIDs...)
}

// 戻したファイルの写しを消すのは、ローカルの行を消したときだけ (#4197)。
func TestDeleteMigrationCopiesReleasesTheRestoredCopies(t *testing.T) {
	r := &recordingReleaser{}
	s := &Service{}
	s.SetRestoredCopyReleaser(r)
	key := "ak"
	s.deleteMigrationCopies(&model.DriveFile{ID: "local", StoredInternal: true, AccessKey: &key})
	s.deleteMigrationCopies(&model.DriveFile{ID: "object", AccessKey: &key})
	s.deleteMigrationCopies(&model.DriveFile{ID: "link", IsLink: true, AccessKey: &key})
	// link 行は storedInternal が true でも実体を持たない。
	s.deleteMigrationCopies(&model.DriveFile{ID: "locallink", StoredInternal: true, IsLink: true, AccessKey: &key})
	assert.Equal(t, []string{"local"}, r.ids)
}

// keyRecordingEnqueuer records the keys queued for deletion from object
// storage.
type keyRecordingEnqueuer struct{ keys []string }

func (e *keyRecordingEnqueuer) EnqueueDeleteObjectStorageFile(key string) error {
	e.keys = append(e.keys, key)
	return nil
}

// deleteBatchFixture is a batch of a deleted account's files: a and b were
// moved back and have copies in object storage, d is local without copies, e
// is in object storage, x is in object storage again with a stale record of
// a copy, and l is a link row. c was moved back too but is not in the batch.
type deleteBatchFixture struct {
	*migrationFixture
	svc   *Service
	enq   *keyRecordingEnqueuer
	asked int
	batch []*model.DriveFile
}

func newDeleteBatchFixture(t *testing.T) *deleteBatchFixture {
	t.Helper()
	f := &deleteBatchFixture{migrationFixture: restoredFixture(t, nil, "a", "b", "c"), enq: &keyRecordingEnqueuer{}}
	f.addLocal(t, "d")
	ek, xk, lk := "ak-e", "ak-x", "ak-l"
	f.dst.objects[ek] = []byte("e")
	f.dst.objects[xk] = []byte("x")
	f.repo.files = append(f.repo.files,
		model.DriveFile{ID: "e", AccessKey: &ek},
		model.DriveFile{ID: "x", AccessKey: &xk},
		model.DriveFile{ID: "l", IsLink: true, AccessKey: &lk})
	f.repo.copies[xk] = model.DriveObjectStorageRestoredCopy{Key: xk, FileID: "x", Location: xk}
	for _, id := range []string{"a", "b", "d", "e", "x", "l"} {
		row := f.repo.file(id)
		require.Equal(t, id, row.ID)
		f.batch = append(f.batch, &row)
	}
	f.svc = &Service{storage: f.dst, localStorage: f.local}
	f.svc.SetObjectDeleteEnqueuer(f.enq)
	f.svc.SetRestoredCopyReleaser(f.m)
	f.svc.SetStorageMigrationRunning(func() bool {
		f.asked++
		return true
	})
	f.repo.copyLists = 0
	return f
}

// state returns what is left: the objects in object storage, the recorded
// copies, the queued deletions and the local objects of the batch.
func (f *deleteBatchFixture) state(t *testing.T) (objects, copies, queued, local []string) {
	t.Helper()
	for k := range f.dst.objects {
		objects = append(objects, k)
	}
	for k := range f.repo.copies {
		copies = append(copies, k)
	}
	queued = slices.Clone(f.enq.keys)
	for _, k := range variantKeys("a", "b", "c", "d") {
		if f.localBody(t, k) != "" {
			local = append(local, k)
		}
	}
	slices.Sort(objects)
	slices.Sort(copies)
	slices.Sort(queued)
	return objects, copies, queued, local
}

// アカウントの削除はバッチの戻した写しを 1 本で引き、移す処理が実行中かを 1 回だけ聞く。
// 消す写しは 1 件ずつ消したときと同じ (#4267)。
func TestDeleteFilesObjectsReleasesTheRestoredCopiesOfTheBatch(t *testing.T) {
	one := newDeleteBatchFixture(t)
	for _, f := range one.batch {
		require.Equal(t, []error{nil}, one.svc.DeleteFilesObjects([]*model.DriveFile{f}), f.ID)
	}
	batch := newDeleteBatchFixture(t)
	errs := batch.svc.DeleteFilesObjects(batch.batch)
	require.Len(t, errs, len(batch.batch))
	for i, err := range errs {
		require.NoError(t, err, batch.batch[i].ID)
	}
	assert.Equal(t, 1, batch.repo.copyLists, "the restored copies were not read once for the batch")
	assert.Equal(t, 1, batch.asked, "whether the migration runs was not asked once for the batch")

	objects, copies, queued, local := batch.state(t)
	// a と b の写しだけを消す。c はバッチに無く、x はオブジェクトストレージから配っているので残す。
	assert.Equal(t, []string{"ak-c", "ak-e", "ak-x", "tk-c", "wk-c"}, objects)
	assert.Equal(t, []string{"ak-c", "ak-x", "tk-c", "wk-c"}, copies)
	// e と x の本体と、移す処理の実行中に消したローカルの行 (a, b, d) の写しの候補。
	assert.Equal(t, []string{"ak-a", "ak-b", "ak-d", "ak-e", "ak-x", "tk-a", "tk-b", "tk-d", "wk-a", "wk-b", "wk-d"}, queued)
	assert.Equal(t, variantKeys("c"), local)

	oObjects, oCopies, oQueued, oLocal := one.state(t)
	assert.Equal(t, oObjects, objects, "the batch left other objects than one by one")
	assert.Equal(t, oCopies, copies, "the batch left other records than one by one")
	assert.Equal(t, oQueued, queued, "the batch queued other deletions than one by one")
	assert.Equal(t, oLocal, local, "the batch left other local objects than one by one")
}

// 移す処理が実行中かは、答えが要るローカルの行があるときだけ聞く (#4267)。
func TestDeleteFilesObjectsAsksWhetherTheMigrationRunsOnlyWhenNeeded(t *testing.T) {
	f := newDeleteBatchFixture(t)
	var objectOnly []*model.DriveFile
	for _, file := range f.batch {
		if !file.StoredInternal {
			objectOnly = append(objectOnly, file)
		}
	}
	require.Len(t, objectOnly, 3)
	f.svc.DeleteFilesObjects(objectOnly)
	assert.Zero(t, f.asked)
	assert.Zero(t, f.repo.copyLists, "the restored copies were read for a batch without local rows")
	assert.Empty(t, f.svc.DeleteFilesObjects(nil))
}

// 本体を消せなかったファイルも、バッチの中で写しを消す (1 件ずつ消していたときと同じ)。
func TestDeleteFilesObjectsReleasesTheCopiesOfAFileItCannotDelete(t *testing.T) {
	f := newDeleteBatchFixture(t)
	f.svc.localStorage = failingLocalDelete{f.local, "ak-a"}
	errs := f.svc.DeleteFilesObjects(f.batch)
	require.Error(t, errs[0])
	for _, err := range errs[1:] {
		assert.NoError(t, err)
	}
	for _, k := range variantKeys("a", "b") {
		assert.NotContains(t, f.dst.objects, k, "the copy %s was kept", k)
	}
}

// failingLocalDelete fails the deletion of key.
type failingLocalDelete struct {
	*LocalStorage
	key string
}

func (s failingLocalDelete) Delete(key string) error {
	if key == s.key {
		return errors.New("disk error")
	}
	return s.LocalStorage.Delete(key)
}

// ローカルとバケットが同じ実体なら、写しを消すとローカルも消える。何も消さずに止める (#4197)。
func TestStorageCleanupRefusesWhenTheLocalStorageIsTheBucket(t *testing.T) {
	ctx := context.Background()
	t.Run("FUSE", func(t *testing.T) {
		f := restoredFixture(t, nil, "a")
		f.m.onFUSE = func(string) (bool, error) { return true, nil }
		f.cleanup(t)
		for _, k := range variantKeys("a") {
			assert.Contains(t, f.dst.objects, k, "a copy was deleted from a bucket mounted as the local storage")
			_, ok := f.repo.copy(k)
			assert.True(t, ok, k)
		}
		held, err := f.lock.Held(ctx)
		require.NoError(t, err)
		assert.False(t, held, "the refused run kept the lock")
	})
	t.Run("the object storage serves the local files", func(t *testing.T) {
		f := restoredFixture(t, nil, "a")
		// 戻した後で、オブジェクトストレージがローカルのディレクトリを配る構成に替わった。
		f.m = NewStorageMigrator(f.repo, f.local, &sharedStorage{Storage: f.local}, f.enq, f.lock)
		f.m.onFUSE = func(string) (bool, error) { return false, nil }
		// 場所の比較は通るよう、記録の場所を今の場所にそろえる。
		for _, k := range variantKeys("a") {
			c, _ := f.repo.copy(k)
			c.Location = k
			f.repo.copies[k] = c
		}
		f.cleanup(t)
		for _, k := range variantKeys("a") {
			assert.Equal(t, "body of "+k, f.localBody(t, k), "the local copy %s was deleted", k)
		}
	})
	t.Run("the probe fails", func(t *testing.T) {
		f := restoredFixture(t, nil, "a")
		f.m.onFUSE = func(string) (bool, error) { return false, errInjected }
		require.NoError(t, f.m.StartCleanup(ctx))
		j, ok := f.enq.popCleanup()
		require.True(t, ok)
		assert.ErrorIs(t, f.m.RunCleanupBatch(ctx, j.token, j.afterID), errInjected)
		assert.Contains(t, f.dst.objects, "ak-a")
	})
}

// バケットの写しが最後の実体になっているときは消さない (#4197)。
func TestStorageCleanupKeepsTheOnlyCopy(t *testing.T) {
	ctx := context.Background()
	f := restoredFixture(t, nil, "gone", "broken", "samesize", "thumbgone", "ok")
	// ローカルの本体が無い (ローカルを消した世代の DB へ戻した、ディスクを失ったなど)。
	require.NoError(t, f.local.Delete("ak-gone"))
	// ローカルの本体が記録と違う。
	_, err := f.local.Put("ak-broken", strings.NewReader("broken"))
	require.NoError(t, err)
	// 大きさは記録と同じで、中身だけ違う (md5 で見分ける)。
	_, err = f.local.Put("ak-samesize", strings.NewReader(strings.Repeat("x", len("body of ak-samesize"))))
	require.NoError(t, err)
	// ローカルのサムネイルが無い。
	require.NoError(t, f.local.Delete("tk-thumbgone"))
	f.cleanup(t)
	for _, k := range []string{"ak-gone", "ak-broken", "ak-samesize", "tk-thumbgone"} {
		assert.Contains(t, f.dst.objects, k, "the only copy %s was deleted", k)
		c, ok := f.repo.copy(k)
		require.True(t, ok, k)
		require.NotNil(t, c.Error, k)
		assert.Contains(t, *c.Error, "the local copy is missing or does not match", k)
	}
	// 同じファイルでも、ローカルに正しい実体のあるキーは消す。
	for _, k := range append(variantKeys("ok"), "tk-gone", "wk-gone", "ak-thumbgone") {
		assert.NotContains(t, f.dst.objects, k, k)
	}
	st, err := f.m.Status(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 4, st.CleanupFailed)

	// 消された (行の無い) ファイルの写しは、ローカルが無くても消す。
	f.repo.files = slices.DeleteFunc(f.repo.files, func(x model.DriveFile) bool { return x.ID == "gone" })
	f.cleanup(t)
	assert.NotContains(t, f.dst.objects, "ak-gone")
	assert.Contains(t, f.dst.objects, "ak-broken")
}

// 写している間に lock が別の実行に取られたら、行を倒さない (#4197)。
func TestStorageMigrationDoesNotMarkAfterTheLockWasTaken(t *testing.T) {
	ctx := context.Background()
	f := newMigrationFixture(t)
	f.addLocal(t, "a")
	require.NoError(t, f.m.Start(ctx))
	j, ok := f.enq.pop()
	require.True(t, ok)
	// 本体を写している間に lock が切れ、写しを消す処理が lock を取った。
	f.m = NewStorageMigrator(f.repo, f.local, &hookPutStorage{Storage: f.dst, onPut: func() {
		f.lock.expire()
		got, err := f.lock.Acquire(ctx, cleanupTokenPrefix+"other", storageMigrationLockTTL)
		require.NoError(t, err)
		require.True(t, got)
	}}, f.enq, f.lock)
	require.NoError(t, f.m.RunBatch(ctx, j.token, j.afterID))
	assert.True(t, f.repo.file("a").StoredInternal, "the row was marked as migrated without the lock")
	assert.Nil(t, f.repo.file("a").ObjectStorageMigratedAt)

	// 期限が切れただけ (誰も取っていない) なら、取り直して倒す。
	f2 := newMigrationFixture(t)
	f2.addLocal(t, "a")
	require.NoError(t, f2.m.Start(ctx))
	j, ok = f2.enq.pop()
	require.True(t, ok)
	var once bool
	f2.m = NewStorageMigrator(f2.repo, f2.local, &hookPutStorage{Storage: f2.dst, onPut: func() {
		if !once {
			once = true
			f2.lock.expire()
		}
	}}, f2.enq, f2.lock)
	require.NoError(t, f2.m.RunBatch(ctx, j.token, j.afterID))
	assert.False(t, f2.repo.file("a").StoredInternal)
}

// hookPutStorage calls onPut before each Put.
type hookPutStorage struct {
	Storage
	onPut func()
}

func (s *hookPutStorage) Put(key string, body io.Reader) (string, error) {
	s.onPut()
	return s.Storage.Put(key, body)
}

// 写しの場所はバケットと、prefix を含めたオブジェクトのキー。prefix の末尾の "/" の有無では
// 変わらない (#4197)。
func TestS3StorageObjectLocation(t *testing.T) {
	for _, tc := range []struct{ prefix, want string }{
		{"", "b/ak"},
		{"files", "b/files/ak"},
		{"files/", "b/files/ak"},
	} {
		st := NewS3Storage(S3StorageConfig{Bucket: "b", Prefix: tc.prefix})
		assert.Equal(t, tc.want, st.ObjectLocation("ak"), tc.prefix)
		assert.Equal(t, tc.want, objectLocation(st, "ak"), tc.prefix)
	}
	assert.NotEqual(t, NewS3Storage(S3StorageConfig{Bucket: "a"}).ObjectLocation("k"), NewS3Storage(S3StorageConfig{Bucket: "b"}).ObjectLocation("k"))
}

// 移行が使う保存先 (設定に従う MetaStorage) を解決すると、S3 のストレージになり、場所に
// バケットが入る。解決しないまま比べると、場所がキーだけになり、バケットや prefix を変えた
// ことを見分けられない。
func TestStorageMigratorLocatesThroughMetaStorage(t *testing.T) {
	meta := objectStorageMeta()
	prefix := "files/"
	meta.ObjectStoragePrefix = &prefix
	primary := NewMetaStorage(func() (*model.Meta, error) { return meta, nil }, t.TempDir(), "https://example.com/files")
	m := NewStorageMigrator(newMemMigrationRepo(), NewLocalStorage(t.TempDir(), "https://example.com/files"), primary, &recordingMigrationEnqueuer{}, &memMigrationLock{})
	dst := m.objectStorage()
	require.NotNil(t, dst)
	assert.Equal(t, "my-bucket/files/ak", objectLocation(dst, "ak"))
}

// hookAfterPutStorage calls onPut after the Put of key.
type hookAfterPutStorage struct {
	Storage
	key   string
	onPut func()
}

func (s *hookAfterPutStorage) Put(key string, body io.Reader) (string, error) {
	url, err := s.Storage.Put(key, body)
	if key == s.key {
		s.onPut()
	}
	return url, err
}

// lock が切れている間に写しを消す処理が始まって終わると、移す処理は lock を取り直せるが、
// 置いた写しは消えている。倒さずに失敗として残す (#4197)。
func TestStorageMigrationChecksTheCopiesAfterTakingTheLockBack(t *testing.T) {
	ctx := context.Background()
	f := restoredFixture(t, nil, "a")
	require.NoError(t, f.m.Start(ctx))
	j, ok := f.enq.pop()
	require.True(t, ok)
	cleaner := f.m
	f.m = NewStorageMigrator(f.repo, f.local, &hookAfterPutStorage{Storage: f.dst, key: "wk-a", onPut: func() {
		f.lock.expire()
		require.NoError(t, cleaner.StartCleanup(ctx))
		for range 10 {
			cj, ok := f.enq.popCleanup()
			if !ok {
				break
			}
			require.NoError(t, cleaner.RunCleanupBatch(ctx, cj.token, cj.afterID))
		}
	}}, f.enq, f.lock)
	require.NoError(t, f.m.RunBatch(ctx, j.token, j.afterID))
	assert.True(t, f.repo.file("a").StoredInternal, "the row was marked as migrated although its copies were deleted")
	assert.Contains(t, f.repo.failures["a/"+repository.DriveStorageMigrationOperationMigrate].Error, "removed or changed while the lock was lost")
}

// 消している途中のファイルは、ローカルを先に消していても、写しを消す (#4197)。
func TestStorageMigratorReleaseRestoredCopiesAfterTheLocalCopyIsGone(t *testing.T) {
	f := restoredFixture(t, nil, "a")
	for _, k := range variantKeys("a") {
		require.NoError(t, f.local.Delete(k))
	}
	f.m.ReleaseRestoredCopies(context.Background(), "a")
	for _, k := range variantKeys("a") {
		assert.NotContains(t, f.dst.objects, k, "the copy %s of a deleted file was kept", k)
		_, ok := f.repo.copy(k)
		assert.False(t, ok, k)
	}
}

// hookGetStorage calls onGet before the Get of key.
type hookGetStorage struct {
	Storage
	key   string
	onGet func()
}

func (s *hookGetStorage) Get(key string) (io.ReadCloser, error) {
	if key == s.key {
		s.onGet()
	}
	return s.Storage.Get(key)
}

// ローカルを読んでいる間に lock が別の実行に取られたら、写しを消さずに止める (#4197)。
func TestStorageCleanupStopsWhenTheLockIsTakenWhileReading(t *testing.T) {
	ctx := context.Background()
	f := restoredFixture(t, nil, "a")
	f.m = NewStorageMigrator(f.repo, &hookGetStorage{Storage: f.local, key: "ak-a", onGet: func() {
		f.lock.expire()
		got, err := f.lock.Acquire(ctx, "other", storageMigrationLockTTL)
		require.NoError(t, err)
		require.True(t, got)
	}}, f.dst, f.enq, f.lock)
	require.NoError(t, f.m.StartCleanup(ctx))
	j, ok := f.enq.popCleanup()
	require.True(t, ok)
	require.NoError(t, f.m.RunCleanupBatch(ctx, j.token, j.afterID))
	assert.Contains(t, f.dst.objects, "ak-a", "the copy was deleted after another run took the lock")
	c, ok := f.repo.copy("ak-a")
	require.True(t, ok)
	assert.Nil(t, c.Error, "a stopped run recorded a failure")
}

// 取り直した後の確かめ直しは、本体の中身 (md5) まで見る。
func TestStorageMigrationChecksTheBodyAfterTakingTheLockBack(t *testing.T) {
	ctx := context.Background()
	f := newMigrationFixture(t)
	f.addLocal(t, "a")
	require.NoError(t, f.m.Start(ctx))
	j, ok := f.enq.pop()
	require.True(t, ok)
	f.m = NewStorageMigrator(f.repo, f.local, &hookAfterPutStorage{Storage: f.dst, key: "wk-a", onPut: func() {
		f.lock.expire()
		f.dst.objects["ak-a"] = []byte(strings.Repeat("x", len("body of ak-a")))
	}}, f.enq, f.lock)
	require.NoError(t, f.m.RunBatch(ctx, j.token, j.afterID))
	assert.True(t, f.repo.file("a").StoredInternal, "the row was marked as migrated with a changed copy")
	// 記録の無い写しを残さない。
	for _, k := range variantKeys("a") {
		assert.NotContains(t, f.dst.objects, k, "the copy %s was left without a record", k)
	}
}

// 取り直した後の確かめ直しは、サムネイルと webpublic の有無も見る。
func TestStorageMigrationChecksTheVariantsAfterTakingTheLockBack(t *testing.T) {
	ctx := context.Background()
	f := newMigrationFixture(t)
	f.addLocal(t, "a")
	require.NoError(t, f.m.Start(ctx))
	j, ok := f.enq.pop()
	require.True(t, ok)
	f.m = NewStorageMigrator(f.repo, f.local, &hookAfterPutStorage{Storage: f.dst, key: "wk-a", onPut: func() {
		f.lock.expire()
		delete(f.dst.objects, "tk-a")
	}}, f.enq, f.lock)
	require.NoError(t, f.m.RunBatch(ctx, j.token, j.afterID))
	assert.True(t, f.repo.file("a").StoredInternal, "the row was marked as migrated without its thumbnail")
}

// eventStorage records the deletions from the local storage in events.
type eventStorage struct {
	Storage
	events *[]string
}

func (s eventStorage) Delete(key string) error {
	*s.events = append(*s.events, "delete "+key)
	return s.Storage.Delete(key)
}

// eventEnqueuer records the queued deletions in events.
type eventEnqueuer struct{ events *[]string }

func (e eventEnqueuer) EnqueueDeleteObjectStorageFile(key string) error {
	*e.events = append(*e.events, "queue "+key)
	return nil
}

// eventReleaser records in events when the restored copies are released.
type eventReleaser struct {
	RestoredCopiesReleaser
	events *[]string
}

func (r eventReleaser) ReleaseRestoredCopiesOfFiles(ctx context.Context, fileIDs []string) {
	*r.events = append(*r.events, "release")
	r.RestoredCopiesReleaser.ReleaseRestoredCopiesOfFiles(ctx, fileIDs)
}

// 移す処理が実行中かは、バッチの本体を消し終えた直後、戻した写しを消すより前に読む。後で
// 読むと、移す処理が終わったときに写しを消すジョブを積み損ねる窓が、戻した写しを消す時間
// だけ広がる (#4267)。
func TestDeleteFilesObjectsAsksWhetherTheMigrationRunsBeforeReleasingTheCopies(t *testing.T) {
	f := newDeleteBatchFixture(t)
	var events []string
	f.svc.localStorage = eventStorage{Storage: f.local, events: &events}
	f.svc.SetObjectDeleteEnqueuer(eventEnqueuer{events: &events})
	f.svc.SetRestoredCopyReleaser(eventReleaser{RestoredCopiesReleaser: f.m, events: &events})
	f.svc.SetStorageMigrationRunning(func() bool {
		events = append(events, "asked")
		return true
	})
	f.svc.DeleteFilesObjects(f.batch)

	asked := slices.Index(events, "asked")
	require.NotEqual(t, -1, asked, "%v", events)
	assert.Equal(t, 1, countOf(events, "asked"), "asked more than once: %v", events)
	release := slices.Index(events, "release")
	require.NotEqual(t, -1, release, "%v", events)
	assert.Less(t, asked, release, "asked after releasing the restored copies: %v", events)
	primaries := append(variantKeys("a", "b", "d"), "ak-e", "ak-x")
	for _, k := range primaries {
		i := slices.IndexFunc(events, func(e string) bool { return e == "delete "+k || e == "queue "+k })
		require.NotEqual(t, -1, i, "the primary %s was not deleted: %v", k, events)
		assert.Less(t, i, asked, "asked before deleting the primary %s: %v", k, events)
	}
}

// releaseStepKey tags the context that releaseTimeout made: one step of
// ReleaseRestoredCopiesOfFiles for several files, or the whole release for one.
type releaseStepKey struct{}

// countReleaseSteps makes m tag the context of each step with its number,
// and returns the number of steps.
func countReleaseSteps(m *StorageMigrator) *int {
	steps := 0
	next := m.releaseTimeout
	m.releaseTimeout = func(ctx context.Context) (context.Context, context.CancelFunc) {
		steps++
		return next(context.WithValue(ctx, releaseStepKey{}, steps))
	}
	return &steps
}

// ctxRecordingRepo records the context each copy is deleted with.
type ctxRecordingRepo struct {
	*memMigrationRepo
	finds []ctxRecord
}

type ctxRecord struct {
	fileID   string
	step     any
	deadline time.Duration
	hasLimit bool
}

func (r *ctxRecordingRepo) FindDriveFile(ctx context.Context, id string) (*model.DriveFile, error) {
	dl, ok := ctx.Deadline()
	r.finds = append(r.finds, ctxRecord{fileID: id, step: ctx.Value(releaseStepKey{}), deadline: time.Until(dl), hasLimit: ok})
	return r.memMigrationRepo.FindDriveFile(ctx, id)
}

// stepsByFile returns the step each file's copies were deleted in, failing
// when the copies of one file were deleted in different steps.
func (r *ctxRecordingRepo) stepsByFile(t *testing.T) map[string]any {
	t.Helper()
	steps := map[string]any{}
	for _, c := range r.finds {
		if s, ok := steps[c.fileID]; ok {
			assert.Equal(t, s, c.step, "the copies of %s were deleted in different steps", c.fileID)
		}
		steps[c.fileID] = c.step
	}
	return steps
}

// 写しを消す時間の上限は、バッチ全体でなくファイルごとに置く。まとめると、写しの多い
// バッチで後ろのファイルの写しが時間切れで残る (#4267)。
func TestReleaseRestoredCopiesOfFilesBoundsEachFile(t *testing.T) {
	f := restoredFixture(t, nil, "a", "b")
	repo := &ctxRecordingRepo{memMigrationRepo: f.repo}
	m := NewStorageMigrator(repo, f.local, f.dst, f.enq, f.lock)
	countReleaseSteps(m)
	m.ReleaseRestoredCopiesOfFiles(context.Background(), []string{"a", "b"})

	require.Len(t, repo.finds, 6)
	for _, r := range repo.finds {
		require.True(t, r.hasLimit, "the copy of %s was deleted without a time limit", r.fileID)
		assert.Positive(t, r.deadline, r.fileID)
		assert.LessOrEqual(t, r.deadline, restoredCopyReleaseTimeout, r.fileID)
	}
	// 1 は一覧の取得。続くファイルごとに上限を取り直す。
	assert.Equal(t, map[string]any{"a": 2, "b": 3}, repo.stepsByFile(t))
	for _, k := range variantKeys("a", "b") {
		assert.NotContains(t, f.dst.objects, k, k)
	}
}

// 1 件のファイルでは、一覧と写しの削除を 1 つの上限 (30 秒) で包む。段ごとに取り直すと、
// 利用者の削除の待ちが最悪で倍になる (#4267)。
func TestReleaseRestoredCopiesOfOneFileBoundsTheWhole(t *testing.T) {
	for name, release := range map[string]func(m *StorageMigrator){
		"ReleaseRestoredCopies":        func(m *StorageMigrator) { m.ReleaseRestoredCopies(context.Background(), "a") },
		"ReleaseRestoredCopiesOfFiles": func(m *StorageMigrator) { m.ReleaseRestoredCopiesOfFiles(context.Background(), []string{"a"}) },
	} {
		t.Run(name, func(t *testing.T) {
			f := restoredFixture(t, nil, "a", "b")
			repo := &ctxRecordingRepo{memMigrationRepo: f.repo}
			m := NewStorageMigrator(repo, f.local, f.dst, f.enq, f.lock)
			steps := countReleaseSteps(m)
			release(m)

			require.Len(t, repo.finds, 3)
			for _, r := range repo.finds {
				require.True(t, r.hasLimit, "the copy of %s was deleted without a time limit", r.fileID)
				assert.LessOrEqual(t, r.deadline, restoredCopyReleaseTimeout)
			}
			assert.Equal(t, 1, *steps, "the list and the deletion took separate limits")
			assert.Equal(t, map[string]any{"a": 1}, repo.stepsByFile(t))
			for _, k := range variantKeys("a") {
				assert.NotContains(t, f.dst.objects, k, k)
			}
			for _, k := range variantKeys("b") {
				assert.Contains(t, f.dst.objects, k, "a copy of another file was deleted")
			}
		})
	}
}

// 写しは並び順に頼らずファイルごとにまとめる (#4267)。
func TestReleaseRestoredCopiesOfFilesGroupsUnorderedCopies(t *testing.T) {
	f := restoredFixture(t, nil, "a", "b")
	repo := &unorderedCopiesRepo{ctxRecordingRepo: ctxRecordingRepo{memMigrationRepo: f.repo}}
	m := NewStorageMigrator(repo, f.local, f.dst, f.enq, f.lock)
	countReleaseSteps(m)
	m.ReleaseRestoredCopiesOfFiles(context.Background(), []string{"a", "b"})

	require.Len(t, repo.finds, 6)
	assert.Equal(t, map[string]any{"a": 2, "b": 3}, repo.stepsByFile(t))
	for _, k := range variantKeys("a", "b") {
		assert.NotContains(t, f.dst.objects, k, k)
	}
}

// unorderedCopiesRepo returns the copies interleaving the files.
type unorderedCopiesRepo struct{ ctxRecordingRepo }

func (r *unorderedCopiesRepo) ListRestoredCopiesOfFiles(ctx context.Context, fileIDs []string) ([]model.DriveObjectStorageRestoredCopy, error) {
	copies, err := r.memMigrationRepo.ListRestoredCopiesOfFiles(ctx, fileIDs)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(copies, func(a, b model.DriveObjectStorageRestoredCopy) int { return strings.Compare(a.Key, b.Key) })
	return copies, nil
}

func countOf(events []string, e string) int {
	n := 0
	for _, x := range events {
		if x == e {
			n++
		}
	}
	return n
}
