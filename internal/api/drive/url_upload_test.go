package drive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptr[T any](v T) *T { return &v }

// --- mocks ---

type mockDriveUploader struct {
	gotInput *coredrive.UploadInput
	ret      *model.DriveFile
	err      error
}

func (m *mockDriveUploader) Upload(_ context.Context, in coredrive.UploadInput) (*model.DriveFile, error) {
	cp := in
	m.gotInput = &cp
	return m.ret, m.err
}

type publishedMainEvent struct {
	userID    string
	eventType string
	body      any
}

type mockMainPublisher struct {
	mu     sync.Mutex
	events []publishedMainEvent
}

func (m *mockMainPublisher) PublishMainEvent(userID, eventType string, body any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, publishedMainEvent{userID, eventType, body})
}

func newTestURLUploader(uploader driveUploader, pub mainEventPublisher) *URLUploader {
	idGen, _ := id.NewGenerator("aidx")
	// http.DefaultClient で httptest (127.0.0.1) を叩く。production の SSRF-safe
	// transport は private IP を弾くため unit test では使わない (SSRF 防御は
	// safehttp package 側でテスト済み)。
	return NewURLUploader(http.DefaultClient, uploader, pub, idGen, "mk-go-test", 0)
}

// --- URLUploader.Process ---

func TestURLUploader_Process_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("file-bytes"))
	}))
	defer srv.Close()

	idGen, _ := id.NewGenerator("aidx")
	fileID := idGen.Generate(time.Now())
	up := &mockDriveUploader{ret: &model.DriveFile{ID: fileID, Name: "cat.png", Type: "image/png"}}
	pub := &mockMainPublisher{}
	u := NewURLUploader(http.DefaultClient, up, pub, idGen, "mk-go-test", 0)

	marker := "m-123"
	u.Process(context.Background(), URLUploadInput{
		User:        &model.User{ID: "u1"},
		URL:         srv.URL + "/path/cat.png",
		FolderID:    ptr("f1"),
		Comment:     ptr("nice"),
		Marker:      &marker,
		IsSensitive: true,
		Force:       true,
		RequestIP:   "1.2.3.4",
	})

	// upload に渡った input が正しいこと。
	require.NotNil(t, up.gotInput)
	require.NotNil(t, up.gotInput.User)
	assert.Equal(t, "u1", up.gotInput.User.ID)
	assert.Equal(t, []byte("file-bytes"), up.gotInput.Body)
	assert.Equal(t, "cat.png", up.gotInput.Name) // URL path basename
	assert.True(t, up.gotInput.IsSensitive)
	assert.True(t, up.gotInput.Force)
	require.NotNil(t, up.gotInput.Comment)
	assert.Equal(t, "nice", *up.gotInput.Comment)
	require.NotNil(t, up.gotInput.FolderID)
	assert.Equal(t, "f1", *up.gotInput.FolderID)
	require.NotNil(t, up.gotInput.RequestIP)
	assert.Equal(t, "1.2.3.4", *up.gotInput.RequestIP)

	// urlUploadFinished が marker + packed file 付きで publish されること。
	require.Len(t, pub.events, 1)
	ev := pub.events[0]
	assert.Equal(t, "u1", ev.userID)
	assert.Equal(t, "urlUploadFinished", ev.eventType)
	body, ok := ev.body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, &marker, body["marker"])
	require.NotNil(t, body["file"])
	// upstream の pack(file, {self:true}) single pack は userId も null
	// (#1564)。folder / user は PackDriveFile が元々 nil。
	file, ok := body["file"].(entity.DriveFileEntity)
	require.True(t, ok)
	assert.Nil(t, file.UserID, "urlUploadFinished payload の userId は null")
	assert.Nil(t, file.Folder)
	assert.Nil(t, file.User)
}

func TestURLUploader_Process_FetchErrorNoPublish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	up := &mockDriveUploader{}
	pub := &mockMainPublisher{}
	u := newTestURLUploader(up, pub)

	u.Process(context.Background(), URLUploadInput{User: &model.User{ID: "u1"}, URL: srv.URL + "/x"})

	// 4xx は fetch 失敗扱い → upload も publish もしない。
	assert.Nil(t, up.gotInput)
	assert.Empty(t, pub.events)
}

func TestURLUploader_Process_InvalidURLNoUpload(t *testing.T) {
	up := &mockDriveUploader{}
	pub := &mockMainPublisher{}
	u := newTestURLUploader(up, pub)

	// http(s) 以外の scheme は弾く。
	u.Process(context.Background(), URLUploadInput{User: &model.User{ID: "u1"}, URL: "file:///etc/passwd"})

	assert.Nil(t, up.gotInput)
	assert.Empty(t, pub.events)
}

