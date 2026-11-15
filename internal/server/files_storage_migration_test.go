package server

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// putStorage is an in-memory object storage that keeps what is put.
type putStorage struct {
	mu    sync.Mutex
	byKey map[string]string
}

func (s *putStorage) Put(key string, body io.Reader) (string, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byKey[key] = string(b)
	return "https://s3.example/" + key, nil
}

func (s *putStorage) Get(key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.byKey[key]
	if !ok {
		return nil, coredrive.ErrObjectNotFound
	}
	return io.NopCloser(strings.NewReader(b)), nil
}

func (s *putStorage) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byKey, key)
	return nil
}

// onlyFileRepo limits the migration to one file, so the test does not touch
// the rows other tests left in the shared schema.
type onlyFileRepo struct {
	*repository.DriveStorageMigrationRepository
	id string
}

func (r onlyFileRepo) ListLocal(ctx context.Context, afterID string, limit int) ([]model.DriveFile, error) {
	files, err := r.DriveStorageMigrationRepository.ListLocal(ctx, "", 1<<20)
	if err != nil {
		return nil, err
	}
	var out []model.DriveFile
	for _, f := range files {
		if f.ID == r.id && f.ID > afterID && len(out) < limit {
			out = append(out, f)
		}
	}
	return out, nil
}

func (r onlyFileRepo) ListMigrated(ctx context.Context, afterID string, limit int) ([]model.DriveFile, error) {
	files, err := r.DriveStorageMigrationRepository.ListMigrated(ctx, "", 1<<20)
	if err != nil {
		return nil, err
	}
	var out []model.DriveFile
	for _, f := range files {
		if f.ID == r.id && f.ID > afterID && len(out) < limit {
			out = append(out, f)
		}
	}
	return out, nil
}

