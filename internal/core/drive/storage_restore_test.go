package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

func (f *migrationFixture) restore(t *testing.T) {
	t.Helper()
	require.NoError(t, f.m.StartRestore(context.Background()))
	f.drainRestore(t)
}

// drainRestore runs the queued restore jobs until none is left.
func (f *migrationFixture) drainRestore(t *testing.T) {
	t.Helper()
	for range 1000 {
		j, ok := f.enq.popRestore()
		if !ok {
			return
		}
		require.NoError(t, f.m.RunRestoreBatch(context.Background(), j.token, j.afterID))
	}
	t.Fatal("the jobs did not end")
}

// localBody returns the local copy of key, or "" when it is not there.
func (f *migrationFixture) localBody(t *testing.T, key string) string {
	t.Helper()
	rc, err := f.local.Get(key)
	if errors.Is(err, ErrObjectNotFound) {
		return ""
	}
	require.NoError(t, err)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	return string(b)
}

func TestStorageRestoreMovesBackOnlyMigratedFiles(t *testing.T) {
	ctx := context.Background()
	f := verifiedFixture(t, "kept", "purged", "broken", "both", "thumbgone", "nothumb", "thumbdiff", "thumbonlylocal")
	// ローカルを消したファイル。
	require.NoError(t, f.m.StartPurge(ctx))
	f.drainPurge(t)
	for i := range f.repo.files {
		// purged 以外は、ローカルを消す前の世代へ戻したことにして、ローカルを置き直す。
		if id := f.repo.files[i].ID; id != "purged" {
			f.repo.files[i].ObjectStorageLocalDeletedAt = nil
			for _, k := range []string{"ak-" + id, "tk-" + id, "wk-" + id} {
				_, err := f.local.Put(k, strings.NewReader("body of "+k))
				require.NoError(t, err)
			}
		}
	}
	// ローカルの本体が壊れている。オブジェクトストレージから取り直す。
	_, err := f.local.Put("ak-broken", strings.NewReader("broken"))
	require.NoError(t, err)
	// どちらも壊れている。戻さない。
	_, err = f.local.Put("ak-both", strings.NewReader("broken"))
	require.NoError(t, err)
	// 大きさは記録と同じで、中身だけ違う (md5 で見分ける)。
	f.dst.objects["ak-both"] = []byte(strings.Repeat("x", len("body of ak-both")))
	// ローカルのサムネイルが無い。オブジェクトストレージから取る。
	require.NoError(t, f.local.Delete("tk-thumbgone"))
	// サムネイルがどちらにも無い。飛ばす。
	require.NoError(t, f.local.Delete("tk-nothumb"))
	delete(f.dst.objects, "tk-nothumb")
	// ローカルのサムネイルがオブジェクトストレージの写しと違う (書きかけで止まったなど)。
	// 写しで置き換える。
	_, err = f.local.Put("tk-thumbdiff", strings.NewReader("stale"))
	require.NoError(t, err)
	// オブジェクトストレージにサムネイルが無い。ローカルをそのまま使う。
	delete(f.dst.objects, "tk-thumbonlylocal")
	// 初めからオブジェクトストレージへアップロードしたファイルは戻さない。
	direct := "ak-s3"
	f.repo.files = append(f.repo.files, model.DriveFile{ID: "zz-s3", AccessKey: &direct, Size: 1, MD5: "x"})
	f.dst.objects["ak-s3"] = []byte("s3 only")

	f.restore(t)

	for _, id := range []string{"kept", "purged", "broken", "thumbgone", "nothumb", "thumbdiff", "thumbonlylocal"} {
		file := f.repo.file(id)
		assert.True(t, file.StoredInternal, id)
		assert.Nil(t, file.ObjectStorageMigratedAt, id)
		assert.Nil(t, file.ObjectStorageVerifiedAt, id)
		assert.Nil(t, file.ObjectStorageLocalDeletedAt, id)
		assert.Equal(t, "body of ak-"+id, f.localBody(t, "ak-"+id), id)
		assert.NotContains(t, f.repo.failures, id+"/restore", id)
	}
	for _, id := range []string{"purged", "thumbgone"} {
		assert.Equal(t, "body of tk-"+id, f.localBody(t, "tk-"+id), id)
	}
	assert.Equal(t, "body of tk-thumbdiff", f.localBody(t, "tk-thumbdiff"))
	assert.Equal(t, "body of tk-thumbonlylocal", f.localBody(t, "tk-thumbonlylocal"))
	assert.Empty(t, f.localBody(t, "tk-nothumb"))

	both := f.repo.file("both")
	assert.False(t, both.StoredInternal, "a file matching neither copy was moved back")
	assert.NotNil(t, both.ObjectStorageMigratedAt)
	assert.Contains(t, f.repo.failures["both/"+repository.DriveStorageMigrationOperationRestore].Error, "matches the recorded size and md5")

	s3 := f.repo.file("zz-s3")
	assert.False(t, s3.StoredInternal)
	assert.Empty(t, f.localBody(t, "ak-s3"))
	// オブジェクトストレージの実体は消さない。
	assert.Contains(t, f.dst.objects, "ak-kept")
	assert.Contains(t, f.dst.objects, "ak-purged")

	st, err := f.m.Status(ctx)
	require.NoError(t, err)
	assert.Empty(t, st.Running)
	assert.Equal(t, int64(1), st.Migrated)
	assert.Equal(t, int64(1), st.RestoreFailed)
	assert.Len(t, st.RestoreFailures, 1)
	held, _ := f.lock.Held(ctx)
	assert.False(t, held)

	// 戻した行は、もう一度移せる (確かめて一致しなかったファイルも、戻せば写し直せる)。
	require.NoError(t, f.m.Start(ctx))
}

