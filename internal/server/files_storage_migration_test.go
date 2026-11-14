package server

import (
	"context"
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

type queuedMigrationJobs struct{ jobs [][2]string }

func (q *queuedMigrationJobs) EnqueueMigrateLocalDriveFiles(token, afterID string) error {
	q.jobs = append(q.jobs, [2]string{token, afterID})
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

func (l *soloLock) Held(context.Context) (bool, error) { return l.holder != "", nil }

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
	}
	clean()
	t.Cleanup(clean)
	url := "https://example.test/files/" + ak
	require.NoError(t, db.Create(&model.DriveFile{
		ID: id, MD5: "0123456789abcdef0123456789abcdef", Name: "a.png", Type: "image/png", Size: 4,
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

	// ローカルの実体を消しても、同じ URL でオブジェクトストレージから配信する。
	for _, k := range []string{ak, tk, wk} {
		require.NoError(t, local.Delete(k))
		code, body := serve(k)
		assert.Equal(t, http.StatusOK, code, k)
		assert.Equal(t, "bytes of "+k, body, k)
	}
}
