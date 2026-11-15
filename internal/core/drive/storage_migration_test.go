package drive

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// memObjectStorage is an object storage held in memory. It is not a
// *LocalStorage, so StorageIsLocal reports false for it.
type memObjectStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
	putErr  map[string]error
	// deleteErr fails the deletion of the key (#4197).
	deleteErr map[string]error
	deleted   []string
}

func newMemObjectStorage() *memObjectStorage {
	return &memObjectStorage{objects: map[string][]byte{}, putErr: map[string]error{}, deleteErr: map[string]error{}}
}

func (s *memObjectStorage) Put(key string, body io.Reader) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.putErr[key]; err != nil {
		return "", err
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	s.objects[key] = b
	return "https://s3.example/" + key, nil
}

func (s *memObjectStorage) Get(key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	if !ok {
		return nil, ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *memObjectStorage) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.deleteErr[key]; err != nil {
		return err
	}
	delete(s.objects, key)
	s.deleted = append(s.deleted, key)
	return nil
}

// memMigrationRepo is a StorageMigrationRepository over a slice of rows.
type memMigrationRepo struct {
	mu       sync.Mutex
	files    []model.DriveFile
	failures map[string]model.DriveObjectStorageMigrationFailure
	// copies are the recorded copies left by moving files back (#4197), by
	// key.
	copies map[string]model.DriveObjectStorageRestoredCopy
	// deleteOnMark deletes the row just before MarkMigrated, as if the file
	// were deleted while it was being copied.
	deleteOnMark string
	// markOnMark marks the row as migrated just before MarkMigrated, as if
	// another job of the same run had migrated it first.
	markOnMark string
	// linkOnMark turns the row into a link row just before MarkMigrated, as if
	// the remote cache cleanup had expired it meanwhile.
	linkOnMark string
	listErr    error
}

func newMemMigrationRepo() *memMigrationRepo {
	return &memMigrationRepo{failures: map[string]model.DriveObjectStorageMigrationFailure{},
		copies: map[string]model.DriveObjectStorageRestoredCopy{}}
}

