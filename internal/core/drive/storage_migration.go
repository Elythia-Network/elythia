package drive

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// Errors Start returns when it does not start the migration.
var (
	ErrStorageMigrationNotConfigured = errors.New("drive: object storage is not configured")
	ErrStorageMigrationNothingToDo   = errors.New("drive: no file is stored locally")
	ErrStorageMigrationRunning       = errors.New("drive: a storage migration is already running")
	ErrStorageMigrationNoQueue       = errors.New("drive: the job queue is not available")
	// ErrStorageMigrationNothingToVerify is returned by StartVerify when no
	// file has been migrated.
	ErrStorageMigrationNothingToVerify = errors.New("drive: no file has been migrated")
	// ErrStorageMigrationNothingToPurge is returned by StartPurge when no
	// verified file keeps its local copy.
	ErrStorageMigrationNothingToPurge = errors.New("drive: no verified file keeps its local copy")
	// ErrStorageMigrationNothingToRestore is returned by StartRestore when no
	// file has been migrated.
	ErrStorageMigrationNothingToRestore = errors.New("drive: no migrated file to move back")
	// ErrStorageMigrationNothingToCleanUp is returned by StartCleanup when no
	// copy left by moving files back is recorded (#4197).
	ErrStorageMigrationNothingToCleanUp = errors.New("drive: no copy left by moving files back")
)

// The operations of a storage migration run, reported by Status.
const (
	StorageMigrationOperationMigrate = "migrate"
	StorageMigrationOperationVerify  = "verify"
	StorageMigrationOperationPurge   = "purge"
	StorageMigrationOperationRestore = "restore"
	StorageMigrationOperationCleanup = "cleanup"
)

// verifyTokenPrefix starts the token of a verification run. The lock holds
// the token, so the token tells which operation runs.
//
// 移す処理の token は接頭辞を持たない。#4181 より前の版が取った lock (反映の
// 前後で実行中だったもの) も、移す処理として読める。
const verifyTokenPrefix = "verify-"

// purgeTokenPrefix starts the token of a purge run (#4182).
const purgeTokenPrefix = "purge-"

// restoreTokenPrefix starts the token of a run moving the migrated files
// back (#4183).
const restoreTokenPrefix = "restore-"

// cleanupTokenPrefix starts the token of a run deleting the copies left in
// object storage by moving files back (#4197).
const cleanupTokenPrefix = "cleanup-"

// operationOfToken returns the operation of the run token names.
func operationOfToken(token string) string {
	switch {
	case strings.HasPrefix(token, verifyTokenPrefix):
		return StorageMigrationOperationVerify
	case strings.HasPrefix(token, purgeTokenPrefix):
		return StorageMigrationOperationPurge
	case strings.HasPrefix(token, restoreTokenPrefix):
		return StorageMigrationOperationRestore
	case strings.HasPrefix(token, cleanupTokenPrefix):
		return StorageMigrationOperationCleanup
	}
	return StorageMigrationOperationMigrate
}

// storageMigrationBatch is the number of files one job copies.
//
// 1 件ごとにロックの期限を延ばすので、件数はジョブ 1 本の長さ (再試行で
// やり直す量) だけを決める。
const storageMigrationBatch = 50

// storageMigrationLockTTL is how long the lock outlives the last file. When
// the chain of jobs breaks (the next job could not be enqueued, or retries
// ran out), the lock expires and the migration can be started again; it
// resumes from the files still stored locally.
const storageMigrationLockTTL = 10 * time.Minute

// maxStorageMigrationError bounds the error text kept for a failure.
const maxStorageMigrationError = 1000

// StorageMigrationRepository is what the migration reads and writes in the
// database (repository.DriveStorageMigrationRepository).
type StorageMigrationRepository interface {
	ListLocal(ctx context.Context, afterID string, limit int) ([]model.DriveFile, error)
	MarkMigrated(ctx context.Context, id string, at time.Time) (bool, error)
	ListMigrated(ctx context.Context, afterID string, limit int) ([]model.DriveFile, error)
	SetVerified(ctx context.Context, id string, migratedAt time.Time, at *time.Time) (bool, error)
	ListPurgeable(ctx context.Context, afterID string, limit int) ([]model.DriveFile, error)
	MarkLocalDeleted(ctx context.Context, id string, migratedAt, at time.Time) (bool, error)
	MarkRestored(ctx context.Context, id string, migratedAt time.Time, localDeletedAt *time.Time, copies []model.DriveObjectStorageRestoredCopy) (bool, error)
	ListRestoredCopies(ctx context.Context, afterKey string, limit int) ([]model.DriveObjectStorageRestoredCopy, error)
	ListRestoredCopiesOfFile(ctx context.Context, fileID string) ([]model.DriveObjectStorageRestoredCopy, error)
	DeleteRestoredCopy(ctx context.Context, key, location string) error
	RecordRestoredCopyFailure(ctx context.Context, key, location, msg string, at time.Time) error
	ListRestoredCopyFailures(ctx context.Context, limit int) ([]model.DriveObjectStorageRestoredCopy, error)
	FindDriveFile(ctx context.Context, id string) (*model.DriveFile, error)
	IsStoredLocally(ctx context.Context, id string) (bool, error)
	IsMigrated(ctx context.Context, id string) (bool, error)
	RecordFailure(ctx context.Context, fileID, operation, msg string, at time.Time) error
	ClearFailure(ctx context.Context, fileID, operation string) error
	ListFailures(ctx context.Context, operation string, limit int) ([]model.DriveObjectStorageMigrationFailure, error)
	Counts(ctx context.Context) (repository.DriveStorageMigrationCounts, error)
}

// StorageMigrationEnqueuer enqueues the next job of the migration and of the
// verification.
type StorageMigrationEnqueuer interface {
	EnqueueMigrateLocalDriveFiles(token, afterID string) error
	EnqueueVerifyMigratedDriveFiles(token, afterID string) error
	EnqueuePurgeLocalDriveFiles(token, afterID string) error
	EnqueueRestoreMigratedDriveFiles(token, afterID string) error
	EnqueueCleanupRestoredDriveFileCopies(token, afterKey string) error
}

// StorageMigrationLock marks that a migration is running. The token names
// the run; a job whose token no longer holds the lock belongs to an older
// run and does nothing.
type StorageMigrationLock interface {
	// Acquire takes the lock for token, or extends it when token already
	// holds it. It reports whether token holds the lock afterwards.
	Acquire(ctx context.Context, token string, ttl time.Duration) (bool, error)
	// Extend extends the lock when token holds it. It does not take a free
	// lock: a job of a run that ended (released or expired) must not revive
	// it.
	Extend(ctx context.Context, token string, ttl time.Duration) (bool, error)
	// Release gives up the lock if token holds it.
	Release(ctx context.Context, token string) error
	// Held reports whether any run holds the lock.
	Held(ctx context.Context) (bool, error)
	// Holder returns the token holding the lock, or "" when no run holds it.
	Holder(ctx context.Context) (string, error)
}

