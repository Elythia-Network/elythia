package drive

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const apiChunkSize = 5 * 1024 * 1024

// chunkedRoleChecker grants the shipped defaults to every user.
type chunkedRoleChecker struct{ moderator bool }

func (c *chunkedRoleChecker) IsModerator(string) bool { return c.moderator }

func (c *chunkedRoleChecker) GetUserPolicies(string) map[string]any {
	return map[string]any{
		role.PolicyCanUseChunkedUpload:                true,
		role.PolicyChunkedUploadMaxConcurrentSessions: 4,
		role.PolicyChunkedUploadMaxPendingMb:          1024,
		"maxFileSizeMb":                               100,
		"driveCapacityMb":                             1024,
	}
}

// newChunkedHandler builds a handler whose storage supports multipart upload,
// so chunked upload is actually available.
func newChunkedHandler(t *testing.T) (*Handler, *mockChunkedSessionRepo, *testutil.MockDriveFolderRepository) {
	t.Helper()
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	folderRepo.FilesRef = fileRepo
	storage := newMockMultipartStorage()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	svc := coredrive.NewService(fileRepo, folderRepo, storage, idGen)
	svc.SetRoleChecker(&chunkedRoleChecker{})
	sessions := newMockChunkedSessionRepo()
	svc.SetChunkedUpload(sessions, func() coredrive.ChunkedUploadSettings {
		return coredrive.ChunkedUploadSettings{
			Enabled:                true,
			ChunkSize:              apiChunkSize,
			SessionTTL:             time.Hour,
			MaxSessionsPerUser:     8,
			MaxPendingBytesPerUser: 2048 * 1024 * 1024,
		}
	})
	return NewHandler(svc, idGen), sessions, folderRepo
}

// newChunkReq builds the multipart request the append endpoint expects.
func newChunkReq(t *testing.T, uploadID string, index int, chunk []byte, omitChunk bool) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	require.NoError(t, mw.WriteField("uploadId", uploadID))
	require.NoError(t, mw.WriteField("index", strconv.Itoa(index)))
	if !omitChunk {
		part, err := mw.CreateFormFile("chunk", "chunk.bin")
		require.NoError(t, err)
		_, err = part.Write(chunk)
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.Header.Set(echo.HeaderContentType, mw.FormDataContentType())
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	body := decodeJSON(t, rec)
	e, ok := body["error"].(map[string]any)
	require.True(t, ok, "response must carry a Misskey error envelope: %s", rec.Body.String())
	return e["code"].(string)
}

// startSession drives the start endpoint and returns the opaque session id.
func startSession(t *testing.T, h *Handler, size int64) string {
	t.Helper()
	c, rec := newJSONReq(t, `{"name":"video.bin","size":`+strconv.FormatInt(size, 10)+`}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedStart(c))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return decodeJSON(t, rec)["uploadId"].(string)
}

// ---------------------------------------------------------------------------
// start
// ---------------------------------------------------------------------------

func TestFilesCreateChunkedStart_Success(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	c, rec := newJSONReq(t, `{"name":"video.bin","size":10485760}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedStart(c))
	require.Equal(t, http.StatusOK, rec.Code)

	body := decodeJSON(t, rec)
	assert.NotEmpty(t, body["uploadId"])
	assert.Equal(t, float64(apiChunkSize), body["chunkSize"])
	assert.Equal(t, float64(2), body["totalChunks"])
	assert.NotEmpty(t, body["expiresAt"])
	// S3 の UploadId は決してクライアントに出さない。
	assert.NotContains(t, rec.Body.String(), "upload-")
}

// ローカルストレージ構成 (= MultipartStorage 非対応) では start が通らない。
func TestFilesCreateChunkedStart_Unavailable(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"name":"video.bin","size":10485760}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedStart(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "CHUNKED_UPLOAD_UNAVAILABLE", errorCode(t, rec))
}