func (r *memMigrationRepo) ListLocal(_ context.Context, afterID string, limit int) ([]model.DriveFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	var out []model.DriveFile
	for _, f := range r.files {
		if f.StoredInternal && !f.IsLink && f.ID > afterID {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b model.DriveFile) int { return strings.Compare(a.ID, b.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *memMigrationRepo) MarkMigrated(_ context.Context, id string, at time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleteOnMark == id {
		r.files = slices.DeleteFunc(r.files, func(f model.DriveFile) bool { return f.ID == id })
	}
	if r.linkOnMark == id {
		for i := range r.files {
			if r.files[i].ID == id {
				r.files[i].IsLink, r.files[i].StoredInternal = true, false
			}
		}
	}
	if r.markOnMark == id {
		for i := range r.files {
			if r.files[i].ID == id {
				r.files[i].StoredInternal = false
				r.files[i].ObjectStorageMigratedAt = &at
			}
		}
	}
	for i := range r.files {
		if r.files[i].ID == id && r.files[i].StoredInternal && !r.files[i].IsLink {
			r.files[i].StoredInternal = false
			r.files[i].ObjectStorageMigratedAt = &at
			for k, c := range r.copies {
				if c.FileID == id {
					delete(r.copies, k)
				}
			}
			return true, nil
		}
	}
	return false, nil
}

func (r *memMigrationRepo) ListMigrated(_ context.Context, afterID string, limit int) ([]model.DriveFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	var out []model.DriveFile
	for _, f := range r.files {
		if !f.StoredInternal && !f.IsLink && f.ObjectStorageMigratedAt != nil && f.ID > afterID {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b model.DriveFile) int { return strings.Compare(a.ID, b.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *memMigrationRepo) SetVerified(_ context.Context, id string, migratedAt time.Time, at *time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.files {
		f := &r.files[i]
		if f.ID == id && !f.StoredInternal && !f.IsLink && f.ObjectStorageMigratedAt != nil && f.ObjectStorageMigratedAt.Equal(migratedAt) {
			f.ObjectStorageVerifiedAt = at
			return true, nil
		}
	}
	return false, nil
}

func (r *memMigrationRepo) ListPurgeable(_ context.Context, afterID string, limit int) ([]model.DriveFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	var out []model.DriveFile
	for _, f := range r.files {
		if !f.StoredInternal && !f.IsLink && f.ObjectStorageMigratedAt != nil && f.ObjectStorageVerifiedAt != nil &&
			f.ObjectStorageLocalDeletedAt == nil && f.ID > afterID {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b model.DriveFile) int { return strings.Compare(a.ID, b.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *memMigrationRepo) MarkLocalDeleted(_ context.Context, id string, migratedAt, at time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.files {
		f := &r.files[i]
		if f.ID == id && !f.StoredInternal && !f.IsLink && f.ObjectStorageMigratedAt != nil && f.ObjectStorageMigratedAt.Equal(migratedAt) &&
			f.ObjectStorageLocalDeletedAt == nil {
			f.ObjectStorageLocalDeletedAt = &at
			return true, nil
		}
	}
	return false, nil
}

func (r *memMigrationRepo) IsStoredLocally(_ context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.files {
		if f.ID == id {
			return f.StoredInternal && !f.IsLink, nil
		}
	}
	return false, nil
}

func (r *memMigrationRepo) MarkRestored(_ context.Context, id string, migratedAt time.Time, localDeletedAt *time.Time, copies []model.DriveObjectStorageRestoredCopy) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sameTime := func(a, b *time.Time) bool { return (a == nil) == (b == nil) && (a == nil || a.Equal(*b)) }
	for i := range r.files {
		f := &r.files[i]
		if f.ID == id && !f.StoredInternal && !f.IsLink && f.ObjectStorageMigratedAt != nil && f.ObjectStorageMigratedAt.Equal(migratedAt) &&
			sameTime(f.ObjectStorageLocalDeletedAt, localDeletedAt) {
			f.StoredInternal = true
			f.ObjectStorageMigratedAt, f.ObjectStorageVerifiedAt, f.ObjectStorageLocalDeletedAt = nil, nil, nil
			for _, c := range copies {
				r.copies[c.Key] = c
			}
			return true, nil
		}
	}
	return false, nil
}

func (r *memMigrationRepo) ListRestoredCopies(_ context.Context, afterKey string, limit int) ([]model.DriveObjectStorageRestoredCopy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	var out []model.DriveObjectStorageRestoredCopy
	for _, c := range r.copies {
		if c.Key > afterKey {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b model.DriveObjectStorageRestoredCopy) int { return strings.Compare(a.Key, b.Key) })
	return out[:min(len(out), limit)], nil
}

func (r *memMigrationRepo) ListRestoredCopiesOfFile(_ context.Context, fileID string) ([]model.DriveObjectStorageRestoredCopy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.DriveObjectStorageRestoredCopy
	for _, c := range r.copies {
		if c.FileID == fileID {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b model.DriveObjectStorageRestoredCopy) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

func (r *memMigrationRepo) DeleteRestoredCopy(_ context.Context, key, location string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.copies[key]; ok && c.Location == location {
		delete(r.copies, key)
	}
	return nil
}

func (r *memMigrationRepo) RecordRestoredCopyFailure(_ context.Context, key, location, msg string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.copies[key]; ok && c.Location == location {
		c.Error, c.FailedAt = &msg, &at
		r.copies[key] = c
	}
	return nil
}

func (r *memMigrationRepo) ListRestoredCopyFailures(_ context.Context, limit int) ([]model.DriveObjectStorageRestoredCopy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.DriveObjectStorageRestoredCopy
	for _, c := range r.copies {
		if c.FailedAt != nil {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b model.DriveObjectStorageRestoredCopy) int { return strings.Compare(a.Key, b.Key) })
	return out[:min(len(out), limit)], nil
}

func (r *memMigrationRepo) FindDriveFile(_ context.Context, id string) (*model.DriveFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.files {
		if f.ID == id {
			return &f, nil
		}
	}
	return nil, nil
}

// copy returns the recorded copy with key.
func (r *memMigrationRepo) copy(key string) (model.DriveObjectStorageRestoredCopy, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.copies[key]
	return c, ok
}

func (r *memMigrationRepo) IsMigrated(_ context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.files {
		if f.ID == id {
			return !f.StoredInternal && !f.IsLink && f.ObjectStorageMigratedAt != nil, nil
		}
	}
	return false, nil
}

func (r *memMigrationRepo) RecordFailure(_ context.Context, fileID, op, msg string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures[fileID+"/"+op] = model.DriveObjectStorageMigrationFailure{FileID: fileID, Operation: op, Error: msg, FailedAt: at}
	return nil
}

func (r *memMigrationRepo) ClearFailure(_ context.Context, fileID, op string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failures, fileID+"/"+op)
	return nil
}

func (r *memMigrationRepo) ListFailures(_ context.Context, op string, limit int) ([]model.DriveObjectStorageMigrationFailure, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.DriveObjectStorageMigrationFailure
	for _, f := range r.failures {
		if f.Operation == op {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b model.DriveObjectStorageMigrationFailure) int { return strings.Compare(a.FileID, b.FileID) })
	return out[:min(len(out), limit)], nil
}

func (r *memMigrationRepo) Counts(context.Context) (repository.DriveStorageMigrationCounts, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var c repository.DriveStorageMigrationCounts
	for _, f := range r.files {
		if f.StoredInternal && !f.IsLink {
			c.Local++
		}
		if f.ObjectStorageMigratedAt != nil {
			c.Migrated++
		}
		if f.ObjectStorageVerifiedAt != nil {
			c.Verified++
		}
		if !f.StoredInternal && !f.IsLink && f.ObjectStorageMigratedAt != nil && f.ObjectStorageVerifiedAt != nil && f.ObjectStorageLocalDeletedAt == nil {
			c.Purgeable++
		}
		if f.ObjectStorageLocalDeletedAt != nil {
			c.LocalDeleted++
		}
	}
	for _, f := range r.failures {
		switch f.Operation {
		case repository.DriveStorageMigrationOperationMigrate:
			c.Failed++
		case repository.DriveStorageMigrationOperationVerify:
			c.VerifyFailed++
		case repository.DriveStorageMigrationOperationPurge:
			c.PurgeFailed++
		case repository.DriveStorageMigrationOperationRestore:
			c.RestoreFailed++
		}
	}
	for _, cp := range r.copies {
		c.RestoredCopies++
		if cp.FailedAt != nil {
			c.CleanupFailed++
		}
	}
	return c, nil
}

func (r *memMigrationRepo) file(id string) model.DriveFile {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.files {
		if f.ID == id {
			return f
		}
	}
	return model.DriveFile{}
}

// memMigrationLock is a StorageMigrationLock in memory.
type memMigrationLock struct {
	mu     sync.Mutex
	holder string
}

func (l *memMigrationLock) Acquire(_ context.Context, token string, _ time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder == "" || l.holder == token {
		l.holder = token
		return true, nil
	}
	return false, nil
}

func (l *memMigrationLock) Extend(_ context.Context, token string, _ time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holder != "" && l.holder == token, nil
}

func (l *memMigrationLock) Release(_ context.Context, token string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder == token {
		l.holder = ""
	}
	return nil
}

func (l *memMigrationLock) Held(context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holder != "", nil
}

func (l *memMigrationLock) Holder(context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holder, nil
}

// expire drops the lock, as if its TTL ran out.
func (l *memMigrationLock) expire() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holder = ""
}

type migrationJob struct{ token, afterID string }

type recordingMigrationEnqueuer struct {
	mu   sync.Mutex
	jobs []migrationJob
	// verifyJobs are the jobs of the verification (#4181).
	verifyJobs []migrationJob
	// purgeJobs are the jobs of the purge (#4182).
	purgeJobs []migrationJob
	// restoreJobs are the jobs moving the files back (#4183).
	restoreJobs []migrationJob
	// cleanupJobs are the jobs deleting the copies left by moving files back
	// (#4197).
	cleanupJobs []migrationJob
	err         error
}

func (e *recordingMigrationEnqueuer) EnqueueCleanupRestoredDriveFileCopies(token, afterKey string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.cleanupJobs = append(e.cleanupJobs, migrationJob{token, afterKey})
	return nil
}

// popCleanup removes and returns the oldest cleanup job.
func (e *recordingMigrationEnqueuer) popCleanup() (migrationJob, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.cleanupJobs) == 0 {
		return migrationJob{}, false
	}
	j := e.cleanupJobs[0]
	e.cleanupJobs = e.cleanupJobs[1:]
	return j, true
}

func (e *recordingMigrationEnqueuer) EnqueueRestoreMigratedDriveFiles(token, afterID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.restoreJobs = append(e.restoreJobs, migrationJob{token, afterID})
	return nil
}

// popRestore removes and returns the oldest restore job.
func (e *recordingMigrationEnqueuer) popRestore() (migrationJob, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.restoreJobs) == 0 {
		return migrationJob{}, false
	}
	j := e.restoreJobs[0]
	e.restoreJobs = e.restoreJobs[1:]
	return j, true
}

func (e *recordingMigrationEnqueuer) EnqueuePurgeLocalDriveFiles(token, afterID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.purgeJobs = append(e.purgeJobs, migrationJob{token, afterID})
	return nil
}

// popPurge removes and returns the oldest purge job.
func (e *recordingMigrationEnqueuer) popPurge() (migrationJob, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.purgeJobs) == 0 {
		return migrationJob{}, false
	}
	j := e.purgeJobs[0]
	e.purgeJobs = e.purgeJobs[1:]
	return j, true
}

func (e *recordingMigrationEnqueuer) EnqueueVerifyMigratedDriveFiles(token, afterID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.verifyJobs = append(e.verifyJobs, migrationJob{token, afterID})
	return nil
}

// popVerify removes and returns the oldest verification job.
func (e *recordingMigrationEnqueuer) popVerify() (migrationJob, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.verifyJobs) == 0 {
		return migrationJob{}, false
	}
	j := e.verifyJobs[0]
	e.verifyJobs = e.verifyJobs[1:]
	return j, true
}

func (e *recordingMigrationEnqueuer) EnqueueMigrateLocalDriveFiles(token, afterID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.jobs = append(e.jobs, migrationJob{token, afterID})
	return nil
}

// pop removes and returns the oldest job, or false when none is queued.
func (e *recordingMigrationEnqueuer) pop() (migrationJob, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.jobs) == 0 {
		return migrationJob{}, false
	}
	j := e.jobs[0]
	e.jobs = e.jobs[1:]
	return j, true
}

type migrationFixture struct {
	local *LocalStorage
	dst   *memObjectStorage
	repo  *memMigrationRepo
	lock  *memMigrationLock
	enq   *recordingMigrationEnqueuer
	m     *StorageMigrator
}

func newMigrationFixture(t *testing.T) *migrationFixture {
	t.Helper()
	f := &migrationFixture{
		local: NewLocalStorage(t.TempDir(), "https://local.example/files"),
		dst:   newMemObjectStorage(),
		repo:  newMemMigrationRepo(),
		lock:  &memMigrationLock{},
		enq:   &recordingMigrationEnqueuer{},
	}
	f.m = NewStorageMigrator(f.repo, f.local, f.dst, f.enq, f.lock)
	return f
}

// addLocal stores a file on the local storage with a thumbnail and a
// webpublic variant, and adds its row.
func (f *migrationFixture) addLocal(t *testing.T, id string) {
	t.Helper()
	ak, tk, wk := "ak-"+id, "tk-"+id, "wk-"+id
	for _, k := range []string{ak, tk, wk} {
		_, err := f.local.Put(k, strings.NewReader("body of "+k))
		require.NoError(t, err)
	}
	sum := md5.Sum([]byte("body of " + ak))
	f.repo.files = append(f.repo.files, model.DriveFile{ID: id, StoredInternal: true, AccessKey: &ak, ThumbnailAccessKey: &tk, WebpublicAccessKey: &wk,
		Size: len("body of " + ak), MD5: hex.EncodeToString(sum[:])})
}

// drainVerify runs the queued verification jobs until none is left.
func (f *migrationFixture) drainVerify(t *testing.T) {
	t.Helper()
	for range 1000 {
		j, ok := f.enq.popVerify()
		if !ok {
			return
		}
		require.NoError(t, f.m.RunVerifyBatch(context.Background(), j.token, j.afterID))
	}
	t.Fatal("the jobs did not end")
}

// drain runs the queued jobs until none is left.
func (f *migrationFixture) drain(t *testing.T) {
	t.Helper()
	for range 1000 {
		j, ok := f.enq.pop()
		if !ok {
			return
		}
		require.NoError(t, f.m.RunBatch(context.Background(), j.token, j.afterID))
	}
	t.Fatal("the jobs did not end")
}

func TestStorageMigrationCopiesAndMarks(t *testing.T) {
	f := newMigrationFixture(t)
	f.addLocal(t, "a1")
	link := "ak-link"
	f.repo.files = append(f.repo.files, model.DriveFile{ID: "a2", IsLink: true, AccessKey: &link})
	direct := "ak-s3"
	f.repo.files = append(f.repo.files, model.DriveFile{ID: "a3", AccessKey: &direct})

	require.NoError(t, f.m.Start(context.Background()))
	f.drain(t)

	for _, k := range []string{"ak-a1", "tk-a1", "wk-a1"} {
		assert.Equal(t, "body of "+k, string(f.dst.objects[k]), k)
		// ローカルの実体は消さない。
		rc, err := f.local.Get(k)
		require.NoError(t, err, k)
		rc.Close()
	}
	a1 := f.repo.file("a1")
	assert.False(t, a1.StoredInternal)
	require.NotNil(t, a1.ObjectStorageMigratedAt)
	// isLink の行と、初めからオブジェクトストレージにある行は対象にしない。
	assert.Nil(t, f.repo.file("a2").ObjectStorageMigratedAt)
	assert.Nil(t, f.repo.file("a3").ObjectStorageMigratedAt)
	assert.Len(t, f.dst.objects, 3)
	held, _ := f.lock.Held(context.Background())
	assert.False(t, held, "the lock was not released at the end")

	st, err := f.m.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StorageMigrationStatus{ObjectStorageEnabled: true, Migrated: 1}, st)
}

func TestStorageMigrationChainsBatches(t *testing.T) {
	f := newMigrationFixture(t)
	n := storageMigrationBatch*2 + 3
	for i := range n {
		f.addLocal(t, fmt.Sprintf("f%04d", i))
	}
	require.NoError(t, f.m.Start(context.Background()))
	first, ok := f.enq.pop()
	require.True(t, ok)
	assert.Empty(t, first.afterID)
	require.NoError(t, f.m.RunBatch(context.Background(), first.token, first.afterID))
	// 1 本目は batch の件数だけ写し、最後の id を続きのカーソルにして次を積む。
	next, ok := f.enq.pop()
	require.True(t, ok)
	assert.Equal(t, migrationJob{first.token, fmt.Sprintf("f%04d", storageMigrationBatch-1)}, next)
	f.enq.jobs = append([]migrationJob{next}, f.enq.jobs...)
	f.drain(t)

	c, _ := f.repo.Counts(context.Background())
	assert.Equal(t, int64(0), c.Local)
	assert.Equal(t, int64(n), c.Migrated)
}

func TestStorageMigrationStartRefuses(t *testing.T) {
	ctx := context.Background()
	t.Run("no queue", func(t *testing.T) {
		f := newMigrationFixture(t)
		f.addLocal(t, "a1")
		m := NewStorageMigrator(f.repo, f.local, f.dst, nil, f.lock)
		assert.ErrorIs(t, m.Start(ctx), ErrStorageMigrationNoQueue)
	})
	t.Run("object storage not configured", func(t *testing.T) {
		f := newMigrationFixture(t)
		f.addLocal(t, "a1")
		m := NewStorageMigrator(f.repo, f.local, f.local, f.enq, f.lock)
		assert.ErrorIs(t, m.Start(ctx), ErrStorageMigrationNotConfigured)
		st, err := m.Status(ctx)
		require.NoError(t, err)
		assert.False(t, st.ObjectStorageEnabled)
	})
	t.Run("nothing to migrate", func(t *testing.T) {
		f := newMigrationFixture(t)
		assert.ErrorIs(t, f.m.Start(ctx), ErrStorageMigrationNothingToDo)
	})
	t.Run("already running", func(t *testing.T) {
		f := newMigrationFixture(t)
		f.addLocal(t, "a1")
		require.NoError(t, f.m.Start(ctx))
		// 二重に押しても 2 本目は走らない。
		assert.ErrorIs(t, f.m.Start(ctx), ErrStorageMigrationRunning)
		assert.Len(t, f.enq.jobs, 1)
		st, err := f.m.Status(ctx)
		require.NoError(t, err)
		assert.Equal(t, StorageMigrationOperationMigrate, st.Running)
	})
	t.Run("enqueue fails", func(t *testing.T) {
		f := newMigrationFixture(t)
		f.addLocal(t, "a1")
		f.enq.err = errors.New("redis down")
		require.Error(t, f.m.Start(ctx))
		// 積めなかったら lock を返し、もう一度開始できる。
		held, _ := f.lock.Held(ctx)
		assert.False(t, held)
	})
}

// TestStorageMigrationResumesAfterTheChainBreaks covers a run whose chain of
// jobs broke (the lock expired): starting again copies the files left, and
// the job of the old run does nothing.
func TestStorageMigrationResumesAfterTheChainBreaks(t *testing.T) {
	ctx := context.Background()
	f := newMigrationFixture(t)
	for i := range storageMigrationBatch + 5 {
		f.addLocal(t, fmt.Sprintf("f%04d", i))
	}
	require.NoError(t, f.m.Start(ctx))
	j, _ := f.enq.pop()
	require.NoError(t, f.m.RunBatch(ctx, j.token, j.afterID))
	stale, ok := f.enq.pop()
	require.True(t, ok)

	f.lock.expire()
	require.NoError(t, f.m.Start(ctx))
	// 古い実行のジョブは、lock を持っていないので何もしない (DB も読まない)。
	f.repo.listErr = errors.New("must not be read")
	require.NoError(t, f.m.RunBatch(ctx, stale.token, stale.afterID))
	f.repo.listErr = nil
	assert.Len(t, f.enq.jobs, 1, "the stale job enqueued a next job")
	f.drain(t)

	c, _ := f.repo.Counts(ctx)
	assert.Equal(t, int64(0), c.Local)
	assert.Equal(t, int64(storageMigrationBatch+5), c.Migrated)
}

func TestStorageMigrationRecordsFailuresAndRetries(t *testing.T) {
	ctx := context.Background()
	f := newMigrationFixture(t)
	f.addLocal(t, "a1")
	f.addLocal(t, "a2")
	f.addLocal(t, "a3")
	// a1 は本体がローカルに無い。a2 はオブジェクトストレージへ置けない。
	// a3 はサムネイルだけが無い (飛ばして写す)。
	require.NoError(t, f.local.Delete("ak-a1"))
	f.dst.putErr["ak-a2"] = errors.New(strings.Repeat("x", maxStorageMigrationError+10))
	require.NoError(t, f.local.Delete("tk-a3"))

	require.NoError(t, f.m.Start(ctx))
	f.drain(t)

	assert.True(t, f.repo.file("a1").StoredInternal)
	assert.True(t, f.repo.file("a2").StoredInternal)
	assert.False(t, f.repo.file("a3").StoredInternal)
	assert.NotContains(t, f.dst.objects, "tk-a3")
	st, err := f.m.Status(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), st.Failed)
	assert.Equal(t, int64(2), st.Local)
	require.Len(t, st.Failures, 2)
	assert.Equal(t, "a1", st.Failures[0].FileID)
	assert.Equal(t, errLocalFileMissing.Error(), st.Failures[0].Error)
	assert.Len(t, st.Failures[1].Error, maxStorageMigrationError, "the error text was not bounded")

	// 直してからもう一度流すと、失敗した分を取り直し、失敗の記録を消す。
	delete(f.dst.putErr, "ak-a2")
	require.NoError(t, f.m.Start(ctx))
	f.drain(t)
	assert.False(t, f.repo.file("a2").StoredInternal)
	st, err = f.m.Status(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), st.Failed)
	assert.Equal(t, "a1", st.Failures[0].FileID)
}