// StorageMigrationStatus is what the admin API shows of the migration.
type StorageMigrationStatus struct {
	// ObjectStorageEnabled reports whether object storage is configured, so
	// the migration has somewhere to copy to.
	ObjectStorageEnabled bool
	// Running is the operation of the running run
	// (StorageMigrationOperationMigrate / Verify), or "" when none runs.
	Running string
	// Local, Migrated and Failed count the files still stored locally, the
	// files the migration copied, and the files whose last attempt failed.
	Local    int64
	Migrated int64
	Failed   int64
	// Failures are the most recent failures, newest first.
	Failures []model.DriveObjectStorageMigrationFailure
	// Verified and VerifyFailed count the migrated files whose copy was
	// verified and whose last verification failed (#4181).
	Verified     int64
	VerifyFailed int64
	// VerifyFailures are the most recent verification failures, newest
	// first.
	VerifyFailures []model.DriveObjectStorageMigrationFailure
	// Purgeable, LocalDeleted and PurgeFailed count the verified files whose
	// local copy has not been deleted, the files whose local copy was
	// deleted, and the files whose last purge attempt failed (#4182).
	Purgeable    int64
	LocalDeleted int64
	PurgeFailed  int64
	// PurgeFailures are the most recent purge failures, newest first.
	PurgeFailures []model.DriveObjectStorageMigrationFailure
	// RestoreFailed counts the files whose last attempt to move back failed,
	// and RestoreFailures are the most recent of them, newest first (#4183).
	RestoreFailed   int64
	RestoreFailures []model.DriveObjectStorageMigrationFailure
	// RestoredCopies counts the copies left in object storage by moving files
	// back, CleanupFailed those whose last deletion failed, and
	// CleanupFailures are the most recent of them, newest first (#4197).
	RestoredCopies  int64
	CleanupFailed   int64
	CleanupFailures []model.DriveObjectStorageRestoredCopy
}

// StorageMigrator copies the drive files stored locally to object storage
// (#4180), verifies the copies (#4181) and deletes the local copies of the
// verified files (#4182). Copying leaves the local files in place, and the
// url columns keep pointing at /files/<accessKey>, which serves the file
// from object storage once storedInternal is false.
type StorageMigrator struct {
	repo    StorageMigrationRepository
	local   Storage
	primary Storage
	enq     StorageMigrationEnqueuer
	lock    StorageMigrationLock
	now     func() time.Time
	// onFUSE reports whether a directory is on a FUSE file system.
	onFUSE func(dir string) (bool, error)
}

// NewStorageMigrator constructs a StorageMigrator. local is the local storage
// the files are on, primary the storage the server uploads to (a
// MetaStorage); the migration copies only while primary resolves to object
// storage.
func NewStorageMigrator(repo StorageMigrationRepository, local, primary Storage, enq StorageMigrationEnqueuer, lock StorageMigrationLock) *StorageMigrator {
	return &StorageMigrator{repo: repo, local: local, primary: primary, enq: enq, lock: lock, now: time.Now, onFUSE: onFUSE}
}

// objectStorage returns the object storage files are copied to, or nil when
// object storage is not configured.
func (m *StorageMigrator) objectStorage() Storage {
	st := ResolveStorage(m.primary)
	if st == nil || StorageIsLocal(st) {
		return nil
	}
	return st
}

// Start starts a migration run: it takes the lock and enqueues the first job.
func (m *StorageMigrator) Start(ctx context.Context) error {
	if m.enq == nil {
		return ErrStorageMigrationNoQueue
	}
	if m.objectStorage() == nil {
		return ErrStorageMigrationNotConfigured
	}
	c, err := m.repo.Counts(ctx)
	if err != nil {
		return fmt.Errorf("drive: count local files: %w", err)
	}
	if c.Local == 0 {
		return ErrStorageMigrationNothingToDo
	}
	return m.begin(ctx, "", m.enq.EnqueueMigrateLocalDriveFiles)
}

// StartVerify starts a verification run (#4181): it takes the lock and
// enqueues the first job. The lock is the one of the migration, so the two
// never run at once.
func (m *StorageMigrator) StartVerify(ctx context.Context) error {
	if m.enq == nil {
		return ErrStorageMigrationNoQueue
	}
	if m.objectStorage() == nil {
		return ErrStorageMigrationNotConfigured
	}
	c, err := m.repo.Counts(ctx)
	if err != nil {
		return fmt.Errorf("drive: count migrated files: %w", err)
	}
	if c.Migrated == 0 {
		return ErrStorageMigrationNothingToVerify
	}
	return m.begin(ctx, verifyTokenPrefix, m.enq.EnqueueVerifyMigratedDriveFiles)
}

// StartPurge starts a purge run (#4182): it takes the lock and enqueues the
// first job. The lock is the one of the migration, so the purge never runs
// with the migration or the verification.
func (m *StorageMigrator) StartPurge(ctx context.Context) error {
	if m.enq == nil {
		return ErrStorageMigrationNoQueue
	}
	if m.objectStorage() == nil {
		return ErrStorageMigrationNotConfigured
	}
	c, err := m.repo.Counts(ctx)
	if err != nil {
		return fmt.Errorf("drive: count purgeable files: %w", err)
	}
	if c.Purgeable == 0 {
		return ErrStorageMigrationNothingToPurge
	}
	return m.begin(ctx, purgeTokenPrefix, m.enq.EnqueuePurgeLocalDriveFiles)
}

// StartRestore starts a run moving the migrated files back to the local
// storage (#4183): it takes the lock and enqueues the first job. It needs
// object storage to read the files whose local copy is gone or broken.
func (m *StorageMigrator) StartRestore(ctx context.Context) error {
	if m.enq == nil {
		return ErrStorageMigrationNoQueue
	}
	if m.objectStorage() == nil {
		return ErrStorageMigrationNotConfigured
	}
	c, err := m.repo.Counts(ctx)
	if err != nil {
		return fmt.Errorf("drive: count migrated files: %w", err)
	}
	if c.Migrated == 0 {
		return ErrStorageMigrationNothingToRestore
	}
	return m.begin(ctx, restoreTokenPrefix, m.enq.EnqueueRestoreMigratedDriveFiles)
}

// StartCleanup starts a run deleting the copies left in object storage by
// moving files back (#4197): it takes the lock and enqueues the first job.
// The lock is the one of the migration, so the run never deletes a copy the
// migration is writing again.
func (m *StorageMigrator) StartCleanup(ctx context.Context) error {
	if m.enq == nil {
		return ErrStorageMigrationNoQueue
	}
	if m.objectStorage() == nil {
		return ErrStorageMigrationNotConfigured
	}
	c, err := m.repo.Counts(ctx)
	if err != nil {
		return fmt.Errorf("drive: count copies left by moving files back: %w", err)
	}
	if c.RestoredCopies == 0 {
		return ErrStorageMigrationNothingToCleanUp
	}
	return m.begin(ctx, cleanupTokenPrefix, m.enq.EnqueueCleanupRestoredDriveFileCopies)
}

// begin takes the lock for a new token starting with prefix and enqueues the
// first job of the run.
func (m *StorageMigrator) begin(ctx context.Context, prefix string, enqueue func(token, afterID string) error) error {
	token, err := newMigrationToken()
	if err != nil {
		return err
	}
	token = prefix + token
	ok, err := m.lock.Acquire(ctx, token, storageMigrationLockTTL)
	if err != nil {
		return fmt.Errorf("drive: take the storage migration lock: %w", err)
	}
	if !ok {
		return ErrStorageMigrationRunning
	}
	if err := enqueue(token, ""); err != nil {
		// 積めなかったら走っていないので、lock を返して開始し直せるようにする。
		if rerr := m.lock.Release(ctx, token); rerr != nil {
			slog.Warn("drive: release the storage migration lock", "err", rerr)
		}
		return fmt.Errorf("drive: enqueue the storage migration: %w", err)
	}
	return nil
}

// RunBatch copies up to storageMigrationBatch local files whose id is greater
// than afterID, then enqueues the next job, or releases the lock when no file
// is left. A file that fails is recorded and skipped; the next run retries
// it. An error means the job should be retried.
func (m *StorageMigrator) RunBatch(ctx context.Context, token, afterID string) error {
	return m.runBatch(ctx, token, afterID, storageMigrationRun{
		list: m.repo.ListLocal,
		each: func(ctx context.Context, dst Storage, f *model.DriveFile) error {
			return m.migrateFile(ctx, dst, f, token)
		},
		operation: repository.DriveStorageMigrationOperationMigrate,
		enqueue:   m.enq.EnqueueMigrateLocalDriveFiles,
	})
}