func TestFilesCreateChunkedStart_InvalidBody(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	c, rec := newJSONReq(t, `{"size":"nope"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedStart(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// name の正規化は drive/files/create と揃える (#1564)。分割経路だけ別扱いに
// すると同じファイルでも保存名が変わってしまう。
func TestFilesCreateChunkedStart_NameNormalisation(t *testing.T) {
	h, sessions, _ := newChunkedHandler(t)
	for _, name := range []string{"", " ", "blob"} {
		payload, err := json.Marshal(map[string]any{"name": name, "size": 100})
		require.NoError(t, err)
		c, rec := newJSONReq(t, string(payload))
		setUser(c, "u1")
		require.NoError(t, h.FilesCreateChunkedStart(c))
		require.Equal(t, http.StatusOK, rec.Code)
		id := decodeJSON(t, rec)["uploadId"].(string)
		assert.Equal(t, "untitled", sessions.Get(id).Name)
	}
}

func TestFilesCreateChunkedStart_InvalidFileName(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	c, rec := newJSONReq(t, `{"name":"bad/name.txt","size":100}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedStart(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "INVALID_FILE_NAME", errorCode(t, rec))
}

func TestFilesCreateChunkedStart_CommentTooLong(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	long := make([]byte, maxDriveCommentLength+1)
	for i := range long {
		long[i] = 'a'
	}
	payload, err := json.Marshal(map[string]any{"name": "a.bin", "size": 100, "comment": string(long)})
	require.NoError(t, err)
	c, rec := newJSONReq(t, string(payload))
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedStart(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "INVALID_PARAM", errorCode(t, rec))
}

func TestFilesCreateChunkedStart_InvalidSize(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	c, rec := newJSONReq(t, `{"name":"a.bin","size":0}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedStart(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "INVALID_UPLOAD_SIZE", errorCode(t, rec))
}

func TestFilesCreateChunkedStart_NoSuchFolder(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	c, rec := newJSONReq(t, `{"name":"a.bin","size":100,"folderId":"nope"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedStart(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "NO_SUCH_FOLDER", errorCode(t, rec))
}

// 他人の folder も「不在」と同じ応答にする (存在 oracle を作らない、#1908)。
func TestFilesCreateChunkedStart_OthersFolderIsIndistinguishable(t *testing.T) {
	h, _, folderRepo := newChunkedHandler(t)
	other := "u2"
	require.NoError(t, folderRepo.Create(&model.DriveFolder{ID: "fo1", UserID: &other}))

	c, rec := newJSONReq(t, `{"name":"a.bin","size":100,"folderId":"fo1"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedStart(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "NO_SUCH_FOLDER", errorCode(t, rec))
}

// ---------------------------------------------------------------------------
// append
// ---------------------------------------------------------------------------

func TestFilesCreateChunkedAppend_Success(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	uploadID := startSession(t, h, apiChunkSize+10)

	c, rec := newChunkReq(t, uploadID, 0, chunkBytes(apiChunkSize), false)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAppend(c))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	body := decodeJSON(t, rec)
	assert.Equal(t, float64(0), body["index"])
	assert.Equal(t, float64(1), body["next"])
	assert.Equal(t, float64(apiChunkSize), body["receivedBytes"])
	assert.Equal(t, false, body["completed"])
}

func TestFilesCreateChunkedAppend_MissingParams(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	uploadID := startSession(t, h, 100)

	// uploadId 欠落
	c, rec := newChunkReq(t, "", 0, chunkBytes(10), false)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAppend(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// chunk 欠落
	c, rec = newChunkReq(t, uploadID, 0, nil, true)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAppend(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesCreateChunkedAppend_InvalidIndexParam(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	uploadID := startSession(t, h, 100)

	for _, raw := range []string{"", "abc", "-1"} {
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		require.NoError(t, mw.WriteField("uploadId", uploadID))
		require.NoError(t, mw.WriteField("index", raw))
		part, err := mw.CreateFormFile("chunk", "chunk.bin")
		require.NoError(t, err)
		_, err = part.Write(chunkBytes(10))
		require.NoError(t, err)
		require.NoError(t, mw.Close())

		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/", body)
		req.Header.Set(echo.HeaderContentType, mw.FormDataContentType())
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		setUser(c, "u1")
		require.NoError(t, h.FilesCreateChunkedAppend(c))
		assert.Equal(t, http.StatusBadRequest, rec.Code, "index=%q", raw)
	}
}

// 他人のセッション ID は「存在しない」と同じ応答にする。
func TestFilesCreateChunkedAppend_OtherUserIsNotFound(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	uploadID := startSession(t, h, 100)

	c, rec := newChunkReq(t, uploadID, 0, chunkBytes(100), false)
	setUser(c, "u2")
	require.NoError(t, h.FilesCreateChunkedAppend(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "NO_SUCH_UPLOAD_SESSION", errorCode(t, rec))

	c, rec = newChunkReq(t, "no-such-session", 0, chunkBytes(100), false)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAppend(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "NO_SUCH_UPLOAD_SESSION", errorCode(t, rec))
}

// index 不一致は期待値を info で返す。これが無いとクライアントは再開位置を
// 決められない (応答の消失と送信の失敗を区別できないため)。
func TestFilesCreateChunkedAppend_IndexMismatchCarriesExpected(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	uploadID := startSession(t, h, 2*apiChunkSize)

	c, rec := newChunkReq(t, uploadID, 1, chunkBytes(apiChunkSize), false)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAppend(c))
	assert.Equal(t, http.StatusConflict, rec.Code)

	body := decodeJSON(t, rec)
	e := body["error"].(map[string]any)
	assert.Equal(t, "INVALID_CHUNK_INDEX", e["code"])
	info := e["info"].(map[string]any)
	assert.Equal(t, float64(0), info["expected"])
}

func TestFilesCreateChunkedAppend_InvalidChunkSize(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	uploadID := startSession(t, h, 2*apiChunkSize)

	c, rec := newChunkReq(t, uploadID, 0, chunkBytes(apiChunkSize-1), false)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAppend(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "INVALID_CHUNK_SIZE", errorCode(t, rec))
}

func TestFilesCreateChunkedAppend_ContentMismatchOnResend(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	uploadID := startSession(t, h, 2*apiChunkSize)

	c, _ := newChunkReq(t, uploadID, 0, chunkBytes(apiChunkSize), false)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAppend(c))

	other := append(chunkBytes(apiChunkSize-1), 'b')
	c, rec := newChunkReq(t, uploadID, 0, other, false)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAppend(c))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "CHUNK_CONTENT_MISMATCH", errorCode(t, rec))
}

func TestFilesCreateChunkedAppend_OvershootIsTooLarge(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	uploadID := startSession(t, h, 100)

	c, rec := newChunkReq(t, uploadID, 0, chunkBytes(101), false)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAppend(c))
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, "MAX_FILE_SIZE_EXCEEDED", errorCode(t, rec))
}

// ---------------------------------------------------------------------------
// finish
// ---------------------------------------------------------------------------

// finish のレスポンスは drive/files/create と同一 shape (pack(self)) にする。
func TestFilesCreateChunkedFinish_Success(t *testing.T) {
	h, sessions, _ := newChunkedHandler(t)
	uploadID := startSession(t, h, apiChunkSize+10)

	for i, chunk := range [][]byte{chunkBytes(apiChunkSize), chunkBytes(10)} {
		c, rec := newChunkReq(t, uploadID, i, chunk, false)
		setUser(c, "u1")
		require.NoError(t, h.FilesCreateChunkedAppend(c))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	c, rec := newJSONReq(t, `{"uploadId":"`+uploadID+`"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedFinish(c))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	body := decodeJSON(t, rec)
	// 中身はテキストなので拡張子補正で .txt が付く (upstream と同じ)。
	assert.Equal(t, "video.bin.txt", body["name"])
	assert.Equal(t, float64(apiChunkSize+10), body["size"])
	// pack(self) は folder / userId / user を null 化する。
	assert.Nil(t, body["folder"])
	assert.Nil(t, body["userId"])
	assert.Nil(t, body["user"])
	assert.Equal(t, 0, sessions.Count())
}

func TestFilesCreateChunkedFinish_MissingUploadID(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedFinish(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesCreateChunkedFinish_Incomplete(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	uploadID := startSession(t, h, apiChunkSize+10)
	c, _ := newChunkReq(t, uploadID, 0, chunkBytes(apiChunkSize), false)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAppend(c))

	c, rec := newJSONReq(t, `{"uploadId":"`+uploadID+`"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedFinish(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "INCOMPLETE_UPLOAD", errorCode(t, rec))
}

func TestFilesCreateChunkedFinish_OtherUserIsNotFound(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	uploadID := startSession(t, h, 10)
	c, _ := newChunkReq(t, uploadID, 0, chunkBytes(10), false)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAppend(c))

	c, rec := newJSONReq(t, `{"uploadId":"`+uploadID+`"}`)
	setUser(c, "u2")
	require.NoError(t, h.FilesCreateChunkedFinish(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "NO_SUCH_UPLOAD_SESSION", errorCode(t, rec))
}

func TestFilesCreateChunkedFinish_Busy(t *testing.T) {
	h, sessions, _ := newChunkedHandler(t)
	uploadID := startSession(t, h, 10)
	c, _ := newChunkReq(t, uploadID, 0, chunkBytes(10), false)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAppend(c))

	ok, err := sessions.ClaimFinish(uploadID, time.Now())
	require.NoError(t, err)
	require.True(t, ok)

	c, rec := newJSONReq(t, `{"uploadId":"`+uploadID+`"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedFinish(c))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "UPLOAD_SESSION_BUSY", errorCode(t, rec))
}

// ---------------------------------------------------------------------------
// abort
// ---------------------------------------------------------------------------

func TestFilesCreateChunkedAbort_Success(t *testing.T) {
	h, sessions, _ := newChunkedHandler(t)
	uploadID := startSession(t, h, 2*apiChunkSize)
	c, _ := newChunkReq(t, uploadID, 0, chunkBytes(apiChunkSize), false)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAppend(c))

	c, rec := newJSONReq(t, `{"uploadId":"`+uploadID+`"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAbort(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 0, sessions.Count())
}

func TestFilesCreateChunkedAbort_MissingUploadID(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAbort(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesCreateChunkedAbort_OtherUserIsNotFound(t *testing.T) {
	h, sessions, _ := newChunkedHandler(t)
	uploadID := startSession(t, h, apiChunkSize)

	c, rec := newJSONReq(t, `{"uploadId":"`+uploadID+`"}`)
	setUser(c, "u2")
	require.NoError(t, h.FilesCreateChunkedAbort(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "NO_SUCH_UPLOAD_SESSION", errorCode(t, rec))
	assert.Equal(t, 1, sessions.Count())
}

// ---------------------------------------------------------------------------
// error mapping
// ---------------------------------------------------------------------------

// 分割経路でも既存の制約は drive/files/create と同じ code / status で返す。
// 経路によって error が変わるとフロントエンドが分岐を二重に持つことになる。
func TestChunkedError_Mapping(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{coredrive.ErrChunkedUploadUnavailable, http.StatusBadRequest, "CHUNKED_UPLOAD_UNAVAILABLE"},
		{coredrive.ErrChunkedUploadNotAllowed, http.StatusForbidden, "CHUNKED_UPLOAD_NOT_ALLOWED"},
		{coredrive.ErrTooManyUploadSessions, http.StatusTooManyRequests, "TOO_MANY_UPLOAD_SESSIONS"},
		{coredrive.ErrPendingUploadLimitExceeded, http.StatusTooManyRequests, "PENDING_UPLOAD_LIMIT_EXCEEDED"},
		{coredrive.ErrUploadSessionNotFound, http.StatusNotFound, "NO_SUCH_UPLOAD_SESSION"},
		{coredrive.ErrUploadSessionBusy, http.StatusConflict, "UPLOAD_SESSION_BUSY"},
		{coredrive.ErrInvalidChunkSize, http.StatusBadRequest, "INVALID_CHUNK_SIZE"},
		{coredrive.ErrChunkContentMismatch, http.StatusConflict, "CHUNK_CONTENT_MISMATCH"},
		{coredrive.ErrIncompleteUpload, http.StatusBadRequest, "INCOMPLETE_UPLOAD"},
		{coredrive.ErrInvalidUploadSize, http.StatusBadRequest, "INVALID_UPLOAD_SIZE"},
		{coredrive.ErrFolderNotFound, http.StatusBadRequest, "NO_SUCH_FOLDER"},
		{coredrive.ErrUnallowedFileType, http.StatusBadRequest, "UNALLOWED_FILE_TYPE"},
		{coredrive.ErrMaxFileSizeExceeded, http.StatusRequestEntityTooLarge, "MAX_FILE_SIZE_EXCEEDED"},
		// **#3037 レビュー 3 周目。** 落とすと汎用の 500 INTERNAL_ERROR に
		// なるのに、テストが無いので外から見える挙動の変化を検出できなかった。
		{coredrive.ErrUndecodableImage, http.StatusRequestEntityTooLarge, "MAX_FILE_SIZE_EXCEEDED"},
		{coredrive.ErrNoFreeSpace, http.StatusBadRequest, "NO_FREE_SPACE"},
		{coredrive.ErrAccessDenied, http.StatusBadRequest, "ACCESS_DENIED"},
		{errStub, http.StatusInternalServerError, "INTERNAL_ERROR"},
	}
	for _, tc := range cases {
		c, rec := newJSONReq(t, `{}`)
		require.NoError(t, h.chunkedError(c, tc.err))
		assert.Equal(t, tc.status, rec.Code, tc.code)
		assert.Equal(t, tc.code, errorCode(t, rec))
	}
}

// 発番した UUID が endpoint 間で重複していないこと。重複すると frontend の
// error mapping が別のエラーに反応する。
func TestChunkedErrorUUIDsAreUnique(t *testing.T) {
	uuids := []string{
		uuidChunkedUploadUnavailable, uuidChunkedUploadNotAllowed, uuidTooManyUploadSessions,
		uuidPendingUploadLimit, uuidNoSuchUploadSession, uuidUploadSessionBusy,
		uuidInvalidChunkIndex, uuidInvalidChunkSize, uuidChunkContentMismatch,
		uuidIncompleteUpload, uuidInvalidUploadSize,
	}
	seen := map[string]bool{}
	for _, u := range uuids {
		assert.Len(t, u, 36, "%s must be a UUID", u)
		assert.False(t, seen[u], "duplicate UUID: %s", u)
		seen[u] = true
	}
}

// **セッションの上限を読む前に引くこと (#3037 レビュー)。**
//
// `AppendChunk` は `size > sess.ChunkSize` を拒否するが、そこへ届く時点で
// チャンクは全部メモリに載っている。body limit はこの経路で 33MiB なので、
// 既定 10MiB のセッションでも 1 リクエストあたり 33MiB を確保させられていた。
func TestReadMultipartChunk_StopsAtTheSessionChunkSize(t *testing.T) {
	// 11 バイトの本体に 5 バイトの上限。
	c := newChunkMultipart(t, "hello world")
	_, err := readMultipartChunk(c, 5)
	require.ErrorIs(t, err, coredrive.ErrInvalidChunkSize)

	// 上限ちょうどは通る。
	c = newChunkMultipart(t, "hello")
	body, err := readMultipartChunk(c, 5)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(body))

	// 上限を決められないときは掛けない (判定は AppendChunk が行う)。
	c = newChunkMultipart(t, "hello world")
	body, err = readMultipartChunk(c, 0)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(body))
}

// **`FileHeader.Size` だけに頼らない。** あれはパーサが数えた値なので、
// 実際の読み込み量と食い違いうる (`readMultipartFile` と同じ理由)。
func TestReadMultipartChunk_LimitsEvenWhenHeaderSizeLies(t *testing.T) {
	prev := openMultipartFile
	t.Cleanup(func() { openMultipartFile = prev })
	openMultipartFile = func(*multipart.FileHeader) (multipart.File, error) {
		return &countingMultipartFile{data: []byte("hello world")}, nil
	}

	c := newChunkMultipart(t, "hello") // 申告は 5 バイト
	_, err := readMultipartChunk(c, 5)
	assert.ErrorIs(t, err, coredrive.ErrInvalidChunkSize)
}

// **申告が上限を超えていれば開かない。** 開いてから落とすと、その時点で
// パーサは既に全部受信している。
func TestReadMultipartChunk_DoesNotOpenWhenSizeExceeds(t *testing.T) {
	opened := false
	prev := openMultipartFile
	t.Cleanup(func() { openMultipartFile = prev })
	openMultipartFile = func(fh *multipart.FileHeader) (multipart.File, error) {
		opened = true
		return prev(fh)
	}

	c := newChunkMultipart(t, "hello world")
	_, err := readMultipartChunk(c, 5)
	require.ErrorIs(t, err, coredrive.ErrInvalidChunkSize)
	assert.False(t, opened, "上限超過が申告で分かるのに本体を開いている")
}

func newChunkMultipart(t *testing.T, content string) echo.Context {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	part, err := mw.CreateFormFile("chunk", "c")
	require.NoError(t, err)
	_, err = part.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.Header.Set(echo.HeaderContentType, mw.FormDataContentType())
	return e.NewContext(req, httptest.NewRecorder())
}

// **handler がセッションの上限を引いていること (#3037 レビュー)。**
//
// `readMultipartChunk` 側の判定が正しくても、handler が上限を渡さなければ
// 何も効かない。実際に読んだバイト数で見る — 応答だけだと `AppendChunk` が
// 落とした場合と区別が付かない。
func TestFilesCreateChunkedAppend_DoesNotReadPastTheSessionChunkSize(t *testing.T) {
	h, _, _ := newChunkedHandler(t)
	uploadID := startSession(t, h, apiChunkSize*4)

	var seen *countingMultipartFile
	prev := openMultipartFile
	t.Cleanup(func() { openMultipartFile = prev })
	openMultipartFile = func(*multipart.FileHeader) (multipart.File, error) {
		// 申告は上限内なのに、実体はその 4 倍を返す。
		seen = &countingMultipartFile{data: chunkBytes(apiChunkSize * 4)}
		return seen, nil
	}

	c, rec := newChunkReq(t, uploadID, 0, chunkBytes(apiChunkSize), false)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreateChunkedAppend(c))

	assert.NotEqual(t, http.StatusOK, rec.Code, "上限を超えたチャンクを受理している")
	require.NotNil(t, seen, "openMultipartFile が呼ばれていない")
	assert.LessOrEqual(t, seen.read, apiChunkSize+1,
		"セッションの上限を超えて読んでいる (読んだのは %d バイト)", seen.read)
}