func TestStorageMigrationRemovesTheCopyOfAFileDeletedMeanwhile(t *testing.T) {
	ctx := context.Background()
	f := newMigrationFixture(t)
	f.addLocal(t, "a1")
	f.repo.deleteOnMark = "a1"
	require.NoError(t, f.m.Start(ctx))
	f.drain(t)
	assert.Empty(t, f.dst.objects, "the copy of a deleted file stayed in object storage")
	assert.ElementsMatch(t, []string{"ak-a1", "tk-a1", "wk-a1"}, f.dst.deleted)
}

// TestStorageMigrationKeepsTheCopyOfAFileMigratedByAnotherJob covers two jobs
// of the same run copying the same file (a redelivered job, or a handler
// retried after its deadline): the one that finds the row already migrated
// must not delete the objects the row now points at.
func TestStorageMigrationKeepsTheCopyOfAFileMigratedByAnotherJob(t *testing.T) {
	ctx := context.Background()
	f := newMigrationFixture(t)
	f.addLocal(t, "a1")
	f.repo.markOnMark = "a1"
	require.NoError(t, f.m.Start(ctx))
	f.drain(t)
	assert.Len(t, f.dst.objects, 3)
	assert.Empty(t, f.dst.deleted)
	assert.False(t, f.repo.file("a1").StoredInternal)
}

