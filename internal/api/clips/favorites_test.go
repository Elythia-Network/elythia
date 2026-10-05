package clips

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coreclip "github.com/elythia-network/elythia/internal/core/clip"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStubHandler(t *testing.T) (*Handler, *testutil.MockClipRepository, *testutil.MockClipFavoriteRepository) {
	t.Helper()
	repo := testutil.NewMockClipRepository()
	noteRepo := testutil.NewMockClipNoteRepository()
	notes := testutil.NewMockNoteRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreclip.NewService(repo, noteRepo, notes, idGen)
	h := NewHandler(svc, idGen)
	favRepo := testutil.NewMockClipFavoriteRepository()
	h.SetFavoriteRepo(favRepo)
	return h, repo, favRepo
}

func postStubWithBody(t *testing.T, handler func(echo.Context) error, body string, userID string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if userID != "" {
		c.Set(string(middleware.UserContextKey), &model.User{ID: userID})
	}
	_ = handler(c)
	return rec
}

func TestClipFavorite_MissingClipID(t *testing.T) {
	h, _, _ := newStubHandler(t)
	rec := postStubWithBody(t, h.Favorite, `{}`, "u1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestClipFavorite_Success(t *testing.T) {
	h, clipRepo, favRepo := newStubHandler(t)
	clipRepo.Clips["cl1"] = &model.Clip{ID: "cl1", UserID: "u1", Name: "test", IsPublic: true}
	rec := postStubWithBody(t, h.Favorite, `{"clipId":"cl1"}`, "u1")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	exists, _ := favRepo.Exists("u1", "cl1")
	assert.True(t, exists)
}

func TestClipFavorite_AlreadyFavorited(t *testing.T) {
	h, clipRepo, favRepo := newStubHandler(t)
	clipRepo.Clips["cl1"] = &model.Clip{ID: "cl1", UserID: "u1", Name: "test", IsPublic: true}
	favRepo.Favorites["u1:cl1"] = &model.ClipFavorite{ID: "f1", UserID: "u1", ClipID: "cl1"}
	// upstream favorite.ts は重複 favorite を ALREADY_FAVORITED 400 で拒否する (#1562)
	rec := postStubWithBody(t, h.Favorite, `{"clipId":"cl1"}`, "u1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ALREADY_FAVORITED")
	assert.Contains(t, rec.Body.String(), "92658936-c625-4273-8326-2d790129256e")
}

func TestClipFavorite_NilRepo(t *testing.T) {
	h, _, _, _ := newHandler(t)
	// favoriteRepo is nil → graceful NoContent
	rec := postStubWithBody(t, h.Favorite, `{"clipId":"cl1"}`, "u1")
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestClipUnfavorite_Success(t *testing.T) {
	h, clipRepo, favRepo := newStubHandler(t)
	clipRepo.Clips["cl1"] = &model.Clip{ID: "cl1", UserID: "u1", Name: "test", IsPublic: true}
	favRepo.Favorites["u1:cl1"] = &model.ClipFavorite{ID: "f1", UserID: "u1", ClipID: "cl1"}
	rec := postStubWithBody(t, h.Unfavorite, `{"clipId":"cl1"}`, "u1")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	exists, _ := favRepo.Exists("u1", "cl1")
	assert.False(t, exists)
}

// upstream unfavorite.ts は clip 不在を NO_SUCH_CLIP、未 favorite を
// NOT_FAVORITED で拒否する (#1562)。
func TestClipUnfavorite_NoSuchClip(t *testing.T) {
	h, _, _ := newStubHandler(t)
	rec := postStubWithBody(t, h.Unfavorite, `{"clipId":"ghost"}`, "u1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_CLIP")
	assert.Contains(t, rec.Body.String(), "2603966e-b865-426c-94a7-af4a01241dc1")
}

func TestClipUnfavorite_NotFavorited(t *testing.T) {
	h, clipRepo, _ := newStubHandler(t)
	clipRepo.Clips["cl1"] = &model.Clip{ID: "cl1", UserID: "u1", Name: "test", IsPublic: true}
	rec := postStubWithBody(t, h.Unfavorite, `{"clipId":"cl1"}`, "u1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NOT_FAVORITED")
	assert.Contains(t, rec.Body.String(), "90c3a9e8-b321-4dae-bf57-2bf79bbcc187")
}

// favorite 済の非公開化 clip も unfavorite できる (upstream は visibility を
// 見ない、#1562)。
func TestClipUnfavorite_PrivateClipStillRemovable(t *testing.T) {
	h, clipRepo, favRepo := newStubHandler(t)
	clipRepo.Clips["cl1"] = &model.Clip{ID: "cl1", UserID: "owner", Name: "test", IsPublic: false}
	favRepo.Favorites["u1:cl1"] = &model.ClipFavorite{ID: "f1", UserID: "u1", ClipID: "cl1"}
	rec := postStubWithBody(t, h.Unfavorite, `{"clipId":"cl1"}`, "u1")
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestClipUnfavorite_MissingClipID(t *testing.T) {
	h, _, _ := newStubHandler(t)
	rec := postStubWithBody(t, h.Unfavorite, `{}`, "u1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestClipMyFavorites_Empty(t *testing.T) {
	h, _, _ := newStubHandler(t)
	rec := postStubWithBody(t, h.MyFavorites, `{}`, "u1")
	assert.Equal(t, http.StatusOK, rec.Code)
	var arr []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &arr))
	assert.Empty(t, arr)
}

func TestClipMyFavorites_WithData(t *testing.T) {
	h, clipRepo, favRepo := newStubHandler(t)
	clipRepo.Clips["cl1"] = &model.Clip{ID: "cl1", UserID: "u1", Name: "test", IsPublic: true}
	favRepo.Favorites["u1:cl1"] = &model.ClipFavorite{ID: "f1", UserID: "u1", ClipID: "cl1"}
	rec := postStubWithBody(t, h.MyFavorites, `{}`, "u1")
	assert.Equal(t, http.StatusOK, rec.Code)
	var arr []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &arr))
	assert.Len(t, arr, 1)
}

