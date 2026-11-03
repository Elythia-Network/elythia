package drive_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #3996: Upload の 3 つの書き込み (原本・サムネイル・webpublic) を並行にした。

var (
	altThumbData = []byte("THUMBNAIL-DATA")
	altWebData   = []byte("WEBPUBLIC-DATA")
	errPutFailed = errors.New("put failed")
)

// parallelTestPNG returns a small PNG so that Upload takes the image path.
func parallelTestPNG(t testing.TB) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := range 4 {
		img.Set(i, i, color.RGBA{R: 255, A: 255})
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// altProcessor returns fixed alternate images, optionally after hook runs.
type altProcessor struct {
	hook  func() // runs inside GenerateThumbnail (the first alternate generated)
	delay time.Duration
}

func (p *altProcessor) GenerateThumbnail([]byte, string) (*drive.ProcessedImage, error) {
	if p.hook != nil {
		p.hook()
	}
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	return &drive.ProcessedImage{Data: altThumbData, MimeType: "image/webp"}, nil
}

func (p *altProcessor) GenerateWebpublic([]byte, string) (*drive.ProcessedImage, error) {
	return &drive.ProcessedImage{Data: altWebData, MimeType: "image/webp"}, nil
}

func (p *altProcessor) GetDimensions([]byte, string) (int, int, error) { return 4, 4, nil }

func (p *altProcessor) CalculateBlurhash([]byte, string) (string, error) { return "blur", nil }

// probeStorage is an in-memory Storage that tells the three objects apart by
// content and records how the writes overlapped.
type probeStorage struct {
	original []byte

	// before runs at the start of Put for the given role ("original" /
	// "thumbnail" / "webpublic") and may return an error or panic.
	before func(role string) error
	delay  time.Duration

	mu       sync.Mutex
	objects  map[string][]byte
	puts     []string // roles, in call order
	deleted  []string
	inFlight int
	peak     int
}

func newProbeStorage(original []byte) *probeStorage {
	return &probeStorage{original: original, objects: map[string][]byte{}}
}

func (s *probeStorage) role(b []byte) string {
	switch {
	case bytes.Equal(b, s.original):
		return "original"
	case bytes.Equal(b, altThumbData):
		return "thumbnail"
	case bytes.Equal(b, altWebData):
		return "webpublic"
	}
	return "unknown"
}

func (s *probeStorage) Put(accessKey string, body io.Reader) (string, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	role := s.role(b)
	s.mu.Lock()
	s.puts = append(s.puts, role)
	s.inFlight++
	s.peak = max(s.peak, s.inFlight)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if s.before != nil {
		if err := s.before(role); err != nil {
			return "", err
		}
	}
	s.mu.Lock()
	s.objects[accessKey] = b
	s.mu.Unlock()
	return "https://cdn.example.test/" + accessKey, nil
}

func (s *probeStorage) Get(accessKey string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[accessKey]
	if !ok {
		return nil, drive.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *probeStorage) Delete(accessKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, accessKey)
	s.deleted = append(s.deleted, accessKey)
	return nil
}

func (s *probeStorage) snapshot() (objects map[string][]byte, puts []string, inFlight, peak int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	objects = make(map[string][]byte, len(s.objects))
	for k, v := range s.objects {
		objects[k] = v
	}
	return objects, append([]string(nil), s.puts...), s.inFlight, s.peak
}

// waitOrTimeout waits for ch and reports whether it closed before the timeout.
// 逐次の実装ではここで待ちが解けないので、時間切れで抜けて false を返す
// (テストが固まらずに落ちる)。
func waitOrTimeout(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

func newParallelSvc(t *testing.T, st drive.Storage, proc drive.ImageProcessor) (*drive.Service, map[string]*model.DriveFile) {
	t.Helper()
	svc, repo := newSvcWithStorage(t, st)
	svc.SetImageProcessor(proc)
	return svc, repo.Files
}

// The three writes are in flight at the same time.
func TestUpload_WritesOriginalAndAlternatesInParallel(t *testing.T) {
	body := parallelTestPNG(t)
	st := newProbeStorage(body)
	allIn := make(chan struct{})
	var once sync.Once
	var entered int
	var mu sync.Mutex
	var sawAll [3]bool
	st.before = func(string) error {
		mu.Lock()
		entered++
		idx := entered - 1
		if entered == 3 {
			once.Do(func() { close(allIn) })
		}
		mu.Unlock()
		sawAll[idx] = waitOrTimeout(allIn)
		return nil
	}
	svc, files := newParallelSvc(t, st, &altProcessor{})

	f, err := svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: body, Name: "a.png"})
	require.NoError(t, err)
	_, puts, inFlight, peak := st.snapshot()
	assert.Equal(t, 3, peak, "the original, thumbnail and webpublic writes must overlap")
	assert.ElementsMatch(t, []string{"original", "thumbnail", "webpublic"}, puts)
	assert.Equal(t, [3]bool{true, true, true}, sawAll)
	assert.Zero(t, inFlight, "Upload returned while a write was still running")
	require.Len(t, files, 1)
	assert.NotNil(t, f.ThumbnailURL)
	assert.NotNil(t, f.WebpublicURL)
}

