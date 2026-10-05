package drive

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// mockMultipartStorage / mockChunkedSessionRepo は core/drive のテストにある
// 同名 fake と同じもの。internal/testutil に置いて共有できないのは、testutil を
// core/role・core/notification・repository の内部テストが import しており、
// testutil → core/drive の依存が import cycle になるため。
//
// ---------------------------------------------------------------------------
// mockMultipartStorage — in-memory Storage + MultipartStorage
// ---------------------------------------------------------------------------

// mockMultipartStorage models the parts of S3 that matter for chunked upload:
// parts are addressed by number, completion concatenates them in part-number
// order, and the ETags supplied at completion are verified against the stored
// bytes (which is what makes a DB/storage divergence fail the completion
// instead of assembling a corrupt Object).
type mockMultipartStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]*mockMultipartUpload
	nextID  int

	// Recorded for assertions.
	Puts        []string
	Deletes     []string
	Aborted     []string
	ContentType string

	CreateErr   error
	UploadErr   error
	CompleteErr error
	AbortErr    error
	GetErr      error
}

type mockMultipartUpload struct {
	accessKey string
	parts     map[int32][]byte
}

func newMockMultipartStorage() *mockMultipartStorage {
	return &mockMultipartStorage{
		objects: map[string][]byte{},
		uploads: map[string]*mockMultipartUpload{},
	}
}

func (f *mockMultipartStorage) Put(accessKey string, body io.Reader) (string, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[accessKey] = data
	f.Puts = append(f.Puts, accessKey)
	return "https://cdn.example.com/" + accessKey, nil
}