func TestURLUploader_Process_UploadErrorNoPublish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	up := &mockDriveUploader{err: coredrive.ErrMaxFileSizeExceeded}
	pub := &mockMainPublisher{}
	u := newTestURLUploader(up, pub)

	u.Process(context.Background(), URLUploadInput{User: &model.User{ID: "u1"}, URL: srv.URL + "/big.bin"})

	// fetch は成功するが upload が失敗 → publish しない。
	require.NotNil(t, up.gotInput)
	assert.Empty(t, pub.events)
}

func TestURLUploader_Process_RootPathNameFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	idGen, _ := id.NewGenerator("aidx")
	up := &mockDriveUploader{ret: &model.DriveFile{ID: idGen.Generate(time.Now()), Name: "unknown"}}
	pub := &mockMainPublisher{}
	u := NewURLUploader(http.DefaultClient, up, pub, idGen, "", 0)

	// path 無し (root) の URL は name="unknown" にフォールバックする。
	u.Process(context.Background(), URLUploadInput{User: &model.User{ID: "u1"}, URL: srv.URL})

	require.NotNil(t, up.gotInput)
	assert.Equal(t, "unknown", up.gotInput.Name)
	assert.Nil(t, up.gotInput.RequestIP) // RequestIP 空なら nil のまま
}

func TestURLUploader_Process_NotWiredNoPanic(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	// uploader/httpClient nil でも panic せず no-op。
	u := NewURLUploader(nil, nil, nil, idGen, "", 0)
	u.Process(context.Background(), URLUploadInput{User: &model.User{ID: "u1"}, URL: "https://example.com/x"})
}

func TestNewURLUploader_MaxBytesDefault(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	u := NewURLUploader(http.DefaultClient, &mockDriveUploader{}, &mockMainPublisher{}, idGen, "", 0)
	assert.Equal(t, defaultURLUploadMaxBytes, u.maxBytes)

	u2 := NewURLUploader(http.DefaultClient, &mockDriveUploader{}, &mockMainPublisher{}, idGen, "", 100)
	assert.Equal(t, int64(100), u2.maxBytes)
}

// --- FilesUploadFromURL handler ---

// recordingProcessor captures Process invocations for the handler test. The
// handler launches Process in a goroutine, so we sync via a channel.
type recordingProcessor struct {
	calls chan URLUploadInput
}

func (r *recordingProcessor) Process(_ context.Context, in URLUploadInput) {
	r.calls <- in
}

func TestFilesUploadFromURL_Success(t *testing.T) {
	h, _, _ := newHandler(t)
	rp := &recordingProcessor{calls: make(chan URLUploadInput, 1)}
	h.SetURLUploader(rp)

	c, rec := newJSONReq(t, `{"url":"https://example.com/a/b.png","marker":"mk1","isSensitive":true}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUploadFromURL(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	select {
	case in := <-rp.calls:
		assert.Equal(t, "https://example.com/a/b.png", in.URL)
		assert.Equal(t, "u1", in.User.ID)
		require.NotNil(t, in.Marker)
		assert.Equal(t, "mk1", *in.Marker)
		assert.True(t, in.IsSensitive)
	case <-time.After(2 * time.Second):
		t.Fatal("Process was not invoked")
	}
}

func TestFilesUploadFromURL_MissingURL(t *testing.T) {
	h, _, _ := newHandler(t)
	h.SetURLUploader(&recordingProcessor{calls: make(chan URLUploadInput, 1)})

	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUploadFromURL(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesUploadFromURL_CommentTooLong(t *testing.T) {
	h, _, _ := newHandler(t)
	h.SetURLUploader(&recordingProcessor{calls: make(chan URLUploadInput, 1)})

	long := strings.Repeat("a", 513)
	c, rec := newJSONReq(t, `{"url":"https://example.com/x","comment":"`+long+`"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUploadFromURL(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 多バイト文字 512 字以内 (byte 数では 512 超) のコメントは受理されること。
// byte 長判定だと過剰に弾いていた (rune 数判定への修正の回帰防止)。
func TestFilesUploadFromURL_MultibyteCommentWithinLimit(t *testing.T) {
	h, _, _ := newHandler(t)
	rp := &recordingProcessor{calls: make(chan URLUploadInput, 1)}
	h.SetURLUploader(rp)

	comment := strings.Repeat("あ", 512) // 512 runes = 1536 bytes
	c, rec := newJSONReq(t, `{"url":"https://example.com/x","comment":"`+comment+`"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUploadFromURL(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	select {
	case in := <-rp.calls:
		require.NotNil(t, in.Comment)
		assert.Equal(t, comment, *in.Comment)
	case <-time.After(2 * time.Second):
		t.Fatal("Process was not invoked")
	}
}

func TestFilesUploadFromURL_NotWired(t *testing.T) {
	h, _, _ := newHandler(t) // urlUploader 未配線
	c, rec := newJSONReq(t, `{"url":"https://example.com/x"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUploadFromURL(c))
	// upstream と同じく空レスポンス (204)。
	assert.Equal(t, http.StatusNoContent, rec.Code)
}
