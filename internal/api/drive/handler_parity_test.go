package drive

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// #1564 parity sweep の handler 層テスト群。

// --- FilesCreate: name pipeline ---

func TestFilesCreate_NameTrimmed(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newMultipartReq(t, "raw.txt", "hello", map[string]string{"name": "  photo.jpg  "})
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	// 前後空白は trim される。中身はテキストなので拡張子補正で .txt が付く
	// (upstream DriveService.addFile の correctFilename と同じ)。
	assert.Equal(t, "photo.jpg.txt", out["name"])
}

func TestFilesCreate_BlobNameFallsBackToUntitled(t *testing.T) {
	// upstream は 'blob' を null 化し addFile が 'untitled' + 検出拡張子で
	// 保存する (#1564)。
	h, _, _ := newHandler(t)
	c, rec := newMultipartReq(t, "blob", "hello", nil)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "untitled.txt", out["name"])
}

func TestFilesCreate_InvalidName(t *testing.T) {
	h, _, _ := newHandler(t)
	for _, bad := range []string{"a/b.png", `a\b.png`, "a..png", strings.Repeat("x", 201)} {
		c, rec := newMultipartReq(t, "raw.txt", "hello", map[string]string{"name": bad})
		setUser(c, "u1")
		require.NoError(t, h.FilesCreate(c))
		assert.Equal(t, http.StatusBadRequest, rec.Code, bad)
		assert.Contains(t, rec.Body.String(), "INVALID_FILE_NAME", bad)
		// upstream create.ts 固有 UUID (update とは別)。
		assert.Contains(t, rec.Body.String(), "f449b209-0c60-4e51-84d5-29486263bfd4", bad)
	}
}

func TestFilesCreate_CommentTooLong(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newMultipartReq(t, "a.txt", "hello", map[string]string{"comment": strings.Repeat("あ", 513)})
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
}