// TestStorageMigrationStaleJobDoesNotReviveTheRun covers a job of a run that
// already ended (released, or the lock expired): it does nothing and does not
// take the free lock.
func TestStorageMigrationStaleJobDoesNotReviveTheRun(t *testing.T) {
	ctx := context.Background()
	f := newMigrationFixture(t)
	f.addLocal(t, "a1")
	require.NoError(t, f.m.Start(ctx))
	j, _ := f.enq.pop()
	f.lock.expire()
	require.NoError(t, f.m.RunBatch(ctx, j.token, j.afterID))
	held, _ := f.lock.Held(ctx)
	assert.False(t, held, "a stale job took the lock")
	assert.True(t, f.repo.file("a1").StoredInternal)
	assert.Empty(t, f.enq.jobs)
}

func TestTruncateUTF8(t *testing.T) {
	assert.Equal(t, "abc", truncateUTF8("abc", 5))
	assert.Equal(t, "ab", truncateUTF8("abc", 2))
	// 「あ」は 3 バイト。途中で切らない。
	assert.Equal(t, "aあ", truncateUTF8("aあい", 5))
	assert.Equal(t, "aあ", truncateUTF8("aあい", 4))
	assert.Equal(t, "a", truncateUTF8("aあい", 3))
}

// TestStorageMigrationRemovesTheCopyOfAFileTurnedIntoALink covers the remote
// cache cleanup turning the row into a link row while it was being copied:
// nothing points at the copy, so it is removed.
func TestStorageMigrationRemovesTheCopyOfAFileTurnedIntoALink(t *testing.T) {
	ctx := context.Background()
	f := newMigrationFixture(t)
	f.addLocal(t, "a1")
	f.repo.linkOnMark = "a1"
	require.NoError(t, f.m.Start(ctx))
	f.drain(t)
	assert.Empty(t, f.dst.objects)
}