// The original write overlaps the generation of the alternate images.
func TestUpload_OriginalWriteOverlapsAltGeneration(t *testing.T) {
	body := parallelTestPNG(t)
	st := newProbeStorage(body)
	generating := make(chan struct{})
	var overlapped bool
	st.before = func(role string) error {
		if role == "original" {
			overlapped = waitOrTimeout(generating)
		}
		return nil
	}
	proc := &altProcessor{hook: func() { close(generating) }}
	svc, _ := newParallelSvc(t, st, proc)

	_, err := svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: body, Name: "a.png"})
	require.NoError(t, err)
	assert.True(t, overlapped, "the original write must not have to finish before the alternates are generated")
}

// Stored bytes, keys, URLs and the row are the same as before: each object
// under its own key, and the row points at the matching key and URL.
func TestUpload_ParallelWritesStoreSameContent(t *testing.T) {
	body := parallelTestPNG(t)
	st := newProbeStorage(body)
	svc, files := newParallelSvc(t, st, &altProcessor{})

	f, err := svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: body, Name: "a.png"})
	require.NoError(t, err)
	objects, _, _, _ := st.snapshot()
	require.Len(t, objects, 3)
	require.NotNil(t, f.AccessKey)
	require.NotNil(t, f.ThumbnailAccessKey)
	require.NotNil(t, f.WebpublicAccessKey)
	assert.Equal(t, body, objects[*f.AccessKey])
	assert.Equal(t, altThumbData, objects[*f.ThumbnailAccessKey])
	assert.Equal(t, altWebData, objects[*f.WebpublicAccessKey])
	assert.Equal(t, "https://cdn.example.test/"+*f.AccessKey, f.URL)
	assert.Equal(t, "https://cdn.example.test/"+*f.ThumbnailAccessKey, *f.ThumbnailURL)
	assert.Equal(t, "https://cdn.example.test/"+*f.WebpublicAccessKey, *f.WebpublicURL)
	require.NotNil(t, f.WebpublicType)
	assert.Equal(t, "image/webp", *f.WebpublicType)
	assert.Same(t, f, files[f.ID])
}

// LocalStorage writes each object to its own file.
func TestUpload_ParallelWritesWithLocalStorage(t *testing.T) {
	body := parallelTestPNG(t)
	dir := t.TempDir()
	svc, _ := newParallelSvc(t, drive.NewLocalStorage(dir, "https://example.com/files"), &altProcessor{})

	f, err := svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: body, Name: "a.png"})
	require.NoError(t, err)
	for key, want := range map[*string][]byte{f.AccessKey: body, f.ThumbnailAccessKey: altThumbData, f.WebpublicAccessKey: altWebData} {
		require.NotNil(t, key)
		got, err := os.ReadFile(filepath.Join(dir, *key))
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	assert.Equal(t, "https://example.com/files/"+*f.AccessKey, f.URL)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 3)
}

// When the original fails after the alternates were written, they are
// deleted: no object is left without a row.
func TestUpload_OriginalFailureDeletesWrittenAlternates(t *testing.T) {
	body := parallelTestPNG(t)
	st := newProbeStorage(body)
	altsDone := make(chan struct{})
	var altCount int
	var mu sync.Mutex
	st.before = func(role string) error {
		if role == "original" {
			waitOrTimeout(altsDone)
			return errPutFailed
		}
		return nil
	}
	wrapped := &afterPutStorage{probeStorage: st, after: func(role string) {
		if role == "original" {
			return
		}
		mu.Lock()
		altCount++
		if altCount == 2 {
			close(altsDone)
		}
		mu.Unlock()
	}}
	svc, files := newParallelSvc(t, wrapped, &altProcessor{})

	_, err := svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: body, Name: "a.png"})
	require.ErrorIs(t, err, errPutFailed)
	objects, puts, inFlight, _ := st.snapshot()
	assert.ElementsMatch(t, []string{"original", "thumbnail", "webpublic"}, puts, "the alternates must have been written for this case to mean anything")
	assert.Empty(t, objects, "alternates written before the original failed must be deleted")
	assert.Zero(t, inFlight)
	assert.Empty(t, files)
}