func TestFilesCreate_Comment512OK(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newMultipartReq(t, "a.txt", "hello", map[string]string{"comment": strings.Repeat("あ", 512)})
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- FilesUpdate ---

func TestFilesUpdate_InvalidName(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Name: "old"}
	c, rec := newJSONReq(t, `{"fileId":"f1","name":"a/b.png"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUpdate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_FILE_NAME")
	// upstream update.ts 固有 UUID (create とは別)。
	assert.Contains(t, rec.Body.String(), "395e7156-f9f0-475e-af89-53c3c23080c2")
}

func TestFilesUpdate_CommentTooLong(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid}
	body, _ := json.Marshal(map[string]any{"fileId": "f1", "comment": strings.Repeat("あ", 513)})
	c, rec := newJSONReq(t, string(body))
	setUser(c, "u1")
	require.NoError(t, h.FilesUpdate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
}

// --- FilesShow: url anyOf + detail folder ---

func TestFilesShow_ByURL(t *testing.T) {
	h, fileRepo, _ := newHandlerWithRepos(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, URL: "https://example.com/files/x"}
	c, rec := newJSONReq(t, `{"url":"https://example.com/files/x"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesShow(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "f1", out["id"])
}

func TestFilesShow_NeitherFileIDNorURL(t *testing.T) {
	// upstream paramDef は anyOf {fileId}|{url} 必須 (#1564)。
	h, _, _ := newHandlerWithRepos(t)
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesShow(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesShow_FolderIsDetailPacked(t *testing.T) {
	// upstream pack(file, {detail:true}) は埋め込み folder を detail mode で
	// pack するため foldersCount / filesCount を含む (#1564)。
	h, fileRepo, folderRepo := newHandlerWithRepos(t)
	uid := "u1"
	folderRepo.Folders["fo1"] = &model.DriveFolder{ID: "fo1", Name: "docs", UserID: &uid}
	fid := "fo1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, FolderID: &fid}
	c, rec := newJSONReq(t, `{"fileId":"f1"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesShow(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		Folder map[string]any `json:"folder"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotNil(t, out.Folder)
	assert.Contains(t, out.Folder, "foldersCount")
	assert.Contains(t, out.Folder, "filesCount")
}

// --- FilesFindByHash: 全件 ---

func TestFilesFindByHash_ReturnsAllMatches(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, MD5: "h"}
	fileRepo.Files["f2"] = &model.DriveFile{ID: "f2", UserID: &uid, MD5: "h"}
	c, rec := newJSONReq(t, `{"md5":"h"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesFindByHash(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 2, "md5 一致の全件を返す (#1564)")
}

// --- FilesList: sort / type / self pack ---

func TestFilesList_InvalidSortRejected(t *testing.T) {
	h, _, _ := newHandlerWithRepos(t)
	c, rec := newJSONReq(t, `{"sort":"+bogus"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesList(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesList_TypeFilterAndSort(t *testing.T) {
	h, fileRepo, _ := newHandlerWithRepos(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Type: "image/png", Size: 10}
	fileRepo.Files["f2"] = &model.DriveFile{ID: "f2", UserID: &uid, Type: "image/jpeg", Size: 30}
	fileRepo.Files["f3"] = &model.DriveFile{ID: "f3", UserID: &uid, Type: "video/webm", Size: 20}

	// type prefix filter ("image/*")
	c, rec := newJSONReq(t, `{"type":"image/*","sort":"+size"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesList(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 2, "video は除外")
	assert.Equal(t, "f2", out[0]["id"], "+size は size DESC")
	assert.Equal(t, "f1", out[1]["id"])

	// type exact match (upstream pattern は数字を許さないため digit-less MIME)
	c, rec = newJSONReq(t, `{"type":"video/webm"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesList(c))
	out = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.Equal(t, "f3", out[0]["id"])
}

func TestFilesList_InvalidTypePatternRejected(t *testing.T) {
	// upstream paramDef の type pattern は ^[a-zA-Z\/\-*]+$ (#1564 review)。
	// 数字を含む MIME (video/mp4) も % 等の記号も upstream は INVALID_PARAM
	// で弾く。LIKE wildcard (% _) がここで止まるため repo 層に混入しない。
	h, _, _ := newHandlerWithRepos(t)
	for _, bad := range []string{"video/mp4", "%/*", "ima_e/*", "image/%"} {
		c, rec := newJSONReq(t, `{"type":"`+bad+`"}`)
		setUser(c, "u1")
		require.NoError(t, h.FilesList(c))
		assert.Equal(t, http.StatusBadRequest, rec.Code, bad)
	}
	// Stream も同じ pattern 検証
	c, rec := newJSONReq(t, `{"type":"%/*"}`)
	setUser(c, "u1")
	require.NoError(t, h.Stream(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesShow_EmptyURLIs404(t *testing.T) {
	// upstream の anyOf required は「キー存在」のみ検証するため
	// {"url":""} は lookup に進み 404 NO_SUCH_FILE になる (#1564 review)。
	h, _, _ := newHandlerWithRepos(t)
	c, rec := newJSONReq(t, `{"url":""}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesShow(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")

	// fileId は format misskey:id なので空文字は INVALID_PARAM。
	c, rec = newJSONReq(t, `{"fileId":""}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesShow(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFilesCreate_ExplicitEmptyNameFallsBackToUntitled(t *testing.T) {
	// multipart で name="" を明示送信した場合、upstream は ''→null→'untitled'
	// (filename への fallback はしない) (#1564 review)。
	h, _, _ := newHandler(t)
	c, rec := newMultipartReq(t, "photo.jpg", "hello", map[string]string{"name": ""})
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "untitled.txt", out["name"])
}

func TestFoldersShow_CyclicDataDoesNotHang(t *testing.T) {
	// 旧 mk-go で作れてしまった循環 folder data (#1564 で作成自体は禁止) を
	// detail pack が visited guard で打ち切ること。guard が無いと無限再帰で
	// stack overflow → プロセスクラッシュする (#1564 review)。
	h, _, folderRepo := newHandlerWithRepos(t)
	uid := "u1"
	aID := "cyc-a"
	bID := "cyc-b"
	folderRepo.Folders[aID] = &model.DriveFolder{ID: aID, Name: "a", UserID: &uid, ParentID: &bID}
	folderRepo.Folders[bID] = &model.DriveFolder{ID: bID, Name: "b", UserID: &uid, ParentID: &aID}
	c, rec := newJSONReq(t, `{"folderId":"cyc-a"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersShow(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	// a → parent b までは pack され、b の parent (= a, 既出) で打ち切られる。
	parent, ok := out["parent"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, bID, parent["id"])
	assert.Nil(t, parent["parent"], "循環は既出 folder で打ち切る")
}

func TestFilesList_SelfPackNullsFolderAndUser(t *testing.T) {
	// upstream files.ts は packMany({detail:false, self:true}) なので
	// folder/user は null、userId は owner ID (#1564)。
	h, fileRepo, folderRepo := newHandlerWithRepos(t)
	uid := "u1"
	folderRepo.Folders["fo1"] = &model.DriveFolder{ID: "fo1", UserID: &uid}
	fid := "fo1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, FolderID: &fid}
	c, rec := newJSONReq(t, `{"folderId":"fo1"}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesList(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.Nil(t, out[0]["folder"], "self list は folder null")
	assert.Nil(t, out[0]["user"], "self list は user null")
	assert.Equal(t, "u1", out[0]["userId"], "userId は owner ID を維持")
}

// --- Stream: 全 folder 横断 + type ---

func TestStream_IncludesSubfolderFiles(t *testing.T) {
	// upstream stream.ts は folder 条件を付けないため、サブフォルダ内の
	// ファイルも返る (#1564 で root 限定の過剰絞り込みを解消)。
	h, fileRepo, folderRepo := newHandlerWithRepos(t)
	uid := "u1"
	folderRepo.Folders["fo1"] = &model.DriveFolder{ID: "fo1", UserID: &uid}
	fid := "fo1"
	fileRepo.Files["root1"] = &model.DriveFile{ID: "root1", UserID: &uid, Type: "image/png"}
	fileRepo.Files["sub1"] = &model.DriveFile{ID: "sub1", UserID: &uid, FolderID: &fid, Type: "image/png"}
	fileRepo.Files["sub2"] = &model.DriveFile{ID: "sub2", UserID: &uid, FolderID: &fid, Type: "video/mp4"}

	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.Stream(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 3, "root + サブフォルダの全ファイル")
	assert.Nil(t, out[0]["folder"], "self list pack")

	// type filter も効く
	c, rec = newJSONReq(t, `{"type":"image/*"}`)
	setUser(c, "u1")
	require.NoError(t, h.Stream(c))
	out = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 2)
}

// --- FoldersList / FoldersFind: createdAt ---

func TestFoldersList_IncludesCreatedAt(t *testing.T) {
	h, _, folderRepo := newHandlerWithRepos(t)
	uid := "u1"
	// aidx 形式 id なら createdAt が復元できる
	fo := &model.DriveFolder{ID: h.idGen.Generate(time.Now()), Name: "docs", UserID: &uid}
	folderRepo.Folders[fo.ID] = fo
	c, rec := newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersList(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.NotEmpty(t, out[0]["createdAt"], "upstream pack(folder) は createdAt を含む (#1564)")
	assert.Equal(t, "docs", out[0]["name"])
}

func TestFoldersFind_IncludesCreatedAt(t *testing.T) {
	h, _, folderRepo := newHandlerWithRepos(t)
	uid := "u1"
	fo := &model.DriveFolder{ID: h.idGen.Generate(time.Now()), Name: "docs", UserID: &uid}
	folderRepo.Folders[fo.ID] = fo
	c, rec := newJSONReq(t, `{"name":"docs"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersFind(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.NotEmpty(t, out[0]["createdAt"])
}

// --- Folders name maxLength ---

func TestFoldersCreate_NameTooLong(t *testing.T) {
	h, _, _ := newHandler(t)
	body, _ := json.Marshal(map[string]any{"name": strings.Repeat("あ", 201)})
	c, rec := newJSONReq(t, string(body))
	setUser(c, "u1")
	require.NoError(t, h.FoldersCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFoldersUpdate_NameTooLong(t *testing.T) {
	h, _, folderRepo := newHandler(t)
	uid := "u1"
	folderRepo.Folders["fo1"] = &model.DriveFolder{ID: "fo1", UserID: &uid}
	body, _ := json.Marshal(map[string]any{"folderId": "fo1", "name": strings.Repeat("あ", 201)})
	c, rec := newJSONReq(t, string(body))
	setUser(c, "u1")
	require.NoError(t, h.FoldersUpdate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- FoldersUpdate: RECURSIVE_NESTING mapping ---

func TestFoldersUpdate_RecursiveNesting(t *testing.T) {
	h, _, folderRepo := newHandler(t)
	uid := "u1"
	folderRepo.Folders["fo1"] = &model.DriveFolder{ID: "fo1", UserID: &uid}
	c, rec := newJSONReq(t, `{"folderId":"fo1","parentId":"fo1"}`)
	setUser(c, "u1")
	require.NoError(t, h.FoldersUpdate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "RECURSIVE_NESTING")
	// upstream はダッシュ無し id 文字列をそのまま使う。
	assert.Contains(t, rec.Body.String(), "dbeb024837894013aed44279f9199740")
}

// --- FilesAttachedNotes: moderator bypass ---

// fakeRoleChecker は coredrive.RoleChecker の test double (moderator 判定のみ)。
type fakeRoleChecker struct{ mods map[string]bool }

func (f *fakeRoleChecker) IsModerator(userID string) bool        { return f.mods[userID] }
func (f *fakeRoleChecker) GetUserPolicies(string) map[string]any { return nil }

func TestFilesAttachedNotes_ModeratorCanQueryOthersFile(t *testing.T) {
	// upstream attached-notes.ts は moderator のとき userId filter を外す
	// (#1564)。非 moderator の他人 file は従来どおり NO_SUCH_FILE (#1470)。
	// moderator 判定は core 側 RoleChecker (svc.IsModerator) に一本化。
	h, fileRepo, _ := newHandlerWithRepos(t)
	h.svc.SetRoleChecker(&fakeRoleChecker{mods: map[string]bool{"mod1": true}})
	other := "u2"
	fileRepo.Files["f-other"] = &model.DriveFile{ID: "f-other", UserID: &other}

	c, rec := newJSONReq(t, `{"fileId":"f-other"}`)
	setUser(c, "mod1")
	require.NoError(t, h.FilesAttachedNotes(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	// 非 moderator は引き続き拒否
	c, rec = newJSONReq(t, `{"fileId":"f-other"}`)
	setUser(c, "u3")
	require.NoError(t, h.FilesAttachedNotes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
}

// --- 列に入らない値を弾く (#3037) ---

// `drive_file.comment` / `drive_folder.name` は varchar。NUL も不正な UTF-8 も
// そのまま INSERT / UPDATE すると SQLSTATE 22021 でクエリごと落ちるので、
// **認証済みの利用者が 1 文字で 500 を起こせていた**。#3022 と同じく長さと同じ
// 述語 (`colfit.Fits`) に畳んで、既存の 400 に落とす。
func TestFilesCreate_UnstorableCommentIs400(t *testing.T) {
	for name, comment := range map[string]string{
		"NUL":           "a\x00b",
		"invalid UTF-8": "a\x80b",
	} {
		t.Run(name, func(t *testing.T) {
			h, fileRepo, _ := newHandler(t)
			c, rec := newMultipartReq(t, "a.txt", "hello", map[string]string{"comment": comment})
			setUser(c, "u1")
			require.NoError(t, h.FilesCreate(c))
			assert.Equal(t, http.StatusBadRequest, rec.Code, "列に入らない comment を受け入れている")
			assert.Empty(t, fileRepo.Files)
		})
	}
}

// **JSON body から来るのは NUL だけ。** Go の json は不正な UTF-8 も孤立
// サロゲートのエスケープも U+FFFD へ矯正するので、`\u0000` エスケープだけが
// 実バイトとして届く。不正な UTF-8 が届くのは multipart のフォーム値や
// クエリパラメータ側 (上の `TestFilesCreate_UnstorableCommentIs400` が
// そちらを踏む)。
func TestFoldersCreate_UnstorableNameIs400(t *testing.T) {
	h, _, folderRepo := newHandler(t)
	body, _ := json.Marshal(map[string]any{"name": "a\x00b"})
	c, rec := newJSONReq(t, string(body))
	setUser(c, "u1")
	require.NoError(t, h.FoldersCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "列に入らない folder 名を受け入れている")
	assert.Empty(t, folderRepo.Folders)
}

// **普通の値は通ったまま** (`TestFilesCreate_Comment512OK` /
// `TestFoldersCreate_Success` が担当だが、ここでも非 ASCII を明示しておく)。
func TestFoldersCreate_NonASCIINameStillPasses(t *testing.T) {
	h, _, folderRepo := newHandler(t)
	body, _ := json.Marshal(map[string]any{"name": "絵文字\U0001F600フォルダ"})
	c, rec := newJSONReq(t, string(body))
	setUser(c, "u1")
	require.NoError(t, h.FoldersCreate(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, folderRepo.Folders, 1)
}
