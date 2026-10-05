package drive

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errStub = errors.New("stub error")

func newHandler(t *testing.T) (*Handler, *testutil.MockDriveFileRepository, *testutil.MockDriveFolderRepository) {
	t.Helper()
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	folderRepo.FilesRef = fileRepo
	storage := coredrive.NewLocalStorage(t.TempDir(), "https://example.com/files")
	idGen, _ := id.NewGenerator("aidx")
	svc := coredrive.NewService(fileRepo, folderRepo, storage, idGen)
	return NewHandler(svc, idGen), fileRepo, folderRepo
}

func setUser(c echo.Context, id string) {
	c.Set(string(middleware.UserContextKey), &model.User{ID: id})
}

func newJSONReq(t *testing.T, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

// newMultipartReq builds a multipart form request with the given file body and form fields.
func newMultipartReq(t *testing.T, fileName, content string, fields map[string]string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if fileName != "" {
		part, err := mw.CreateFormFile("file", fileName)
		require.NoError(t, err)
		_, err = part.Write([]byte(content))
		require.NoError(t, err)
	}
	for k, v := range fields {
		require.NoError(t, mw.WriteField(k, v))
	}
	require.NoError(t, mw.Close())

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.Header.Set(echo.HeaderContentType, mw.FormDataContentType())
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

// --- FilesCreate ---

func TestFilesCreate_Success(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newMultipartReq(t, "hello.txt", "hello", nil)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFilesCreate_AllFormFields(t *testing.T) {
	h, _, folderRepo := newHandler(t)
	uid := "u1"
	folderRepo.Folders["fid"] = &model.DriveFolder{ID: "fid", UserID: &uid}
	c, rec := newMultipartReq(t, "hello.txt", "hello", map[string]string{
		"name":        "renamed.txt",
		"folderId":    "fid",
		"comment":     "this is a comment",
		"isSensitive": "true",
		"force":       "true",
	})
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "renamed.txt", resp["name"])
	assert.Equal(t, true, resp["isSensitive"])
	// upstream `pack(file, { self: true })` (= withUser=false) に整合し、
	// userId / user は null を返す (#812)。
	assert.Nil(t, resp["userId"])
	assert.Nil(t, resp["user"])
	// L3 (#1270): drive/files/create の実レスポンスを golden DriveFile に突合する。
	shapetest.Assert(t, "DriveFile", resp)
}

// TestFilesCreate_RecordsRequestIPAndHeaders guards #1148 root cause:
// upload 時点で `requestIp` / `requestHeaders` が drive_file row に記録
// されないと、後段の admin/drive/show-file がいくら field を返しても
// 中身が常に null になり IP タブが空表示になる。本 test では multipart
// upload 後に repo 内の row を直接覗いて両 column が populate されている
// ことを assert。
//
// 加えて: mk-go 独自 hardening (#1148) として `Authorization` / `Cookie`
// 等 credential 系 header は保存時に deny-list で除外される。これらが
// 含まれていないことも assert (DB dump 流出時の token 漏洩防止)。
func TestFilesCreate_RecordsRequestIPAndHeaders(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	// #2106 S4: requestIp/headers は enableIpLogging=true のときのみ記録される。
	mr := testutil.NewMockMetaRepository()
	mr.Meta = &model.Meta{ID: "x", EnableIPLogging: true}
	h.SetMetaRepo(mr)
	c, rec := newMultipartReq(t, "ip.txt", "hello", nil)
	// echo.Context の RealIP() は X-Forwarded-For / X-Real-IP / RemoteAddr
	// の順で resolve される。test では RemoteAddr が "192.0.2.1:1234" 形式
	// だが、明示的に X-Forwarded-For をセットして確実性を上げる。
	c.Request().Header.Set("X-Forwarded-For", "203.0.113.7")
	c.Request().Header.Set("X-Custom-Test", "marker-value")
	// credential 系 header (= deny-list で除外されるべき)。
	c.Request().Header.Set("Authorization", "Bearer secret-token-XYZ")
	c.Request().Header.Set("Cookie", "session=secret-session-id")
	c.Request().Header.Set("X-Api-Key", "secret-api-key")
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	require.Equal(t, http.StatusOK, rec.Code)

	// upload 結果の row を repo から拾って IP / Headers を確認。
	var got *model.DriveFile
	for _, f := range fileRepo.Files {
		got = f
		break
	}
	require.NotNil(t, got)
	require.NotNil(t, got.RequestIP, "requestIp が記録されていること")
	assert.Equal(t, "203.0.113.7", *got.RequestIP)
	require.NotEmpty(t, got.RequestHeaders, "requestHeaders が記録されていること")
	// jsonb を unmarshal して特定 header が含まれることを確認。
	var headers map[string]string
	require.NoError(t, json.Unmarshal(got.RequestHeaders, &headers))
	assert.Equal(t, "marker-value", headers["X-Custom-Test"])
	// 通常 header (X-Forwarded-For 等) は保存される。
	assert.NotEmpty(t, headers["X-Forwarded-For"])

	// credential 系は deny-list で除外され保存されない (mk-go 独自 hardening)。
	// Go の http.Header は MIME canonical で保存するので key は "Authorization"
	// (Title-Case)、deny-list は ToLower で比較するので一致する。
	assert.NotContains(t, headers, "Authorization", "Authorization は保存しない")
	assert.NotContains(t, headers, "Cookie", "Cookie は保存しない")
	assert.NotContains(t, headers, "X-Api-Key", "X-Api-Key は保存しない")
	// 念のため body 全体に secret token 文字列が残っていないことも確認
	// (= JSON marshal 経路で何らかの漏出を防ぐ defense-in-depth)。
	assert.NotContains(t, string(got.RequestHeaders), "secret-token-XYZ")
	assert.NotContains(t, string(got.RequestHeaders), "secret-session-id")
	assert.NotContains(t, string(got.RequestHeaders), "secret-api-key")
}

func TestFilesCreate_NoFile(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newMultipartReq(t, "", "", nil)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesCreate_FolderNotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newMultipartReq(t, "hello.txt", "hello", map[string]string{"folderId": "ghost"})
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 他人所有の宛先 folder は NO_SUCH_FOLDER(404) で返す (missing と同じ response、
// folder 存在 oracle を閉じる、#1908)。
func TestFilesCreate_OtherOwnerFolderNotFound(t *testing.T) {
	h, _, folderRepo := newHandler(t)
	other := "other"
	folderRepo.Folders["fid"] = &model.DriveFolder{ID: "fid", UserID: &other}
	c, rec := newMultipartReq(t, "hello.txt", "hello", map[string]string{"folderId": "fid"})
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FOLDER")
}

// failingFileRepo causes Create to fail.
type failingFileRepo struct {
	*testutil.MockDriveFileRepository
}

func (f *failingFileRepo) Create(_ *model.DriveFile) error {
	return errStub
}

func TestFilesCreate_RepoError(t *testing.T) {
	repo := &failingFileRepo{MockDriveFileRepository: testutil.NewMockDriveFileRepository()}
	folderRepo := testutil.NewMockDriveFolderRepository()
	idGen, _ := id.NewGenerator("aidx")
	storage := coredrive.NewLocalStorage(t.TempDir(), "")
	svc := coredrive.NewService(repo, folderRepo, storage, idGen)
	h := NewHandler(svc, idGen)

	c, rec := newMultipartReq(t, "hello.txt", "hello", nil)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- #1029 PR-2: drive policy handler-level mapping ---

// driveRoleStub は coredrive.RoleChecker を実装する最小 stub。
// moderator 判定は常に false、policies は固定 map を返す。
type driveRoleStub struct {
	policies map[string]any
}

func (s *driveRoleStub) IsModerator(_ string) bool               { return false }
func (s *driveRoleStub) GetUserPolicies(_ string) map[string]any { return s.policies }

func newHandlerWithPolicy(t *testing.T, policies map[string]any) (*Handler, *testutil.MockDriveFileRepository) {
	t.Helper()
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	folderRepo.FilesRef = fileRepo
	storage := coredrive.NewLocalStorage(t.TempDir(), "https://example.com/files")
	idGen, _ := id.NewGenerator("aidx")
	svc := coredrive.NewService(fileRepo, folderRepo, storage, idGen)
	svc.SetRoleChecker(&driveRoleStub{policies: policies})
	return NewHandler(svc, idGen), fileRepo
}

// maxFileSizeMb 超過は 400 MAX_FILE_SIZE_EXCEEDED で upstream UUID と完全一致。
func TestFilesCreate_MaxFileSizeExceeded(t *testing.T) {
	h, _ := newHandlerWithPolicy(t, map[string]any{"maxFileSizeMb": 1})
	// 2MB body (= 1MB cap 超過)
	body := strings.Repeat("a", 2*1024*1024)
	c, rec := newMultipartReq(t, "x.bin", body, nil)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Contains(t, rec.Body.String(), "MAX_FILE_SIZE_EXCEEDED")
	assert.Contains(t, rec.Body.String(), "b9d8c348-33f0-4673-b9a9-5d4da058977a")
}

// driveCapacityMb 超過 (= 既存 usage + 新 file > capacity) は 400 NO_FREE_SPACE。
func TestFilesCreate_NoFreeSpace(t *testing.T) {
	h, fileRepo := newHandlerWithPolicy(t, map[string]any{
		// size gate は余裕、capacity が tight
		"maxFileSizeMb":   100,
		"driveCapacityMb": 1,
	})
	owner := "u1"
	// 1MB 既存使用中 + 任意の新 file → capacity 超過
	fileRepo.Files["existing"] = &model.DriveFile{ID: "existing", UserID: &owner, Size: 1024 * 1024}
	c, rec := newMultipartReq(t, "x.txt", "small body content", nil)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_FREE_SPACE")
	assert.Contains(t, rec.Body.String(), "d08dbc37-a6a9-463a-8c47-96c32ab5f064")
}

// --- FilesShow ---

func TestFilesShow_Success(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Name: "hi"}
	c, rec := newJSONReq(t, `{"fileId":"f1"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesShow(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	shapetest.Assert(t, "DriveFile", resp) // L3 (#1312)
}

func TestFilesShow_InvalidParam(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesShow(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesShow_NotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"fileId":"ghost"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesShow(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesShow_AccessDenied(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	other := "other"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &other}
	c, rec := newJSONReq(t, `{"fileId":"f1"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesShow(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesShow_EmbedsFolderAndUser(t *testing.T) {
	h, fileRepo, folderRepo := newHandler(t)
	// folder/user/noteRepoを配線 (本番router.go相当)
	h.SetRepos(fileRepo, folderRepo, testutil.NewMockNoteRepository())
	userRepo := testutil.NewMockUserRepository()
	h.SetUserRepo(userRepo)

	uid := "u1"
	folderID := "fl1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Name: "hi", FolderID: &folderID}
	folderRepo.Folders[folderID] = &model.DriveFolder{ID: folderID, Name: "Pics", UserID: &uid}
	userRepo.Users[uid] = &model.User{ID: uid, Username: "alice"}

	c, rec := newJSONReq(t, `{"fileId":"f1"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesShow(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	folder, ok := resp["folder"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Pics", folder["name"])
	user, ok := resp["user"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "alice", user["username"])
}

// --- FilesUpdate ---

func TestFilesUpdate_Success(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Name: "old"}
	c, rec := newJSONReq(t, `{"fileId":"f1","name":"new","comment":"hi","isSensitive":true}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUpdate(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	// upstream `pack(file.id, { self: true })` (= self single) に整合し、
	// folder / userId / user は null を返す (#829 PR-A)。
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp["folder"])
	assert.Nil(t, resp["userId"])
	assert.Nil(t, resp["user"])
	shapetest.Assert(t, "DriveFile", resp) // L3 (#1312)
}

// #1769: comment:null で既存 comment をクリアできる。
func TestFilesUpdate_ClearsCommentWithNull(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	uid := "u1"
	existing := "old comment"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Name: "n", Comment: &existing}
	c, rec := newJSONReq(t, `{"fileId":"f1","comment":null}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUpdate(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Nil(t, fileRepo.Files["f1"].Comment, "comment:null で comment がクリアされる")
}

// #1769: comment 省略時は既存 comment を維持する (null クリアと区別)。
func TestFilesUpdate_OmittedCommentKept(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	uid := "u1"
	existing := "keep"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Name: "n", Comment: &existing}
	c, rec := newJSONReq(t, `{"fileId":"f1","name":"n2"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUpdate(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, fileRepo.Files["f1"].Comment)
	assert.Equal(t, "keep", *fileRepo.Files["f1"].Comment)
}

func TestFilesUpdate_InvalidParam(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUpdate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// folderId: null でルートへ移動できる (upstream paramDef は nullable)。
func TestFilesUpdate_NullFolderMovesToRoot(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	uid := "u1"
	folderID := "fid"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, FolderID: &folderID}
	c, rec := newJSONReq(t, `{"fileId":"f1","folderId":null}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUpdate(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFilesUpdate_SetFolder(t *testing.T) {
	h, fileRepo, folderRepo := newHandler(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid}
	folderRepo.Folders["fid"] = &model.DriveFolder{ID: "fid", UserID: &uid}
	c, rec := newJSONReq(t, `{"fileId":"f1","folderId":"fid"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUpdate(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFilesUpdate_FileNotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"fileId":"ghost"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUpdate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesUpdate_FolderNotFound(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid}
	c, rec := newJSONReq(t, `{"fileId":"f1","folderId":"ghost"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUpdate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 他人所有の宛先 folder は missing と同じ NO_SUCH_FOLDER(404) を返す (upstream
// files/update の NoSuchFolderError ea8fb7a5、folder 存在 oracle を閉じる、#1908)。
func TestFilesUpdate_OtherOwnerFolderNotFound(t *testing.T) {
	h, fileRepo, folderRepo := newHandler(t)
	uid := "u1"
	other := "other"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid}
	folderRepo.Folders["fid"] = &model.DriveFolder{ID: "fid", UserID: &other}
	c, rec := newJSONReq(t, `{"fileId":"f1","folderId":"fid"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUpdate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FOLDER")
	assert.Contains(t, rec.Body.String(), "ea8fb7a5-af77-4a08-b608-c0218176cd73")
}

// failingUpdateFileRepo causes Update to fail with non-mapped error.
type failingUpdateFileRepo struct {
	*testutil.MockDriveFileRepository
}

func (f *failingUpdateFileRepo) Update(_ string, _ map[string]any) error {
	return errStub
}

func TestFilesUpdate_RepoError(t *testing.T) {
	mock := testutil.NewMockDriveFileRepository()
	uid := "u1"
	mock.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid}
	repo := &failingUpdateFileRepo{MockDriveFileRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := coredrive.NewService(repo, testutil.NewMockDriveFolderRepository(), coredrive.NewLocalStorage(t.TempDir(), ""), idGen)
	h := NewHandler(svc, idGen)

	c, rec := newJSONReq(t, `{"fileId":"f1","name":"x"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUpdate(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- FilesDelete ---

func TestFilesDelete_Success(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid}
	c, rec := newJSONReq(t, `{"fileId":"f1"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesDelete(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestFilesDelete_InvalidParam(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesDelete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesDelete_NotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"fileId":"ghost"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesDelete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- FilesFindByHash ---

func TestFilesFindByHash_Success(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, MD5: "abc"}
	c, rec := newJSONReq(t, `{"md5":"abc"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesFindByHash(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	// upstream `packMany(files, { self: true })` 経路 (#818): folder / user
	// は null、userId は owner ID 維持。
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "u1", resp[0]["userId"])
	assert.Nil(t, resp[0]["folder"])
	assert.Nil(t, resp[0]["user"])
}

// upstream find-by-hash は md5 {type:'string'} で空文字も valid。空 md5 は 0 件
// 一致で 200 + [] を返す (INVALID_PARAM で弾かない、#1831)。
func TestFilesFindByHash_EmptyMD5ReturnsEmpty(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesFindByHash(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())
}

func TestFilesFindByHash_NotFound(t *testing.T) {
	// upstream find-by-hash は不一致でも 404 ではなく空配列 200 (#1564)。
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"md5":"ghost"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesFindByHash(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())
}

// --- FoldersCreate ---

func TestFoldersCreate_Success(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"name":"My Folder"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersCreate(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	shapetest.Assert(t, "DriveFolder", resp) // L3 (#1312)
}

func TestFoldersCreate_DefaultName(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersCreate(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "Untitled", resp["name"])
}

// #1948-16: explicit な空文字 name は 'Untitled' に置換せずそのまま保持する
// (upstream paramDef は default を absent 時のみ適用、minLength 無し)。
func TestFoldersCreate_ExplicitEmptyNameKept(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"name":""}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersCreate(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "", resp["name"], "explicit \"\" は Untitled に置換しない (#1948-16)")
}

// #1948-16: move-bulk は fileIds の maxItems:100 / uniqueItems を強制する。
func TestFilesMoveBulk_TooManyAndDuplicate(t *testing.T) {
	h, _, _ := newHandler(t)
	// 101 件 → 400。
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = "f" + timeSuffixDrive(i)
	}
	body, _ := json.Marshal(map[string]any{"fileIds": ids})
	c, rec := newJSONReq(t, string(body))
	setUser(c, "u1")
	require.NoError(t, h.FilesMoveBulk(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "maxItems:100 超過は 400 (#1948-16)")

	// 重複 → 400。
	c, rec = newJSONReq(t, `{"fileIds":["a","a"]}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesMoveBulk(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "uniqueItems 違反は 400 (#1948-16)")
}

func timeSuffixDrive(i int) string {
	return time.Date(2025, 1, 1, 0, 0, i, 0, time.UTC).Format("20060102150405")
}

func TestFoldersCreate_InvalidJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{invalid`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFoldersCreate_ParentNotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"name":"x","parentId":"ghost"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// #977: folders/create の parent 不在 UUID は 53326628-...
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "NO_SUCH_FOLDER", errObj["code"])
	assert.Equal(t, "53326628-a00d-40a6-a3cd-8975105c0f95", errObj["id"])
}

// 他人所有 parent は upstream folders/create 同様 NO_SUCH_FOLDER(404)、
// ACCESS_DENIED(403) にしない (#1831)。
func TestFoldersCreate_ParentOtherOwnerNotFound(t *testing.T) {
	h, _, folderRepo := newHandler(t)
	other := "other"
	folderRepo.Folders["p"] = &model.DriveFolder{ID: "p", UserID: &other}
	c, rec := newJSONReq(t, `{"name":"x","parentId":"p"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FOLDER")
}

// failingFolderRepo causes Create to fail
type failingFolderRepo struct {
	*testutil.MockDriveFolderRepository
}

func (f *failingFolderRepo) Create(_ *model.DriveFolder) error {
	return errStub
}

func TestFoldersCreate_RepoError(t *testing.T) {
	repo := &failingFolderRepo{MockDriveFolderRepository: testutil.NewMockDriveFolderRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := coredrive.NewService(testutil.NewMockDriveFileRepository(), repo, coredrive.NewLocalStorage(t.TempDir(), ""), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newJSONReq(t, `{"name":"x"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersCreate(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- FoldersShow ---

func TestFoldersShow_Success(t *testing.T) {
	h, _, folderRepo := newHandlerWithRepos(t)
	uid := "u1"
	folderRepo.Folders["p"] = &model.DriveFolder{ID: "p", Name: "Test", UserID: &uid}
	c, rec := newJSONReq(t, `{"folderId":"p"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersShow(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	// upstream `pack(folder, { detail: true })` (#845): foldersCount /
	// filesCount は 0 (= 子なし)、parent は parentId=nil なので nil。
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.EqualValues(t, 0, resp["foldersCount"])
	assert.EqualValues(t, 0, resp["filesCount"])
	assert.Nil(t, resp["parent"])
	shapetest.Assert(t, "DriveFolder", resp) // L3 (#1312)
}

// detail mode: 親 folder + 子 folder + 子内の file を持つ shape の確認 (#845)。
func TestFoldersShow_DetailWithChildren(t *testing.T) {
	h, fileRepo, folderRepo := newHandlerWithRepos(t)
	uid := "u1"
	parentID := "p"
	childID := "c"
	folderRepo.Folders[parentID] = &model.DriveFolder{ID: parentID, Name: "P", UserID: &uid}
	folderRepo.Folders[childID] = &model.DriveFolder{ID: childID, Name: "C", UserID: &uid, ParentID: &parentID}
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, FolderID: &parentID}
	fileRepo.Files["f2"] = &model.DriveFile{ID: "f2", UserID: &uid, FolderID: &parentID}

	c, rec := newJSONReq(t, `{"folderId":"`+parentID+`"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersShow(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.EqualValues(t, 1, resp["foldersCount"])
	assert.EqualValues(t, 2, resp["filesCount"])
	assert.Nil(t, resp["parent"])

	// 子 folder show: parent (= 親) が recursive に埋まる。
	c2, rec2 := newJSONReq(t, `{"folderId":"`+childID+`"}`)
	setUser(c2, "u1")
	require.NoError(t, h.FoldersShow(c2))
	require.Equal(t, http.StatusOK, rec2.Code)
	var resp2 map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	parent, ok := resp2["parent"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, parentID, parent["id"])
	assert.EqualValues(t, 1, parent["foldersCount"])
}

func TestFoldersShow_InvalidParam(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersShow(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFoldersShow_NotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"folderId":"ghost"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersShow(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// #977: folders/show の target 不在 UUID は d74ab9eb-...
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "NO_SUCH_FOLDER", errObj["code"])
	assert.Equal(t, "d74ab9eb-bb09-4bba-bf24-fb58f761e1e9", errObj["id"])
}

// 他人所有 folder は upstream folders/show 同様 NO_SUCH_FOLDER(404)、
// ACCESS_DENIED(403) にしない (folder ID 存在 oracle 防止、#1831)。
func TestFoldersShow_OtherOwnerNotFound(t *testing.T) {
	h, _, folderRepo := newHandler(t)
	other := "other"
	folderRepo.Folders["p"] = &model.DriveFolder{ID: "p", UserID: &other}
	c, rec := newJSONReq(t, `{"folderId":"p"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersShow(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FOLDER")
}

// --- FoldersUpdate ---

func TestFoldersUpdate_Success(t *testing.T) {
	h, _, folderRepo := newHandler(t)
	uid := "u1"
	folderRepo.Folders["p"] = &model.DriveFolder{ID: "p", Name: "Old", UserID: &uid}
	c, rec := newJSONReq(t, `{"folderId":"p","name":"New"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersUpdate(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFoldersUpdate_InvalidParam(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersUpdate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// parentId: null で親を外せる。
func TestFoldersUpdate_NullParentDetaches(t *testing.T) {
	h, _, folderRepo := newHandler(t)
	uid := "u1"
	pid := "p"
	folderRepo.Folders["c"] = &model.DriveFolder{ID: "c", UserID: &uid, ParentID: &pid}
	c, rec := newJSONReq(t, `{"folderId":"c","parentId":null}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersUpdate(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFoldersUpdate_SetParent(t *testing.T) {
	h, _, folderRepo := newHandler(t)
	uid := "u1"
	folderRepo.Folders["p"] = &model.DriveFolder{ID: "p", UserID: &uid}
	folderRepo.Folders["c"] = &model.DriveFolder{ID: "c", UserID: &uid}
	c, rec := newJSONReq(t, `{"folderId":"c","parentId":"p"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersUpdate(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFoldersUpdate_NotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"folderId":"ghost"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersUpdate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// #977: folders/update target not found UUID は f7974dac-...
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "NO_SUCH_FOLDER", errObj["code"])
	assert.Equal(t, "f7974dac-2c0d-4a27-926e-23583b28e98e", errObj["id"])
}

// #977: folders/update で parent が未存在のときに NO_SUCH_PARENT_FOLDER
// (UUID ce104e3a-...) を返すこと。ErrParentFolderNotFound branch のカバー。
func TestFoldersUpdate_ParentNotFound(t *testing.T) {
	h, _, folderRepo := newHandler(t)
	uid := "u1"
	folderRepo.Folders["c"] = &model.DriveFolder{ID: "c", UserID: &uid}
	c, rec := newJSONReq(t, `{"folderId":"c","parentId":"ghost"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersUpdate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "NO_SUCH_PARENT_FOLDER", errObj["code"])
	assert.Equal(t, "ce104e3a-faaf-49d5-b459-10ff0cbbcaa1", errObj["id"])
}

// --- FoldersDelete ---

func TestFoldersDelete_Success(t *testing.T) {
	h, _, folderRepo := newHandler(t)
	uid := "u1"
	folderRepo.Folders["p"] = &model.DriveFolder{ID: "p", UserID: &uid}
	c, rec := newJSONReq(t, `{"folderId":"p"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersDelete(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestFoldersDelete_InvalidParam(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersDelete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFoldersDelete_NotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"folderId":"ghost"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersDelete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// #977: folders/delete の UUID は 1069098f-...
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "NO_SUCH_FOLDER", errObj["code"])
	assert.Equal(t, "1069098f-c281-440f-b085-f9932edbe091", errObj["id"])
}

func TestFoldersDelete_NotEmpty(t *testing.T) {
	h, fileRepo, folderRepo := newHandler(t)
	uid := "u1"
	folderRepo.Folders["p"] = &model.DriveFolder{ID: "p", UserID: &uid}
	pid := "p"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, FolderID: &pid}
	c, rec := newJSONReq(t, `{"folderId":"p"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersDelete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- New drive listing endpoints ---

func newHandlerWithRepos(t *testing.T) (*Handler, *testutil.MockDriveFileRepository, *testutil.MockDriveFolderRepository) {
	t.Helper()
	h, fileRepo, folderRepo := newHandler(t)
	h.SetRepos(fileRepo, folderRepo, testutil.NewMockNoteRepository())
	return h, fileRepo, folderRepo
}

func TestUsage_Success(t *testing.T) {
	h, fileRepo, _ := newHandlerWithRepos(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Size: 1024}
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.Usage(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUsage_NilRepo(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.Usage(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFilesList_Success(t *testing.T) {
	h, fileRepo, _ := newHandlerWithRepos(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Name: "test.png"}
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesList(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFilesList_NilRepo(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesList(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFilesFind_Success(t *testing.T) {
	h, fileRepo, _ := newHandlerWithRepos(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Name: "test.png"}
	c, rec := newJSONReq(t, `{"name":"test.png"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesFind(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	// upstream `packMany(files, { self: true })` 経路 (#818): folder / user
	// は null、userId は owner ID 維持。
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "u1", resp[0]["userId"])
	assert.Nil(t, resp[0]["folder"])
	assert.Nil(t, resp[0]["user"])
}

// upstream files/find は name {type:'string'} で空文字も valid。空 name は 0 件
// 一致で 200 + [] を返す (INVALID_PARAM で弾かない、#1831)。
func TestFilesFind_EmptyNameReturnsEmpty(t *testing.T) {
	h, _, _ := newHandlerWithRepos(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesFind(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())
}

func TestFilesFind_NilRepo(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"name":"x"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesFind(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFilesCheckExistence_True(t *testing.T) {
	h, fileRepo, _ := newHandlerWithRepos(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, MD5: "abc123"}
	c, rec := newJSONReq(t, `{"md5":"abc123"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCheckExistence(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "true")
}

func TestFilesCheckExistence_False(t *testing.T) {
	h, _, _ := newHandlerWithRepos(t)
	c, rec := newJSONReq(t, `{"md5":"ghost"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCheckExistence(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "false")
}

func TestFilesCheckExistence_InvalidParam(t *testing.T) {
	h, _, _ := newHandlerWithRepos(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCheckExistence(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesCheckExistence_NilRepo(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"md5":"x"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCheckExistence(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFilesAttachedNotes_Success(t *testing.T) {
	h, fileRepo, _ := newHandlerWithRepos(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid}
	c, rec := newJSONReq(t, `{"fileId":"f1"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesAttachedNotes(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFilesAttachedNotes_InvalidParam(t *testing.T) {
	h, _, _ := newHandlerWithRepos(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesAttachedNotes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// fileRepo 未配線時は fail-closed (= NO_SUCH_FILE 400)。旧実装は ListByFileID
// を直に叩いて 200 を返していたが、IDOR (#1470) 防止のため file ownership 検証
// 経路を必ず通すこと自体を要求に格上げした。
func TestFilesAttachedNotes_NilRepo(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"fileId":"f1"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesAttachedNotes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
	// upstream Misskey TS attached-notes.ts と同じ UUID。汎用 NoSuchFile
	// (UUIDNoSuchFile) と区別するために値比較しておく。
	assert.Contains(t, rec.Body.String(), "c118ece3-2e4b-4296-99d1-51756e32d232")
}

// 他人 owner の fileID を投げると NO_SUCH_FILE で隠蔽する (#1470)。旧実装は
// file ownership 検証なしに ListByFileID を叩いていたため、認証 viewer が
// 他人 owner の fileID を投げて該当 file を attach した followers / specified
// note の本文を列挙できる IDOR が成立していた。
func TestFilesAttachedNotes_OtherOwnerDenied(t *testing.T) {
	h, fileRepo, _ := newHandlerWithRepos(t)
	other := "u2"
	fileRepo.Files["f-other"] = &model.DriveFile{ID: "f-other", UserID: &other}
	c, rec := newJSONReq(t, `{"fileId":"f-other"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesAttachedNotes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
	// upstream Misskey TS attached-notes.ts と同じ UUID。汎用 NoSuchFile
	// (UUIDNoSuchFile) と区別するために値比較しておく。
	assert.Contains(t, rec.Body.String(), "c118ece3-2e4b-4296-99d1-51756e32d232")
}

// system file (userId IS NULL — emoji copy / import zip 用、#670) も viewer の
// 所有では無いので NO_SUCH_FILE。owner check に NULL handling を残さないと
// `*f.UserID` で nil pointer panic を起こすので noregress test も兼ねる。
func TestFilesAttachedNotes_SystemFileDenied(t *testing.T) {
	h, fileRepo, _ := newHandlerWithRepos(t)
	fileRepo.Files["f-sys"] = &model.DriveFile{ID: "f-sys", UserID: nil}
	c, rec := newJSONReq(t, `{"fileId":"f-sys"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesAttachedNotes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
	// upstream Misskey TS attached-notes.ts と同じ UUID。汎用 NoSuchFile
	// (UUIDNoSuchFile) と区別するために値比較しておく。
	assert.Contains(t, rec.Body.String(), "c118ece3-2e4b-4296-99d1-51756e32d232")
}

// 不存在 fileID も NO_SUCH_FILE で同じ shape に集約 (file 存在判定 oracle を
// 塞ぐ; 他人 owner 判定との timing oracle にもならないように同経路に揃える)。
func TestFilesAttachedNotes_NotFound(t *testing.T) {
	h, _, _ := newHandlerWithRepos(t)
	c, rec := newJSONReq(t, `{"fileId":"nonexistent"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesAttachedNotes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
	// upstream Misskey TS attached-notes.ts と同じ UUID。汎用 NoSuchFile
	// (UUIDNoSuchFile) と区別するために値比較しておく。
	assert.Contains(t, rec.Body.String(), "c118ece3-2e4b-4296-99d1-51756e32d232")
}

// frontend Paginator から渡される sinceId / untilId / limit が
// noteRepo.ListByFileID に正しく forward されることを確認 (#488 回帰
// 防止)。bind 漏れがあると同じ note が無限スクロールで何度も返る。
type spyNoteRepo struct {
	*testutil.MockNoteRepository
	gotFileID  string
	gotSinceID string
	gotUntilID string
	gotLimit   int
}

func (s *spyNoteRepo) ListByFileID(fileID, sinceID, untilID string, limit int) ([]*model.Note, error) {
	s.gotFileID = fileID
	s.gotSinceID = sinceID
	s.gotUntilID = untilID
	s.gotLimit = limit
	return nil, nil
}

func TestFilesAttachedNotes_ForwardsCursorParams(t *testing.T) {
	h, fileRepo, folderRepo := newHandler(t)
	spy := &spyNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
	h.SetRepos(fileRepo, folderRepo, spy)
	// #1470 IDOR fix: handler が ListByFileID を呼ぶ前に viewer 所有 file の
	// 存在を要求するようになったので、cursor forward test も該当 file を
	// 用意してから経路を通す。
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid}

	c, rec := newJSONReq(t, `{"fileId":"f1","untilId":"u_xxx","limit":7}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesAttachedNotes(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "f1", spy.gotFileID)
	assert.Equal(t, "u_xxx", spy.gotUntilID)
	assert.Equal(t, "", spy.gotSinceID)
	assert.Equal(t, 7, spy.gotLimit)
}

func TestFilesUploadFromURL(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"url":"https://example.com/img.png"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUploadFromURL(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestFilesMoveBulk_Success(t *testing.T) {
	h, fileRepo, _ := newHandlerWithRepos(t)
	c, rec := newJSONReq(t, `{"fileIds":["f1","f2"]}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesMoveBulk(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	// IDOR guard: handler は必ず呼出ユーザーの ID を repo に渡す。
	assert.Equal(t, "u1", fileRepo.BulkFolderUserID)
	assert.Equal(t, []string{"f1", "f2"}, fileRepo.BulkFolderFileIDs)
}

func TestFilesMoveBulk_RejectsForeignFolder(t *testing.T) {
	h, fileRepo, folderRepo := newHandlerWithRepos(t)
	owner := "someoneElse"
	folderRepo.Folders["folderX"] = &model.DriveFolder{ID: "folderX", UserID: &owner}
	c, rec := newJSONReq(t, `{"fileIds":["f1"],"folderId":"folderX"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesMoveBulk(c))
	// 他人所有の宛先 folder は NO_SUCH_FOLDER で拒否し、移動は実行しない。
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, fileRepo.BulkFolderUserID, "must not move files into a foreign folder")
}

func TestFilesMoveBulk_AcceptsOwnFolder(t *testing.T) {
	h, fileRepo, folderRepo := newHandlerWithRepos(t)
	owner := "u1"
	folderRepo.Folders["folderOwn"] = &model.DriveFolder{ID: "folderOwn", UserID: &owner}
	c, rec := newJSONReq(t, `{"fileIds":["f1"],"folderId":"folderOwn"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesMoveBulk(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "u1", fileRepo.BulkFolderUserID)
}

func TestFilesMoveBulk_NonexistentFolder(t *testing.T) {
	h, fileRepo, _ := newHandlerWithRepos(t)
	c, rec := newJSONReq(t, `{"fileIds":["f1"],"folderId":"ghost"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesMoveBulk(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, fileRepo.BulkFolderUserID, "must not move into a nonexistent folder")
}

// folderId 未指定 (root への移動) は folder 検証をスキップして移動する。
func TestFilesMoveBulk_RootMoveSkipsFolderCheck(t *testing.T) {
	h, fileRepo, _ := newHandlerWithRepos(t)
	c, rec := newJSONReq(t, `{"fileIds":["f1"]}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesMoveBulk(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "u1", fileRepo.BulkFolderUserID)
}

func TestFilesMoveBulk_InvalidParam(t *testing.T) {
	h, _, _ := newHandlerWithRepos(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesMoveBulk(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesMoveBulk_NilRepo(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"fileIds":["f1"]}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesMoveBulk(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestStream_Success(t *testing.T) {
	h, _, _ := newHandlerWithRepos(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.Stream(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestStream_NilRepo(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.Stream(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFoldersList_Success(t *testing.T) {
	h, _, folderRepo := newHandlerWithRepos(t)
	uid := "u1"
	folderRepo.Folders["d1"] = &model.DriveFolder{ID: "d1", UserID: &uid, Name: "photos"}
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersList(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFoldersList_NilRepo(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersList(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFoldersFind_Success(t *testing.T) {
	h, _, folderRepo := newHandlerWithRepos(t)
	uid := "u1"
	folderRepo.Folders["d1"] = &model.DriveFolder{ID: "d1", UserID: &uid, Name: "photos"}
	c, rec := newJSONReq(t, `{"name":"photos"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersFind(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// upstream folders/find は name {type:'string'} で空文字も valid。空 name は
// 0 件一致で 200 + [] を返す (INVALID_PARAM で弾かない、#1831)。
func TestFoldersFind_EmptyNameReturnsEmpty(t *testing.T) {
	h, _, _ := newHandlerWithRepos(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersFind(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())
}

func TestFoldersFind_NilRepo(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newJSONReq(t, `{"name":"x"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersFind(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #2106 L14: folderId:"" は root (folderId IS NULL) として扱う (0 件返却にしない)。
func TestFilesList_EmptyFolderIDTreatedAsRoot(t *testing.T) {
	h, fileRepo, _ := newHandlerWithRepos(t)
	uid := "u1"
	// root (FolderID=nil) の file。
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Name: "root.png"}
	c, rec := newJSONReq(t, `{"folderId":""}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesList(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	// "" → nil 正規化により root の file が返る。
	assert.Contains(t, rec.Body.String(), "root.png")
}

// **列に入らないカーソルは 400 (#3025)。** NUL を含む値は `id < ?` の bind
// parameter に載せた時点で PostgreSQL が落とすので、そのまま repository へ渡すと
// 500 になる。**JSON では NUL をエスケープで送る** — 生バイトを body に載せると
// JSON として不正になり、`Bind` の 400 で guard まで届かない。
func TestCursorGuardRejectsUnstorableCursor(t *testing.T) {
	const body = `{"untilId":"a\u0000b"}`
	for _, tt := range []struct {
		name string
		fn   func(h *Handler) func(echo.Context) error
	}{
		{"FilesList", func(h *Handler) func(echo.Context) error { return h.FilesList }},
		{"FilesAttachedNotes", func(h *Handler) func(echo.Context) error { return h.FilesAttachedNotes }},
		{"Stream", func(h *Handler) func(echo.Context) error { return h.Stream }},
		{"FoldersList", func(h *Handler) func(echo.Context) error { return h.FoldersList }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, _, _ := newHandlerWithRepos(t)
			c, rec := newJSONReq(t, body)
			setUser(c, "alice")
			require.NoError(t, tt.fn(h)(c))
			assert.Equal(t, http.StatusBadRequest, rec.Code,
				"列に入らないカーソルを repository へ渡している (SELECT がそこで落ちる)")
			assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
		})
	}
}

// --- multipart の error を捨てない (#3037) ---

// failingMultipartFile yields n bytes then fails, standing in for a temp file
// that becomes unreadable mid-upload (disk full / removed / I/O error).
type failingMultipartFile struct {
	data []byte
	pos  int
	stop int
	err  error
}

func (f *failingMultipartFile) Read(p []byte) (int, error) {
	if f.pos >= f.stop {
		return 0, f.err
	}
	n := copy(p, f.data[f.pos:f.stop])
	f.pos += n
	return n, nil
}
func (f *failingMultipartFile) ReadAt([]byte, int64) (int, error) { return 0, f.err }
func (f *failingMultipartFile) Seek(int64, int) (int64, error)    { return 0, f.err }
func (f *failingMultipartFile) Close() error                      { return nil }

// **`Open` の失敗を捨てない。** 以前は `src, _ :=` と書いていたので、
// `src` が nil のまま `defer src.Close()` に落ちて **nil 参照 panic** していた。
// echo の multipart パーサは `maxMemory` を超えた分を一時ファイルへ落とすので、
// `Open` は実ファイルを開く = 失敗しうる。
func TestReadMultipartFile_OpenFailureIsReported(t *testing.T) {
	prev := openMultipartFile
	t.Cleanup(func() { openMultipartFile = prev })
	openMultipartFile = func(*multipart.FileHeader) (multipart.File, error) {
		return nil, errors.New("disk is gone")
	}

	c, _ := newMultipartReq(t, "hello.txt", "hello", nil)
	require.NotPanics(t, func() {
		body, name, err := readMultipartFile(c, 0)
		assert.Error(t, err, "Open の失敗を握り潰している")
		assert.Nil(t, body)
		assert.Empty(t, name)
	})
}

// **`ReadAll` の途中失敗を捨てない。** 以前は `body, _ :=` と書いていたので、
// **無言で切り詰められた本体**がそのまま保存され、MD5 / size / MIME がその
// 切れ端で確定していた (利用者には成功として返る)。
func TestReadMultipartFile_ReadFailureIsReported(t *testing.T) {
	prev := openMultipartFile
	t.Cleanup(func() { openMultipartFile = prev })
	openMultipartFile = func(*multipart.FileHeader) (multipart.File, error) {
		return &failingMultipartFile{
			data: []byte("hello world"),
			stop: 5, // 5 バイト読めた時点で失敗する
			err:  errors.New("input/output error"),
		}, nil
	}

	c, _ := newMultipartReq(t, "hello.txt", "hello world", nil)
	body, _, err := readMultipartFile(c, 0)
	require.Error(t, err, "途中で切れた本体を成功として返している")
	assert.Nil(t, body, "切れ端を返している")
}

// 切れ端が保存されないこと、**かつ 5xx で返ること**を handler まで通して見る。
//
// `Open` / `ReadAll` の失敗はディスク満杯 / fd 枯渇 / 一時ファイル消失で、
// 全部サーバー側の事象。`INVALID_PARAM` に潰すと利用者もフロントも再試行せず、
// 監視にも 4xx しか出ないので障害が見えない (#2792)。
func TestFilesCreate_ReadFailureDoesNotStoreTruncatedFile(t *testing.T) {
	prev := openMultipartFile
	t.Cleanup(func() { openMultipartFile = prev })
	openMultipartFile = func(*multipart.FileHeader) (multipart.File, error) {
		return &failingMultipartFile{
			data: []byte("hello world"),
			stop: 5,
			err:  errors.New("input/output error"),
		}, nil
	}

	h, fileRepo, _ := newHandler(t)
	c, rec := newMultipartReq(t, "hello.txt", "hello world", nil)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"サーバー側の I/O 障害をクライアントエラーに潰している")
	assert.Empty(t, fileRepo.Files, "切れ端が保存されている")
}

// **普通のアップロードは通ったまま。** これが無いと「常にエラーを返す」実装でも
// 上のテストが通る。
func TestReadMultipartFile_OrdinaryUploadStillReads(t *testing.T) {
	c, _ := newMultipartReq(t, "hello.txt", "hello world", nil)
	body, name, err := readMultipartFile(c, 0)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(body))
	assert.Equal(t, "hello.txt", name)
}

// **`file` フィールドが無いのは本当にクライアント起因なので 400 のまま。**
// これが無いと「multipart の失敗を全部 500 にする」実装でも上のテストが通る。
func TestFilesCreate_MissingFileFieldStays400(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	c, rec := newMultipartReq(t, "", "", map[string]string{"name": "x"})
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, fileRepo.Files)
}

// roleStub は maxFileSizeMb policy だけを返す最小の RoleChecker。
type roleStub struct {
	maxFileSizeMb any
}

func (roleStub) IsModerator(string) bool { return false }

func (r roleStub) GetUserPolicies(string) map[string]any {
	return map[string]any{"maxFileSizeMb": r.maxFileSizeMb}
}

// **policy の上限は読み切る前に効くこと (#3037)。**
//
// 既定では `maxFileSizeMb` が 30MB なのに `config.maxFileSize` が 250MB
// なので、判定が `Upload` の中だけだと「30MB しか保存できない利用者が
// 250MB を送り付けてメモリを確保させる」ことができた。
func TestReadMultipartFile_StopsAtPolicyLimit(t *testing.T) {
	// 11 バイトの本体に 5 バイトの上限。
	c, _ := newMultipartReq(t, "hello.txt", "hello world", nil)
	body, _, err := readMultipartFile(c, 5)
	require.ErrorIs(t, err, coredrive.ErrMaxFileSizeExceeded)
	assert.Nil(t, body, "上限を超えた本体を返している")

	// 上限ちょうどは通る。
	c, _ = newMultipartReq(t, "hello.txt", "hello", nil)
	body, _, err = readMultipartFile(c, 5)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(body))
}

// **`FileHeader.Size` だけに頼らない。** あれはパーサが数えた値なので、
// 実際の読み込み量と食い違いうる。上限を強制するのは実際に読む側。
func TestReadMultipartFile_LimitsEvenWhenHeaderSizeLies(t *testing.T) {
	prev := openMultipartFile
	t.Cleanup(func() { openMultipartFile = prev })
	// FileHeader.Size は 5 のまま、実体は 11 バイト返す。
	openMultipartFile = func(*multipart.FileHeader) (multipart.File, error) {
		return &failingMultipartFile{data: []byte("hello world"), stop: 11, err: io.EOF}, nil
	}

	c, _ := newMultipartReq(t, "hello.txt", "hello", nil)
	_, _, err := readMultipartFile(c, 5)
	require.ErrorIs(t, err, coredrive.ErrMaxFileSizeExceeded)
}

// handler まで通して 413 になること。**`Upload` が返すのと同じ応答**なので、
// 読む前に落ちたか後で落ちたかで挙動は変わらない。
func TestFilesCreate_PolicyLimitReturns413(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	h.svc.SetRoleChecker(roleStub{maxFileSizeMb: 0.000001}) // 約 1 バイト

	c, rec := newMultipartReq(t, "hello.txt", "hello world", nil)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Contains(t, rec.Body.String(), "MAX_FILE_SIZE_EXCEEDED")
	assert.Empty(t, fileRepo.Files, "上限を超えたファイルが保存されている")
}

// **普通のアップロードは通ったまま。** これが無いと「常に 413 を返す」実装でも
// 上のテストが通る。
func TestFilesCreate_UnderPolicyLimitStillUploads(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	h.svc.SetRoleChecker(roleStub{maxFileSizeMb: 30})

	c, rec := newMultipartReq(t, "hello.txt", "hello world", nil)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, fileRepo.Files, 1)
}

// countingMultipartFile records how many bytes were actually read.
type countingMultipartFile struct {
	data []byte
	pos  int
	read int
}

func (f *countingMultipartFile) Read(p []byte) (int, error) {
	if f.pos >= len(f.data) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.pos:])
	f.pos += n
	f.read += n
	return n, nil
}
func (f *countingMultipartFile) ReadAt([]byte, int64) (int, error) { return 0, io.EOF }
func (f *countingMultipartFile) Seek(int64, int) (int64, error)    { return 0, nil }
func (f *countingMultipartFile) Close() error                      { return nil }

// **上限を超える本体を「最後まで読んでから」落としていないこと (#3037)。**
//
// 応答だけを見ると、読む前に落としても `Upload` が落としても同じ 413 に
// なるので区別が付かない。**実際に読んだバイト数**を見る。
func TestFilesCreate_DoesNotReadPastPolicyLimit(t *testing.T) {
	const size = 64 * 1024

	// **申告どおりの大きさなら、そもそも開かない。** `FileHeader.Size` で
	// 落とせる早い枝。
	t.Run("申告が正しければ開きもしない", func(t *testing.T) {
		opened := false
		prev := openMultipartFile
		t.Cleanup(func() { openMultipartFile = prev })
		openMultipartFile = func(fh *multipart.FileHeader) (multipart.File, error) {
			opened = true
			return prev(fh)
		}

		h, fileRepo, _ := newHandler(t)
		h.svc.SetRoleChecker(roleStub{maxFileSizeMb: 1.0 / 1024}) // 1KiB

		c, rec := newMultipartReq(t, "big.bin", strings.Repeat("x", size), nil)
		setUser(c, "u1")
		require.NoError(t, h.FilesCreate(c))

		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		assert.Empty(t, fileRepo.Files)
		assert.False(t, opened, "上限超過が申告で分かるのに本体を開いている")
	})

	// **申告が嘘でも、読むのは上限 +1 バイトまで。**
	t.Run("申告が嘘でも読み切らない", func(t *testing.T) {
		var seen *countingMultipartFile
		prev := openMultipartFile
		t.Cleanup(func() { openMultipartFile = prev })
		openMultipartFile = func(*multipart.FileHeader) (multipart.File, error) {
			seen = &countingMultipartFile{data: bytes.Repeat([]byte("x"), size)}
			return seen, nil
		}

		h, fileRepo, _ := newHandler(t)
		h.svc.SetRoleChecker(roleStub{maxFileSizeMb: 1.0 / 1024}) // 1KiB

		// 申告は 5 バイト (上限内) なので Size の枝は通り抜ける。
		c, rec := newMultipartReq(t, "small.bin", "hello", nil)
		setUser(c, "u1")
		require.NoError(t, h.FilesCreate(c))

		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		assert.Empty(t, fileRepo.Files)
		require.NotNil(t, seen, "openMultipartFile が呼ばれていない")
		assert.LessOrEqual(t, seen.read, 1024+1,
			"上限を超えた本体を最後まで読んでいる (読んだのは %d バイト)", seen.read)
	})
}

// **`maxBytes+1` のオーバーフローで 0 バイトの本体を保存しない (#3037 レビュー)。**
//
// `policyMegabytes` は `safemath.MulFloat64` で `MaxInt64` に飽和するので、
// `maxFileSizeMb` に `math.MaxFloat64` (= このリポジトリが「無制限」の意味で
// 使うイディオム) を入れると `maxBytes+1` が `MinInt64` になり、
// `io.LimitReader` が即 EOF を返す。**error 無しで空の本体が保存される**という、
// この関数の doc が塞いだばかりの形。
func TestReadAtMost_SaturatedLimitIsTreatedAsUnlimited(t *testing.T) {
	body, err := readAtMost(strings.NewReader("hello world"), math.MaxInt64)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(body), "飽和した上限で本体が切り詰められている")
}

func TestFilesCreate_SaturatedPolicyStillUploads(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	h.svc.SetRoleChecker(roleStub{maxFileSizeMb: math.MaxFloat64})

	c, rec := newMultipartReq(t, "hello.txt", "hello world", nil)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))

	require.Equal(t, http.StatusOK, rec.Code, "無制限の policy でアップロードが落ちている")
	require.Len(t, fileRepo.Files, 1)
	for _, f := range fileRepo.Files {
		assert.Equal(t, len("hello world"), f.Size, "0 バイトで保存されている")
	}
}
