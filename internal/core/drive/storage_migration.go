package drive

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
)

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
	IsMigrated(ctx context.Context, id string) (bool, error)
	RecordFailure(ctx context.Context, fileID, operation, msg string, at time.Time) error
	ClearFailure(ctx context.Context, fileID, operation string) error
	ListFailures(ctx context.Context, operation string, limit int) ([]model.DriveObjectStorageMigrationFailure, error)
	Counts(ctx context.Context) (repository.DriveStorageMigrationCounts, error)
}

// StorageMigrationEnqueuer enqueues the next job of the migration.
type StorageMigrationEnqueuer interface {
	EnqueueMigrateLocalDriveFiles(token, afterID string) error
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
}

// StorageMigrationStatus is what the admin API shows of the migration.
type StorageMigrationStatus struct {
	// ObjectStorageEnabled reports whether object storage is configured, so
	// the migration has somewhere to copy to.
	ObjectStorageEnabled bool
	Running              bool
	// Local, Migrated and Failed count the files still stored locally, the
	// files the migration copied, and the files whose last attempt failed.
	Local    int64
	Migrated int64
	Failed   int64
	// Failures are the most recent failures, newest first.
	Failures []model.DriveObjectStorageMigrationFailure
}

// StorageMigrator copies the drive files stored locally to object storage
// (#4180). It only copies: the local files stay, and the url columns keep
// pointing at /files/<accessKey>, which serves the file from object storage
// once storedInternal is false.
type StorageMigrator struct {
	repo    StorageMigrationRepository
	local   Storage
	primary Storage
	enq     StorageMigrationEnqueuer
	lock    StorageMigrationLock
	now     func() time.Time
}

// NewStorageMigrator constructs a StorageMigrator. local is the local storage
// the files are on, primary the storage the server uploads to (a
// MetaStorage); the migration copies only while primary resolves to object
// storage.
func NewStorageMigrator(repo StorageMigrationRepository, local, primary Storage, enq StorageMigrationEnqueuer, lock StorageMigrationLock) *StorageMigrator {
	return &StorageMigrator{repo: repo, local: local, primary: primary, enq: enq, lock: lock, now: time.Now}
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
	token, err := newMigrationToken()
	if err != nil {
		return err
	}
	ok, err := m.lock.Acquire(ctx, token, storageMigrationLockTTL)
	if err != nil {
		return fmt.Errorf("drive: take the storage migration lock: %w", err)
	}
	if !ok {
		return ErrStorageMigrationRunning
	}
	if err := m.enq.EnqueueMigrateLocalDriveFiles(token, ""); err != nil {
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
	files, err := m.repo.ListLocal(ctx, afterID, storageMigrationBatch)
	if err != nil {
		return fmt.Errorf("drive: list local files: %w", err)
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
		if err := m.migrateFile(ctx, dst, f); err != nil {
			msg := truncateUTF8(err.Error(), maxStorageMigrationError)
			if rerr := m.repo.RecordFailure(ctx, f.ID, repository.DriveStorageMigrationOperationMigrate, msg, m.now()); rerr != nil {
				return fmt.Errorf("drive: record the failure of %s: %w", f.ID, rerr)
			}
			continue
		}
		if err := m.repo.ClearFailure(ctx, f.ID, repository.DriveStorageMigrationOperationMigrate); err != nil {
			return fmt.Errorf("drive: clear the failure of %s: %w", f.ID, err)
		}
	}
	if len(files) < storageMigrationBatch {
		return m.lock.Release(ctx, token)
	}
	return m.enq.EnqueueMigrateLocalDriveFiles(token, files[len(files)-1].ID)
}

// errLocalFileMissing is recorded when the local storage has no file for the
// row.
var errLocalFileMissing = errors.New("the file is not on the local storage")

// migrateFile copies the file, its thumbnail and its webpublic variant to
// dst under the same access keys, then marks the row as migrated.
//
// サムネイルと webpublic の実体がローカルに無いときは飛ばす。もともと配信できて
// いなかったもので、写した後も同じく見つからない。本体が無いときは失敗にする。
func (m *StorageMigrator) migrateFile(ctx context.Context, dst Storage, f *model.DriveFile) error {
	keys := []struct {
		key      *string
		required bool
	}{{f.AccessKey, true}, {f.ThumbnailAccessKey, false}, {f.WebpublicAccessKey, false}}
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
func (m *StorageMigrator) Running(ctx context.Context) (bool, error) {
	return m.lock.Held(ctx)
}

// Status returns the state of the migration.
func (m *StorageMigrator) Status(ctx context.Context) (StorageMigrationStatus, error) {
	st := StorageMigrationStatus{ObjectStorageEnabled: m.objectStorage() != nil}
	running, err := m.lock.Held(ctx)
	if err != nil {
		return st, fmt.Errorf("drive: read the storage migration lock: %w", err)
	}
	st.Running = running
	c, err := m.repo.Counts(ctx)
	if err != nil {
		return st, fmt.Errorf("drive: count files: %w", err)
	}
	st.Local, st.Migrated, st.Failed = c.Local, c.Migrated, c.Failed
	st.Failures, err = m.repo.ListFailures(ctx, repository.DriveStorageMigrationOperationMigrate, storageMigrationFailureLimit)
	if err != nil {
		return st, fmt.Errorf("drive: list failures: %w", err)
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

// Held implements StorageMigrationLock.
func (l *RedisStorageMigrationLock) Held(ctx context.Context) (bool, error) {
	n, err := l.rdb.Exists(ctx, l.key).Result()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