// 原本が既に失敗していたら代替画像を書かないことは、パッケージ内のテスト
// (upload_store_internal_test.go) で、時刻に頼らずに固定している。

// When the original write fails, the video thumbnail extraction (ffmpeg) is
// cancelled instead of running to completion, and the upload returns the
// write error rather than a cancellation error.
func TestUpload_OriginalFailureCancelsVideoThumbnail(t *testing.T) {
	body := quickTimeHeader()
	require.True(t, strings.HasPrefix(drive.DetectMIME(body), "video/"), "fixture must be detected as video")
	st := newProbeStorage(body)
	st.before = func(role string) error {
		if role == "original" {
			return errPutFailed
		}
		return nil
	}
	video := &ctxWaitingVideoProcessor{}
	svc, files := newParallelSvc(t, st, &altProcessor{})
	svc.SetVideoProcessor(video)

	_, err := svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: body, Name: "a.mov"})
	require.ErrorIs(t, err, errPutFailed)
	assert.NotErrorIs(t, err, context.Canceled)
	assert.True(t, video.cancelled.Load(), "thumbnail extraction must be cancelled once the original write failed")
	assert.Empty(t, files)
}

// When the original write fails while the upload is waiting for a media
// processing slot, it stops waiting and returns the write error; the image is
// never decoded.
func TestUpload_OriginalFailureStopsWaitingForMediaSlot(t *testing.T) {
	body := parallelTestPNG(t)
	st := newProbeStorage(body)
	st.before = func(role string) error {
		if role == "original" {
			return errPutFailed
		}
		return nil
	}
	holder := []byte("slot-holder")
	proc := &slotHolderProcessor{holder: holder, held: make(chan struct{}), release: make(chan struct{})}
	svc, files := newParallelSvc(t, st, proc)
	svc.SetMediaProcessingConcurrency(1)

	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		svc.GenerateAltsCtxForTest(context.Background(), holder, "image/png")
	}()
	require.True(t, waitOrTimeout(proc.held), "the slot holder never started")

	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: body, Name: "a.png"})
		done <- result{err}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(2 * time.Second):
		t.Error("Upload kept waiting for a media slot after the original write failed")
		close(proc.release)
		<-holderDone
		r = <-done
		require.ErrorIs(t, r.err, errPutFailed)
		return
	}
	close(proc.release)
	<-holderDone
	require.ErrorIs(t, r.err, errPutFailed)
	assert.NotErrorIs(t, r.err, context.Canceled)
	assert.Zero(t, proc.others.Load(), "the image must not be processed after the original write failed")
	assert.Empty(t, files)
}

// quickTimeHeader returns a minimal ISO-BMFF header detected as video.
func quickTimeHeader() []byte {
	b := []byte{0, 0, 0, 0x14, 'f', 't', 'y', 'p', 'q', 't', ' ', ' ', 0, 0, 0, 0, 'q', 't', ' ', ' '}
	for len(b) < 512 {
		b = append(b, 0)
	}
	return b
}

// ctxWaitingVideoProcessor stands in for ffmpeg: it runs until its context is
// cancelled (or a timeout, so a missing cancellation fails instead of hanging).
type ctxWaitingVideoProcessor struct {
	cancelled atomic.Bool
}

func (v *ctxWaitingVideoProcessor) GenerateThumbnail(ctx context.Context, _ []byte, _ string) (*drive.ProcessedImage, error) {
	select {
	case <-ctx.Done():
		v.cancelled.Store(true)
		return nil, ctx.Err()
	case <-time.After(2 * time.Second):
		return &drive.ProcessedImage{Data: altThumbData, MimeType: "image/webp"}, nil
	}
}

// slotHolderProcessor blocks while processing holder, keeping the media slot
// taken, and counts every other image it is asked to process.
type slotHolderProcessor struct {
	altProcessor
	holder  []byte
	held    chan struct{}
	release chan struct{}
	others  atomic.Int32
}

func (p *slotHolderProcessor) GenerateThumbnail(body []byte, mime string) (*drive.ProcessedImage, error) {
	if bytes.Equal(body, p.holder) {
		close(p.held)
		<-p.release
		return nil, nil
	}
	p.others.Add(1)
	return p.altProcessor.GenerateThumbnail(body, mime)
}