// 本体がどちらにも無ければ戻さない。
func TestStorageRestoreRefusesAFileMissingEverywhere(t *testing.T) {
	f := migratedFixture(t, "a1")
	require.NoError(t, f.local.Delete("ak-a1"))
	delete(f.dst.objects, "ak-a1")
	f.restore(t)
	assert.False(t, f.repo.file("a1").StoredInternal)
	assert.Contains(t, f.repo.failures["a1/restore"].Error, "neither on the local storage")
}

func TestStorageRestoreChainsBatches(t *testing.T) {
	ids := make([]string, storageMigrationBatch+3)
	for i := range ids {
		ids[i] = fmt.Sprintf("f%04d", i)
	}
	f := migratedFixture(t, ids...)
	require.NoError(t, f.m.StartRestore(context.Background()))
	first, ok := f.enq.popRestore()
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(first.token, restoreTokenPrefix))
	require.NoError(t, f.m.RunRestoreBatch(context.Background(), first.token, first.afterID))
	next, ok := f.enq.popRestore()
	require.True(t, ok)
	assert.Equal(t, migrationJob{first.token, fmt.Sprintf("f%04d", storageMigrationBatch-1)}, next)
	f.enq.restoreJobs = append([]migrationJob{next}, f.enq.restoreJobs...)
	f.drainRestore(t)
	c, _ := f.repo.Counts(context.Background())
	assert.Zero(t, c.Migrated)
	assert.Equal(t, int64(len(ids)), c.Local)
}

func TestStorageRestoreStartRefuses(t *testing.T) {
	ctx := context.Background()
	t.Run("no queue", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		m := NewStorageMigrator(f.repo, f.local, f.dst, nil, f.lock)
		assert.ErrorIs(t, m.StartRestore(ctx), ErrStorageMigrationNoQueue)
	})
	t.Run("object storage not configured", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		m := NewStorageMigrator(f.repo, f.local, f.local, f.enq, f.lock)
		assert.ErrorIs(t, m.StartRestore(ctx), ErrStorageMigrationNotConfigured)
	})
	t.Run("nothing migrated", func(t *testing.T) {
		f := newMigrationFixture(t)
		f.addLocal(t, "a1")
		assert.ErrorIs(t, f.m.StartRestore(ctx), ErrStorageMigrationNothingToRestore)
	})
	t.Run("counts fail", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		m := NewStorageMigrator(&failingMigrationRepo{memMigrationRepo: f.repo, fail: "Counts"}, f.local, f.dst, f.enq, f.lock)
		assert.ErrorIs(t, m.StartRestore(ctx), errInjected)
	})
	t.Run("restore running", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		f.addLocal(t, "a2")
		require.NoError(t, f.m.StartRestore(ctx))
		assert.ErrorIs(t, f.m.StartRestore(ctx), ErrStorageMigrationRunning)
		assert.ErrorIs(t, f.m.Start(ctx), ErrStorageMigrationRunning)
		assert.ErrorIs(t, f.m.StartVerify(ctx), ErrStorageMigrationRunning)
		st, err := f.m.Status(ctx)
		require.NoError(t, err)
		assert.Equal(t, StorageMigrationOperationRestore, st.Running)
		// 戻す処理は写さないので、削除の経路は写しを消さなくてよい。
		running, err := f.m.Running(ctx)
		require.NoError(t, err)
		assert.False(t, running)
	})
	t.Run("purge running", func(t *testing.T) {
		f := verifiedFixture(t, "a1")
		require.NoError(t, f.m.StartPurge(ctx))
		assert.ErrorIs(t, f.m.StartRestore(ctx), ErrStorageMigrationRunning)
		assert.Empty(t, f.enq.restoreJobs)
	})
	t.Run("enqueue fails", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		f.enq.err = errors.New("redis down")
		require.Error(t, f.m.StartRestore(ctx))
		held, _ := f.lock.Held(ctx)
		assert.False(t, held)
	})
}