func (r onlyFileRepo) ListRestoredCopies(ctx context.Context, afterKey string, limit int) ([]model.DriveObjectStorageRestoredCopy, error) {
	copies, err := r.DriveStorageMigrationRepository.ListRestoredCopiesOfFile(ctx, r.id)
	if err != nil {
		return nil, err
	}
	var out []model.DriveObjectStorageRestoredCopy
	for _, c := range copies {
		if c.Key > afterKey && len(out) < limit {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r onlyFileRepo) ListPurgeable(ctx context.Context, afterID string, limit int) ([]model.DriveFile, error) {
	files, err := r.DriveStorageMigrationRepository.ListPurgeable(ctx, "", 1<<20)
	if err != nil {
		return nil, err
	}
	var out []model.DriveFile
	for _, f := range files {
		if f.ID == r.id && f.ID > afterID && len(out) < limit {
			out = append(out, f)
		}
	}
	return out, nil
}

type queuedMigrationJobs struct{ jobs, verifyJobs, purgeJobs, restoreJobs, cleanupJobs [][2]string }

func (q *queuedMigrationJobs) EnqueueCleanupRestoredDriveFileCopies(token, afterKey string) error {
	q.cleanupJobs = append(q.cleanupJobs, [2]string{token, afterKey})
	return nil
}

func (q *queuedMigrationJobs) EnqueueRestoreMigratedDriveFiles(token, afterID string) error {
	q.restoreJobs = append(q.restoreJobs, [2]string{token, afterID})
	return nil
}

func (q *queuedMigrationJobs) EnqueuePurgeLocalDriveFiles(token, afterID string) error {
	q.purgeJobs = append(q.purgeJobs, [2]string{token, afterID})
	return nil
}

func (q *queuedMigrationJobs) EnqueueMigrateLocalDriveFiles(token, afterID string) error {
	q.jobs = append(q.jobs, [2]string{token, afterID})
	return nil
}

func (q *queuedMigrationJobs) EnqueueVerifyMigratedDriveFiles(token, afterID string) error {
	q.verifyJobs = append(q.verifyJobs, [2]string{token, afterID})
	return nil
}

type soloLock struct{ holder string }

func (l *soloLock) Acquire(_ context.Context, token string, _ time.Duration) (bool, error) {
	if l.holder == "" || l.holder == token {
		l.holder = token
		return true, nil
	}
	return false, nil
}

func (l *soloLock) Extend(_ context.Context, token string, _ time.Duration) (bool, error) {
	return l.holder != "" && l.holder == token, nil
}

func (l *soloLock) Release(_ context.Context, token string) error {
	if l.holder == token {
		l.holder = ""
	}
	return nil
}

func (l *soloLock) Held(context.Context) (bool, error)     { return l.holder != "", nil }
func (l *soloLock) Holder(context.Context) (string, error) { return l.holder, nil }

// TestFilesServesTheSameURLAfterTheStorageMigration pins the premise of
// #4180: the migration leaves the url columns alone, and /files/<accessKey>
// keeps serving the same bytes from object storage once the row says
// storedInternal=false — for the file, its thumbnail and its webpublic
// variant — even after the local copy is gone.
func TestFilesServesTheSameURLAfterTheStorageMigration(t *testing.T) {
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	ctx := context.Background()
	db := serverIntegrationDB
	const id = "storagemig4180f1"
	ak, tk, wk := "storagemig4180ak", "storagemig4180tk", "storagemig4180wk"
	clean := func() {
		require.NoError(t, db.Exec(`DELETE FROM "drive_file" WHERE "id" = ?`, id).Error)
		require.NoError(t, db.Exec(`DELETE FROM "drive_object_storage_restored_copy" WHERE "fileId" = ?`, id).Error)
	}
	clean()
	t.Cleanup(clean)
	url := "https://example.test/files/" + ak
	sum := md5.Sum([]byte("bytes of " + ak))
	require.NoError(t, db.Create(&model.DriveFile{
		ID: id, MD5: hex.EncodeToString(sum[:]), Name: "a.png", Type: "image/png", Size: len("bytes of " + ak),
		StoredInternal: true, URL: url, AccessKey: &ak, ThumbnailAccessKey: &tk, WebpublicAccessKey: &wk,
	}).Error)

	local := coredrive.NewLocalStorage(t.TempDir(), "https://example.test/files")
	for _, k := range []string{ak, tk, wk} {
		_, err := local.Put(k, strings.NewReader("bytes of "+k))
		require.NoError(t, err)
	}
	obj := &putStorage{byKey: map[string]string{}}
	jobs := &queuedMigrationJobs{}
	m := coredrive.NewStorageMigrator(onlyFileRepo{repository.NewDriveStorageMigrationRepository(db), id}, local, obj, jobs, &soloLock{})
	handler := filesHandler(repository.NewDriveFileRepository(db), obj, local)
	serve := func(key string) (int, string) {
		c, rec := newFilesTestContext(t, key)
		require.NoError(t, handler(c))
		return rec.Code, rec.Body.String()
	}

	// 流す前は、ローカルから配信する。
	for _, k := range []string{ak, tk, wk} {
		code, body := serve(k)
		assert.Equal(t, http.StatusOK, code, k)
		assert.Equal(t, "bytes of "+k, body, k)
	}

	// 開始から、続きのジョブが無くなるまで流す。
	require.NoError(t, m.Start(ctx))
	for i := 0; i < len(jobs.jobs); i++ {
		require.NoError(t, m.RunBatch(ctx, jobs.jobs[i][0], jobs.jobs[i][1]))
	}
	var f model.DriveFile
	require.NoError(t, db.Where(`"id" = ?`, id).Take(&f).Error)
	require.False(t, f.StoredInternal, "the row was not migrated")
	require.NotNil(t, f.ObjectStorageMigratedAt)
	assert.Equal(t, url, f.URL, "the url column was rewritten")

	// 確かめる処理 (#4181) は、実 DB の移した日時で行を突き合わせ、確かめた日時を入れる。
	verify := func() model.DriveFile {
		t.Helper()
		jobs.verifyJobs = nil
		require.NoError(t, m.StartVerify(ctx))
		for i := 0; i < len(jobs.verifyJobs); i++ {
			require.NoError(t, m.RunVerifyBatch(ctx, jobs.verifyJobs[i][0], jobs.verifyJobs[i][1]))
		}
		var got model.DriveFile
		require.NoError(t, db.Where(`"id" = ?`, id).Take(&got).Error)
		return got
	}
	require.NotNil(t, verify().ObjectStorageVerifiedAt, "the matching copy was not marked as verified")
	obj.byKey[ak] = "broken!" + obj.byKey[ak]
	assert.Nil(t, verify().ObjectStorageVerifiedAt, "the broken copy kept the verified mark")
	obj.byKey[ak] = "bytes of " + ak
	require.NotNil(t, verify().ObjectStorageVerifiedAt)

	// 消す処理 (#4182) がローカルの実体を消しても、同じ URL でオブジェクトストレージから
	// 配信する。消した日時は実 DB の移した日時で突き合わせて入る。
	require.NoError(t, m.StartPurge(ctx))
	for i := 0; i < len(jobs.purgeJobs); i++ {
		require.NoError(t, m.RunPurgeBatch(ctx, jobs.purgeJobs[i][0], jobs.purgeJobs[i][1]))
	}
	require.NoError(t, db.Where(`"id" = ?`, id).Take(&f).Error)
	require.NotNil(t, f.ObjectStorageLocalDeletedAt, "the deletion was not recorded")
	for _, k := range []string{ak, tk, wk} {
		_, err := local.Get(k)
		require.ErrorIs(t, err, coredrive.ErrObjectNotFound, "the local copy %s was kept", k)
		code, body := serve(k)
		assert.Equal(t, http.StatusOK, code, k)
		assert.Equal(t, "bytes of "+k, body, k)
	}

	// 戻す処理 (#4183) は、オブジェクトストレージから取ってローカルへ置き、実 DB の行を
	// 移した日時で突き合わせて倒す。倒した後は、同じ URL でローカルから配信する。
	require.NoError(t, m.StartRestore(ctx))
	for i := 0; i < len(jobs.restoreJobs); i++ {
		require.NoError(t, m.RunRestoreBatch(ctx, jobs.restoreJobs[i][0], jobs.restoreJobs[i][1]))
	}
	// 前の読み込みの値を残さないよう、新しい変数に読む (GORM は NULL の列で既存の
	// ポインタを書き換えない)。
	var back model.DriveFile
	require.NoError(t, db.Where(`"id" = ?`, id).Take(&back).Error)
	require.True(t, back.StoredInternal, "the row was not moved back")
	assert.Nil(t, back.ObjectStorageMigratedAt)
	assert.Nil(t, back.ObjectStorageVerifiedAt)
	assert.Nil(t, back.ObjectStorageLocalDeletedAt)
	assert.Equal(t, url, back.URL, "the url column was rewritten")

	// 戻した行の写しは、実 DB に記録が残る (#4197)。写しを消す処理は、記録した写しを
	// オブジェクトストレージから消して記録を消し、その後もローカルから配信する。
	var recorded []model.DriveObjectStorageRestoredCopy
	require.NoError(t, db.Where(`"fileId" = ?`, id).Order(`"key"`).Find(&recorded).Error)
	require.Len(t, recorded, 3, "the copies were not recorded")
	for _, c := range recorded {
		assert.Contains(t, obj.byKey, c.Key, "the recorded copy %s is not in object storage", c.Key)
	}
	require.NoError(t, m.StartCleanup(ctx))
	for i := 0; i < len(jobs.cleanupJobs); i++ {
		require.NoError(t, m.RunCleanupBatch(ctx, jobs.cleanupJobs[i][0], jobs.cleanupJobs[i][1]))
	}
	var left int64
	require.NoError(t, db.Model(&model.DriveObjectStorageRestoredCopy{}).Where(`"fileId" = ?`, id).Count(&left).Error)
	assert.Zero(t, left, "the records of the deleted copies were kept")
	for _, k := range []string{ak, tk, wk} {
		assert.NotContains(t, obj.byKey, k, "the copy %s was kept", k)
		code, body := serve(k)
		assert.Equal(t, http.StatusOK, code, k)
		assert.Equal(t, "bytes of "+k, body, k)
	}
}