// RunVerifyBatch verifies up to storageMigrationBatch migrated files whose id
// is greater than afterID (#4181), then enqueues the next job, or releases
// the lock when no file is left. A file that does not match is recorded and
// loses its verified mark; the next run verifies it again. An error means the
// job should be retried.
func (m *StorageMigrator) RunVerifyBatch(ctx context.Context, token, afterID string) error {
	return m.runBatch(ctx, token, afterID, storageMigrationRun{
		list:      m.repo.ListMigrated,
		each:      m.verifyFile,
		operation: repository.DriveStorageMigrationOperationVerify,
		enqueue:   m.enq.EnqueueVerifyMigratedDriveFiles,
	})
}

// RunPurgeBatch deletes the local copies of up to storageMigrationBatch
// verified files whose id is greater than afterID (#4182), then enqueues the
// next job, or releases the lock when no file is left. A file that does not
// match right before the deletion keeps its local copy and is recorded. An
// error means the job should be retried.
func (m *StorageMigrator) RunPurgeBatch(ctx context.Context, token, afterID string) error {
	return m.runBatch(ctx, token, afterID, storageMigrationRun{
		// 実行の初め (1 本目のジョブ) に 1 回だけ、lock を確かめた後で調べる。ジョブごとに
		// 番兵を置くと、バージョニングを有効にしたバケットに番兵の版が溜まる。
		precheck: func(ctx context.Context, dst Storage) (bool, error) {
			if afterID != "" {
				return true, nil
			}
			refuse, err := m.localIsObjectStorage(dst)
			if err != nil {
				return false, fmt.Errorf("drive: probe the local storage: %w", err)
			}
			if refuse != "" {
				// ローカルを消すとオブジェクトストレージの実体も消える構成。消さずに止める。
				slog.Error("drive: refusing to delete the local copies of the drive files", "reason", refuse)
				return false, m.lock.Release(ctx, token)
			}
			return true, nil
		},
		list: m.repo.ListPurgeable,
		each: func(ctx context.Context, dst Storage, f *model.DriveFile) error {
			return m.purgeFile(ctx, dst, f, token)
		},
		operation: repository.DriveStorageMigrationOperationPurge,
		enqueue:   m.enq.EnqueuePurgeLocalDriveFiles,
	})
}

// RunRestoreBatch moves up to storageMigrationBatch migrated files whose id
// is greater than afterID back to the local storage (#4183), then enqueues
// the next job, or releases the lock when no file is left. A file whose
// copies do not match the row stays migrated and is recorded. An error means
// the job should be retried.
func (m *StorageMigrator) RunRestoreBatch(ctx context.Context, token, afterID string) error {
	return m.runBatch(ctx, token, afterID, storageMigrationRun{
		list: m.repo.ListMigrated,
		each: func(ctx context.Context, dst Storage, f *model.DriveFile) error {
			return m.restoreFile(ctx, dst, f, token)
		},
		operation: repository.DriveStorageMigrationOperationRestore,
		enqueue:   m.enq.EnqueueRestoreMigratedDriveFiles,
	})
}

// errRestoredCopyMoved is recorded when the object storage settings no longer
// put a copy where it was when the file was moved back (#4197).
var errRestoredCopyMoved = errors.New("the object storage settings changed since the file was moved back; the copy is not deleted")

// RunCleanupBatch deletes up to storageMigrationBatch recorded copies whose
// key is greater than afterKey (#4197), then enqueues the next job, or
// releases the lock when no copy is left. A copy that cannot be deleted is
// recorded and skipped; the next run retries it. An error means the job
// should be retried.
func (m *StorageMigrator) RunCleanupBatch(ctx context.Context, token, afterKey string) error {
	ok, err := m.lock.Extend(ctx, token, storageMigrationLockTTL)
	if err != nil {
		return fmt.Errorf("drive: extend the storage migration lock: %w", err)
	}
	if !ok {
		slog.Info("drive: the storage migration lock is no longer held by this run; stopping", "afterKey", afterKey)
		return nil
	}
	dst := m.objectStorage()
	if dst == nil {
		slog.Warn("drive: object storage was turned off; stopping the cleanup of the copies")
		return m.lock.Release(ctx, token)
	}
	// ローカルとバケットが同じ実体 (バケットのマウント、ローカルのディレクトリを配る S3 互換
	// サーバー) なら、写しを消すとローカルの実体も消える。ローカルを消す処理 (#4182) と同じ
	// 検査を、実行の初めに 1 回だけ流す。
	if afterKey == "" {
		refuse, err := m.localIsObjectStorage(dst)
		if err != nil {
			return fmt.Errorf("drive: probe the local storage: %w", err)
		}
		if refuse != "" {
			slog.Error("drive: refusing to delete the copies left by moving files back", "reason", refuse)
			return m.lock.Release(ctx, token)
		}
	}
	copies, err := m.repo.ListRestoredCopies(ctx, afterKey, storageMigrationBatch)
	if err != nil {
		return fmt.Errorf("drive: list copies to clean up: %w", err)
	}
	for i := range copies {
		if err := ctx.Err(); err != nil {
			return err
		}
		c := &copies[i]
		if ok, err := m.lock.Extend(ctx, token, storageMigrationLockTTL); err != nil {
			return fmt.Errorf("drive: extend the storage migration lock: %w", err)
		} else if !ok {
			slog.Info("drive: the storage migration lock is no longer held by this run; stopping", "afterKey", c.Key)
			return nil
		}
		if err := m.deleteRestoredCopy(ctx, dst, c, token); err != nil {
			if errors.Is(err, errStorageMigrationLockLost) {
				slog.Info("drive: the storage migration lock is no longer held by this run; stopping", "afterKey", c.Key)
				return nil
			}
			var retry *storageMigrationRetry
			if errors.As(err, &retry) {
				return fmt.Errorf("drive: clean up the copy %s: %w", c.Key, retry.err)
			}
			msg := truncateUTF8(err.Error(), maxStorageMigrationError)
			if rerr := m.repo.RecordRestoredCopyFailure(ctx, c.Key, c.Location, msg, m.now()); rerr != nil {
				return fmt.Errorf("drive: record the failure of the copy %s: %w", c.Key, rerr)
			}
		}
	}
	if len(copies) < storageMigrationBatch {
		return m.lock.Release(ctx, token)
	}
	return m.enq.EnqueueCleanupRestoredDriveFileCopies(token, copies[len(copies)-1].Key)
}

// errRestoredCopyIsTheOnlyCopy is recorded when the local storage has no
// matching copy of a file whose copy the cleanup would delete (#4197).
var errRestoredCopyIsTheOnlyCopy = errors.New("the local copy is missing or does not match the file; the copy in object storage is kept")