func TestStorageRestoreErrors(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, f *migrationFixture, m *StorageMigrator) error {
		t.Helper()
		require.NoError(t, m.StartRestore(ctx))
		j, ok := f.enq.popRestore()
		require.True(t, ok)
		return m.RunRestoreBatch(ctx, j.token, j.afterID)
	}
	t.Run("row not migrated", func(t *testing.T) {
		f := newMigrationFixture(t)
		f.addLocal(t, "a1")
		file := f.repo.file("a1")
		assert.ErrorContains(t, f.m.restoreFile(ctx, f.dst, &file, "t"), "has not been migrated")
	})
	t.Run("no access key", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		file := f.repo.file("a1")
		file.AccessKey = nil
		assert.ErrorContains(t, f.m.restoreFile(ctx, f.dst, &file, "t"), "no access key")
	})
	for name, setup := range map[string]func(f *migrationFixture) *StorageMigrator{
		"local body read fails": func(f *migrationFixture) *StorageMigrator {
			return NewStorageMigrator(f.repo, &erroringStorage{Storage: f.local, fail: map[string]bool{"ak-a1": true}}, f.dst, f.enq, f.lock)
		},
		"object storage body read fails": func(f *migrationFixture) *StorageMigrator {
			require.NoError(t, f.local.Delete("ak-a1"))
			return NewStorageMigrator(f.repo, f.local, &erroringStorage{Storage: f.dst, fail: map[string]bool{"ak-a1": true}}, f.enq, f.lock)
		},
		"object storage body read breaks": func(f *migrationFixture) *StorageMigrator {
			require.NoError(t, f.local.Delete("ak-a1"))
			return NewStorageMigrator(f.repo, f.local, &erroringStorage{Storage: f.dst, readFail: map[string]bool{"ak-a1": true}}, f.enq, f.lock)
		},
		"local body put fails": func(f *migrationFixture) *StorageMigrator {
			require.NoError(t, f.local.Delete("ak-a1"))
			return NewStorageMigrator(f.repo, &keyPutFailingStorage{Storage: f.local, key: "ak-a1"}, f.dst, f.enq, f.lock)
		},
		"local variant read fails": func(f *migrationFixture) *StorageMigrator {
			return NewStorageMigrator(f.repo, &erroringStorage{Storage: f.local, fail: map[string]bool{"tk-a1": true}}, f.dst, f.enq, f.lock)
		},
		"object storage variant read fails without a local copy": func(f *migrationFixture) *StorageMigrator {
			require.NoError(t, f.local.Delete("wk-a1"))
			return NewStorageMigrator(f.repo, f.local, &erroringStorage{Storage: f.dst, fail: map[string]bool{"wk-a1": true}}, f.enq, f.lock)
		},
		"the body put does not stick": func(f *migrationFixture) *StorageMigrator {
			require.NoError(t, f.local.Delete("ak-a1"))
			return NewStorageMigrator(f.repo, &truncatingPutStorage{LocalStorage: f.local, key: "ak-a1"}, f.dst, f.enq, f.lock)
		},
		"the variant put does not stick": func(f *migrationFixture) *StorageMigrator {
			require.NoError(t, f.local.Delete("tk-a1"))
			return NewStorageMigrator(f.repo, &truncatingPutStorage{LocalStorage: f.local, key: "tk-a1"}, f.dst, f.enq, f.lock)
		},
		"reading the put back fails": func(f *migrationFixture) *StorageMigrator {
			require.NoError(t, f.local.Delete("ak-a1"))
			return NewStorageMigrator(f.repo, &readBackFailingStorage{LocalStorage: f.local, key: "ak-a1"}, f.dst, f.enq, f.lock)
		},
		"local variant put fails": func(f *migrationFixture) *StorageMigrator {
			require.NoError(t, f.local.Delete("tk-a1"))
			return NewStorageMigrator(f.repo, &keyPutFailingStorage{Storage: f.local, key: "tk-a1"}, f.dst, f.enq, f.lock)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := migratedFixture(t, "a1")
			m := setup(f)
			require.NoError(t, run(t, f, m))
			assert.False(t, f.repo.file("a1").StoredInternal)
			require.Contains(t, f.repo.failures, "a1/restore")
			msg := f.repo.failures["a1/restore"].Error
			switch name {
			case "reading the put back fails":
				assert.Contains(t, msg, "back from the local storage", msg)
			case "local body read fails", "local variant read fails":
				// ローカルを読めないときは、置き直さずにその失敗を返す。
				assert.NotContains(t, msg, "back", msg)
			}
		})
	}
	t.Run("moving the row back fails", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		m := NewStorageMigrator(&failingMigrationRepo{memMigrationRepo: f.repo, fail: "MarkRestored"}, f.local, f.dst, f.enq, f.lock)
		// ジョブごとやり直す。ファイルの失敗としては残さない。
		assert.ErrorIs(t, run(t, f, m), errInjected)
		assert.NotContains(t, f.repo.failures, "a1/restore")
		assert.False(t, f.repo.file("a1").StoredInternal)
	})
	t.Run("ListFailures of the restore in Status", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		m := NewStorageMigrator(&failingRestoreFailuresRepo{memMigrationRepo: f.repo}, f.local, f.dst, f.enq, f.lock)
		_, err := m.Status(ctx)
		assert.ErrorIs(t, err, errInjected)
	})
}

