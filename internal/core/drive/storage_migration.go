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
)

// The operations of a storage migration run, reported by Status.
const (
	StorageMigrationOperationMigrate = "migrate"
	StorageMigrationOperationVerify  = "verify"
)

// verifyTokenPrefix starts the token of a verification run. The lock holds
// the token, so the token tells which operation runs.
//
// 移す処理の token は接頭辞を持たない。#4181 より前の版が取った lock (反映の
// 前後で実行中だったもの) も、移す処理として読める。
const verifyTokenPrefix = "verify-"

// operationOfToken returns the operation of the run token names.
func operationOfToken(token string) string {
	if strings.HasPrefix(token, verifyTokenPrefix) {
		return StorageMigrationOperationVerify
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
		list:      m.repo.ListLocal,
		each:      m.migrateFile,
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

// storageMigrationRun is what one operation does in runBatch.
type storageMigrationRun struct {
	// list returns the next files of the operation after afterID.
	list func(ctx context.Context, afterID string, limit int) ([]model.DriveFile, error)
	// each handles one file. An error is recorded as the failure of the file.
	each      func(ctx context.Context, dst Storage, f *model.DriveFile) error
	operation string
	enqueue   func(token, afterID string) error
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
	st.Failures, err = m.repo.ListFailures(ctx, repository.DriveStorageMigrationOperationMigrate, storageMigrationFailureLimit)
	if err != nil {
		return st, fmt.Errorf("drive: list failures: %w", err)
	}
	st.VerifyFailures, err = m.repo.ListFailures(ctx, repository.DriveStorageMigrationOperationVerify, storageMigrationFailureLimit)
	if err != nil {
		return st, fmt.Errorf("drive: list verification failures: %w", err)
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