// deleteRestoredCopy deletes the copy c from dst and its record. It keeps the
// copy when dst would not delete it where it was, and drops the record
// without deleting when the file is served from object storage again. With
// the token of a cleanup run, it also keeps the copy when the local storage
// has no matching copy of a file still stored locally, and extends the lock
// of the run before deleting. An empty token is for a file being deleted.
func (m *StorageMigrator) deleteRestoredCopy(ctx context.Context, dst Storage, c *model.DriveObjectStorageRestoredCopy, token string) error {
	checkLocal := token != ""
	if objectLocation(dst, c.Key) != c.Location {
		// prefix やバケットを変えた後で消すと、同じアクセスキーの別の場所を消してしまう。
		return errRestoredCopyMoved
	}
	f, err := m.repo.FindDriveFile(ctx, c.FileID)
	if err != nil {
		return &storageMigrationRetry{err: fmt.Errorf("read the file of the copy: %w", err)}
	}
	// 移す処理は写し直した行の記録を消すが、写す前の世代の DB のバックアップへ戻したときなど、
	// 記録が残ったまま行がオブジェクトストレージを指していることがある。その写しは配信中の
	// 実体なので、消さずに記録だけを外す。
	served := f != nil && !f.StoredInternal && !f.IsLink
	if !served && checkLocal && f != nil && f.StoredInternal && !f.IsLink {
		// 戻した後で、ローカルを消した世代の DB へ戻したときや、ローカルのディスクを失った
		// ときは、バケットの写しが最後の実体になっている。ローカルに正しい実体があるときだけ
		// 消す。消している途中の行 (ReleaseRestoredCopies) は、ローカルの有無に依らず写しが
		// 要らなくなるので確かめない。
		ok, err := m.localCopyMatches(f, c.Key)
		if err != nil {
			return err
		}
		if !ok {
			return errRestoredCopyIsTheOnlyCopy
		}
		// ローカルの本体を読むのに時間がかかる (大きなファイル) ので、消す前に lock を延ばし
		// 直す。切れている間に移す処理が同じキーへ写して行を倒していると、配信中の実体を消す。
		if ok, err := m.lock.Extend(ctx, token, storageMigrationLockTTL); err != nil {
			return &storageMigrationRetry{err: fmt.Errorf("extend the storage migration lock: %w", err)}
		} else if !ok {
			return errStorageMigrationLockLost
		}
	}
	if !served {
		if err := dst.Delete(c.Key); err != nil {
			return err
		}
	} else {
		slog.Warn("drive: a copy left by moving a file back is served from object storage again; keeping it", "fileId", c.FileID, "key", c.Key)
	}
	if err := m.repo.DeleteRestoredCopy(ctx, c.Key, c.Location); err != nil {
		return &storageMigrationRetry{err: fmt.Errorf("delete the record of the copy: %w", err)}
	}
	return nil
}

// localCopyMatches reports whether the local storage has a copy of key for
// f: the body must match the recorded size and md5, a thumbnail or a
// webpublic variant must exist (the row has no md5 for them).
func (m *StorageMigrator) localCopyMatches(f *model.DriveFile, key string) (bool, error) {
	size, sum, err := readObject(m.local, key)
	if errors.Is(err, ErrObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if f.AccessKey != nil && *f.AccessKey == key {
		return size == int64(f.Size) && strings.EqualFold(sum, f.MD5), nil
	}
	return true, nil
}

// restoredCopyReleaseTimeout bounds ReleaseRestoredCopies.
const restoredCopyReleaseTimeout = 30 * time.Second

// ReleaseRestoredCopies deletes the copies the file left in object storage
// when it was moved back (#4197), as the file is being deleted. Best effort:
// a copy it cannot delete keeps its record, and the cleanup deletes it later.
//
// 戻した行は storedInternal=true なので、削除はローカルだけを消す (本家と同じ)。
// 写しはバケットに残り、public-read なら消したファイルが取れ続けるので、ここで消す。
func (m *StorageMigrator) ReleaseRestoredCopies(ctx context.Context, fileID string) {
	// 利用者の削除の中で動くので、DB の待ちで削除を止め続けない。
	ctx, cancel := context.WithTimeout(ctx, restoredCopyReleaseTimeout)
	defer cancel()
	copies, err := m.repo.ListRestoredCopiesOfFile(ctx, fileID)
	if err != nil {
		slog.Warn("drive: list the copies left by moving a deleted file back", "fileId", fileID, "err", err)
		return
	}
	if len(copies) == 0 {
		return
	}
	dst := m.objectStorage()
	if dst == nil {
		// オブジェクトストレージを無効にしている。記録を残し、有効に戻してから消す。
		return
	}
	for i := range copies {
		if err := m.deleteRestoredCopy(ctx, dst, &copies[i], ""); err != nil {
			slog.Warn("drive: delete a copy left by moving a deleted file back", "fileId", fileID, "key", copies[i].Key, "err", err)
			msg := truncateUTF8(err.Error(), maxStorageMigrationError)
			if rerr := m.repo.RecordRestoredCopyFailure(ctx, copies[i].Key, copies[i].Location, msg, m.now()); rerr != nil {
				slog.Warn("drive: record the failure of a copy", "key", copies[i].Key, "err", rerr)
			}
		}
	}
}

// objectLocation returns where st puts the object with key: the bucket and
// the object key with the prefix for object storage, or key itself for a
// storage that does not tell.
func objectLocation(st Storage, key string) string {
	if l, ok := st.(interface{ ObjectLocation(key string) string }); ok {
		return l.ObjectLocation(key)
	}
	return key
}

// storageMigrationRun is what one operation does in runBatch.
type storageMigrationRun struct {
	// list returns the next files of the operation after afterID.
	list func(ctx context.Context, afterID string, limit int) ([]model.DriveFile, error)
	// each handles one file. An error is recorded as the failure of the file.
	each      func(ctx context.Context, dst Storage, f *model.DriveFile) error
	operation string
	enqueue   func(token, afterID string) error
	// precheck, when set, runs once the lock is confirmed and before the files
	// are listed. It reports whether to go on; when it does not, it has dealt
	// with the lock itself.
	precheck func(ctx context.Context, dst Storage) (bool, error)
}

// errStorageMigrationLockLost is returned by a file handler that found its
// run no longer holds the lock. runBatch stops the job without recording a
// failure.
var errStorageMigrationLockLost = errors.New("the storage migration lock is no longer held by this run")

// localIsObjectStorage reports why deleting a local copy could delete the
// object in dst too, or "" when it found no reason: the local storage is on
// a FUSE file system (a bucket mounted with rclone or s3fs), or an object
// put to dst under a probe key replaces the local file of that key (the
// object storage serves the local directory).
//
// 番兵は「オブジェクトストレージに無いことを読む」形にしない。S3 は ListBucket の
// 権限が無いと無いキーにも 403 を返すので、最小の権限の構成で流せなくなる。置く・
// 消すの権限は移す処理と削除でもともと要る。マウントの構成は、書き込みを遅らせて
// 届けるもの (rclone の write-back) があり番兵では見分けられないので、FUSE で断る。
func (m *StorageMigrator) localIsObjectStorage(dst Storage) (string, error) {
	if ls, ok := m.local.(*LocalStorage); ok {
		fuse, err := m.onFUSE(ls.rootDir)
		if err != nil {
			return "", err
		}
		if fuse {
			return "the local storage is on a FUSE file system (a mounted bucket?)", nil
		}
	}
	token, err := newMigrationToken()
	if err != nil {
		return "", err
	}
	key := "elythia-purge-probe-" + token
	localBody, objectBody := "local "+key, "object "+key
	if _, err := m.local.Put(key, strings.NewReader(localBody)); err != nil {
		// 書き込みの途中で失敗しても、作りかけのファイルが残りうる。
		if derr := m.local.Delete(key); derr != nil {
			slog.Warn("drive: delete the purge probe from the local storage", "key", key, "err", derr)
		}
		return "", err
	}
	defer func() {
		if err := m.local.Delete(key); err != nil {
			slog.Warn("drive: delete the purge probe from the local storage", "key", key, "err", err)
		}
	}()
	if _, err := dst.Put(key, strings.NewReader(objectBody)); err != nil {
		return "", err
	}
	defer func() {
		if err := dst.Delete(key); err != nil {
			slog.Warn("drive: delete the purge probe from the object storage", "key", key, "err", err)
		}
	}()
	rc, err := m.local.Get(key)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	if string(b) != localBody {
		return "the object storage serves the local drive files", nil
	}
	return "", nil
}

func (m *StorageMigrator) runBatch(ctx context.Context, token, afterID string, run storageMigrationRun) error {
	// 取り直さず、延ばすだけ。終わった実行 (解放した・期限が切れた) のジョブが遅れて
	// 届いても (再配送など)、その実行を生き返らせない。
	ok, err := m.lock.Extend(ctx, token, storageMigrationLockTTL)
	if err != nil {
		return fmt.Errorf("drive: extend the storage migration lock: %w", err)
	}
	if !ok {
		// 別の実行に取って代わられたか、lock の期限が切れた (ジョブが動き出すまでの
		// 待ちが期限を超えたなど)。何もしないが、止まった理由が見えるよう記録する。
		slog.Info("drive: the storage migration lock is no longer held by this run; stopping", "afterId", afterID)
		return nil
	}
	dst := m.objectStorage()
	if dst == nil {
		// 実行の途中でオブジェクトストレージの設定を外した。写す先が無いので止める。
		slog.Warn("drive: object storage was turned off; stopping the storage migration")
		return m.lock.Release(ctx, token)
	}
	if run.precheck != nil {
		if ok, err := run.precheck(ctx, dst); err != nil || !ok {
			return err
		}
	}
	files, err := run.list(ctx, afterID, storageMigrationBatch)
	if err != nil {
		return fmt.Errorf("drive: list files to %s: %w", run.operation, err)
	}
	for i := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		f := &files[i]
		// 1 件ごとに期限を延ばす。大きなファイルが続いても、実行中に lock が切れない。
		if ok, err := m.lock.Extend(ctx, token, storageMigrationLockTTL); err != nil {
			return fmt.Errorf("drive: extend the storage migration lock: %w", err)
		} else if !ok {
			slog.Info("drive: the storage migration lock is no longer held by this run; stopping", "afterId", f.ID)
			return nil
		}
		if err := run.each(ctx, dst, f); err != nil {
			if errors.Is(err, errStorageMigrationLockLost) {
				slog.Info("drive: the storage migration lock is no longer held by this run; stopping", "afterId", f.ID)
				return nil
			}
			var retry *storageMigrationRetry
			if errors.As(err, &retry) {
				// ファイルの問題ではなく DB に書けなかった。失敗として残して先へ進むと、
				// 書けなかった結果 (確かめた印を消すなど) が抜けたままになるので、
				// ジョブごとやり直す。
				return fmt.Errorf("drive: %s %s: %w", run.operation, f.ID, retry.err)
			}
			msg := truncateUTF8(err.Error(), maxStorageMigrationError)
			if rerr := m.repo.RecordFailure(ctx, f.ID, run.operation, msg, m.now()); rerr != nil {
				return fmt.Errorf("drive: record the failure of %s: %w", f.ID, rerr)
			}
			continue
		}
		if err := m.repo.ClearFailure(ctx, f.ID, run.operation); err != nil {
			return fmt.Errorf("drive: clear the failure of %s: %w", f.ID, err)
		}
	}
	if len(files) < storageMigrationBatch {
		return m.lock.Release(ctx, token)
	}
	return run.enqueue(token, files[len(files)-1].ID)
}