// 読んでいる間に別の実行が lock を取ったら、行を倒さずに止める。期限が切れただけなら
// 取り直して続ける。取り直しにも失敗したらジョブごとやり直す。
func TestStorageRestoreLock(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, onGet func(f *migrationFixture, lock *failingAcquireLock)) (*migrationFixture, *failingAcquireLock, *StorageMigrator) {
		f := migratedFixture(t, "a1")
		require.NoError(t, f.local.Delete("ak-a1"))
		lock := &failingAcquireLock{memMigrationLock: f.lock}
		dst := &hookStorage{Storage: f.dst, onGet: func(key string) {
			if key == "ak-a1" {
				onGet(f, lock)
			}
		}}
		return f, lock, NewStorageMigrator(f.repo, f.local, dst, f.enq, lock)
	}
	t.Run("another run took it", func(t *testing.T) {
		f, _, m := setup(t, func(f *migrationFixture, _ *failingAcquireLock) {
			f.lock.expire()
			_, _ = f.lock.Acquire(ctx, "other", time.Minute)
		})
		require.NoError(t, m.StartRestore(ctx))
		j, _ := f.enq.popRestore()
		require.NoError(t, m.RunRestoreBatch(ctx, j.token, j.afterID))
		assert.False(t, f.repo.file("a1").StoredInternal)
		assert.NotContains(t, f.repo.failures, "a1/restore")
	})
	t.Run("it expired", func(t *testing.T) {
		f, _, m := setup(t, func(f *migrationFixture, _ *failingAcquireLock) { f.lock.expire() })
		require.NoError(t, m.StartRestore(ctx))
		j, _ := f.enq.popRestore()
		require.NoError(t, m.RunRestoreBatch(ctx, j.token, j.afterID))
		assert.True(t, f.repo.file("a1").StoredInternal)
	})
	t.Run("taking it again fails", func(t *testing.T) {
		f, _, m := setup(t, func(f *migrationFixture, lock *failingAcquireLock) {
			f.lock.expire()
			lock.fail = true
		})
		require.NoError(t, m.StartRestore(ctx))
		j, _ := f.enq.popRestore()
		assert.ErrorIs(t, m.RunRestoreBatch(ctx, j.token, j.afterID), errInjected)
		assert.False(t, f.repo.file("a1").StoredInternal)
	})
	t.Run("extending it fails", func(t *testing.T) {
		f := migratedFixture(t, "a1")
		lock := &flakyExtendLock{memMigrationLock: f.lock}
		m := NewStorageMigrator(f.repo, f.local, f.dst, f.enq, lock)
		require.NoError(t, m.StartRestore(ctx))
		j, _ := f.enq.popRestore()
		// 1 回目は runBatch の頭、2 回目は 1 件目の前、3 回目が倒す直前。
		lock.failAt = 3
		assert.ErrorIs(t, m.RunRestoreBatch(ctx, j.token, j.afterID), errInjected)
		assert.False(t, f.repo.file("a1").StoredInternal)
	})
}

