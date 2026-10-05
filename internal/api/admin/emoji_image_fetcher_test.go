package admin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/safehttp"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingStorage implements drive.Storage with deterministic URLs and an
// in-memory blob store. Used to give Upload a real backing storage in tests
// without spinning up the LocalStorage filesystem path.
type recordingStorage struct {
	objects map[string][]byte
}

func newRecordingStorage() *recordingStorage {
	return &recordingStorage{objects: map[string][]byte{}}
}

func (r *recordingStorage) Put(accessKey string, body io.Reader) (string, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	r.objects[accessKey] = b
	return fmt.Sprintf("https://drive.local/%s", accessKey), nil
}

func (r *recordingStorage) Get(accessKey string) (io.ReadCloser, error) {
	b, ok := r.objects[accessKey]
	if !ok {
		return nil, drive.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (r *recordingStorage) Delete(accessKey string) error {
	delete(r.objects, accessKey)
	return nil
}

// We need a drive.Storage interface; use a minimal embedding fake. Build a
// real drive.Service with mock repositories so Upload exercises the full
// path including row creation.
func newDriveService(t *testing.T) (*drive.Service, *testutil.MockDriveFileRepository) {
	t.Helper()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	return drive.NewService(fileRepo, folderRepo, newRecordingStorage(), idGen), fileRepo
}

func TestEmojiImageFetcher_FetchAndStore_Success(t *testing.T) {
	const pixel = "\x89PNG\r\n\x1a\n" // PNG magic so AnalyseFile detects image/png
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.Header.Get("Accept"), "image/")
		assert.Equal(t, "mk-go/test", r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte(pixel))
	}))
	t.Cleanup(srv.Close)

	driveSvc, fileRepo := newDriveService(t)
	f := NewEmojiImageFetcher(&http.Client{Timeout: 5 * time.Second, Transport: srv.Client().Transport}, driveSvc, "mk-go/test")

	// nil user で「system 所有 drive file」として upload する (#670)。
	// Misskey TS uploadFromUrl({user: null}) と等価。
	df, err := f.FetchAndStore(context.Background(), srv.URL+"/x.png", nil, "happy")
	require.NoError(t, err)
	require.NotNil(t, df)
	assert.NotEmpty(t, df.URL)
	// drive レコードが永続化されており、UserID は nil (system-owned)
	require.Len(t, fileRepo.Files, 1)
	for _, file := range fileRepo.Files {
		assert.Nil(t, file.UserID, "system drive file must have UserID=nil")
		assert.Nil(t, file.UserHost, "system drive file must have UserHost=nil")
	}
}

func TestEmojiImageFetcher_InvalidScheme(t *testing.T) {
	driveSvc, _ := newDriveService(t)
	f := NewEmojiImageFetcher(&http.Client{}, driveSvc, "")
	_, err := f.FetchAndStore(context.Background(), "file:///etc/passwd", &model.User{ID: "u"}, "x")
	assert.Error(t, err)
}

func TestEmojiImageFetcher_EmptyURL(t *testing.T) {
	driveSvc, _ := newDriveService(t)
	f := NewEmojiImageFetcher(&http.Client{}, driveSvc, "")
	_, err := f.FetchAndStore(context.Background(), "", &model.User{ID: "u"}, "x")
	assert.Error(t, err)
}

func TestEmojiImageFetcher_NotWired(t *testing.T) {
	f := &EmojiImageFetcherImpl{} // both nil
	_, err := f.FetchAndStore(context.Background(), "https://x", &model.User{ID: "u"}, "x")
	assert.Error(t, err)
}

func TestEmojiImageFetcher_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	driveSvc, _ := newDriveService(t)
	f := NewEmojiImageFetcher(&http.Client{Transport: srv.Client().Transport}, driveSvc, "")
	_, err := f.FetchAndStore(context.Background(), srv.URL, &model.User{ID: "u"}, "x")
	assert.Error(t, err)
}

func TestEmojiImageFetcher_OversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// MaxEmojiImageBytes = 8 MiB; send 9 MiB to trigger the cap.
		_, _ = w.Write(make([]byte, 9<<20))
	}))
	t.Cleanup(srv.Close)

	driveSvc, _ := newDriveService(t)
	f := NewEmojiImageFetcher(&http.Client{Transport: srv.Client().Transport}, driveSvc, "")
	_, err := f.FetchAndStore(context.Background(), srv.URL, &model.User{ID: "u"}, "x")
	assert.Error(t, err)
}