// TestClipMyFavorites_IncludesNonPublicNonOwned: upstream は favorite に紐づく
// clip を visibility 無検査で返す。公開時に favorite した他人所有 clip を所有者が
// 後で非公開化しても、自分の favorites からは消えない (#1830)。
func TestClipMyFavorites_IncludesNonPublicNonOwned(t *testing.T) {
	h, clipRepo, favRepo := newStubHandler(t)
	// u2 所有の非公開 clip を u1 が favorite している。
	clipRepo.Clips["cl2"] = &model.Clip{ID: "cl2", UserID: "u2", Name: "private", IsPublic: false}
	favRepo.Favorites["u1:cl2"] = &model.ClipFavorite{ID: "f2", UserID: "u1", ClipID: "cl2"}
	rec := postStubWithBody(t, h.MyFavorites, `{}`, "u1")
	assert.Equal(t, http.StatusOK, rec.Code)
	var arr []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &arr))
	require.Len(t, arr, 1, "他人所有の非公開 favorited clip も返す (visibility 無検査)")
	assert.Equal(t, "cl2", arr[0]["id"])
}

// TestClipMyFavorites_DropsDeletedClip: clip が削除済み (orphan favorite) の
// 場合だけ drop する。
func TestClipMyFavorites_DropsDeletedClip(t *testing.T) {
	h, _, favRepo := newStubHandler(t)
	favRepo.Favorites["u1:gone"] = &model.ClipFavorite{ID: "f3", UserID: "u1", ClipID: "gone"}
	rec := postStubWithBody(t, h.MyFavorites, `{}`, "u1")
	assert.Equal(t, http.StatusOK, rec.Code)
	var arr []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &arr))
	assert.Empty(t, arr)
}