func (f *mockMultipartStorage) Get(accessKey string) (io.ReadCloser, error) {
	if f.GetErr != nil {
		return nil, f.GetErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[accessKey]
	if !ok {
		return nil, coredrive.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *mockMultipartStorage) Delete(accessKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, accessKey)
	f.Deletes = append(f.Deletes, accessKey)
	return nil
}

func (f *mockMultipartStorage) CreateMultipartUpload(_ context.Context, accessKey, contentType string) (string, error) {
	if f.CreateErr != nil {
		return "", f.CreateErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("upload-%d", f.nextID)
	f.uploads[id] = &mockMultipartUpload{accessKey: accessKey, parts: map[int32][]byte{}}
	f.ContentType = contentType
	return id, nil
}

func (f *mockMultipartStorage) UploadPart(_ context.Context, accessKey, uploadID string, partNumber int32, body []byte) (string, error) {
	if f.UploadErr != nil {
		return "", f.UploadErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	up, ok := f.uploads[uploadID]
	if !ok || up.accessKey != accessKey {
		return "", errors.New("no such upload")
	}
	stored := append([]byte(nil), body...)
	up.parts[partNumber] = stored
	return etagOf(stored), nil
}

func (f *mockMultipartStorage) CompleteMultipartUpload(_ context.Context, accessKey, uploadID string, parts []coredrive.UploadedPart) (string, error) {
	if f.CompleteErr != nil {
		return "", f.CompleteErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	up, ok := f.uploads[uploadID]
	if !ok || up.accessKey != accessKey {
		return "", errors.New("no such upload")
	}
	var buf bytes.Buffer
	for i, p := range parts {
		if int32(i+1) != p.PartNumber {
			return "", fmt.Errorf("parts must be ascending: got %d at %d", p.PartNumber, i)
		}
		stored, ok := up.parts[p.PartNumber]
		if !ok {
			return "", errors.New("InvalidPart: missing part")
		}
		// S3 は渡された ETag と実体を突き合わせる。ここを緩めると DB の記録と
		// storage の実体がずれたときに壊れたファイルが組み上がってしまう。
		if etagOf(stored) != p.ETag {
			return "", errors.New("InvalidPart: etag mismatch")
		}
		buf.Write(stored)
	}
	f.objects[accessKey] = buf.Bytes()
	delete(f.uploads, uploadID)
	return "https://cdn.example.com/" + accessKey, nil
}

func (f *mockMultipartStorage) AbortMultipartUpload(_ context.Context, _, uploadID string) error {
	if f.AbortErr != nil {
		return f.AbortErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.uploads, uploadID)
	f.Aborted = append(f.Aborted, uploadID)
	return nil
}

func (f *mockMultipartStorage) OpenUploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

func (f *mockMultipartStorage) Object(accessKey string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[accessKey]
	return b, ok
}

func etagOf(b []byte) string {
	sum := md5.Sum(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// ---------------------------------------------------------------------------
// mockChunkedSessionRepo — in-memory ChunkedUploadSessionRepository
// ---------------------------------------------------------------------------

// The optimistic guards (`receivedChunks = expected`, `uploadId IS NULL`,
// `finishing = false`) are reproduced faithfully because the concurrency
// behaviour of append/finish depends entirely on them.
type mockChunkedSessionRepo struct {
	mu       sync.Mutex
	sessions map[string]*model.ChunkedUploadSession

	CreateErr      error
	FindErr        error
	CountErr       error
	PendingErr     error
	ListErr        error
	DeleteErr      error
	SetUploadIDErr error

	// OnSetUploadID fires once, immediately before SetUploadID evaluates its
	// guard, so tests can simulate a concurrent first append.
	OnSetUploadID func()
}

func newMockChunkedSessionRepo() *mockChunkedSessionRepo {
	return &mockChunkedSessionRepo{sessions: map[string]*model.ChunkedUploadSession{}}
}

func (r *mockChunkedSessionRepo) Create(s *model.ChunkedUploadSession) error {
	if r.CreateErr != nil {
		return r.CreateErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *s
	r.sessions[s.ID] = &cp
	return nil
}

func (r *mockChunkedSessionRepo) FindByID(id string) (*model.ChunkedUploadSession, error) {
	if r.FindErr != nil {
		return nil, r.FindErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cp := *s
	return &cp, nil
}

func (r *mockChunkedSessionRepo) SetUploadID(id, uploadID, contentType string, now time.Time) (bool, error) {
	// 並行した最初の append を再現するためのフック。service が SetUploadID を
	// 呼ぶ直前に別リクエストが確定させた、という状況を作れる。
	if r.OnSetUploadID != nil {
		hook := r.OnSetUploadID
		r.OnSetUploadID = nil
		hook()
	}
	if r.SetUploadIDErr != nil {
		return false, r.SetUploadIDErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok || s.UploadID != nil {
		return false, nil
	}
	s.UploadID = &uploadID
	s.ContentType = &contentType
	s.UpdatedAt = now
	return true, nil
}

func (r *mockChunkedSessionRepo) CommitPart(id string, expectedChunks int, parts datatypes.JSON, receivedBytes int64, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok || s.ReceivedChunks != expectedChunks || s.Finishing {
		return false, nil
	}
	s.Parts = parts
	s.ReceivedBytes = receivedBytes
	s.ReceivedChunks = expectedChunks + 1
	s.UpdatedAt = now
	return true, nil
}

func (r *mockChunkedSessionRepo) ClaimFinish(id string, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok || s.Finishing {
		return false, nil
	}
	s.Finishing = true
	s.UpdatedAt = now
	return true, nil
}

func (r *mockChunkedSessionRepo) ReleaseFinish(id string, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[id]; ok {
		s.Finishing = false
		s.UpdatedAt = now
	}
	return nil
}

func (r *mockChunkedSessionRepo) Delete(id string) error {
	if r.DeleteErr != nil {
		return r.DeleteErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
	return nil
}

func (r *mockChunkedSessionRepo) CountActiveByUser(userID string, now time.Time) (int64, error) {
	if r.CountErr != nil {
		return 0, r.CountErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for _, s := range r.sessions {
		if s.UserID == userID && s.ExpiresAt.After(now) {
			n++
		}
	}
	return n, nil
}

func (r *mockChunkedSessionRepo) PendingBytesByUser(userID string, now time.Time) (int64, error) {
	if r.PendingErr != nil {
		return 0, r.PendingErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var total int64
	for _, s := range r.sessions {
		if s.UserID == userID && s.ExpiresAt.After(now) {
			total += s.TotalSize
		}
	}
	return total, nil
}

func (r *mockChunkedSessionRepo) ListExpired(now time.Time, limit int) ([]*model.ChunkedUploadSession, error) {
	if r.ListErr != nil {
		return nil, r.ListErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*model.ChunkedUploadSession
	for _, s := range r.sessions {
		if !s.ExpiresAt.After(now) {
			cp := *s
			out = append(out, &cp)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (r *mockChunkedSessionRepo) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

func (r *mockChunkedSessionRepo) Get(id string) *model.ChunkedUploadSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return nil
	}
	cp := *s
	return &cp
}

// 実装が interface を満たしていることを compile time に固定する。
var _ repository.ChunkedUploadSessionRepository = (*mockChunkedSessionRepo)(nil)
var _ coredrive.MultipartStorage = (*mockMultipartStorage)(nil)
var _ coredrive.Storage = (*mockMultipartStorage)(nil)

// chunkBytes builds a deterministic filler chunk of the given size whose first
// bytes make http.DetectContentType report text/plain.
func chunkBytes(size int) []byte {
	return []byte(strings.Repeat("a", size))
}
