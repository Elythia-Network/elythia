package notes

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	corechannel "github.com/shiroha-a/mk/internal/core/channel"
	corenote "github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/entitycompat/shapetest"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func newTestHandler(t *testing.T) (*Handler, *testutil.MockNoteRepository) {
	t.Helper()
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, nil, idGen)
	return h, noteRepo
}

func setAuthUser(c echo.Context, user *model.User) {
	c.Set(string(middleware.UserContextKey), user)
}

func TestCreate_Success(t *testing.T) {
	h, noteRepo := newTestHandler(t)

	user := &model.User{
		ID:                "user1",
		Username:          "testuser",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	body := `{"text": "Hello, world!", "visibility": "public"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	err := h.Create(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	createdNote, ok := resp["createdNote"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Hello, world!", createdNote["text"])
	assert.Equal(t, "public", createdNote["visibility"])
	shapetest.Assert(t, "Note", createdNote) // L3 (#1312)

	// リポジトリにノートが保存されていることを確認
	assert.Len(t, noteRepo.Notes, 1)
}

func TestCreate_EmptyBody(t *testing.T) {
	h, _ := newTestHandler(t)

	user := &model.User{ID: "user1", Username: "testuser"}

	// text, fileIds, renoteIdがすべてない場合
	body := `{}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	err := h.Create(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_WithPoll(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, nil, idGen)

	user := &model.User{
		ID:                "user1",
		Username:          "testuser",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	body := `{"text": "Vote!", "poll": {"choices": ["A", "B", "C"], "multiple": false}}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	err := h.Create(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Pollがリポジトリに保存されていることを確認
	assert.Len(t, pollRepo.Polls, 1)
}

func TestCreate_DefaultVisibility(t *testing.T) {
	h, noteRepo := newTestHandler(t)

	user := &model.User{
		ID:                "user1",
		Username:          "testuser",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	body := `{"text": "no visibility specified"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	err := h.Create(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	// デフォルトはpublic
	for _, note := range noteRepo.Notes {
		assert.Equal(t, model.NoteVisibilityPublic, note.Visibility)
	}
}

func TestShow_Success(t *testing.T) {
	h, noteRepo := newTestHandler(t)

	text := "existing note"
	noteRepo.Notes["note1"] = &model.Note{
		ID:         "note1",
		UserID:     "user1",
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		User: &model.User{
			ID:                "user1",
			Username:          "testuser",
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		},
	}

	body := `{"noteId": "note1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "note1", resp["id"])
	assert.Equal(t, "existing note", resp["text"])
	// L3 (#1270): notes/show の実レスポンスを golden Note に突合する。
	shapetest.Assert(t, "Note", resp)
}

func TestShow_AttachmentEmbedsFolderAndUser(t *testing.T) {
	// #317: 添付ファイルの folder / user を best-effort で埋めることを検証。
	h, noteRepo := newTestHandler(t)

	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	userRepo := testutil.NewMockUserRepository()

	ownerID := "user1"
	folderID := "folder1"
	fileURL := "https://example.com/f.png"
	fileRepo.Files["file1"] = &model.DriveFile{
		ID:       "file1",
		UserID:   &ownerID,
		FolderID: &folderID,
		Type:     "image/png",
		URL:      fileURL,
		Name:     "pic.png",
	}
	folderRepo.Folders[folderID] = &model.DriveFolder{
		ID:     folderID,
		Name:   "My Folder",
		UserID: &ownerID,
	}
	userRepo.Users[ownerID] = &model.User{
		ID:                ownerID,
		Username:          "testuser",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	h.SetDriveFileRepo(fileRepo)
	h.SetDriveFolderRepo(folderRepo)
	h.SetUserRepo(userRepo)

	text := "note with attachment"
	noteRepo.Notes["n1"] = &model.Note{
		ID:         "n1",
		UserID:     ownerID,
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		FileIDs:    []string{"file1"},
		User: &model.User{
			ID:                ownerID,
			Username:          "testuser",
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		},
	}

	body := `{"noteId": "n1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	filesRaw, ok := resp["files"].([]any)
	require.True(t, ok)
	require.Len(t, filesRaw, 1)
	file := filesRaw[0].(map[string]any)
	folder, ok := file["folder"].(map[string]any)
	require.True(t, ok, "attachment must embed folder")
	assert.Equal(t, "My Folder", folder["name"])
	user, ok := file["user"].(map[string]any)
	require.True(t, ok, "attachment must embed user")
	assert.Equal(t, "testuser", user["username"])
}

func TestShow_AttachmentWithoutRepos_OmitsFolderUser(t *testing.T) {
	// folderRepo / userRepo 未配線のときは folder/user を埋めず、
	// 従来通り file オブジェクトに folder/user が null になる。
	h, noteRepo := newTestHandler(t)

	fileRepo := testutil.NewMockDriveFileRepository()
	ownerID := "user1"
	fileURL := "https://example.com/f.png"
	fileRepo.Files["file1"] = &model.DriveFile{
		ID:     "file1",
		UserID: &ownerID,
		Type:   "image/png",
		URL:    fileURL,
		Name:   "pic.png",
	}
	h.SetDriveFileRepo(fileRepo)

	text := "note with attachment"
	noteRepo.Notes["n1"] = &model.Note{
		ID:         "n1",
		UserID:     ownerID,
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		FileIDs:    []string{"file1"},
		User: &model.User{
			ID:                ownerID,
			Username:          "testuser",
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		},
	}

	body := `{"noteId": "n1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, h.Show(c))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	filesRaw := resp["files"].([]any)
	require.Len(t, filesRaw, 1)
	file := filesRaw[0].(map[string]any)
	assert.Nil(t, file["folder"])
	assert.Nil(t, file["user"])
}

func TestShow_PopulatesUserInstanceForRemoteAuthor(t *testing.T) {
	h, noteRepo := newTestHandler(t)

	// InstanceRepo を注入して remote host に対応する Instance row を用意する。
	instanceRepo := testutil.NewMockInstanceRepository()
	name := "Remote Misskey"
	instanceRepo.Instances["remote.example"] = &model.Instance{
		Host: "remote.example",
		Name: &name,
	}
	h.SetInstanceRepo(instanceRepo)

	host := "remote.example"
	text := "remote note"
	noteRepo.Notes["n1"] = &model.Note{
		ID:         "n1",
		UserID:     "uR",
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		User: &model.User{
			ID:                "uR",
			Username:          "remoteuser",
			Host:              &host,
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		},
	}

	body := `{"noteId": "n1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	user, ok := resp["user"].(map[string]any)
	require.True(t, ok, "response must contain user object")
	instance, ok := user["instance"].(map[string]any)
	require.True(t, ok, "user must have instance field populated for remote author")
	assert.Equal(t, "Remote Misskey", instance["name"])
}

func TestShow_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)

	body := `{"noteId": "nonexistent"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_MissingNoteId(t *testing.T) {
	h, _ := newTestHandler(t)

	body := `{}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_MyReaction(t *testing.T) {
	h, noteRepo := newTestHandler(t)

	// リアクションリポジトリをセット
	reactionRepo := testutil.NewMockNoteReactionRepository()
	h.SetNoteReactionRepo(reactionRepo)

	text := "test note"
	noteRepo.Notes["note1"] = &model.Note{
		ID:         "note1",
		UserID:     "author1",
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte(`{"👍":1}`)),
		User: &model.User{
			ID:                "author1",
			Username:          "author",
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		},
	}

	// viewerのリアクションを登録
	reactionRepo.Reactions["r1"] = &model.NoteReaction{
		ID:       "r1",
		UserID:   "viewer1",
		NoteID:   "note1",
		Reaction: "👍",
	}

	viewer := &model.User{ID: "viewer1", Username: "viewer"}

	body := `{"noteId": "note1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, viewer)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "👍", resp["myReaction"])
	assert.Equal(t, float64(1), resp["reactionCount"])
}

func TestShow_Channel(t *testing.T) {
	h, noteRepo := newTestHandler(t)

	// チャンネルリポジトリをセット
	channelRepo := testutil.NewMockChannelRepository()
	h.SetChannelRepo(channelRepo)

	channelRepo.Channels["ch1"] = &model.Channel{
		ID:                    "ch1",
		Name:                  "test-channel",
		Color:                 "#ff0000",
		IsSensitive:           true,
		AllowRenoteToExternal: false,
	}

	text := "channel note"
	chID := "ch1"
	noteRepo.Notes["note2"] = &model.Note{
		ID:         "note2",
		UserID:     "user1",
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
		ChannelID:  &chID,
		Reactions:  datatypes.JSON([]byte("{}")),
		User: &model.User{
			ID:                "user1",
			Username:          "testuser",
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		},
	}

	body := `{"noteId": "note2"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ch := resp["channel"].(map[string]any)
	assert.Equal(t, "ch1", ch["id"])
	assert.Equal(t, "test-channel", ch["name"])
	assert.Equal(t, "#ff0000", ch["color"])
	assert.Equal(t, true, ch["isSensitive"])
	assert.Equal(t, false, ch["allowRenoteToExternal"])
}

func TestDelete_Success(t *testing.T) {
	h, noteRepo := newTestHandler(t)

	noteRepo.Notes["note1"] = &model.Note{
		ID:     "note1",
		UserID: "user1",
	}

	user := &model.User{ID: "user1", Username: "testuser"}

	body := `{"noteId": "note1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/delete", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	err := h.Delete(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// リポジトリから削除されていることを確認
	assert.Len(t, noteRepo.Notes, 0)
}

func TestDelete_NotOwner(t *testing.T) {
	h, noteRepo := newTestHandler(t)

	noteRepo.Notes["note1"] = &model.Note{
		ID:     "note1",
		UserID: "other-user",
	}

	user := &model.User{ID: "user1", Username: "testuser"}

	body := `{"noteId": "note1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/delete", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	err := h.Delete(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// ノートは削除されていない
	assert.Len(t, noteRepo.Notes, 1)
}

func TestDelete_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)

	user := &model.User{ID: "user1", Username: "testuser"}

	body := `{"noteId": "nonexistent"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/delete", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	err := h.Delete(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_InvalidJSON(t *testing.T) {
	h, _ := newTestHandler(t)

	user := &model.User{ID: "user1", Username: "testuser"}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader("{invalid"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	err := h.Create(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #1930: 不正な visibility enum は 400 INVALID_PARAM。
func TestCreate_InvalidVisibility(t *testing.T) {
	h, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "testuser"}
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(`{"text":"x","visibility":"foo"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
}

// #1930: 不正な reactionAcceptance enum は 400 INVALID_PARAM。
func TestCreate_InvalidReactionAcceptance(t *testing.T) {
	h, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "testuser"}
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(`{"text":"x","reactionAcceptance":"bogus"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
}

func TestCreate_WithVisibleUserIDs(t *testing.T) {
	h, noteRepo := newTestHandler(t)

	user := &model.User{
		ID:                "user1",
		Username:          "testuser",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	body := `{"text": "secret", "visibility": "specified", "visibleUserIds": ["user2", "user3"]}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	err := h.Create(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	for _, note := range noteRepo.Notes {
		assert.Equal(t, model.NoteVisibility("specified"), note.Visibility)
		assert.Equal(t, []string{"user2", "user3"}, []string(note.VisibleUserIDs))
	}
}

func TestCreate_WithPollExpiresAt(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, nil, idGen)

	user := &model.User{
		ID:                "user1",
		Username:          "testuser",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	// 未来日時を動的に算出 (hardcoded 値だと時間経過でテストが expired 判定で落ちる)
	futureMs := time.Now().Add(24 * time.Hour).UnixMilli()
	body := fmt.Sprintf(`{"text": "Vote!", "poll": {"choices": ["A", "B"], "multiple": true, "expiresAt": %d}}`, futureMs)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	err := h.Create(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	for _, poll := range pollRepo.Polls {
		assert.NotNil(t, poll.ExpiresAt)
		assert.True(t, poll.Multiple)
	}
}

func TestCreate_RepoError(t *testing.T) {
	noteRepo := &failingNoteRepo{}
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, nil, idGen)

	user := &model.User{ID: "user1", Username: "testuser"}

	body := `{"text": "will fail"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	err := h.Create(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestCreate_FindByIDWithRelationsFails(t *testing.T) {
	noteRepo := &findFailNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, nil, idGen)

	user := &model.User{
		ID:                "user1",
		Username:          "testuser",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	body := `{"text": "fallback path"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	err := h.Create(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// decodeError extracts the error.id field from a JSON error response.
func decodeError(t *testing.T, body []byte) (code, id string) {
	t.Helper()
	var resp map[string]any
	require.NoError(t, json.Unmarshal(body, &resp))
	errObj := resp["error"].(map[string]any)
	return errObj["code"].(string), errObj["id"].(string)
}

func TestCreate_RenoteTargetNotFound(t *testing.T) {
	noteRepo := &failingNoteRepo{}
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, nil, idGen)

	user := &model.User{ID: "user1", Username: "testuser"}

	body := `{"renoteId": "nonexistent"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	code, uuidStr := decodeError(t, rec.Body.Bytes())
	assert.Equal(t, "NO_SUCH_RENOTE_TARGET", code)
	assert.Equal(t, apierr.UUIDNoSuchRenoteTarget, uuidStr)
}

// fixedNoteRepo returns a preset note for FindByIDWithUser so visibility-check
// paths in CreateService (reply/renote target lookup, #425 軽量経路) can be
// exercised without a real database. FindByIDWithRelations 経由の caller を
// テストしたい場合は別途上書きが要る (本テスト群では visibility 弾きで先に
// return するので不要)。
type fixedNoteRepo struct {
	*testutil.MockNoteRepository
	note *model.Note
}

func (r *fixedNoteRepo) FindByIDWithUser(_ string) (*model.Note, error) {
	return r.note, nil
}

func TestCreate_CannotReplyToInvisibleNote(t *testing.T) {
	hiddenNote := &model.Note{ID: "n1", UserID: "other", Visibility: model.NoteVisibilityFollowers}
	noteRepo := &fixedNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository(), note: hiddenNote}
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, nil, idGen)

	user := &model.User{ID: "user1", Username: "testuser"}

	body := `{"text": "reply", "replyId": "n1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	code, uuidStr := decodeError(t, rec.Body.Bytes())
	assert.Equal(t, "CANNOT_REPLY_TO_AN_INVISIBLE_NOTE", code)
	assert.Equal(t, apierr.UUIDCannotReplyToAnInvisibleNote, uuidStr)
}

// channelErrHook は EnsureChannelExists が常に err を返すテストスタブ。
type channelErrHook struct{ err error }

func (h channelErrHook) EnsureChannelExists(_ string) error { return h.err }
func (channelErrHook) OnNotePosted(_, _, _ string)          {}

func TestCreate_ChannelNotFound(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	createSvc.SetChannelHook(channelErrHook{err: corechannel.ErrChannelNotFound})
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, nil, idGen)

	user := &model.User{ID: "user1", Username: "testuser"}

	body := `{"text": "to channel", "channelId": "nonexistent"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	code, uuidStr := decodeError(t, rec.Body.Bytes())
	assert.Equal(t, "NO_SUCH_CHANNEL", code)
	assert.Equal(t, apierr.UUIDNoSuchChannel, uuidStr)
}

// チャンネルの lookup が DB 障害で失敗したら NO_SUCH_CHANNEL に丸めず 500 (#2792)。
func TestCreate_ChannelLookupErrorIsInternal(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	createSvc.SetChannelHook(channelErrHook{err: errors.New("connection refused")})
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, nil, idGen)

	body := `{"text": "to channel", "channelId": "c1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, &model.User{ID: "user1", Username: "testuser"})

	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "NO_SUCH_CHANNEL")
	assert.Empty(t, noteRepo.Notes, "障害時にノートを作らない")
}

// **アーカイブ済みチャンネルへの notes/create は NO_SUCH_CHANNEL。**
//
// 本番と同じ corechannel の hook を配線して確かめる (upstream は
// `isArchived: false` で引くので同じ応答になる)。
func TestCreate_ArchivedChannel(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	channelRepo := testutil.NewMockChannelRepository()
	channelRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "old", IsArchived: true}
	channelSvc := corechannel.NewService(channelRepo, testutil.NewMockChannelFollowingRepository(), noteRepo, idGen)
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	createSvc.SetChannelHook(corechannel.NewNoteCreateHook(channelSvc))
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, nil, idGen)

	user := &model.User{ID: "user1", Username: "testuser"}
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(`{"text": "to channel", "channelId": "ch1"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	code, uuidStr := decodeError(t, rec.Body.Bytes())
	assert.Equal(t, "NO_SUCH_CHANNEL", code)
	assert.Equal(t, apierr.UUIDNoSuchChannel, uuidStr)
	assert.Empty(t, noteRepo.Notes, "ノートを作らないこと")
}

func TestDelete_InvalidJSON(t *testing.T) {
	h, _ := newTestHandler(t)

	user := &model.User{ID: "user1", Username: "testuser"}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/delete", strings.NewReader("{invalid"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	err := h.Delete(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingNoteRepo always returns errors on Create.
type failingNoteRepo struct{}

func (f *failingNoteRepo) Create(_ *model.Note) error { return testutil.ErrNotFound }
func (f *failingNoteRepo) ListRenoteOrReplyRemoteUserIDs(_ string) ([]string, error) {
	return nil, testutil.ErrNotFound
}
func (f *failingNoteRepo) FindByID(_ string) (*model.Note, error) { return nil, testutil.ErrNotFound }
func (f *failingNoteRepo) FindByIDWithUser(_ string) (*model.Note, error) {
	return nil, testutil.ErrNotFound
}
func (f *failingNoteRepo) FindByIDWithRelations(_ string) (*model.Note, error) {
	return nil, testutil.ErrNotFound
}
func (f *failingNoteRepo) FindByURI(_ string) (*model.Note, error) {
	return nil, testutil.ErrNotFound
}
func (f *failingNoteRepo) Delete(_ *model.Note) error                        { return nil }
func (f *failingNoteRepo) Update(_ *model.Note, _ string, _ any) error       { return nil }
func (f *failingNoteRepo) UpdateFields(_ string, _ map[string]any) error     { return nil }
func (f *failingNoteRepo) IncrementCount(_ string, _ string, _ int) error    { return nil }
func (f *failingNoteRepo) IncrementReaction(_ string, _ string, _ int) error { return nil }
func (f *failingNoteRepo) ListByUserID(_ string, _, _ string, _ int) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) ListPublicNotesForFeed(_ string, _ int) ([]*model.Note, error) {
	return nil, nil
}

func (f *failingNoteRepo) ListPublicByUserID(_ string, _, _ string, _ int) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) ListByUserIDFiltered(_, _, _, _ string, _ int, _, _, _, _ bool) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) ListByChannelID(_, _, _, _ string, _ int) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) FindManyByIDsWithUser(_ []string) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) ListRenotesOf(_, _, _, _ string, _ int) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) ListRepliesOf(_, _, _, _ string, _ int) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) ListChildrenOf(_, _, _, _ string, _ int) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) SearchByFilter(_ model.NoteSearchFilter) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) ListFeatured(_, _ string, _, _ int) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) ListFeaturedByUser(_, _, _ string, _ int) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) FindRenoteByUser(_, _ string) (*model.Note, error) {
	return nil, testutil.ErrNotFound
}
func (f *failingNoteRepo) ListMentions(_, _ string, _ bool, _ int, _, _ string) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) SearchByTag(_ [][]string, _ string, _ int, _, _ string, _ model.NoteSearchTagFilter) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) ListByFileID(_, _, _ string, _ int) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) IncrementUserNotesCount(_ string, _ int) error { return nil }
func (f *failingNoteRepo) ListHomeTimeline(_ string, _ int, _, _ string, _ model.TimelineDBFilter) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) ListLocalTimeline(_ int, _, _ string, _ model.TimelineDBFilter) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) ListGlobalTimeline(_ int, _, _ string, _ model.TimelineDBFilter) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) ListPublicNotes(_ model.PublicNotesFilter, _ int, _, _ string) ([]*model.Note, error) {
	return nil, errors.New("boom")
}
func (f *failingNoteRepo) DeleteExpiredRemoteNotes(_, _ int) (int64, error) { return 0, nil }
func (f *failingNoteRepo) DeleteExpiredRemoteNotesAfter(_, _ int, _ string) (int64, int, string, error) {
	return 0, 0, "", nil
}
func (f *failingNoteRepo) DeleteByUserBatch(_ string, _ int) (int64, error) { return 0, nil }
func (f *failingNoteRepo) CountReplyTargets(_, _ string, _ int) ([]model.ReplyTargetCount, error) {
	return nil, nil
}
func (f *failingNoteRepo) ListByUserList(_ string, _ int, _, _ string, _ model.TimelineDBFilter) ([]*model.Note, error) {
	return nil, nil
}
func (f *failingNoteRepo) CountLocalNotes() (int64, error)    { return 0, nil }
func (f *failingNoteRepo) CountLocalComments() (int64, error) { return 0, nil }

// findFailNoteRepo creates successfully but FindByIDWithRelations always
// fails. CreateService の finalNote が full 経路 (#425) を踏むため、fallback
// で finalNote.User = in.User が埋まる挙動を exercise する。
type findFailNoteRepo struct {
	*testutil.MockNoteRepository
}

func (f *findFailNoteRepo) FindByIDWithRelations(_ string) (*model.Note, error) {
	return nil, testutil.ErrNotFound
}

// deleteFailNoteRepo finds a note successfully but fails on Delete, used to
// trigger the handler's default error path.
type deleteFailNoteRepo struct {
	*testutil.MockNoteRepository
}

func (f *deleteFailNoteRepo) Delete(_ *model.Note) error { return testutil.ErrNotFound }

func TestDelete_RepoError(t *testing.T) {
	mockRepo := testutil.NewMockNoteRepository()
	mockRepo.Notes["note1"] = &model.Note{ID: "note1", UserID: "user1"}
	noteRepo := &deleteFailNoteRepo{MockNoteRepository: mockRepo}
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, nil, idGen)

	user := &model.User{ID: "user1", Username: "testuser"}
	body := `{"noteId": "note1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/delete", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)

	err := h.Delete(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestExtractMentions(t *testing.T) {
	tests := []struct {
		text     string
		expected []string
	}{
		{"Hello @alice @bob", []string{"alice", "bob"}},
		{"No mentions here", nil},
		{"@single", []string{"single"}},
		{"@", nil},
		{"", nil},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			result := corenote.ExtractMentions(tt.text)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestBulkShow_Success(t *testing.T) {
	h, noteRepo := newTestHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: "public", User: &model.User{ID: "u1"}}
	noteRepo.Notes["n2"] = &model.Note{ID: "n2", UserID: "u1", Visibility: "public", User: &model.User{ID: "u1"}}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes", strings.NewReader(`{"noteIds":["n1","n2","ghost"]}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.BulkShow(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Len(t, out, 2)
}

func TestBulkShow_Empty(t *testing.T) {
	h, _ := newTestHandler(t)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes", strings.NewReader(`{"noteIds":[]}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.BulkShow(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

func TestBulkShow_NoBody(t *testing.T) {
	h, _ := newTestHandler(t)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.BulkShow(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

// #2027: poll choices uniqueItems + expiredAfter>=1 を validateCreateInput が弾く。
func TestValidateCreateInput_PollUniqueAndExpiredAfter(t *testing.T) {
	dupChoices := &CreateRequest{Poll: &PollRequest{Choices: []string{"a", "a"}}}
	assert.Error(t, validateCreateInput(dupChoices, nil), "重複 choice は弾く")

	ok := &CreateRequest{Poll: &PollRequest{Choices: []string{"a", "b"}}}
	assert.NoError(t, validateCreateInput(ok, nil), "ユニークな choice は通る")

	bad := int64(0)
	badExpire := &CreateRequest{Poll: &PollRequest{Choices: []string{"a", "b"}, ExpiredAfter: &bad}}
	assert.Error(t, validateCreateInput(badExpire, nil), "expiredAfter<1 は弾く")
}

// #2106 L6: poll で expiresAt と expiredAfter 両方指定時は expiredAfter (相対) を優先する。
func TestCreate_PollExpiredAfterTakesPriority(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, nil, idGen)
	user := &model.User{ID: "user1", Username: "testuser", AvatarDecorations: datatypes.JSON([]byte("[]"))}

	farFutureMs := time.Now().Add(30 * 24 * time.Hour).UnixMilli() // expiresAt: 30 日後
	expiredAfterMs := int64(3600 * 1000)                           // expiredAfter: 1 時間
	body := fmt.Sprintf(`{"text": "Vote!", "poll": {"choices": ["A", "B"], "expiresAt": %d, "expiredAfter": %d}}`, farFutureMs, expiredAfterMs)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, user)
	require.NoError(t, h.Create(c))
	require.Equal(t, http.StatusOK, rec.Code)

	require.Len(t, pollRepo.Polls, 1)
	for _, poll := range pollRepo.Polls {
		require.NotNil(t, poll.ExpiresAt)
		// expiredAfter (now+1h) を採用 → 30 日後の expiresAt よりずっと前になる。
		assert.True(t, poll.ExpiresAt.Before(time.Now().Add(2*time.Hour)), "expiredAfter を優先すべき、got %v", poll.ExpiresAt)
	}
}

// #2106 L4 / #2215: noteIds 不在の POST /notes は upstream notes.ts の public note 一覧を返す
// (localOnly note は除外)。
func TestBulkShow_PublicTimeline(t *testing.T) {
	h, noteRepo := newTestHandler(t)
	noteRepo.Notes["np1"] = &model.Note{ID: "np1", UserID: "u1", Visibility: "public", User: &model.User{ID: "u1"}}
	noteRepo.Notes["np2"] = &model.Note{ID: "np2", UserID: "u1", Visibility: "public", LocalOnly: true, User: &model.User{ID: "u1"}}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes", strings.NewReader(`{"limit":30}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.BulkShow(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	// localOnly (np2) は除外され、public な np1 のみ返る。
	require.Len(t, out, 1)
	assert.Equal(t, "np1", out[0]["id"])
}

// ExistingNoteIDsOnPrimary implements repository.NoteRepository (#2719).
func (f *failingNoteRepo) ExistingNoteIDsOnPrimary(_ []string) ([]string, error) {
	return nil, nil
}
