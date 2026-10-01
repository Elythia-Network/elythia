package notes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	coreachievement "github.com/shiroha-a/mk/internal/core/achievement"
	corenote "github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/core/translate"
	"github.com/shiroha-a/mk/internal/entitycompat/shapetest"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func newExtraHandler(t *testing.T) (*Handler, *testutil.MockNoteRepository, *testutil.MockNoteFavoriteRepository) {
	t.Helper()
	h, noteRepo, favRepo, _ := newExtraHandlerWithFollowing(t)
	return h, noteRepo, favRepo
}

// newExtraHandlerWithFollowing は visibility check が必要なテスト用に
// followingRepo を含めて wire した helper。followers visibility の note を
// favorite 化するテスト等で follow 関係を mock に登録するために使う (#1443)。
func newExtraHandlerWithFollowing(t *testing.T) (*Handler, *testutil.MockNoteRepository, *testutil.MockNoteFavoriteRepository, *testutil.MockFollowingRepository) {
	t.Helper()
	noteRepo := testutil.NewMockNoteRepository()
	favRepo := testutil.NewMockNoteFavoriteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, followingRepo)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, nil, idGen)
	h.SetFavoriteRepo(favRepo)
	return h, noteRepo, favRepo, followingRepo
}

func postExtra(h func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	_ = h(c)
	return rec
}

// --- Clips (#1554) ---