// 読んでいる間に行が消された・別のジョブが先に戻したら、倒せないので何もしない。
func TestStorageRestoreSkipsARowChangedWhileReading(t *testing.T) {
	ctx := context.Background()
	f := migratedFixture(t, "a1")
	require.NoError(t, f.local.Delete("ak-a1"))
	dst := &hookStorage{Storage: f.dst, onGet: func(key string) {
		if key == "ak-a1" {
			_, _ = f.repo.MarkRestored(ctx, "a1", *f.repo.file("a1").ObjectStorageMigratedAt, nil, nil)
		}
	}}
	m := NewStorageMigrator(f.repo, f.local, dst, f.enq, f.lock)
	require.NoError(t, m.StartRestore(ctx))
	j, _ := f.enq.popRestore()
	require.NoError(t, m.RunRestoreBatch(ctx, j.token, j.afterID))
	assert.True(t, f.repo.file("a1").StoredInternal)
	assert.NotContains(t, f.repo.failures, "a1/restore")
	// 置いたローカルの実体は、戻した行が使うので残す。
	assert.Equal(t, "body of ak-a1", f.localBody(t, "ak-a1"))
}

func TestOperationOfRestoreToken(t *testing.T) {
	assert.Equal(t, StorageMigrationOperationRestore, operationOfToken(restoreTokenPrefix+"abc"))
}

// keyPutFailingStorage fails Put for key.
type keyPutFailingStorage struct {
	Storage
	key string
}

func (s *keyPutFailingStorage) Put(key string, body io.Reader) (string, error) {
	if key == s.key {
		return "", errInjected
	}
	return s.Storage.Put(key, body)
}

// failingRestoreFailuresRepo fails ListFailures for the restore only.
type failingRestoreFailuresRepo struct{ *memMigrationRepo }

func (r *failingRestoreFailuresRepo) ListFailures(ctx context.Context, op string, limit int) ([]model.DriveObjectStorageMigrationFailure, error) {
	if op == repository.DriveStorageMigrationOperationRestore {
		return nil, errInjected
	}
	return r.memMigrationRepo.ListFailures(ctx, op, limit)
}

// truncatingPutStorage reports success for a Put of key but keeps only part
// of the bytes, as a file system reporting the error only on close would.
type truncatingPutStorage struct {
	*LocalStorage
	key string
}

func (s *truncatingPutStorage) Put(key string, body io.Reader) (string, error) {
	if key == s.key {
		return s.LocalStorage.Put(key, strings.NewReader("part"))
	}
	return s.LocalStorage.Put(key, body)
}

// readBackFailingStorage fails a Get of key once key has been put.
type readBackFailingStorage struct {
	*LocalStorage
	key string
	put bool
}

func (s *readBackFailingStorage) Put(key string, body io.Reader) (string, error) {
	if key == s.key {
		s.put = true
	}
	return s.LocalStorage.Put(key, body)
}

func (s *readBackFailingStorage) Get(key string) (io.ReadCloser, error) {
	if key == s.key && s.put {
		return nil, errInjected
	}
	return s.LocalStorage.Get(key)
}