// errLocalFileMissing is recorded when the local storage has no file for the
// row.
var errLocalFileMissing = errors.New("the file is not on the local storage")

// migrateFile copies the file, its thumbnail and its webpublic variant to
// dst under the same access keys, then marks the row as migrated.
//
// サムネイルと webpublic の実体がローカルに無いときは飛ばす。もともと配信できて
// いなかったもので、写した後も同じく見つからない。本体が無いときは失敗にする。
func (m *StorageMigrator) migrateFile(ctx context.Context, dst Storage, f *model.DriveFile, token string) error {
	keys := []struct {
		key      *string
		required bool
	}{{f.AccessKey, true}, {f.ThumbnailAccessKey, false}, {f.WebpublicAccessKey, false}}
	var copied []string
	for _, k := range keys {
		if k.key == nil || *k.key == "" {
			if k.required {
				return errors.New("the file has no access key")
			}
			continue
		}
		if err := copyObject(m.local, dst, *k.key); err != nil {
			if errors.Is(err, ErrObjectNotFound) && !k.required {
				continue
			}
			if errors.Is(err, ErrObjectNotFound) {
				return errLocalFileMissing
			}
			return err
		}
		copied = append(copied, *k.key)
	}
	// 写すのに時間がかかる (大きなファイル) ので、倒す前に lock を延ばし直す。期限が切れた
	// だけなら取り直し、別の実行に取られていたら止める。期限が切れている間に写しを消す処理
	// (#4197) が割り込むと、いま置いた写しを消すので、行を倒すとオブジェクトストレージを
	// 指したまま実体が無くなる。止めれば行はローカルのままで、次に流せば写し直す。
	if ok, err := m.lock.Extend(ctx, token, storageMigrationLockTTL); err != nil {
		return &storageMigrationRetry{err: fmt.Errorf("extend the storage migration lock: %w", err)}
	} else if !ok {
		if ok, err := m.lock.Acquire(ctx, token, storageMigrationLockTTL); err != nil {
			return &storageMigrationRetry{err: fmt.Errorf("take the storage migration lock again: %w", err)}
		} else if !ok {
			slog.Warn("drive: another run took the storage migration lock while copying a file", "fileId", f.ID)
			return errStorageMigrationLockLost
		}
		// 取り直せても、切れている間に別の実行 (写しを消す処理など) が始まって終わっていれば、
		// 置いた写しが消えているかもしれない。倒す前に、置いた写しを確かめ直す。
		if err := m.copiesArePresent(dst, f, copied); err != nil {
			if errors.Is(err, errMigrationCopyGone) {
				// lock は取り直して持っているので、残った写しを消してから失敗にする。残すと、
				// どの記録にも無い写しになり、ファイルを消しても消えない。
				for _, key := range copied {
					if derr := dst.Delete(key); derr != nil {
						slog.Warn("drive: delete a copy left by a migration that lost the lock", "fileId", f.ID, "err", derr)
					}
				}
			}
			return err
		}
	}
	// 行を書き換えるのは、すべての実体を置けてから。途中で失敗しても、行は
	// storedInternal=true のままで、ローカルから配信され続ける。
	marked, err := m.repo.MarkMigrated(ctx, f.ID, m.now())
	if err != nil {
		return fmt.Errorf("mark the file as migrated: %w", err)
	}
	if !marked {
		// 印を付けられなかったのは、写している間にファイルが消されたか link 行へ
		// 倒されたか、同じファイルを別のジョブが先に移したとき。同じ実行のジョブが
		// 重なることはある (再配送や、処理の期限を過ぎた handler の再試行)。先に移した
		// 側の実体を消すと、行がオブジェクトストレージを指したまま実体が無くなるので、
		// 行が写した状態にあるときは残し、それ以外 (消えた・link 行) のときだけ消す。
		migrated, err := m.repo.IsMigrated(ctx, f.ID)
		if err != nil {
			return fmt.Errorf("check whether the file was migrated: %w", err)
		}
		if migrated {
			return nil
		}
		for _, k := range keys {
			if k.key != nil && *k.key != "" {
				if err := dst.Delete(*k.key); err != nil {
					slog.Warn("drive: delete the copy of a file deleted during the storage migration", "fileId", f.ID, "err", err)
				}
			}
		}
	}
	return nil
}

// errMigrationCopyGone is recorded when a copy put by the migration was gone
// or changed after the run lost and took back the lock (#4197). The row stays
// local, and the next run copies the file again.
var errMigrationCopyGone = errors.New("a copy was removed or changed while the lock was lost; the next run copies the file again")

// copiesArePresent reports errMigrationCopyGone unless dst has every key in
// copied: the body with the recorded size and md5, the variants at all.
func (m *StorageMigrator) copiesArePresent(dst Storage, f *model.DriveFile, copied []string) error {
	for _, key := range copied {
		size, sum, err := readObject(dst, key)
		if errors.Is(err, ErrObjectNotFound) {
			return errMigrationCopyGone
		}
		if err != nil {
			return err
		}
		if f.AccessKey != nil && *f.AccessKey == key && (size != int64(f.Size) || !strings.EqualFold(sum, f.MD5)) {
			return errMigrationCopyGone
		}
	}
	return nil
}

