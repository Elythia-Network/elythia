package notes

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func newReactionHandler(t *testing.T) (*Handler, *testutil.MockNoteRepository, *testutil.MockNoteReactionRepository) {
	t.Helper()
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	reactRepo := testutil.NewMockNoteReactionRepository()
	emojiRepo := testutil.NewMockEmojiRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	reactSvc := corereaction.NewService(noteRepo, reactRepo, emojiRepo, nil, idGen)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, reactSvc, nil, nil, idGen)
	return h, noteRepo, reactRepo
}

func seedReactionNote(repo *testutil.MockNoteRepository, id, vis string) {
	repo.Notes[id] = &model.Note{
		ID:         id,
		UserID:     "author",
		Visibility: model.NoteVisibility(vis),
		Reactions:  datatypes.JSON([]byte("{}")),
		User: &model.User{
			ID:                "author",
			Username:          "author",
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		},
	}
}

func TestReactionsCreate_Success(t *testing.T) {
	h, repo, _ := newReactionHandler(t)
	seedReactionNote(repo, "n1", "public")

	c, rec := newJSONRequest(t, "/api/notes/reactions/create", `{"noteId":"n1","reaction":"👍"}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsCreate(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestReactionsCreate_InvalidParam(t *testing.T) {
	h, _, _ := newReactionHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/reactions/create", `{}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestReactionsCreate_InvalidJSON(t *testing.T) {
	h, _, _ := newReactionHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/reactions/create", `{invalid`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestReactionsCreate_NoteNotFound(t *testing.T) {
	h, _, _ := newReactionHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/reactions/create", `{"noteId":"ghost","reaction":"👍"}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestReactionsCreate_NotVisible(t *testing.T) {
	h, repo, _ := newReactionHandler(t)
	seedReactionNote(repo, "n1", "followers")
	c, rec := newJSONRequest(t, "/api/notes/reactions/create", `{"noteId":"n1","reaction":"👍"}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestReactionsCreate_AlreadyReacted(t *testing.T) {
	h, repo, _ := newReactionHandler(t)
	seedReactionNote(repo, "n1", "public")
	// First reaction
	c1, _ := newJSONRequest(t, "/api/notes/reactions/create", `{"noteId":"n1","reaction":"👍"}`)
	setAuthUser(c1, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsCreate(c1))

	// Same reaction again
	c2, rec := newJSONRequest(t, "/api/notes/reactions/create", `{"noteId":"n1","reaction":"👍"}`)
	setAuthUser(c2, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsCreate(c2))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// stubBlockingChecker for reaction handler tests.
type stubBlockingCheckerReact struct{}

func (s *stubBlockingCheckerReact) IsBlocked(_, _ string) (bool, error) {
	return true, nil
}

func TestReactionsCreate_Blocked(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	seedReactionNote(noteRepo, "n1", "public")
	pollRepo := testutil.NewMockPollRepository()
	reactRepo := testutil.NewMockNoteReactionRepository()
	emojiRepo := testutil.NewMockEmojiRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	reactSvc := corereaction.NewService(noteRepo, reactRepo, emojiRepo, nil, idGen)
	reactSvc.SetBlockingChecker(&stubBlockingCheckerReact{})
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, reactSvc, nil, nil, idGen)

	c, rec := newJSONRequest(t, "/api/notes/reactions/create", `{"noteId":"n1","reaction":"👍"}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// upstream create.ts は endpoint error youHaveBeenBlocked に変換する。
	// 旧実装は内部 id (e70412a4) と code=BLOCKED を leak していた (#1538)。
	assert.Contains(t, rec.Body.String(), "YOU_HAVE_BEEN_BLOCKED")
	assert.Contains(t, rec.Body.String(), "20ef5475-9f38-4e4c-bd33-de6d979498ec")
	assert.NotContains(t, rec.Body.String(), `"BLOCKED"`)
}

func TestReactionsCreate_PureRenote(t *testing.T) {
	h, repo, _ := newReactionHandler(t)
	target := "target"
	repo.Notes["n1"] = &model.Note{
		ID:         "n1",
		UserID:     "author",
		Visibility: model.NoteVisibilityPublic,
		RenoteID:   &target,
	}
	c, rec := newJSONRequest(t, "/api/notes/reactions/create", `{"noteId":"n1","reaction":"👍"}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingReactionRepoForHandler returns a generic error from Create.
type failingReactionRepoForHandler struct {
	*testutil.MockNoteReactionRepository
}

func (f *failingReactionRepoForHandler) Create(_ *model.NoteReaction) error {
	return errors.New("boom")
}

func TestReactionsCreate_RepoError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	seedReactionNote(noteRepo, "n1", "public")
	pollRepo := testutil.NewMockPollRepository()
	reactRepo := &failingReactionRepoForHandler{MockNoteReactionRepository: testutil.NewMockNoteReactionRepository()}
	emojiRepo := testutil.NewMockEmojiRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	reactSvc := corereaction.NewService(noteRepo, reactRepo, emojiRepo, nil, idGen)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, reactSvc, nil, nil, idGen)

	c, rec := newJSONRequest(t, "/api/notes/reactions/create", `{"noteId":"n1","reaction":"👍"}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsCreate(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestReactionsDelete_Success(t *testing.T) {
	h, repo, _ := newReactionHandler(t)
	seedReactionNote(repo, "n1", "public")

	// First add a reaction
	c1, _ := newJSONRequest(t, "/api/notes/reactions/create", `{"noteId":"n1","reaction":"👍"}`)
	setAuthUser(c1, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsCreate(c1))

	c2, rec := newJSONRequest(t, "/api/notes/reactions/delete", `{"noteId":"n1"}`)
	setAuthUser(c2, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsDelete(c2))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestReactionsDelete_InvalidParam(t *testing.T) {
	h, _, _ := newReactionHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/reactions/delete", `{}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsDelete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestReactionsDelete_NoteNotFound(t *testing.T) {
	h, _, _ := newReactionHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/reactions/delete", `{"noteId":"ghost"}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsDelete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestReactionsDelete_NotReacted(t *testing.T) {
	h, repo, _ := newReactionHandler(t)
	seedReactionNote(repo, "n1", "public")
	c, rec := newJSONRequest(t, "/api/notes/reactions/delete", `{"noteId":"n1"}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsDelete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingDeleteReactionRepo causes Delete to fail.
type failingDeleteReactionRepo struct {
	*testutil.MockNoteReactionRepository
}

func (f *failingDeleteReactionRepo) Delete(_ *model.NoteReaction) (int64, error) {
	return 0, errors.New("delete boom")
}

func TestReactionsDelete_RepoError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	seedReactionNote(noteRepo, "n1", "public")
	pollRepo := testutil.NewMockPollRepository()
	mock := testutil.NewMockNoteReactionRepository()
	mock.Reactions["existing"] = &model.NoteReaction{
		ID: "existing", UserID: "viewer", NoteID: "n1", Reaction: "👍",
	}
	reactRepo := &failingDeleteReactionRepo{MockNoteReactionRepository: mock}
	emojiRepo := testutil.NewMockEmojiRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	reactSvc := corereaction.NewService(noteRepo, reactRepo, emojiRepo, nil, idGen)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, reactSvc, nil, nil, idGen)

	c, rec := newJSONRequest(t, "/api/notes/reactions/delete", `{"noteId":"n1"}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsDelete(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestReactions_List_OK(t *testing.T) {
	h, repo, reactRepo := newReactionHandler(t)
	seedReactionNote(repo, "n1", "public")
	idGen, _ := id.NewGenerator("aidx")
	// 有効なAIDを使うことで createdAt のパース経路もカバーする
	rxID := idGen.Generate(timeNow())
	reactRepo.Reactions[rxID] = &model.NoteReaction{
		ID: rxID, UserID: "viewer", NoteID: "n1", Reaction: "👍",
	}

	c, rec := newJSONRequest(t, "/api/notes/reactions", `{"noteId":"n1"}`)
	require.NoError(t, h.Reactions(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
	assert.Equal(t, "👍", resp[0]["type"])
	assert.NotEmpty(t, resp[0]["createdAt"])
}

func TestReactions_List_PackUserLite(t *testing.T) {
	h, repo, reactRepo := newReactionHandler(t)
	seedReactionNote(repo, "n1", "public")
	idGen, _ := id.NewGenerator("aidx")
	rxID := idGen.Generate(timeNow())
	reactRepo.Reactions[rxID] = &model.NoteReaction{
		ID: rxID, UserID: "u1", NoteID: "n1", Reaction: "❤",
		User: &model.User{
			ID:       "u1",
			Username: "alice",
			Host:     nil,
		},
	}

	c, rec := newJSONRequest(t, "/api/notes/reactions", `{"noteId":"n1"}`)
	require.NoError(t, h.Reactions(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	user := resp[0]["user"].(map[string]any)
	assert.Equal(t, "u1", user["id"])
	assert.Equal(t, "alice", user["username"])
	// PackUserLiteが返す追加フィールドの存在を確認
	_, hasAvatar := user["avatarUrl"]
	assert.True(t, hasAvatar, "PackUserLite should include avatarUrl")
	shapetest.Assert(t, "NoteReaction", resp[0]) // L3 (#1284)
}

func TestReactions_List_UserNil_Fallback(t *testing.T) {
	h, repo, reactRepo := newReactionHandler(t)
	seedReactionNote(repo, "n1", "public")
	idGen, _ := id.NewGenerator("aidx")
	rxID := idGen.Generate(timeNow())
	reactRepo.Reactions[rxID] = &model.NoteReaction{
		ID: rxID, UserID: "u2", NoteID: "n1", Reaction: "👍",
		User: nil,
	}

	c, rec := newJSONRequest(t, "/api/notes/reactions", `{"noteId":"n1"}`)
	require.NoError(t, h.Reactions(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	user := resp[0]["user"].(map[string]any)
	assert.Equal(t, "u2", user["id"])
	// User=nil時はPackUserLiteのフィールドがない
	_, hasUsername := user["username"]
	assert.False(t, hasUsername, "fallback should only have id")
}

func timeNow() time.Time {
	return time.Now()
}

func TestReactions_List_LimitOutOfRangeRejected(t *testing.T) {
	h, repo, _ := newReactionHandler(t)
	seedReactionNote(repo, "n1", "public")
	c, rec := newJSONRequest(t, "/api/notes/reactions", `{"noteId":"n1","limit":1000}`)
	require.NoError(t, h.Reactions(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestReactions_List_InvalidParam(t *testing.T) {
	h, _, _ := newReactionHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/reactions", `{}`)
	require.NoError(t, h.Reactions(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestReactions_List_NoteNotFound(t *testing.T) {
	h, _, _ := newReactionHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/reactions", `{"noteId":"ghost"}`)
	require.NoError(t, h.Reactions(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// listFailingReactionRepo causes ListByNoteID to fail.
type listFailingReactionRepo struct {
	*testutil.MockNoteReactionRepository
}

func (f *listFailingReactionRepo) ListByNoteID(_, _, _ string, _ int, _ []string) ([]*model.NoteReaction, error) {
	return nil, errors.New("list boom")
}

func TestReactions_List_RepoError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	seedReactionNote(noteRepo, "n1", "public")
	pollRepo := testutil.NewMockPollRepository()
	reactRepo := &listFailingReactionRepo{MockNoteReactionRepository: testutil.NewMockNoteReactionRepository()}
	emojiRepo := testutil.NewMockEmojiRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	reactSvc := corereaction.NewService(noteRepo, reactRepo, emojiRepo, nil, idGen)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, reactSvc, nil, nil, idGen)

	c, rec := newJSONRequest(t, "/api/notes/reactions", `{"noteId":"n1"}`)
	require.NoError(t, h.Reactions(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// #2106 L36: reaction param 欠落は 400 INVALID_PARAM (❤ で reaction しない)。
func TestReactionsCreate_MissingReaction(t *testing.T) {
	h, repo, _ := newReactionHandler(t)
	seedReactionNote(repo, "n1", "public")
	c, rec := newJSONRequest(t, "/api/notes/reactions/create", `{"noteId":"n1"}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.ReactionsCreate(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #2106 L7: TS-era legacy reaction (like / host無 :smile:) は convertLegacyReaction 相当の
// 正規化を type に適用する (👍 / :smile@.:)。
func TestReactions_List_LegacyReactionNormalized(t *testing.T) {
	h, repo, reactRepo := newReactionHandler(t)
	seedReactionNote(repo, "n1", "public")
	idGen, _ := id.NewGenerator("aidx")
	rxID := idGen.Generate(timeNow())
	reactRepo.Reactions[rxID] = &model.NoteReaction{
		ID: rxID, UserID: "viewer", NoteID: "n1", Reaction: "like", // legacy
	}

	c, rec := newJSONRequest(t, "/api/notes/reactions", `{"noteId":"n1"}`)
	require.NoError(t, h.Reactions(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "👍", resp[0]["type"], "legacy like を 👍 に正規化")
}