func TestStorageMigrationStopsWhenObjectStorageIsTurnedOff(t *testing.T) {
	ctx := context.Background()
	f := newMigrationFixture(t)
	f.addLocal(t, "a1")
	require.NoError(t, f.m.Start(ctx))
	j, _ := f.enq.pop()
	off := NewStorageMigrator(f.repo, f.local, f.local, f.enq, f.lock)
	require.NoError(t, off.RunBatch(ctx, j.token, j.afterID))
	assert.True(t, f.repo.file("a1").StoredInternal)
	held, _ := f.lock.Held(ctx)
	assert.False(t, held)
}

func TestStorageMigrationRunBatchErrors(t *testing.T) {
	ctx := context.Background()
	f := newMigrationFixture(t)
	f.addLocal(t, "a1")
	require.NoError(t, f.m.Start(ctx))
	j, _ := f.enq.pop()
	f.repo.listErr = errors.New("db down")
	// 一時的な失敗はジョブの再試行に任せる。lock は持ったまま。
	require.Error(t, f.m.RunBatch(ctx, j.token, j.afterID))
	held, _ := f.lock.Held(ctx)
	assert.True(t, held)
}

func TestRedisStorageMigrationLock(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	l := NewRedisStorageMigrationLock(rdb, "pfx:")

	held, err := l.Held(ctx)
	require.NoError(t, err)
	assert.False(t, held)
	ok, err := l.Acquire(ctx, "a", time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.True(t, mr.Exists("pfx:elythia:drive:objectStorageMigration"))
	// 別の実行は取れない。同じ実行は延ばせる。
	ok, err = l.Acquire(ctx, "b", time.Minute)
	require.NoError(t, err)
	assert.False(t, ok)
	mr.FastForward(50 * time.Second)
	ok, err = l.Acquire(ctx, "a", time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
	mr.FastForward(50 * time.Second)
	held, err = l.Held(ctx)
	require.NoError(t, err)
	assert.True(t, held, "extending did not push the expiry")
	// 別の実行は返せない。
	require.NoError(t, l.Release(ctx, "b"))
	held, _ = l.Held(ctx)
	assert.True(t, held)
	require.NoError(t, l.Release(ctx, "a"))
	held, _ = l.Held(ctx)
	assert.False(t, held)
	// 期限が切れたら、別の実行が取れる。
	// Extend は持っている実行だけが延ばせ、空いた lock は取らない。
	ok, err = l.Extend(ctx, "a", time.Minute)
	require.NoError(t, err)
	assert.False(t, ok, "Extend took a free lock")
	ok, _ = l.Acquire(ctx, "a", time.Minute)
	require.True(t, ok)
	ok, err = l.Extend(ctx, "b", time.Minute)
	require.NoError(t, err)
	assert.False(t, ok)
	mr.FastForward(50 * time.Second)
	ok, err = l.Extend(ctx, "a", time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
	mr.FastForward(50 * time.Second)
	held, _ = l.Held(ctx)
	assert.True(t, held, "Extend did not push the expiry")
	mr.FastForward(2 * time.Minute)
	ok, err = l.Acquire(ctx, "b", time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
}

// failingMigrationRepo wraps memMigrationRepo and fails the named method.
type failingMigrationRepo struct {
	*memMigrationRepo
	fail string
}

var errInjected = errors.New("injected")

func (r *failingMigrationRepo) err(name string) error {
	if r.fail == name {
		return errInjected
	}
	return nil
}

func (r *failingMigrationRepo) MarkMigrated(ctx context.Context, id string, at time.Time) (bool, error) {
	if err := r.err("MarkMigrated"); err != nil {
		return false, err
	}
	return r.memMigrationRepo.MarkMigrated(ctx, id, at)
}

func (r *failingMigrationRepo) SetVerified(ctx context.Context, id string, migratedAt time.Time, at *time.Time) (bool, error) {
	if err := r.err("SetVerified"); err != nil {
		return false, err
	}
	return r.memMigrationRepo.SetVerified(ctx, id, migratedAt, at)
}

func (r *failingMigrationRepo) MarkLocalDeleted(ctx context.Context, id string, migratedAt, at time.Time) (bool, error) {
	if err := r.err("MarkLocalDeleted"); err != nil {
		return false, err
	}
	return r.memMigrationRepo.MarkLocalDeleted(ctx, id, migratedAt, at)
}

func (r *failingMigrationRepo) MarkRestored(ctx context.Context, id string, migratedAt time.Time, localDeletedAt *time.Time, copies []model.DriveObjectStorageRestoredCopy) (bool, error) {
	if err := r.err("MarkRestored"); err != nil {
		return false, err
	}
	return r.memMigrationRepo.MarkRestored(ctx, id, migratedAt, localDeletedAt, copies)
}

func (r *failingMigrationRepo) ListRestoredCopies(ctx context.Context, afterKey string, limit int) ([]model.DriveObjectStorageRestoredCopy, error) {
	if err := r.err("ListRestoredCopies"); err != nil {
		return nil, err
	}
	return r.memMigrationRepo.ListRestoredCopies(ctx, afterKey, limit)
}

func (r *failingMigrationRepo) ListRestoredCopiesOfFile(ctx context.Context, fileID string) ([]model.DriveObjectStorageRestoredCopy, error) {
	if err := r.err("ListRestoredCopiesOfFile"); err != nil {
		return nil, err
	}
	return r.memMigrationRepo.ListRestoredCopiesOfFile(ctx, fileID)
}

func (r *failingMigrationRepo) DeleteRestoredCopy(ctx context.Context, key, location string) error {
	if err := r.err("DeleteRestoredCopy"); err != nil {
		return err
	}
	return r.memMigrationRepo.DeleteRestoredCopy(ctx, key, location)
}

func (r *failingMigrationRepo) RecordRestoredCopyFailure(ctx context.Context, key, location, msg string, at time.Time) error {
	if err := r.err("RecordRestoredCopyFailure"); err != nil {
		return err
	}
	return r.memMigrationRepo.RecordRestoredCopyFailure(ctx, key, location, msg, at)
}

func (r *failingMigrationRepo) ListRestoredCopyFailures(ctx context.Context, limit int) ([]model.DriveObjectStorageRestoredCopy, error) {
	if err := r.err("ListRestoredCopyFailures"); err != nil {
		return nil, err
	}
	return r.memMigrationRepo.ListRestoredCopyFailures(ctx, limit)
}

func (r *failingMigrationRepo) FindDriveFile(ctx context.Context, id string) (*model.DriveFile, error) {
	if err := r.err("FindDriveFile"); err != nil {
		return nil, err
	}
	return r.memMigrationRepo.FindDriveFile(ctx, id)
}

func (r *failingMigrationRepo) IsStoredLocally(ctx context.Context, id string) (bool, error) {
	if err := r.err("IsStoredLocally"); err != nil {
		return false, err
	}
	return r.memMigrationRepo.IsStoredLocally(ctx, id)
}

func (r *failingMigrationRepo) IsMigrated(ctx context.Context, id string) (bool, error) {
	if err := r.err("IsMigrated"); err != nil {
		return false, err
	}
	return r.memMigrationRepo.IsMigrated(ctx, id)
}

func (r *failingMigrationRepo) RecordFailure(ctx context.Context, fileID, op, msg string, at time.Time) error {
	if err := r.err("RecordFailure"); err != nil {
		return err
	}
	return r.memMigrationRepo.RecordFailure(ctx, fileID, op, msg, at)
}

func (r *failingMigrationRepo) ClearFailure(ctx context.Context, fileID, op string) error {
	if err := r.err("ClearFailure"); err != nil {
		return err
	}
	return r.memMigrationRepo.ClearFailure(ctx, fileID, op)
}

func (r *failingMigrationRepo) ListFailures(ctx context.Context, op string, limit int) ([]model.DriveObjectStorageMigrationFailure, error) {
	if err := r.err("ListFailures"); err != nil {
		return nil, err
	}
	return r.memMigrationRepo.ListFailures(ctx, op, limit)
}

func (r *failingMigrationRepo) Counts(ctx context.Context) (repository.DriveStorageMigrationCounts, error) {
	if err := r.err("Counts"); err != nil {
		return repository.DriveStorageMigrationCounts{}, err
	}
	return r.memMigrationRepo.Counts(ctx)
}

// failingMigrationLock fails every call.
type failingMigrationLock struct{}

func (failingMigrationLock) Acquire(context.Context, string, time.Duration) (bool, error) {
	return false, errInjected
}
func (failingMigrationLock) Extend(context.Context, string, time.Duration) (bool, error) {
	return false, errInjected
}
func (failingMigrationLock) Release(context.Context, string) error  { return errInjected }
func (failingMigrationLock) Held(context.Context) (bool, error)     { return false, errInjected }
func (failingMigrationLock) Holder(context.Context) (string, error) { return "", errInjected }

func TestStorageMigrationDependencyErrors(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, fail string) (*migrationFixture, *StorageMigrator) {
		f := newMigrationFixture(t)
		f.addLocal(t, "a1")
		repo := &failingMigrationRepo{memMigrationRepo: f.repo, fail: fail}
		return f, NewStorageMigrator(repo, f.local, f.dst, f.enq, f.lock)
	}
	t.Run("Counts in Start", func(t *testing.T) {
		_, m := setup(t, "Counts")
		assert.ErrorIs(t, m.Start(ctx), errInjected)
		_, err := m.Status(ctx)
		assert.ErrorIs(t, err, errInjected)
	})
	t.Run("ListFailures in Status", func(t *testing.T) {
		_, m := setup(t, "ListFailures")
		_, err := m.Status(ctx)
		assert.ErrorIs(t, err, errInjected)
	})
	for _, name := range []string{"RecordFailure", "ClearFailure"} {
		t.Run(name+" in RunBatch", func(t *testing.T) {
			f, m := setup(t, name)
			if name == "RecordFailure" {
				require.NoError(t, f.local.Delete("ak-a1"))
			}
			require.NoError(t, f.lock.Release(ctx, f.lock.holder))
			ok, _ := f.lock.Acquire(ctx, "t", time.Minute)
			require.True(t, ok)
			assert.ErrorIs(t, m.RunBatch(ctx, "t", ""), errInjected)
		})
	}
	t.Run("MarkMigrated is a failure of the file", func(t *testing.T) {
		f, m := setup(t, "MarkMigrated")
		ok, _ := f.lock.Acquire(ctx, "t", time.Minute)
		require.True(t, ok)
		require.NoError(t, m.RunBatch(ctx, "t", ""))
		assert.Contains(t, f.repo.failures["a1/migrate"].Error, "mark the file as migrated")
	})
	t.Run("lock", func(t *testing.T) {
		f := newMigrationFixture(t)
		f.addLocal(t, "a1")
		m := NewStorageMigrator(f.repo, f.local, f.dst, f.enq, failingMigrationLock{})
		assert.ErrorIs(t, m.Start(ctx), errInjected)
		assert.ErrorIs(t, m.RunBatch(ctx, "t", ""), errInjected)
		_, err := m.Status(ctx)
		assert.ErrorIs(t, err, errInjected)
	})
	t.Run("no access key", func(t *testing.T) {
		f := newMigrationFixture(t)
		f.repo.files = append(f.repo.files, model.DriveFile{ID: "a1", StoredInternal: true})
		ok, _ := f.lock.Acquire(ctx, "t", time.Minute)
		require.True(t, ok)
		require.NoError(t, f.m.RunBatch(ctx, "t", ""))
		assert.Equal(t, "the file has no access key", f.repo.failures["a1/migrate"].Error)
	})
	t.Run("canceled", func(t *testing.T) {
		f := newMigrationFixture(t)
		f.addLocal(t, "a1")
		ok, _ := f.lock.Acquire(ctx, "t", time.Minute)
		require.True(t, ok)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		assert.ErrorIs(t, f.m.RunBatch(cctx, "t", ""), context.Canceled)
		assert.True(t, f.repo.file("a1").StoredInternal)
	})
}