// storageMigrationRetry wraps an error writing the database while handling
// a file. runBatch retries the job instead of recording it as the failure of
// the file.
type storageMigrationRetry struct{ err error }

func (e *storageMigrationRetry) Error() string { return e.err.Error() }
func (e *storageMigrationRetry) Unwrap() error { return e.err }

// verifyFile reads the migrated file back from dst and checks it against the
// row (#4181): the file must be there with the recorded size and md5. The
// thumbnail and the webpublic variant must be there too, with the size and
// md5 of the local file when it is still on the local storage. A match sets
// the verified mark; anything else, including a copy that could not be read,
// clears it and is returned as the failure.
//
// サムネイルと webpublic は、ローカルにもオブジェクトストレージにも無ければ飛ばす。
// 移す処理と同じく、もともと配信できていなかったもの。
func (m *StorageMigrator) verifyFile(ctx context.Context, dst Storage, f *model.DriveFile) error {
	if f.ObjectStorageMigratedAt == nil {
		return errors.New("the file has not been migrated")
	}
	// 読めなかったとき (認証切れ・5xx など) も印を消す。S3 は ListBucket の権限が
	// 無いと、無いキーにも 403 を返すので、「読めない」と「無い」を見分けられない。
	// 印を残すと、消えた写しに印が残り続け、ローカルを消す処理 (#4182) がそれを根拠に
	// 唯一の実体を消す。障害の間に流して消えた印は、流し直せば戻る。
	err := m.compareFile(dst, f)
	var at *time.Time
	if err == nil {
		now := m.now()
		at = &now
	}
	// 一致しなかったときは、前に確かめた印を消す。ローカルを消す処理 (#4182) は印の
	// ある行だけを対象にするので、壊れた写しを根拠にローカルを消さない。
	if _, serr := m.repo.SetVerified(ctx, f.ID, *f.ObjectStorageMigratedAt, at); serr != nil {
		return &storageMigrationRetry{err: fmt.Errorf("record the verification: %w", serr)}
	}
	return err
}

// purgeFile deletes the local copies of a verified file (#4182): the file,
// its thumbnail and its webpublic variant. It compares the copies in dst
// with the row once more first, as verifyFile does; when they do not match
// or cannot be read, it clears the verified mark and keeps the local copies.
//
// 確かめてから消すまでの間にも、オブジェクトストレージの写しは消えたり壊れたりしうる
// (ライフサイクルのルール、手作業の削除など)。ローカルを消すと取り戻せないので、
// 消す直前にもう一度確かめる。
func (m *StorageMigrator) purgeFile(ctx context.Context, dst Storage, f *model.DriveFile, token string) error {
	if f.ObjectStorageMigratedAt == nil || f.ObjectStorageVerifiedAt == nil {
		return errors.New("the file has not been verified")
	}
	if err := m.compareFile(dst, f); err != nil {
		if _, serr := m.repo.SetVerified(ctx, f.ID, *f.ObjectStorageMigratedAt, nil); serr != nil {
			return &storageMigrationRetry{err: fmt.Errorf("clear the verification: %w", serr)}
		}
		return fmt.Errorf("the local copy was kept: %w", err)
	}
	// 確かめるのに時間がかかる (大きなファイルは全部読む) ので、消す前に lock を延ばし
	// 直す。期限が切れて別の処理 (戻す処理 #4183 など) が始まっていたら、消さずに止める。
	// 確かめている間に期限が切れただけ (誰も持っていない) なら取り直して続ける。取り
	// 直さないと、読むのに期限を超える大きなファイルが毎回先頭で止め、先へ進めない。
	if ok, err := m.lock.Extend(ctx, token, storageMigrationLockTTL); err != nil {
		return &storageMigrationRetry{err: fmt.Errorf("extend the storage migration lock: %w", err)}
	} else if !ok {
		if ok, err := m.lock.Acquire(ctx, token, storageMigrationLockTTL); err != nil {
			return &storageMigrationRetry{err: fmt.Errorf("take the storage migration lock again: %w", err)}
		} else if !ok {
			slog.Warn("drive: another run took the storage migration lock while the purge compared a file", "fileId", f.ID)
			return errStorageMigrationLockLost
		}
	}
	// 消した日時を先に入れ、入れられた (行が確かめたときのまま移した状態にある) ときだけ
	// 消す。消すのに失敗しても、日時が入ってローカルが残るだけで、戻す処理はオブジェクト
	// ストレージから取り直す (安全な側に倒れる)。逆の順で日時を入れそびれると、ローカルが
	// 無いのに日時の無い行が残る。
	marked, err := m.repo.MarkLocalDeleted(ctx, f.ID, *f.ObjectStorageMigratedAt, m.now())
	if err != nil {
		return &storageMigrationRetry{err: fmt.Errorf("record the deletion: %w", err)}
	}
	if !marked {
		// 確かめてから今までの間に、消された・リンクに倒された・写し直された行。
		return nil
	}
	for _, k := range []*string{f.AccessKey, f.ThumbnailAccessKey, f.WebpublicAccessKey} {
		if k == nil || *k == "" {
			continue
		}
		if err := m.local.Delete(*k); err != nil {
			return fmt.Errorf("delete %s from the local storage (the row is marked as deleted): %w", *k, err)
		}
	}
	return nil
}