// notes/clips は note を含む public clip を Clip entity で返す。private clip と
// 他人 clip(非public)は除外、public なら他人の clip も含む。
func TestClips_ReturnsPublicClips(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	// clip ID は createdAt 復元のため valid aidx である必要がある。
	idGen, _ := id.NewGenerator("aidx")
	c1 := idGen.Generate(time.Now())
	c2 := idGen.Generate(time.Now())
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	clipRepo := testutil.NewMockClipRepository()
	clipRepo.Clips[c1] = &model.Clip{ID: c1, UserID: "u2", Name: "pub", IsPublic: true}
	clipRepo.Clips[c2] = &model.Clip{ID: c2, UserID: "u1", Name: "priv", IsPublic: false}
	clipNoteRepo := testutil.NewMockClipNoteRepository()
	clipNoteRepo.Entries["e1"] = &model.ClipNote{ID: "e1", ClipID: c1, NoteID: "n1"}
	clipNoteRepo.Entries["e2"] = &model.ClipNote{ID: "e2", ClipID: c2, NoteID: "n1"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "u2", UsernameLower: "u2"}
	h.SetUserRepo(userRepo)
	h.SetClipRepos(clipRepo, clipNoteRepo, testutil.NewMockClipFavoriteRepository())

	rec := postExtra(h.Clips, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	// public clip c1 のみ (private c2 は除外)。
	require.Len(t, got, 1)
	assert.Equal(t, c1, got[0]["id"])
	assert.Equal(t, true, got[0]["isPublic"])
	user, ok := got[0]["user"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "u2", user["username"])
	shapetest.Assert(t, "Clip", got[0])
}

// note が存在しなければ NO_SUCH_NOTE (clips 固有 UUID 47db1a1c)。
func TestClips_NoSuchNote(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	h.SetClipRepos(testutil.NewMockClipRepository(), testutil.NewMockClipNoteRepository(), testutil.NewMockClipFavoriteRepository())
	rec := postExtra(h.Clips, `{"noteId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_NOTE")
	assert.Contains(t, rec.Body.String(), "47db1a1c-b0af-458d-8fb4-986e4efafe1e")
}

// clip repo 未配線時は旧 stub 互換で空配列。
func TestClips_NoRepoDegrades(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.Clips, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

func TestClips_InvalidParam(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	h.SetClipRepos(testutil.NewMockClipRepository(), testutil.NewMockClipNoteRepository(), testutil.NewMockClipFavoriteRepository())
	rec := postExtra(h.Clips, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Favorites ---

func TestFavoritesCreate_Success(t *testing.T) {
	h, noteRepo, favRepo := newExtraHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	rec := postExtra(h.FavoritesCreate, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, favRepo.Favorites, 1)
}

func TestFavoritesCreate_AlreadyFavorited(t *testing.T) {
	h, noteRepo, favRepo := newExtraHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	favRepo.Favorites["u1:n1"] = &model.NoteFavorite{ID: "f1", UserID: "u1", NoteID: "n1"}
	rec := postExtra(h.FavoritesCreate, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestFavoritesCreate_NoteNotFound(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.FavoritesCreate, `{"noteId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFavoritesCreate_InvalidParam(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.FavoritesCreate, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// favoriteRepo 未配線時の guard。queryService が wire 済みで visibility は
// 通る public note でも、favoriteRepo nil で 500 を返す挙動を維持する。
func TestFavoritesCreate_NilRepo(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	idGen, _ := id.NewGenerator("aidx")
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, nil, nil, querySvc, nil, nil, nil, nil, idGen)
	// SetFavoriteRepo を呼ばない → favoriteRepo は nil のまま
	rec := postExtra(h.FavoritesCreate, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// queryService が wire されていない場合は visibility check 抜けで favorite
// 化されないよう fail-closed で NO_SUCH_NOTE を返す (#1443、ShowPartialBulk
// と同じ defensive pattern)。
func TestFavoritesCreate_NoQueryServiceRejects(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	favRepo := testutil.NewMockNoteFavoriteRepository()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(noteRepo, nil, nil, nil, nil, nil, nil, nil, idGen) // queryService=nil
	h.SetFavoriteRepo(favRepo)
	rec := postExtra(h.FavoritesCreate, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, favRepo.Favorites,
		"FavoritesCreate must not persist when queryService is unwired so unchecked notes never bypass visibility filtering")
}

// --- Favorites visibility regression (#1443) ---

// followers visibility note を非 follower viewer が favorite 化しようとすると
// 404 (NO_SUCH_NOTE) で存在隠蔽する。
func TestFavoritesCreate_FollowersNote_NonFollower_Hidden(t *testing.T) {
	h, noteRepo, favRepo, _ := newExtraHandlerWithFollowing(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityFollowers}
	rec := postExtra(h.FavoritesCreate, `{"noteId":"n1"}`, &model.User{ID: "u2"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_NOTE")
	assert.Empty(t, favRepo.Favorites)
}

// followers visibility note でも、follower viewer は favorite 化できる。
func TestFavoritesCreate_FollowersNote_Follower_OK(t *testing.T) {
	h, noteRepo, favRepo, followingRepo := newExtraHandlerWithFollowing(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityFollowers}
	followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "u2", FolloweeID: "u1"}
	rec := postExtra(h.FavoritesCreate, `{"noteId":"n1"}`, &model.User{ID: "u2"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, favRepo.Favorites, 1)
}

// followers visibility note を author 本人が favorite 化するのは常に可。
func TestFavoritesCreate_FollowersNote_Author_OK(t *testing.T) {
	h, noteRepo, favRepo, _ := newExtraHandlerWithFollowing(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityFollowers}
	rec := postExtra(h.FavoritesCreate, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, favRepo.Favorites, 1)
}

// specified visibility note を visibleUserIds 外の viewer が favorite 化
// しようとすると 404 で存在隠蔽。
func TestFavoritesCreate_SpecifiedNote_NotInList_Hidden(t *testing.T) {
	h, noteRepo, favRepo, _ := newExtraHandlerWithFollowing(t)
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", Visibility: model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{"u3"},
	}
	rec := postExtra(h.FavoritesCreate, `{"noteId":"n1"}`, &model.User{ID: "u2"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_NOTE")
	assert.Empty(t, favRepo.Favorites)
}

// specified visibility note を visibleUserIds 対象の viewer は favorite
// 化できる。
func TestFavoritesCreate_SpecifiedNote_InList_OK(t *testing.T) {
	h, noteRepo, favRepo, _ := newExtraHandlerWithFollowing(t)
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", Visibility: model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{"u2"},
	}
	rec := postExtra(h.FavoritesCreate, `{"noteId":"n1"}`, &model.User{ID: "u2"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, favRepo.Favorites, 1)
}

// --- Favorites achievement (#1762) ---

type recordingGranter struct {
	calls [][2]string
	err   error
}

func (g *recordingGranter) Grant(_ context.Context, userID, name string) (bool, error) {
	g.calls = append(g.calls, [2]string{userID, name})
	if g.err != nil {
		return false, g.err
	}
	return true, nil
}

// local note を他人が favorite すると著者に myNoteFavorited1 が付与される。
func TestFavoritesCreate_GrantsMyNoteFavorited1(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	g := &recordingGranter{}
	h.SetAchievementGranter(g)
	rec := postExtra(h.FavoritesCreate, `{"noteId":"n1"}`, &model.User{ID: "u2"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, g.calls, 1)
	assert.Equal(t, "u1", g.calls[0][0])
	assert.Equal(t, coreachievement.MyNoteFavorited1, g.calls[0][1])
}

// 自分のノートを favorite しても付与しない (upstream note.userId !== me.id)。
func TestFavoritesCreate_SelfFavorite_NoGrant(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	g := &recordingGranter{}
	h.SetAchievementGranter(g)
	rec := postExtra(h.FavoritesCreate, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, g.calls)
}

// remote note (UserHost != nil) では著者に付与しない (upstream note.userHost == null)。
func TestFavoritesCreate_RemoteNote_NoGrant(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	host := "remote.example"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "ru1", UserHost: &host, Visibility: model.NoteVisibilityPublic}
	g := &recordingGranter{}
	h.SetAchievementGranter(g)
	rec := postExtra(h.FavoritesCreate, `{"noteId":"n1"}`, &model.User{ID: "u2"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, g.calls)
}

// grant が失敗しても favorite (204) は成功する (best-effort)。
func TestFavoritesCreate_GrantError_StillFavorites(t *testing.T) {
	h, noteRepo, favRepo := newExtraHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	g := &recordingGranter{err: errors.New("boom")}
	h.SetAchievementGranter(g)
	rec := postExtra(h.FavoritesCreate, `{"noteId":"n1"}`, &model.User{ID: "u2"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, favRepo.Favorites, 1)
}

func TestFavoritesDelete_Success(t *testing.T) {
	h, noteRepo, favRepo := newExtraHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	favRepo.Favorites["u1:n1"] = &model.NoteFavorite{UserID: "u1", NoteID: "n1"}
	rec := postExtra(h.FavoritesDelete, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, favRepo.Favorites)
}

// TestFavoritesDelete_NoSuchNote: note が存在しなければ NO_SUCH_NOTE (#1538)。
func TestFavoritesDelete_NoSuchNote(t *testing.T) {
	h, _, favRepo := newExtraHandler(t)
	favRepo.Favorites["u1:n1"] = &model.NoteFavorite{UserID: "u1", NoteID: "n1"}
	rec := postExtra(h.FavoritesDelete, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_NOTE")
	assert.Contains(t, rec.Body.String(), "80848a2c-398f-4343-baa9-df1d57696c56")
}

// TestFavoritesDelete_NotFavorited: note は在るが favorite 行が無ければ
// NOT_FAVORITED (旧実装は未 favorite でも 204、#1538)。
func TestFavoritesDelete_NotFavorited(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	rec := postExtra(h.FavoritesDelete, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NOT_FAVORITED")
	assert.Contains(t, rec.Body.String(), "b625fc69-635e-45e9-86f4-dbefbef35af5")
}

func TestFavoritesDelete_InvalidParam(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.FavoritesDelete, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFavoritesDelete_NilRepo(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(testutil.NewMockNoteRepository(), nil, nil, nil, nil, nil, nil, nil, idGen)
	rec := postExtra(h.FavoritesDelete, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Featured ---

func TestFeatured_Success(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: "public", User: &model.User{ID: "u1"}}
	rec := postExtra(h.Featured, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// #1491 audit 指摘 6: NotEmpty だけだと body が `null` / 空配列 / shape
	// 違いの fail-open regression を取り逃す。seeded id がそのまま返ることを fix。
	require.Len(t, resp, 1)
	assert.Equal(t, "n1", resp[0]["id"])
}

func TestFeatured_InvalidJSON(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.Featured, `invalid`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #1682 notes/featured は viewer が mute / 被block した author の note を除外する
// (upstream featured.ts:99-107 の isUserRelated)。
func TestFeatured_FiltersMutedAuthor(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["fn"] = &model.Note{ID: "fn", UserID: "muted", Visibility: "public", User: &model.User{ID: "muted"}}
	mutingRepo := testutil.NewMockMutingRepository()
	mutingRepo.Mutings["m1"] = &model.Muting{ID: "m1", MuterID: "viewer", MuteeID: "muted"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["viewer"] = &model.User{ID: "viewer", Username: "viewer", UsernameLower: "viewer"}
	h.SetMutingRepo(mutingRepo)
	h.SetBlockingRepo(testutil.NewMockBlockingRepository())
	h.SetUserRepo(userRepo)

	rec := postExtra(h.Featured, `{}`, &model.User{ID: "viewer"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp, "mute した author の featured note は除外される")
}

// channelId 指定時は当該チャンネルに属するノートだけが返ること (#489)。
// handler が channelId を bind せずに repo に渡し忘れると、ハイライト
// タブで他チャンネル / グローバルのノートが混入する。
func TestFeatured_ChannelFilter(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	chID := "ch1"
	noteRepo.Notes["n_ch"] = &model.Note{ID: "n_ch", UserID: "u1", Visibility: "public", ChannelID: &chID, User: &model.User{ID: "u1"}}
	noteRepo.Notes["n_glb"] = &model.Note{ID: "n_glb", UserID: "u1", Visibility: "public", User: &model.User{ID: "u1"}}

	rec := postExtra(h.Featured, `{"channelId":"ch1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "n_ch", resp[0]["id"])
}

// --- Unrenote ---

func TestUnrenote_Success(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	renoteID := "orig"
	noteRepo.Notes["orig"] = &model.Note{ID: "orig", UserID: "u1"}
	noteRepo.Notes["rn1"] = &model.Note{ID: "rn1", UserID: "u1", RenoteID: &renoteID, Text: nil}
	rec := postExtra(h.Unrenote, `{"noteId":"orig"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, noteRepo.Notes, "rn1")
}

func TestUnrenote_ExistingTargetWithoutRenoteIsNoOp(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["orig"] = &model.Note{ID: "orig", UserID: "u2"}

	rec := postExtra(h.Unrenote, `{"noteId":"orig"}`, &model.User{ID: "u1"})

	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestUnrenote_DeletesQuotesAndAllMatchingRenotes(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	renoteID := "orig"
	quoteText := "quote"
	otherRenoteID := "other"
	noteRepo.Notes["orig"] = &model.Note{ID: "orig", UserID: "u2"}
	noteRepo.Notes["rn1"] = &model.Note{ID: "rn1", UserID: "u1", RenoteID: &renoteID}
	noteRepo.Notes["quote1"] = &model.Note{ID: "quote1", UserID: "u1", RenoteID: &renoteID, Text: &quoteText}
	noteRepo.Notes["other-user"] = &model.Note{ID: "other-user", UserID: "u3", RenoteID: &renoteID}
	noteRepo.Notes["other-target"] = &model.Note{ID: "other-target", UserID: "u1", RenoteID: &otherRenoteID}

	rec := postExtra(h.Unrenote, `{"noteId":"orig"}`, &model.User{ID: "u1"})

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, noteRepo.Notes, "rn1")
	assert.NotContains(t, noteRepo.Notes, "quote1")
	assert.Contains(t, noteRepo.Notes, "other-user")
	assert.Contains(t, noteRepo.Notes, "other-target")
}

func TestUnrenote_NotFound(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.Unrenote, `{"noteId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUnrenote_InvalidParam(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.Unrenote, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Mentions ---

func TestMentions_Success(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u2", Visibility: "public", Mentions: []string{"u1"}, User: &model.User{ID: "u2"}}
	rec := postExtra(h.Mentions, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	// #1491 audit 指摘 6: 旧 test は status のみで body 未検証。seeded id が
	// 返ることを fix し、push-down 経路 (#1441 / #1484) の under-fill / 空 body
	// regression を取れるようにする。
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "n1", resp[0]["id"])
}

func TestMentions_InvalidJSON(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.Mentions, `invalid`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #1554 mentions は viewer が mute した author の note を除外する。
func TestMentions_FiltersMutedAuthor(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["mm1"] = &model.Note{ID: "mm1", UserID: "muted", Visibility: "public", Mentions: []string{"u1"}, User: &model.User{ID: "muted"}}
	mutingRepo := testutil.NewMockMutingRepository()
	mutingRepo.Mutings["mx"] = &model.Muting{ID: "mx", MuterID: "u1", MuteeID: "muted"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "u1", UsernameLower: "u1"}
	h.SetMutingRepo(mutingRepo)
	h.SetBlockingRepo(testutil.NewMockBlockingRepository())
	h.SetUserRepo(userRepo)

	ids := mentionIDs(t, postExtra(h.Mentions, `{}`, &model.User{ID: "u1"}))
	assert.False(t, ids["mm1"], "mute した author の mention note は除外される")
}

// #1554 mentions は muted thread の note を除外する (generateMutedNoteThreadQuery)。
func TestMentions_FiltersThreadMute(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	thread := "rootthread"
	noteRepo.Notes["mt1"] = &model.Note{ID: "mt1", UserID: "author", Visibility: "public", Mentions: []string{"u1"}, ThreadID: &thread, User: &model.User{ID: "author"}}
	tmRepo := testutil.NewMockNoteThreadMutingRepository()
	require.NoError(t, tmRepo.Create(&model.NoteThreadMuting{UserID: "u1", ThreadID: thread}))
	h.SetThreadMutingRepo(tmRepo)

	ids := mentionIDs(t, postExtra(h.Mentions, `{}`, &model.User{ID: "u1"}))
	assert.False(t, ids["mt1"], "muted thread の mention note は除外される")
}

func mentionIDs(t *testing.T, rec *httptest.ResponseRecorder) map[string]bool {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	ids := map[string]bool{}
	for _, n := range out {
		ids[n["id"].(string)] = true
	}
	return ids
}

// #2106 N27: 非 follower でも mention されていれば followers note は read 経路 (notes/mentions)
// で見える (upstream generateVisibilityQuery の mentions cross-visibility、#1441 の read-side
// 除外を revert)。main-stream realtime push の #1472 strict gate は別途維持 (本 read 緩和とは独立)。
func TestMentions_FollowersMentionedNonFollowerIncluded(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["m_fol"] = &model.Note{ID: "m_fol", UserID: "author", Mentions: []string{"victim"}, Visibility: "followers", User: &model.User{ID: "author"}}

	ids := mentionIDs(t, postExtra(h.Mentions, `{}`, &model.User{ID: "victim"}))
	assert.True(t, ids["m_fol"], "mention されていれば非 follower でも followers note が read 経路で見える")
}

// #2106 N27: visibleUserIds 非対象でも mention されていれば specified note は read 経路で
// 見える (mentions cross-visibility、#1441 の read-side 除外を revert)。
func TestMentions_SpecifiedMentionedIncluded(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	// mentions に victim を含むが visibleUserIds は other のみ。
	noteRepo.Notes["m_spec"] = &model.Note{ID: "m_spec", UserID: "author", Mentions: []string{"victim"}, Visibility: "specified", VisibleUserIDs: []string{"other"}, User: &model.User{ID: "author"}}

	ids := mentionIDs(t, postExtra(h.Mentions, `{"visibility":"specified"}`, &model.User{ID: "victim"}))
	assert.True(t, ids["m_spec"], "mention されていれば visibleUserIds 非対象でも specified note が見える")
}

// #1451: 未指定 (default) は upstream TS と同じく全種別を返す。public mention に
// 加えて specified(DM、me が visibleUserIds 対象) mention も含まれる (旧実装は
// default で specified を除外していたが TS に揃えて含める)。
func TestMentions_DefaultIncludesAllVisibilities(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["m_pub"] = &model.Note{ID: "m_pub", UserID: "author", Mentions: []string{"me"}, Visibility: "public", User: &model.User{ID: "author"}}
	noteRepo.Notes["m_dm"] = &model.Note{ID: "m_dm", UserID: "author", Mentions: []string{"me"}, Visibility: "specified", VisibleUserIDs: []string{"me"}, User: &model.User{ID: "author"}}

	ids := mentionIDs(t, postExtra(h.Mentions, `{}`, &model.User{ID: "me"}))
	assert.True(t, ids["m_pub"], "default は public mention を含む")
	assert.True(t, ids["m_dm"], "default は specified(DM) mention も含む (TS 一致)")
}

// #1451: visibility 指定は exact-match。public 指定では public のみ返り、
// specified は出ない (旧 binary split では非specified に specified 以外が全部
// 入っていたが、TS の note.visibility = <値> に合わせる)。
func TestMentions_VisibilityExactMatch(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["m_pub"] = &model.Note{ID: "m_pub", UserID: "author", Mentions: []string{"me"}, Visibility: "public", User: &model.User{ID: "author"}}
	noteRepo.Notes["m_dm"] = &model.Note{ID: "m_dm", UserID: "author", Mentions: []string{"me"}, Visibility: "specified", VisibleUserIDs: []string{"me"}, User: &model.User{ID: "author"}}

	ids := mentionIDs(t, postExtra(h.Mentions, `{"visibility":"public"}`, &model.User{ID: "me"}))
	assert.True(t, ids["m_pub"], "public 指定で public mention は出る")
	assert.False(t, ids["m_dm"], "public 指定で specified は出ない (exact-match)")
}

// #1484: 本文 @mention の無い specified DM (viewer ∈ visibleUserIds のみ) も
// Direct タブ (visibility=specified) に出る。UI で宛先を選んだだけの DM を
// 受信者が取りこぼさないことを固定する。
func TestMentions_SpecifiedVisibleUserIDsOnly(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	// mentions は空、visibleUserIds にだけ recipient を含む specified DM。
	noteRepo.Notes["m_dm_vu"] = &model.Note{ID: "m_dm_vu", UserID: "author", Visibility: "specified", VisibleUserIDs: []string{"recipient"}, User: &model.User{ID: "author"}}

	ids := mentionIDs(t, postExtra(h.Mentions, `{"visibility":"specified"}`, &model.User{ID: "recipient"}))
	assert.True(t, ids["m_dm_vu"], "本文 @mention の無い specified DM も visibleUserIds 経由で Direct タブに出る")

	// 宛先でない viewer には出ない (#1441 gate との整合)。
	ids = mentionIDs(t, postExtra(h.Mentions, `{"visibility":"specified"}`, &model.User{ID: "stranger"}))
	assert.False(t, ids["m_dm_vu"], "宛先でない viewer には specified DM が出ない")
}

// --- UserListTimeline ---

func TestUserListTimeline_Success(t *testing.T) {
	// #1681 で UserListTimeline が UseMutingSubquery を立てるようになり、bare
	// mock の ListByUserList は muting subquery を未実装で panic する。可視性 /
	// filter の実 SQL は repository test で覆うので、ここでは push-down を行わない
	// userListNotesRepo fake で「handler が repo の返り値をそのまま pack する」
	// 経路だけを検証する。
	n1 := &model.Note{ID: "n1", UserID: "member1", Visibility: "public", User: &model.User{ID: "member1"}}
	noteRepo := &userListNotesRepo{MockNoteRepository: testutil.NewMockNoteRepository(), rows: []*model.Note{n1}}
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	querySvc := corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository())
	h := NewHandler(noteRepo, corenote.NewCreateService(noteRepo, pollRepo, idGen, nil), corenote.NewDeleteService(noteRepo), querySvc, nil, nil, nil, nil, idGen)
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1", Name: "my list"}
	h.SetUserListRepo(listRepo)

	rec := postExtra(h.UserListTimeline, `{"listId":"l1"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "n1", resp[0]["id"])
}

func TestUserListTimeline_NotOwner(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	listRepo := testutil.NewMockUserListRepository()
	// リストは別ユーザー "other" が所有
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "other", Name: "their list"}
	h.SetUserListRepo(listRepo)
	rec := postExtra(h.UserListTimeline, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_LIST", errObj["code"])
}

func TestUserListTimeline_ListNotFound(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	listRepo := testutil.NewMockUserListRepository()
	h.SetUserListRepo(listRepo)
	// リストが存在しない場合
	rec := postExtra(h.UserListTimeline, `{"listId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_LIST", errObj["code"])
}

func TestUserListTimeline_InvalidParam(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.UserListTimeline, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// userListNotesRepo は ListByUserList で固定の note を返しつつ、handler から
// 渡された listID / filter を記録する fake。#1452 で可視性が repo の SQL
// push-down に移ったため、handler は repo の返り値をそのまま返す。可視性絞り
// 込み自体の検証は repository.TestNoteRepository_ListByUserList_VisibilityPushdown
// で実 SQL に対して行う。
type userListNotesRepo struct {
	*testutil.MockNoteRepository
	rows      []*model.Note
	gotListID string
	gotFilter model.TimelineDBFilter
}

func (r *userListNotesRepo) ListByUserList(listID string, _ int, _, _ string, filter model.TimelineDBFilter) ([]*model.Note, error) {
	r.gotListID = listID
	r.gotFilter = filter
	return r.rows, nil
}

// #1452 / #1498: handler は viewer (= me.ID) と renote/file 系 param を
// TimelineDBFilter に詰めて repo に渡し、可視性 / renote 絞り込みは repo の SQL
// push-down に委ねる。post-fetch FilterVisible は撤去済みなので handler は repo の
// 返り値をそのまま pack する。
func TestUserListTimeline_PassesFilterToRepo(t *testing.T) {
	pub := &model.Note{ID: "ul_pub", UserID: "B", Visibility: "public", User: &model.User{ID: "B"}}
	noteRepo := &userListNotesRepo{MockNoteRepository: testutil.NewMockNoteRepository(), rows: []*model.Note{pub}}
	fRepo := testutil.NewMockFollowingRepository()
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "A"}
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	querySvc := corenote.NewQueryService(noteRepo, fRepo)
	h := NewHandler(noteRepo, corenote.NewCreateService(noteRepo, pollRepo, idGen, nil), corenote.NewDeleteService(noteRepo), querySvc, nil, nil, nil, nil, idGen)
	h.SetUserListRepo(listRepo)

	rec := postExtra(h.UserListTimeline,
		`{"listId":"l1","withRenotes":false,"withFiles":true,`+
			`"includeMyRenotes":false,"includeRenotedMyNotes":false,"includeLocalRenotes":false}`,
		&model.User{ID: "A"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	// viewer (me.ID) / listId / renote・file 系 param が filter 経由で repo に渡る。
	assert.Equal(t, "l1", noteRepo.gotListID, "listId が repo に渡る")
	assert.Equal(t, "A", noteRepo.gotFilter.ViewerID, "viewer (me.ID) が filter.ViewerID として渡る")
	require.NotNil(t, noteRepo.gotFilter.WithRenotes)
	assert.False(t, *noteRepo.gotFilter.WithRenotes, "withRenotes=false が filter に渡る")
	assert.True(t, noteRepo.gotFilter.WithFiles, "withFiles=true が filter に渡る")
	// #1504 audit follow-up: Include*Renotes も applyTimelineFilter 経路で
	// 効くため、handler が forward しているかを fix。1 つでも落ちると
	// user-list-timeline で pure-renote 関連の filter が silently 効かなく
	// なる回帰になる。
	require.NotNil(t, noteRepo.gotFilter.IncludeMyRenotes)
	assert.False(t, *noteRepo.gotFilter.IncludeMyRenotes, "includeMyRenotes=false が filter に渡る")
	require.NotNil(t, noteRepo.gotFilter.IncludeRenotedMyNotes)
	assert.False(t, *noteRepo.gotFilter.IncludeRenotedMyNotes, "includeRenotedMyNotes=false が filter に渡る")
	require.NotNil(t, noteRepo.gotFilter.IncludeLocalRenotes)
	assert.False(t, *noteRepo.gotFilter.IncludeLocalRenotes, "includeLocalRenotes=false が filter に渡る")
	// #1681: base-filter (user-mute / renote-mute subquery) が user-list-timeline
	// でも立つことを確認。
	assert.True(t, noteRepo.gotFilter.UseMutingSubquery, "UseMutingSubquery が立つ (#1681)")
	assert.True(t, noteRepo.gotFilter.UseRenoteMutingSubquery, "UseRenoteMutingSubquery が立つ (#1681)")
	// post-fetch filter は無いので repo の返り値がそのまま返る。
	require.Len(t, out, 1)
	assert.Equal(t, "ul_pub", out[0]["id"])
}

func TestUserListTimeline_WithoutUserListRepo(t *testing.T) {
	// userListRepo が nil の場合は所有権チェックをスキップして DB クエリに進む。
	// #1681 で muting subquery が立つため bare mock は panic する → push-down を
	// 行わない userListNotesRepo fake を使う。
	noteRepo := &userListNotesRepo{MockNoteRepository: testutil.NewMockNoteRepository(), rows: nil}
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(noteRepo, nil, nil, nil, nil, nil, nil, nil, idGen)
	rec := postExtra(h.UserListTimeline, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

type failingListByUserListRepo struct{ *testutil.MockNoteRepository }

func (f *failingListByUserListRepo) ListByUserList(_ string, _ int, _, _ string, _ model.TimelineDBFilter) ([]*model.Note, error) {
	return nil, testutil.ErrNotFound
}

func TestUserListTimeline_RepoError(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&failingListByUserListRepo{testutil.NewMockNoteRepository()}, nil, nil, nil, nil, nil, nil, nil, idGen)
	rec := postExtra(h.UserListTimeline, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- SearchByTag ---

func TestSearchByTag_Success(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Tags: []string{"golang"}, Visibility: "public", User: &model.User{ID: "u1"}}
	rec := postExtra(h.SearchByTag, `{"tag":"golang"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	// #1491 audit 指摘 6: status 200 だけでは body=null / 別 id を取り損ねる。
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "n1", resp[0]["id"])
}

func TestSearchByTag_InvalidParam(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.SearchByTag, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #1554 search-by-tag の reply filter: reply=true は reply note のみ返す。
func TestSearchByTag_ReplyFilter(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	parent := "p1"
	noteRepo.Notes["plain"] = &model.Note{ID: "plain", UserID: "u1", Tags: []string{"t"}, Visibility: "public", User: &model.User{ID: "u1"}}
	noteRepo.Notes["rep"] = &model.Note{ID: "rep", UserID: "u1", Tags: []string{"t"}, Visibility: "public", ReplyID: &parent, User: &model.User{ID: "u1"}}
	rec := postExtra(h.SearchByTag, `{"tag":"t","reply":true}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "rep", resp[0]["id"])
}

// #1554 search-by-tag は viewer が mute した author の note を除外する。
func TestSearchByTag_FiltersMutedAuthor(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	noteRepo.Notes["mn"] = &model.Note{ID: "mn", UserID: "muted", Tags: []string{"t"}, Visibility: "public", User: &model.User{ID: "muted"}}
	mutingRepo := testutil.NewMockMutingRepository()
	mutingRepo.Mutings["m1"] = &model.Muting{ID: "m1", MuterID: "viewer", MuteeID: "muted"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["viewer"] = &model.User{ID: "viewer", Username: "viewer", UsernameLower: "viewer"}
	h.SetMutingRepo(mutingRepo)
	h.SetBlockingRepo(testutil.NewMockBlockingRepository())
	h.SetUserRepo(userRepo)

	rec := postExtra(h.SearchByTag, `{"tag":"t"}`, &model.User{ID: "viewer"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp, "mute した author の note は除外される")
}

// #1683 query は外側 OR・内側 AND の複合タグ検索。
func TestSearchByTag_QueryOrOfAnd(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	// goAndRust は go+rust 両方、webOnly は web、goOnly は go のみ。
	noteRepo.Notes["goAndRust"] = &model.Note{ID: "goAndRust", UserID: "u1", Tags: []string{"go", "rust"}, Visibility: "public", User: &model.User{ID: "u1"}}
	noteRepo.Notes["webOnly"] = &model.Note{ID: "webOnly", UserID: "u1", Tags: []string{"web"}, Visibility: "public", User: &model.User{ID: "u1"}}
	noteRepo.Notes["goOnly"] = &model.Note{ID: "goOnly", UserID: "u1", Tags: []string{"go"}, Visibility: "public", User: &model.User{ID: "u1"}}

	// query [["go","rust"],["web"]] = (go AND rust) OR (web)。
	// goAndRust (両方) と webOnly はヒット、goOnly (go だけ、rust 欠) は外れる。
	rec := postExtra(h.SearchByTag, `{"query":[["go","rust"],["web"]]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ids := map[string]bool{}
	for _, n := range resp {
		ids[n["id"].(string)] = true
	}
	assert.True(t, ids["goAndRust"], "go AND rust を満たす note はヒット")
	assert.True(t, ids["webOnly"], "web group を満たす note はヒット")
	assert.False(t, ids["goOnly"], "go だけ (rust 欠) は (go AND rust) を満たさず外れる")
}

func TestSearchByTag_QueryArrayEmpty(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	// query が空配列 → tag も空 → 400
	assert.Equal(t, http.StatusBadRequest, postExtra(h.SearchByTag, `{"query":[]}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, postExtra(h.SearchByTag, `{"query":[[]]}`, nil).Code)
}

// search-by-tag の visibility push-down (#1439)。discovery 系なので匿名/非
// follower には followers / specified note を返さず、author 本人 / follower /
// visibleUserIds 対象には返すことを固定する。
func seedSearchVisibilityNotes(noteRepo *testutil.MockNoteRepository) {
	noteRepo.Notes["t_pub"] = &model.Note{ID: "t_pub", UserID: "author", Tags: []string{"tag"}, Visibility: "public", User: &model.User{ID: "author"}}
	noteRepo.Notes["t_fol"] = &model.Note{ID: "t_fol", UserID: "author", Tags: []string{"tag"}, Visibility: "followers", User: &model.User{ID: "author"}}
	noteRepo.Notes["t_spec"] = &model.Note{ID: "t_spec", UserID: "author", Tags: []string{"tag"}, Visibility: "specified", VisibleUserIDs: []string{"allowed"}, User: &model.User{ID: "author"}}
}

func searchTagIDs(t *testing.T, rec *httptest.ResponseRecorder) map[string]bool {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	ids := map[string]bool{}
	for _, n := range out {
		ids[n["id"].(string)] = true
	}
	return ids
}

func TestSearchByTag_AnonymousExcludesNonPublic(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	seedSearchVisibilityNotes(noteRepo)
	ids := searchTagIDs(t, postExtra(h.SearchByTag, `{"tag":"tag"}`, nil))
	assert.True(t, ids["t_pub"], "public は匿名に見える")
	assert.False(t, ids["t_fol"], "followers は匿名に漏らさない")
	assert.False(t, ids["t_spec"], "specified は対象外に漏らさない")
}

func TestSearchByTag_NonFollowerExcludesFollowers(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	seedSearchVisibilityNotes(noteRepo)
	// stranger は follow なし / specified 対象外 -> public のみ。
	ids := searchTagIDs(t, postExtra(h.SearchByTag, `{"tag":"tag"}`, &model.User{ID: "stranger"}))
	assert.True(t, ids["t_pub"])
	assert.False(t, ids["t_fol"])
	assert.False(t, ids["t_spec"])
}

func TestSearchByTag_FollowerSeesFollowers(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	seedSearchVisibilityNotes(noteRepo)
	noteRepo.Following = map[string][]string{"follower": {"author"}}
	ids := searchTagIDs(t, postExtra(h.SearchByTag, `{"tag":"tag"}`, &model.User{ID: "follower"}))
	assert.True(t, ids["t_pub"])
	assert.True(t, ids["t_fol"], "follower は followers note を見られる")
	assert.False(t, ids["t_spec"])
}

func TestSearchByTag_SpecifiedTargetAndAuthorSee(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	seedSearchVisibilityNotes(noteRepo)
	// visibleUserIds 対象は public + specified。
	allowedIDs := searchTagIDs(t, postExtra(h.SearchByTag, `{"tag":"tag"}`, &model.User{ID: "allowed"}))
	assert.True(t, allowedIDs["t_pub"])
	assert.True(t, allowedIDs["t_spec"], "visibleUserIds 対象は specified を見られる")
	assert.False(t, allowedIDs["t_fol"])
	// author 本人は全 visibility を見られる。
	authorIDs := searchTagIDs(t, postExtra(h.SearchByTag, `{"tag":"tag"}`, &model.User{ID: "author"}))
	assert.True(t, authorIDs["t_pub"] && authorIDs["t_fol"] && authorIDs["t_spec"], "author 本人は全て見られる")
}

type failingSearchByTagRepo struct{ *testutil.MockNoteRepository }

func (f *failingSearchByTagRepo) SearchByTag(_ [][]string, _ string, _ int, _, _ string, _ model.NoteSearchTagFilter) ([]*model.Note, error) {
	return nil, testutil.ErrNotFound
}

func TestSearchByTag_RepoError(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&failingSearchByTagRepo{testutil.NewMockNoteRepository()}, nil, nil, nil, nil, nil, nil, nil, idGen)
	rec := postExtra(h.SearchByTag, `{"tag":"x"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- Clips ---

func TestClips_Success(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.Clips, `{"noteId":"n1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- Translate ---

// #2010: canUseTranslator policy が false なら note fetch より前に 503 UNAVAILABLE を
// 返す (汎用 RequireRolePolicy の 403 ROLE_PERMISSION_DENIED ではない、upstream translate.ts:72-75)。
func TestTranslate_CannotUseTranslator_503Unavailable(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	h.SetPolicyProvider(&draftStubPolicyProvider{policies: map[string]any{"canUseTranslator": false}})
	txt := "hello"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: &txt}
	rec := postExtra(h.Translate, `{"noteId":"n1","targetLang":"en"}`, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "canUseTranslator false は 400 UNAVAILABLE (#2010)")
	assert.Contains(t, rec.Body.String(), "UNAVAILABLE")
	assert.Contains(t, rec.Body.String(), "50a70314", "translate 固有 error id")
}

// #2010: canUseTranslator true なら policy gate を通過する。null-text note を使い、
// gate 通過後に note fetch → text==null で 204 に到達することで gate-pass を確認する
// (gate で弾かれていれば note fetch 前に 400 になる)。
func TestTranslate_CanUseTranslator_PassesPolicyGate(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	h.SetPolicyProvider(&draftStubPolicyProvider{policies: map[string]any{"canUseTranslator": true}})
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: nil}
	rec := postExtra(h.Translate, `{"noteId":"n1","targetLang":"en"}`, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusNoContent, rec.Code, "policy gate 通過後 null-text 204 に到達 (#2010)")
}

func TestTranslate_NoTranslator(t *testing.T) {
	// #1948-17: translator-nil(unavailable) check は note fetch / text check の後に
	// 移動したので、可視 + text 有りの note を seed して 503 path を踏ませる。
	h, noteRepo, _ := newExtraHandler(t)
	txt := "hello"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: &txt}
	rec := postExtra(h.Translate, `{"noteId":"n1","targetLang":"en"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #1948-17: text == null の note は upstream translate.ts:86-88 と同じく 204
// (res optional, return;)。mk-go の旧 400 CANNOT_TRANSLATE は廃止。translator
// unavailable でも null-text は 204 が先に返る。
func TestTranslate_NullText_204(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	// text=nil (file/poll only note)。public で可視。translator は未配線。
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: nil}
	rec := postExtra(h.Translate, `{"noteId":"n1","targetLang":"en"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "null-text は 204 no-op (#1948-17)")
	assert.Empty(t, rec.Body.String(), "body は空")
}

func TestTranslate_InvalidParam(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.Translate, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// newTranslateHandler は translator (non-nil dummy DeepL) + queryService +
// followingRepo を wire した handler。dummy DeepL の Translate は呼ばれた
// 場合のみ network に出るので、visibility gate が translator.Translate より
// 前で弾くこと (= DeepL quota を消費しないこと) を、ネットワークなしの
// テストで検証できる (#1445)。
func newTranslateHandler(t *testing.T) (*Handler, *testutil.MockNoteRepository, *testutil.MockFollowingRepository) {
	t.Helper()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	idGen, _ := id.NewGenerator("aidx")
	querySvc := corenote.NewQueryService(noteRepo, followingRepo)
	h := NewHandler(noteRepo, nil, nil, querySvc, nil, nil, nil, nil, idGen)
	h.SetTranslator(translate.NewDeepL("dummy", false, nil))
	return h, noteRepo, followingRepo
}

func translateBody(t *testing.T, rec *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj, _ := resp["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	idStr, _ := errObj["id"].(string)
	return code, idStr
}

// followers note を非 follower / 匿名 viewer が翻訳しようとすると、本文が
// あっても translator に渡さず 400 CANNOT_TRANSLATE_INVISIBLE_NOTE で弾く。
func TestTranslate_FollowersNote_NonFollower_Invisible(t *testing.T) {
	secret := "secret body"
	for _, viewer := range []*model.User{nil, {ID: "u2"}} {
		h, noteRepo, _ := newTranslateHandler(t)
		noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityFollowers, Text: &secret}
		rec := postExtra(h.Translate, `{"noteId":"n1","targetLang":"en"}`, viewer)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		code, idStr := translateBody(t, rec)
		assert.Equal(t, "CANNOT_TRANSLATE_INVISIBLE_NOTE", code)
		assert.Equal(t, "ea29f2ca-c368-43b3-aaf1-5ac3e74bbe5d", idStr)
	}
}

// specified note を visibleUserIds 外の viewer が翻訳しようとすると 400 で弾く。
func TestTranslate_SpecifiedNote_NotInList_Invisible(t *testing.T) {
	secret := "secret body"
	h, noteRepo, _ := newTranslateHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: []string{"u3"}, Text: &secret}
	rec := postExtra(h.Translate, `{"noteId":"n1","targetLang":"en"}`, &model.User{ID: "u2"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	code, _ := translateBody(t, rec)
	assert.Equal(t, "CANNOT_TRANSLATE_INVISIBLE_NOTE", code)
}

// follower / visibleUserIds 対象 / author は visibility gate を通過する。
// #1948-17: text=nil の note を使い null-text 経路 (204) で止め、DeepL を呼ばずに
// 「gate を通った」ことを検証する (invisible なら手前で 400 になる)。
func TestTranslate_VisibleViewers_PassVisibilityGate(t *testing.T) {
	cases := []struct {
		name   string
		note   *model.Note
		viewer *model.User
		follow bool
	}{
		{"follower", &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityFollowers, Text: nil}, &model.User{ID: "u2"}, true},
		{"author", &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityFollowers, Text: nil}, &model.User{ID: "u1"}, false},
		{"specified target", &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: []string{"u2"}, Text: nil}, &model.User{ID: "u2"}, false},
		{"public anonymous", &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: nil}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, noteRepo, fRepo := newTranslateHandler(t)
			noteRepo.Notes["n1"] = tc.note
			if tc.follow {
				fRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: tc.viewer.ID, FolloweeID: "u1"}
			}
			rec := postExtra(h.Translate, `{"noteId":"n1","targetLang":"en"}`, tc.viewer)
			// 可視 → null-text の 204 に到達 (invisible なら 400 で手前 reject)。
			assert.Equal(t, http.StatusNoContent, rec.Code, "visibility gate を通過し null-text 経路 (204) に到達する")
		})
	}
}

// queryService 未配線時は visibility を確認できないため fail-closed で
// CANNOT_TRANSLATE_INVISIBLE_NOTE を返す (public note でも translate しない)。
func TestTranslate_NoQueryServiceRejects(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	secret := "secret body"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: &secret}
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(noteRepo, nil, nil, nil, nil, nil, nil, nil, idGen) // queryService=nil
	h.SetTranslator(translate.NewDeepL("dummy", false, nil))
	rec := postExtra(h.Translate, `{"noteId":"n1","targetLang":"en"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	code, _ := translateBody(t, rec)
	assert.Equal(t, "CANNOT_TRANSLATE_INVISIBLE_NOTE", code)
}

// --- ShowPartialBulk ---

// queryService が wire されているとき、public note は visibility filter を
// 通って返ること。
func TestShowPartialBulk_Success(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, nil, idGen)

	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic,
		User: &model.User{ID: "u1"},
	}
	rec := postExtra(h.ShowPartialBulk, `{"noteIds":["n1"]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"id":"n1"`,
		"public note must be returned when queryService is wired")
}

// #1538: レスポンスは本家 fetchDiffs と同じ {id, reactions, reactionEmojis} の
// 軽量 diff で、full note フィールド (text/visibility/user 等) を含まない。
func TestShowPartialBulk_DiffShape(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	idGen, _ := id.NewGenerator("aidx")
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, nil, nil, querySvc, nil, nil, nil, nil, idGen)
	text := "hello"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic,
		User: &model.User{ID: "u1"}, Text: &text,
		Reactions: datatypes.JSON([]byte(`{"👍":2}`)),
	}
	rec := postExtra(h.ShowPartialBulk, `{"noteIds":["n1"]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.Equal(t, "n1", out[0]["id"])
	_, hasReactions := out[0]["reactions"]
	assert.True(t, hasReactions, "diff must include reactions")
	_, hasReactionEmojis := out[0]["reactionEmojis"]
	assert.True(t, hasReactionEmojis, "diff must include reactionEmojis")
	assert.NotContains(t, out[0], "text", "diff shape must not include full note text")
	assert.NotContains(t, out[0], "visibility", "diff shape must not include visibility")
	assert.NotContains(t, out[0], "user", "diff shape must not include user")
}

func TestShowPartialBulk_Empty(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.ShowPartialBulk, `{"noteIds":[]}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

type failingFindManyRepo struct{ *testutil.MockNoteRepository }

func (f *failingFindManyRepo) FindManyByIDsWithUser(_ []string) ([]*model.Note, error) {
	return nil, testutil.ErrNotFound
}

func TestShowPartialBulk_Error(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&failingFindManyRepo{testutil.NewMockNoteRepository()}, nil, nil, nil, nil, nil, nil, nil, idGen)
	rec := postExtra(h.ShowPartialBulk, `{"noteIds":["n1"]}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestShowPartialBulk_InvalidParam(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.ShowPartialBulk, `invalid`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// queryService 未配線時は note 行があっても空配列を返す fail-closed 挙動
// を担保する (Devin #529 FLAG-1、Show / BulkShow と同種の defensive)。
// router.go で RequireAuth() 無しに公開している endpoint なので、
// followers / specified visibility のノートを匿名閲覧者に漏らさないため
// に重要。
func TestShowPartialBulk_NoQueryServiceRejects(t *testing.T) {
	// #1443 で newExtraHandler が queryService を wire するようになったため、
	// この test では queryService=nil な handler を明示的に組み直して
	// fail-closed 挙動を担保する。
	noteRepo := testutil.NewMockNoteRepository()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(noteRepo, nil, nil, nil, nil, nil, nil, nil, idGen)
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic,
		User: &model.User{ID: "u1"},
	}
	rec := postExtra(h.ShowPartialBulk, `{"noteIds":["n1"]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String(),
		"ShowPartialBulk must return [] when queryService is unwired so notes never bypass visibility filtering")
}

// --- Failing repo tests ---

type failingFavCreateRepo struct {
	*testutil.MockNoteFavoriteRepository
}

func (f *failingFavCreateRepo) Create(_ *model.NoteFavorite) error { return testutil.ErrNotFound }

func TestFavoritesCreate_CreateError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	idGen, _ := id.NewGenerator("aidx")
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, nil, nil, querySvc, nil, nil, nil, nil, idGen)
	h.SetFavoriteRepo(&failingFavCreateRepo{testutil.NewMockNoteFavoriteRepository()})
	rec := postExtra(h.FavoritesCreate, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingFavDeleteRepo struct {
	*testutil.MockNoteFavoriteRepository
}

func (f *failingFavDeleteRepo) Delete(_, _ string) error { return testutil.ErrNotFound }

func TestFavoritesDelete_Error(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	h := NewHandler(noteRepo, nil, nil, nil, nil, nil, nil, nil, idGen)
	// note 存在 + favorite 存在の状態で Delete だけ失敗させ、500 経路を踏ませる。
	failing := &failingFavDeleteRepo{testutil.NewMockNoteFavoriteRepository()}
	failing.Favorites["u1:n1"] = &model.NoteFavorite{UserID: "u1", NoteID: "n1"}
	h.SetFavoriteRepo(failing)
	rec := postExtra(h.FavoritesDelete, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingFeaturedRepo struct{ *testutil.MockNoteRepository }

func (f *failingFeaturedRepo) ListFeatured(_, _ string, _, _ int) ([]*model.Note, error) {
	return nil, testutil.ErrNotFound
}

func TestFeatured_Error(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&failingFeaturedRepo{testutil.NewMockNoteRepository()}, nil, nil, nil, nil, nil, nil, nil, idGen)
	rec := postExtra(h.Featured, `{}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingDeleteNoteRepo struct{ *testutil.MockNoteRepository }

func (f *failingDeleteNoteRepo) Delete(_ *model.Note) error { return testutil.ErrNotFound }

type failingListRenotesRepo struct{ *testutil.MockNoteRepository }

func (f *failingListRenotesRepo) ListRenotesByUser(_, _ string) ([]*model.Note, error) {
	return nil, errors.New("list renotes failed")
}

func TestUnrenote_ListError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["orig"] = &model.Note{ID: "orig", UserID: "u1"}
	failing := &failingListRenotesRepo{MockNoteRepository: noteRepo}
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(failing, nil, corenote.NewDeleteService(failing), nil, nil, nil, nil, nil, idGen)

	rec := postExtra(h.Unrenote, `{"noteId":"orig"}`, &model.User{ID: "u1"})

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestUnrenote_DeleteError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	renoteID := "orig"
	noteRepo.Notes["orig"] = &model.Note{ID: "orig", UserID: "u1"}
	noteRepo.Notes["rn1"] = &model.Note{ID: "rn1", UserID: "u1", RenoteID: &renoteID}
	idGen, _ := id.NewGenerator("aidx")
	deleteSvc := corenote.NewDeleteService(&failingDeleteNoteRepo{noteRepo})
	h := NewHandler(noteRepo, nil, deleteSvc, nil, nil, nil, nil, nil, idGen)
	rec := postExtra(h.Unrenote, `{"noteId":"orig"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingMentionsRepo struct{ *testutil.MockNoteRepository }

func (f *failingMentionsRepo) ListMentions(_, _ string, _ bool, _ int, _, _ string) ([]*model.Note, error) {
	return nil, testutil.ErrNotFound
}

func TestMentions_Error(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&failingMentionsRepo{testutil.NewMockNoteRepository()}, nil, nil, nil, nil, nil, nil, nil, idGen)
	rec := postExtra(h.Mentions, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- upstream ab26d2b7b2: ノート翻訳で CW も翻訳対象に含める ---

// captureTranslateHandler は DeepL へ実際に送られた text を捕まえられる handler を返す。
// 連結結果を直接見ないと「CW を足した」ことを検証できない (200 になるだけでは
// text と CW のどちらを送ったか区別が付かない)。
func captureTranslateHandler(t *testing.T) (*Handler, *testutil.MockNoteRepository, *string) {
	t.Helper()
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sent = r.FormValue("text")
		_, _ = w.Write([]byte(`{"translations":[{"detected_source_language":"JA","text":"translated"}]}`))
	}))
	t.Cleanup(srv.Close)

	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	idGen, _ := id.NewGenerator("aidx")
	querySvc := corenote.NewQueryService(noteRepo, followingRepo)
	h := NewHandler(noteRepo, nil, nil, querySvc, nil, nil, nil, nil, idGen)
	h.SetTranslator(translate.NewDeepLWithClient("key", srv.URL, srv.Client()))
	return h, noteRepo, &sent
}

func TestTranslate_CWIsPrependedToText(t *testing.T) {
	h, noteRepo, sent := captureTranslateHandler(t)
	cw, txt := "注意", "本文"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, CW: &cw, Text: &txt}
	rec := postExtra(h.Translate, `{"noteId":"n1","targetLang":"en"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "注意\n-----\n本文", *sent, "CW を区切り付きで前置する")
}

// text が無くても CW だけあれば翻訳する (2026.9.0 より前は 204 だった)。
func TestTranslate_CWOnlyNote_IsTranslated(t *testing.T) {
	h, noteRepo, sent := captureTranslateHandler(t)
	cw := "注意"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, CW: &cw, Text: nil}
	rec := postExtra(h.Translate, `{"noteId":"n1","targetLang":"en"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, "CW だけでも翻訳する")
	assert.Equal(t, "注意\n-----\n", *sent)
}

// 空白のみ / 空文字は DeepL へ投げずに 204。判定が `text == nil` から
// `strings.TrimSpace(text) == ""` に変わったことを固定する。
func TestTranslate_BlankText_204(t *testing.T) {
	for _, txt := range []string{"", "   ", "\n\t "} {
		h, noteRepo, sent := captureTranslateHandler(t)
		v := txt
		noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: &v}
		rec := postExtra(h.Translate, `{"noteId":"n1","targetLang":"en"}`, nil)
		assert.Equal(t, http.StatusNoContent, rec.Code, "空白のみは 204 (%q)", txt)
		assert.Empty(t, *sent, "DeepL を呼ばない (%q)", txt)
	}
}