func TestEmojiImageFetcher_DialFails(t *testing.T) {
	failingClient := &http.Client{Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("ssrf block: connection to private IP refused")
	})}
	driveSvc, _ := newDriveService(t)
	f := NewEmojiImageFetcher(failingClient, driveSvc, "")
	_, err := f.FetchAndStore(context.Background(), "http://10.0.0.1/img.png", &model.User{ID: "u"}, "x")
	assert.Error(t, err)
	// 内部詳細はそのまま返す (caller である EmojiCopy 側が log + 静的 message に丸める)
	assert.Contains(t, err.Error(), "fetch image")
}

// TestEmojiImageFetcher_EmptyNameFallsBackToURLPath は name="" のとき URL の
// 最終 path セグメントが drive file 名として使われる Misskey TS 互換 fallback
// を guard する。
func TestEmojiImageFetcher_EmptyNameFallsBackToURLPath(t *testing.T) {
	const pixel = "\x89PNG\r\n\x1a\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte(pixel))
	}))
	t.Cleanup(srv.Close)

	driveSvc, fileRepo := newDriveService(t)
	f := NewEmojiImageFetcher(&http.Client{Timeout: 5 * time.Second, Transport: srv.Client().Transport}, driveSvc, "")

	df, err := f.FetchAndStore(context.Background(), srv.URL+"/path/to/x.png", &model.User{ID: "u"}, "")
	require.NoError(t, err)
	require.NotNil(t, df)
	// drive レコードが name="x.png" で作成される
	require.Len(t, fileRepo.Files, 1)
	for _, file := range fileRepo.Files {
		assert.Equal(t, "x.png", file.Name)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// --- #2966 承認時に system 所有へ複製する ---

// **この修正の中核 (#2966)。** 承認した絵文字が申請者所有の drive ファイルを
// 参照し続けると、申請者がそれを消した時点で / アカウントを消した時点で表示が
// 壊れる。**実装そのものを動かす** — handler のテストは fetcher を差し替えるので、
// ここが無いと `CopyToSystemFile` を丸ごと無効化してもすべて緑になる (実測)。
func TestEmojiImageFetcher_CopyToSystemFile(t *testing.T) {
	svc, fileRepo := newDriveService(t)
	f := NewEmojiImageFetcher(nil, svc, "test-agent")

	// 申請者が持っているファイル。
	owner := &model.User{ID: "u1", Username: "alice"}
	const pixel = "\x89PNG\r\n\x1a\nemoji-bytes"
	src, err := svc.Upload(context.Background(), drive.UploadInput{
		User: owner, Body: []byte(pixel), Name: "src.png",
	})
	require.NoError(t, err)
	require.NotNil(t, src.UserID)

	copied, err := f.CopyToSystemFile(context.Background(), src, "sushi", false)
	require.NoError(t, err)
	require.NotNil(t, copied)

	// **system 所有になっていること。** 利用者に紐付けると、ロールの変更や
	// アカウント削除で巻き込まれる (このバグそのもの)。
	require.Nil(t, copied.UserID, "申請者に紐付いたままになっている")
	require.Nil(t, copied.UserHost, "リモート扱いになっている")
	require.NotEqual(t, src.ID, copied.ID, "複製せず同じ行を返している")
	require.NotEqual(t, src.URL, copied.URL, "複製せず元の URL を返している")

	// 中身が同じこと。
	require.Equal(t, src.MD5, copied.MD5, "複製の中身が元と違う")
	require.Equal(t, src.Type, copied.Type)
	body, err := svc.ReadFileBody(copied, MaxEmojiCopyBytes)
	require.NoError(t, err)
	require.Equal(t, []byte(pixel), body, "複製の実体が元と違う")

	// **元のファイルは触らない。** ノートの添付やプロフィールで使われている
	// 可能性がある。
	stored, err := fileRepo.FindByID(src.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.UserID, "元のファイルの所有者を奪っている")
	require.Equal(t, "u1", *stored.UserID)
	require.Equal(t, src.URL, stored.URL, "元のファイルの URL が変わっている")
}

// 申請で sensitive を指定したら複製にも印を付ける。
func TestEmojiImageFetcher_CopyToSystemFileCarriesSensitive(t *testing.T) {
	svc, _ := newDriveService(t)
	f := NewEmojiImageFetcher(nil, svc, "test-agent")
	owner := &model.User{ID: "u1", Username: "alice"}
	src, err := svc.Upload(context.Background(), drive.UploadInput{
		User: owner, Body: []byte("\x89PNG\r\n\x1a\nx"), Name: "src.png",
	})
	require.NoError(t, err)
	require.False(t, src.IsSensitive)

	copied, err := f.CopyToSystemFile(context.Background(), src, "sushi", true)
	require.NoError(t, err)
	require.True(t, copied.IsSensitive, "申請の sensitive 指定が複製に伝わっていない")
}

// **大きすぎる本体は読まない。** 上限なしにすると 1 リクエストで任意サイズを
// メモリに載せられる。上限に当たったことが呼び出し側に伝わること (承認が
// 「そんなファイルは無い」に化けないため)。
func TestEmojiImageFetcher_CopyToSystemFileRejectsOversized(t *testing.T) {
	svc, _ := newDriveService(t)
	f := NewEmojiImageFetcher(nil, svc, "test-agent")
	owner := &model.User{ID: "u1", Username: "alice"}
	big := make([]byte, MaxEmojiCopyBytes+1)
	copy(big, "\x89PNG\r\n\x1a\n")
	src, err := svc.Upload(context.Background(), drive.UploadInput{
		User: owner, Body: big, Name: "big.png",
	})
	require.NoError(t, err)

	_, err = f.CopyToSystemFile(context.Background(), src, "sushi", false)
	require.Error(t, err)
	require.ErrorIs(t, err, safehttp.ErrResponseTooLarge,
		"大きすぎることが呼び出し側に伝わらない (承認が「ファイルが無い」に化ける)")
}

// **複製の上限が drive の受け入れ上限を下回らないこと (レビュー H2)。**
// 下回ると「申請はできたのに承認だけ恒久的に失敗する」サイズ帯が生まれる。
// role policy の `maxFileSizeMb` の既定は 30 なので、そこを基準にする
// (policy をこれより上げている構成では `EMOJI_IMAGE_TOO_LARGE` になるが、
// 原因が分かる文面が返る)。
func TestMaxEmojiCopyBytesCoversDefaultUploadLimit(t *testing.T) {
	const defaultMaxFileSizeMB = 30
	require.GreaterOrEqual(t, MaxEmojiCopyBytes, int64(defaultMaxFileSizeMB)<<20,
		"複製の上限が drive の既定の受け入れ上限より小さい (承認だけが失敗するサイズ帯ができる)")
	// リモート取得の上限を流用しない (あちらは相手サーバーがいくらでも送れる)。
	require.Greater(t, MaxEmojiCopyBytes, MaxEmojiImageBytes,
		"リモート取得の上限をそのまま複製に使っている")
}

// 未配線なら黙って成功しない。
func TestEmojiImageFetcher_CopyToSystemFileRequiresDrive(t *testing.T) {
	f := NewEmojiImageFetcher(nil, nil, "test-agent")
	_, err := f.CopyToSystemFile(context.Background(), &model.DriveFile{}, "x", false)
	require.Error(t, err)
}

// **複製したファイルを消せること (#2966)。** 承認が途中で失敗したときの後始末。
func TestEmojiImageFetcher_DeleteSystemFile(t *testing.T) {
	svc, fileRepo := newDriveService(t)
	f := NewEmojiImageFetcher(nil, svc, "test-agent")
	sys, err := svc.Upload(context.Background(), drive.UploadInput{
		Body: []byte("\x89PNG\r\n\x1a\nx"), Name: "sys.png",
	})
	require.NoError(t, err)
	require.Nil(t, sys.UserID)

	require.NoError(t, f.DeleteSystemFile(context.Background(), sys.ID))
	_, err = fileRepo.FindByID(sys.ID)
	require.Error(t, err, "複製が残っている")
}