// restoreFile puts the file, its thumbnail and its webpublic variant back on
// the local storage and moves the row back (#4183). It keeps a local copy
// only when it matches: the file by the recorded size and md5, the
// thumbnail and the webpublic variant by the copy in dst (the row records
// no md5 for them). Anything else is read from dst, put, and read back; the
// file must match the row. The copies in dst stay.
//
// ローカルの実体は、残っていても正しいとは限らない (消す処理の途中で止まった、消す
// 前の世代の DB のバックアップへ戻した、など)。objectStorageLocalDeletedAt は
// 置くかどうかの判断には使わない。NULL でもローカルがあるとは限らないため。
func (m *StorageMigrator) restoreFile(ctx context.Context, dst Storage, f *model.DriveFile, token string) error {
	if f.ObjectStorageMigratedAt == nil {
		return errors.New("the file has not been migrated")
	}
	if f.AccessKey == nil || *f.AccessKey == "" {
		return errors.New("the file has no access key")
	}
	var written []string
	put, err := m.restoreBody(dst, f)
	if err != nil {
		return err
	}
	if put {
		written = append(written, *f.AccessKey)
	}
	for _, v := range []struct {
		name string
		key  *string
	}{{"thumbnail", f.ThumbnailAccessKey}, {"webpublic", f.WebpublicAccessKey}} {
		if v.key == nil || *v.key == "" {
			continue
		}
		put, err := m.restoreVariant(dst, v.name, *v.key, storageMigrationVariantReadLimit)
		if err != nil {
			return err
		}
		if put {
			written = append(written, *v.key)
		}
	}
	// 読むのに時間がかかる (大きなファイルは全部読む) ので、倒す前に lock を延ばし直す。
	// 期限が切れただけなら取り直し、別の実行に取られていたら止める。期限が切れている間に
	// 消す処理が割り込んでも、倒す条件 (ローカルを消した日時) が負けさせる。
	if ok, err := m.lock.Extend(ctx, token, storageMigrationLockTTL); err != nil {
		return &storageMigrationRetry{err: fmt.Errorf("extend the storage migration lock: %w", err)}
	} else if !ok {
		if ok, err := m.lock.Acquire(ctx, token, storageMigrationLockTTL); err != nil {
			return &storageMigrationRetry{err: fmt.Errorf("take the storage migration lock again: %w", err)}
		} else if !ok {
			slog.Warn("drive: another run took the storage migration lock while moving a file back", "fileId", f.ID)
			return errStorageMigrationLockLost
		}
	}
	// 戻した後もバケットに残る写しを、行を倒すのと同じトランザクションで記録する (#4197)。
	// 写しの無いキー (サムネイルを作らなかったなど) も記録するが、消すときに無いキーを消す
	// だけで失敗にはならない。
	var copies []model.DriveObjectStorageRestoredCopy
	now := m.now()
	for _, key := range []*string{f.AccessKey, f.ThumbnailAccessKey, f.WebpublicAccessKey} {
		if key != nil && *key != "" {
			copies = append(copies, model.DriveObjectStorageRestoredCopy{
				Key: *key, FileID: f.ID, Location: objectLocation(dst, *key), RestoredAt: now,
			})
		}
	}
	marked, err := m.repo.MarkRestored(ctx, f.ID, *f.ObjectStorageMigratedAt, f.ObjectStorageLocalDeletedAt, copies)
	if err != nil {
		return &storageMigrationRetry{err: fmt.Errorf("move the row back: %w", err)}
	}
	if marked {
		// 戻した行には、確かめる処理と消す処理の失敗の記録は当てはまらない。残すと、
		// 戻した後も status の失敗の件数に数えられ続ける。
		for _, op := range []string{repository.DriveStorageMigrationOperationVerify, repository.DriveStorageMigrationOperationPurge} {
			if err := m.repo.ClearFailure(ctx, f.ID, op); err != nil {
				slog.Warn("drive: clear a failure of a file moved back", "fileId", f.ID, "operation", op, "err", err)
			}
		}
		return nil
	}
	// 倒せなかった。別のジョブが先に戻した行なら、置いた実体はその行が使うので残す。
	// 読んだ後で消す処理がローカルを消した行は、移したままなので次に流せば取り直す。
	// 消された・リンクに倒された行なら、置いた実体は誰も指さないので消す。
	local, err := m.repo.IsStoredLocally(ctx, f.ID)
	if err != nil {
		return &storageMigrationRetry{err: fmt.Errorf("check whether the file was moved back: %w", err)}
	}
	if local {
		return nil
	}
	if migrated, err := m.repo.IsMigrated(ctx, f.ID); err != nil {
		return &storageMigrationRetry{err: fmt.Errorf("check whether the file is still migrated: %w", err)}
	} else if migrated {
		return nil
	}
	for _, k := range written {
		if err := m.local.Delete(k); err != nil {
			slog.Warn("drive: delete a local copy put for a file deleted while moving it back", "fileId", f.ID, "err", err)
		}
	}
	return nil
}

// restoreBody makes the local copy of the file match the row: it keeps a
// matching local copy, or puts the copy in dst when that matches. It reports
// whether it put the copy.
func (m *StorageMigrator) restoreBody(dst Storage, f *model.DriveFile) (bool, error) {
	key := *f.AccessKey
	matches := func(size int64, sum string) bool {
		return size == int64(f.Size) && strings.EqualFold(sum, f.MD5)
	}
	size, sum, err := readObject(m.local, key)
	if err != nil && !errors.Is(err, ErrObjectNotFound) {
		return false, err
	}
	if err == nil && matches(size, sum) {
		return false, nil
	}
	// 記録した大きさを 1 バイトでも超えたら、そこで読むのをやめる (一致しないことは
	// もう決まっている)。全部読むと、バケットで置き換えられた大きなオブジェクト 1 つで
	// worker のメモリを使い切りうる。
	b, err := readAllLimit(dst, key, int64(f.Size))
	if errors.Is(err, ErrObjectNotFound) {
		return false, errors.New("the file is neither on the local storage with the recorded size and md5 nor in the object storage")
	}
	if errors.Is(err, errObjectTooLarge) {
		return false, errors.New("neither the local copy nor the copy in the object storage matches the recorded size and md5")
	}
	if err != nil {
		return false, err
	}
	remoteSum := md5.Sum(b)
	if !matches(int64(len(b)), hex.EncodeToString(remoteSum[:])) {
		return false, errors.New("neither the local copy nor the copy in the object storage matches the recorded size and md5")
	}
	if err := m.putAndReadBack(key, b); err != nil {
		return true, err
	}
	return true, nil
}

// restoreVariant makes the local copy of a thumbnail or a webpublic variant
// match the copy in dst: it keeps a local copy with the size and md5 of the
// copy, or puts the copy. With no copy in dst, a local copy is kept as it is,
// and with neither the variant is skipped. It reports whether it put the
// copy.
//
// ローカルを確かめずに使うと、書きかけで止まったファイル (空き容量の不足や、置いて
// いる途中の停止) を次に流したときにそのまま使い、壊れた中身を配る。行に md5 が無い
// ので、写しと比べる。
func (m *StorageMigrator) restoreVariant(dst Storage, name, key string, limit int64) (bool, error) {
	localSize, localSum, lerr := readObject(m.local, key)
	if lerr != nil && !errors.Is(lerr, ErrObjectNotFound) {
		return false, lerr
	}
	b, rerr := readAllLimit(dst, key, limit)
	switch {
	case errors.Is(rerr, errObjectTooLarge):
		return false, fmt.Errorf("the %s in the object storage is larger than %d bytes", name, limit)
	case errors.Is(rerr, ErrObjectNotFound):
		// オブジェクトストレージに無い。ローカルがあればそれを使い、どちらにも無ければ
		// もともと配信できていなかったもの。
		return false, nil
	case rerr != nil:
		return false, rerr
	}
	remoteSum := md5.Sum(b)
	if lerr == nil && localSize == int64(len(b)) && localSum == hex.EncodeToString(remoteSum[:]) {
		return false, nil
	}
	if err := m.putAndReadBack(key, b); err != nil {
		return true, fmt.Errorf("the %s: %w", name, err)
	}
	return true, nil
}

// putAndReadBack puts b on the local storage under key and reads it back:
// the put must leave the same bytes.
func (m *StorageMigrator) putAndReadBack(key string, b []byte) error {
	if _, err := m.local.Put(key, bytes.NewReader(b)); err != nil {
		return fmt.Errorf("put %s to the local storage: %w", key, err)
	}
	size, sum, err := readObject(m.local, key)
	if err != nil {
		return fmt.Errorf("read %s back from the local storage: %w", key, err)
	}
	want := md5.Sum(b)
	if size != int64(len(b)) || sum != hex.EncodeToString(want[:]) {
		return fmt.Errorf("%s on the local storage does not match what was put", key)
	}
	return nil
}

// errObjectTooLarge is returned by readAllLimit for an object larger than the
// limit.
var errObjectTooLarge = errors.New("the object is larger than expected")

// storageMigrationVariantReadLimit is how many bytes of a thumbnail or a
// webpublic variant restoreVariant reads at most.
//
// 行に大きさが無いので、本体の大きさでは縛らない (本体が大きな動画でも、派生は
// 静止画)。どちらも長辺を縮めた画像なので、この上限に届くのは置き換えられた
// オブジェクトだけになる。
const storageMigrationVariantReadLimit = 64 << 20

// readAllLimit reads the object key of st, or fails with errObjectTooLarge
// when it is larger than limit bytes, without reading past limit+1 bytes.
func readAllLimit(st Storage, key string, limit int64) ([]byte, error) {
	rc, err := st.Get(key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("read %s: %w", key, errObjectTooLarge)
	}
	return b, nil
}