// 置いている途中で止まったサムネイル (空き容量の不足など) は、流し直すと写しで置き直す。
// 書きかけのローカルをそのまま使って戻すと、壊れた中身を配る。
func TestStorageRestoreReplacesAPartlyWrittenVariantOnTheNextRun(t *testing.T) {
	ctx := context.Background()
	f := migratedFixture(t, "a1")
	require.NoError(t, f.local.Delete("tk-a1"))
	m := NewStorageMigrator(f.repo, &truncatingPutStorage{LocalStorage: f.local, key: "tk-a1"}, f.dst, f.enq, f.lock)
	require.NoError(t, m.StartRestore(ctx))
	j, _ := f.enq.popRestore()
	require.NoError(t, m.RunRestoreBatch(ctx, j.token, j.afterID))
	require.False(t, f.repo.file("a1").StoredInternal)
	require.Equal(t, "part", f.localBody(t, "tk-a1"))

	f.restore(t)
	assert.True(t, f.repo.file("a1").StoredInternal)
	assert.Equal(t, "body of tk-a1", f.localBody(t, "tk-a1"), "the partly written thumbnail was used")
}

// lock の期限が切れている間に消す処理がローカルを消したら、取り直しても行を倒さない。
// 倒すと、行がローカルを指すのに実体が無く、404 になる。
func TestStorageRestoreDoesNotMoveBackARowPurgedMeanwhile(t *testing.T) {
	ctx := context.Background()
	f := verifiedFixture(t, "a1")
	dst := &hookStorage{Storage: f.dst, onGet: func(key string) {
		if key != "wk-a1" {
			return
		}
		// 戻す処理がサムネイルを読んでいる間に lock が切れ、消す処理が 1 件流れて終わる。
		f.lock.expire()
		require.NoError(t, f.m.StartPurge(ctx))
		f.drainPurge(t)
	}}
	require.NoError(t, f.local.Delete("wk-a1"))
	m := NewStorageMigrator(f.repo, f.local, dst, f.enq, f.lock)
	require.NoError(t, m.StartRestore(ctx))
	j, _ := f.enq.popRestore()
	// 戻す処理の lock は消す処理に取られて返されたので、取り直して倒そうとする。
	require.NoError(t, m.RunRestoreBatch(ctx, j.token, j.afterID))
	// 行は移したままなので、置いた webpublic は消さずに残す (次に流せばそのまま使う)。
	assert.Equal(t, "body of wk-a1", f.localBody(t, "wk-a1"))
	file := f.repo.file("a1")
	assert.False(t, file.StoredInternal, "a row whose local copy was purged was moved back")
	assert.NotNil(t, file.ObjectStorageMigratedAt)
	assert.NotNil(t, file.ObjectStorageLocalDeletedAt)
	// 移したままなので、もう一度流せば取り直して戻す。
	f.restore(t)
	assert.True(t, f.repo.file("a1").StoredInternal)
	assert.Equal(t, "body of ak-a1", f.localBody(t, "ak-a1"))
}

// 戻している間に行が消された・リンクに倒されたら、置いたローカルを消す (誰も指さない)。
// 置かずに使ったローカルは消さない。
func TestStorageRestoreRemovesWhatItPutForADeletedRow(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(f *migrationFixture){
		"deleted": func(f *migrationFixture) {
			f.repo.mu.Lock()
			f.repo.files = f.repo.files[:0]
			f.repo.mu.Unlock()
		},
		"made a link": func(f *migrationFixture) {
			f.repo.mu.Lock()
			f.repo.files[0].IsLink = true
			f.repo.mu.Unlock()
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := migratedFixture(t, "a1")
			require.NoError(t, f.local.Delete("ak-a1"))
			dst := &hookStorage{Storage: f.dst, onGet: func(key string) {
				if key == "ak-a1" {
					change(f)
				}
			}}
			m := NewStorageMigrator(f.repo, f.local, dst, f.enq, f.lock)
			require.NoError(t, m.StartRestore(ctx))
			j, _ := f.enq.popRestore()
			require.NoError(t, m.RunRestoreBatch(ctx, j.token, j.afterID))
			assert.Empty(t, f.localBody(t, "ak-a1"), "the copy put for a deleted row was kept")
			// サムネイルと webpublic はローカルにあったものを使っただけなので残る。
			assert.Equal(t, "body of tk-a1", f.localBody(t, "tk-a1"))
		})
	}
	for _, fail := range []string{"IsStoredLocally", "IsMigrated"} {
		t.Run(fail+" fails", func(t *testing.T) {
			f := migratedFixture(t, "a1")
			require.NoError(t, f.local.Delete("ak-a1"))
			dst := &hookStorage{Storage: f.dst, onGet: func(key string) {
				if key == "ak-a1" {
					f.repo.mu.Lock()
					f.repo.files[0].IsLink = true
					f.repo.mu.Unlock()
				}
			}}
			m := NewStorageMigrator(&failingMigrationRepo{memMigrationRepo: f.repo, fail: fail}, f.local, dst, f.enq, f.lock)
			require.NoError(t, m.StartRestore(ctx))
			j, _ := f.enq.popRestore()
			assert.ErrorIs(t, m.RunRestoreBatch(ctx, j.token, j.afterID), errInjected)
		})
	}
}