// A failing alternate write is skipped and the upload goes on, as before.
func TestUpload_AlternateFailureKeepsUploading(t *testing.T) {
	for _, failing := range []string{"thumbnail", "webpublic"} {
		t.Run(failing, func(t *testing.T) {
			body := parallelTestPNG(t)
			st := newProbeStorage(body)
			st.before = func(role string) error {
				if role == failing {
					return errPutFailed
				}
				return nil
			}
			svc, files := newParallelSvc(t, st, &altProcessor{})

			f, err := svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: body, Name: "a.png"})
			require.NoError(t, err)
			require.Len(t, files, 1)
			objects, _, _, _ := st.snapshot()
			assert.Len(t, objects, 2)
			assert.Equal(t, body, objects[*f.AccessKey])
			if failing == "thumbnail" {
				assert.Nil(t, f.ThumbnailURL)
				assert.Nil(t, f.ThumbnailAccessKey)
				require.NotNil(t, f.WebpublicAccessKey)
				assert.Equal(t, altWebData, objects[*f.WebpublicAccessKey])
			} else {
				assert.Nil(t, f.WebpublicURL)
				assert.Nil(t, f.WebpublicAccessKey)
				assert.Nil(t, f.WebpublicType)
				require.NotNil(t, f.ThumbnailAccessKey)
				assert.Equal(t, altThumbData, objects[*f.ThumbnailAccessKey])
			}
		})
	}
}

// A cancelled request still waits for every write before returning, so no
// goroutine is left inside Storage.Put. The outcome is the same as before:
// the original is stored and alternates are skipped while waiting for a slot.
func TestUpload_CancelledContextWaitsForWrites(t *testing.T) {
	body := parallelTestPNG(t)
	st := newProbeStorage(body)
	st.delay = 50 * time.Millisecond
	svc, files := newParallelSvc(t, st, &altProcessor{})
	svc.SetMediaProcessingConcurrency(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f, err := svc.Upload(ctx, drive.UploadInput{User: &model.User{ID: "u1"}, Body: body, Name: "a.png"})
	require.NoError(t, err)
	_, puts, inFlight, _ := st.snapshot()
	assert.Zero(t, inFlight, "Upload returned while a write was still running")
	assert.Equal(t, []string{"original"}, puts)
	assert.Nil(t, f.ThumbnailURL)
	assert.Len(t, files, 1)
}

// A panic inside Storage.Put surfaces on the caller's goroutine (as it did
// when the writes were sequential), after the other writes finished and the
// objects already written were removed.
func TestUpload_PutPanicIsRaisedOnCaller(t *testing.T) {
	body := parallelTestPNG(t)
	st := newProbeStorage(body)
	st.before = func(role string) error {
		if role == "thumbnail" {
			panic("boom")
		}
		return nil
	}
	svc, files := newParallelSvc(t, st, &altProcessor{})

	assert.PanicsWithValue(t, "boom", func() {
		_, _ = svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: body, Name: "a.png"})
	})
	objects, _, inFlight, _ := st.snapshot()
	assert.Zero(t, inFlight)
	assert.Empty(t, objects, "objects written before the panic must be removed")
	assert.Empty(t, files)
}

// afterPutStorage runs after once a Put of the given role has returned.
type afterPutStorage struct {
	*probeStorage
	after func(role string)
}

func (s *afterPutStorage) Put(accessKey string, body io.Reader) (string, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	defer s.after(s.role(b))
	return s.probeStorage.Put(accessKey, bytes.NewReader(b))
}

// BenchmarkUpload_DelayedStorage measures one upload against a storage whose
// writes each take storageDelay, with alternate generation taking procDelay.
// S3 の往復の代わりに固定の遅延を入れて、待ちが重なる分を見る (#3996)。
func BenchmarkUpload_DelayedStorage(b *testing.B) {
	const storageDelay = 40 * time.Millisecond
	const procDelay = 20 * time.Millisecond
	body := parallelTestPNG(b)
	st := newProbeStorage(body)
	st.delay = storageDelay
	folderRepo := testutil.NewMockDriveFolderRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(b, err)
	svc := drive.NewService(testutil.NewMockDriveFileRepository(), folderRepo, st, idGen, nil, nil, nil, nil)
	svc.SetImageProcessor(&altProcessor{delay: procDelay})
	user := &model.User{ID: "u1"}
	for b.Loop() {
		if _, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: body, Name: "a.png", Force: true}); err != nil {
			b.Fatal(err)
		}
	}
}