// compareFile returns why the copies in dst do not match the row, or nil.
func (m *StorageMigrator) compareFile(dst Storage, f *model.DriveFile) error {
	if f.AccessKey == nil || *f.AccessKey == "" {
		return errors.New("the file has no access key")
	}
	size, sum, err := readObject(dst, *f.AccessKey)
	if errors.Is(err, ErrObjectNotFound) {
		return errors.New("the file is not in the object storage")
	}
	if err != nil {
		return err
	}
	if size != int64(f.Size) {
		return fmt.Errorf("the size in the object storage (%d) does not match the recorded size (%d)", size, f.Size)
	}
	if !strings.EqualFold(sum, f.MD5) {
		return fmt.Errorf("the md5 in the object storage (%s) does not match the recorded md5 (%s)", sum, f.MD5)
	}
	for _, v := range []struct {
		name string
		key  *string
	}{{"thumbnail", f.ThumbnailAccessKey}, {"webpublic", f.WebpublicAccessKey}} {
		if v.key == nil || *v.key == "" {
			continue
		}
		remote, remoteSum, rerr := readObject(dst, *v.key)
		local, localSum, lerr := readObject(m.local, *v.key)
		if lerr != nil && !errors.Is(lerr, ErrObjectNotFound) {
			return lerr
		}
		switch {
		case errors.Is(rerr, ErrObjectNotFound) && errors.Is(lerr, ErrObjectNotFound):
			continue
		case errors.Is(rerr, ErrObjectNotFound):
			return fmt.Errorf("the %s is not in the object storage", v.name)
		case rerr != nil:
			return rerr
		case lerr == nil && local != remote:
			return fmt.Errorf("the size of the %s in the object storage (%d) does not match the local file (%d)", v.name, remote, local)
		case lerr == nil && remoteSum != localSum:
			return fmt.Errorf("the md5 of the %s in the object storage does not match the local file", v.name)
		}
	}
	return nil
}

// readObject reads the object key from st and returns its size and md5.
func readObject(st Storage, key string) (int64, string, error) {
	rc, err := st.Get(key)
	if err != nil {
		return 0, "", err
	}
	defer rc.Close()
	h := md5.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return 0, "", fmt.Errorf("read %s: %w", key, err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// truncateUTF8 cuts s to at most n bytes without splitting a UTF-8 sequence
// (PostgreSQL refuses an invalid byte sequence in a text column).
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// copyObject copies one object from src to dst.
func copyObject(src, dst Storage, key string) error {
	rc, err := src.Get(key)
	if err != nil {
		return err
	}
	defer rc.Close()
	// S3Storage.Put は *bytes.Reader 以外を一度全部読むので、ここで読んで渡す。
	b, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read %s from the local storage: %w", key, err)
	}
	if _, err := dst.Put(key, bytes.NewReader(b)); err != nil {
		return fmt.Errorf("put %s to the object storage: %w", key, err)
	}
	return nil
}

// storageMigrationFailureLimit is the number of failures Status returns.
const storageMigrationFailureLimit = 100

// Running reports whether a migration run holds the lock.
//
// 確かめる処理 (#4181) の実行は数えない。写さないので、削除の経路がオブジェクト
// ストレージの写しを消す必要が無い。
func (m *StorageMigrator) Running(ctx context.Context) (bool, error) {
	holder, err := m.lock.Holder(ctx)
	if err != nil {
		return false, err
	}
	return holder != "" && operationOfToken(holder) == StorageMigrationOperationMigrate, nil
}

// Status returns the state of the migration and the verification.
func (m *StorageMigrator) Status(ctx context.Context) (StorageMigrationStatus, error) {
	st := StorageMigrationStatus{ObjectStorageEnabled: m.objectStorage() != nil}
	holder, err := m.lock.Holder(ctx)
	if err != nil {
		return st, fmt.Errorf("drive: read the storage migration lock: %w", err)
	}
	if holder != "" {
		st.Running = operationOfToken(holder)
	}
	c, err := m.repo.Counts(ctx)
	if err != nil {
		return st, fmt.Errorf("drive: count files: %w", err)
	}
	st.Local, st.Migrated, st.Failed = c.Local, c.Migrated, c.Failed
	st.Verified, st.VerifyFailed = c.Verified, c.VerifyFailed
	st.Purgeable, st.LocalDeleted, st.PurgeFailed = c.Purgeable, c.LocalDeleted, c.PurgeFailed
	st.RestoreFailed = c.RestoreFailed
	st.RestoredCopies, st.CleanupFailed = c.RestoredCopies, c.CleanupFailed
	st.Failures, err = m.repo.ListFailures(ctx, repository.DriveStorageMigrationOperationMigrate, storageMigrationFailureLimit)
	if err != nil {
		return st, fmt.Errorf("drive: list failures: %w", err)
	}
	st.VerifyFailures, err = m.repo.ListFailures(ctx, repository.DriveStorageMigrationOperationVerify, storageMigrationFailureLimit)
	if err != nil {
		return st, fmt.Errorf("drive: list verification failures: %w", err)
	}
	st.PurgeFailures, err = m.repo.ListFailures(ctx, repository.DriveStorageMigrationOperationPurge, storageMigrationFailureLimit)
	if err != nil {
		return st, fmt.Errorf("drive: list purge failures: %w", err)
	}
	st.RestoreFailures, err = m.repo.ListFailures(ctx, repository.DriveStorageMigrationOperationRestore, storageMigrationFailureLimit)
	if err != nil {
		return st, fmt.Errorf("drive: list restore failures: %w", err)
	}
	st.CleanupFailures, err = m.repo.ListRestoredCopyFailures(ctx, storageMigrationFailureLimit)
	if err != nil {
		return st, fmt.Errorf("drive: list cleanup failures: %w", err)
	}
	return st, nil
}

func newMigrationToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("drive: make a storage migration token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// RedisStorageMigrationLock is a StorageMigrationLock in Redis.
type RedisStorageMigrationLock struct {
	rdb redis.UniversalClient
	key string
}

// NewRedisStorageMigrationLock returns the lock under prefix (the Redis key
// prefix of the server).
func NewRedisStorageMigrationLock(rdb redis.UniversalClient, prefix string) *RedisStorageMigrationLock {
	return &RedisStorageMigrationLock{rdb: rdb, key: prefix + "elythia:drive:objectStorageMigration"}
}

// acquireStorageMigrationScript takes the lock for ARGV[1] or extends it when
// ARGV[1] already holds it, for ARGV[2] milliseconds.
var acquireStorageMigrationScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v == ARGV[1] then redis.call('PEXPIRE', KEYS[1], ARGV[2]) return 1 end
if not v then redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2]) return 1 end
return 0`)

// extendStorageMigrationScript extends the lock for ARGV[2] milliseconds when
// ARGV[1] holds it.
var extendStorageMigrationScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then redis.call('PEXPIRE', KEYS[1], ARGV[2]) return 1 end
return 0`)

var releaseStorageMigrationScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0`)

// Acquire implements StorageMigrationLock.
func (l *RedisStorageMigrationLock) Acquire(ctx context.Context, token string, ttl time.Duration) (bool, error) {
	n, err := acquireStorageMigrationScript.Run(ctx, l.rdb, []string{l.key}, token, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Extend implements StorageMigrationLock.
func (l *RedisStorageMigrationLock) Extend(ctx context.Context, token string, ttl time.Duration) (bool, error) {
	n, err := extendStorageMigrationScript.Run(ctx, l.rdb, []string{l.key}, token, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Release implements StorageMigrationLock.
func (l *RedisStorageMigrationLock) Release(ctx context.Context, token string) error {
	return releaseStorageMigrationScript.Run(ctx, l.rdb, []string{l.key}, token).Err()
}

// Holder implements StorageMigrationLock.
func (l *RedisStorageMigrationLock) Holder(ctx context.Context) (string, error) {
	v, err := l.rdb.Get(ctx, l.key).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

// Held implements StorageMigrationLock.
func (l *RedisStorageMigrationLock) Held(ctx context.Context) (bool, error) {
	n, err := l.rdb.Exists(ctx, l.key).Result()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