// 戻した行の、確かめる処理と消す処理の失敗の記録は消す。
func TestStorageRestoreClearsTheOtherFailures(t *testing.T) {
	f := migratedFixture(t, "a1")
	at := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	for _, op := range []string{repository.DriveStorageMigrationOperationVerify, repository.DriveStorageMigrationOperationPurge, repository.DriveStorageMigrationOperationMigrate} {
		require.NoError(t, f.repo.RecordFailure(context.Background(), "a1", op, "x", at))
	}
	f.restore(t)
	assert.NotContains(t, f.repo.failures, "a1/verify")
	assert.NotContains(t, f.repo.failures, "a1/purge")
	assert.Contains(t, f.repo.failures, "a1/migrate", "only the failures that no longer apply are cleared")
}

// countingGetStorage counts the bytes read from the objects Get returns.
type countingGetStorage struct {
	Storage
	read int64
}

func (s *countingGetStorage) Get(key string) (io.ReadCloser, error) {
	rc, err := s.Storage.Get(key)
	if err != nil {
		return nil, err
	}
	return &countingReadCloser{ReadCloser: rc, n: &s.read}, nil
}

type countingReadCloser struct {
	io.ReadCloser
	n *int64
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	*r.n += int64(n)
	return n, err
}

// A copy in the object storage larger than the recorded size is refused
// without being read past the recorded size, so a replaced object cannot make
// the worker hold all of it.
func TestStorageRestoreStopsReadingAnOversizedCopy(t *testing.T) {
	f := migratedFixture(t, "a1")
	require.NoError(t, f.local.Delete("ak-a1"))
	size := int64(f.repo.file("a1").Size)
	f.dst.objects["ak-a1"] = []byte(strings.Repeat("x", 1<<20))
	dst := &countingGetStorage{Storage: f.dst}
	m := NewStorageMigrator(f.repo, f.local, dst, f.enq, f.lock)
	require.NoError(t, m.StartRestore(context.Background()))
	j, ok := f.enq.popRestore()
	require.True(t, ok)
	require.NoError(t, m.RunRestoreBatch(context.Background(), j.token, j.afterID))

	assert.False(t, f.repo.file("a1").StoredInternal)
	assert.Contains(t, f.repo.failures["a1/restore"].Error, "neither the local copy nor the copy in the object storage matches")
	assert.LessOrEqual(t, dst.read, size+1, "read past the recorded size")
	assert.Empty(t, f.localBody(t, "ak-a1"))
}

func TestReadAllLimit(t *testing.T) {
	st := newMemObjectStorage()
	st.objects["k"] = []byte("12345")
	b, err := readAllLimit(st, "k", 5)
	require.NoError(t, err)
	assert.Equal(t, "12345", string(b))
	_, err = readAllLimit(st, "k", 4)
	assert.ErrorIs(t, err, errObjectTooLarge)
	_, err = readAllLimit(st, "missing", 4)
	assert.ErrorIs(t, err, ErrObjectNotFound)
}

// A thumbnail or a webpublic variant in the object storage larger than the
// limit fails the restore of the file instead of being read whole.
func TestStorageRestoreRefusesAnOversizedVariant(t *testing.T) {
	f := migratedFixture(t, "a1")
	require.NoError(t, f.local.Delete("wk-a1"))
	f.dst.objects["wk-a1"] = []byte("0123456789")
	_, err := f.m.restoreVariant(f.dst, "webpublic", "wk-a1", 4)
	assert.ErrorContains(t, err, "the webpublic in the object storage is larger than 4 bytes")
}
