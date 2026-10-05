package channels

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corechannel "github.com/elythia-network/elythia/internal/core/channel"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStubHandler(t *testing.T) (*Handler, *testutil.MockChannelRepository, *testutil.MockChannelFavoriteRepository, *testutil.MockChannelMutingRepository) {
	t.Helper()
	repo := testutil.NewMockChannelRepository()
	followRepo := testutil.NewMockChannelFollowingRepository()
	noteRepo := testutil.NewMockNoteRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := corechannel.NewService(repo, followRepo, noteRepo, idGen)
	h := NewHandler(svc, idGen)
	favRepo := testutil.NewMockChannelFavoriteRepository()
	mutRepo := testutil.NewMockChannelMutingRepository()
	h.SetFavoriteRepo(favRepo)
	h.SetMutingRepo(mutRepo)
	return h, repo, favRepo, mutRepo
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

// --- Favorite ---

func TestFavorite_MissingChannelID(t *testing.T) {
	h, _, _, _ := newStubHandler(t)
	rec := postStubWithBody(t, h.Favorite, `{}`, "u1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFavorite_ChannelNotFound(t *testing.T) {
	h, _, _, _ := newStubHandler(t)
	rec := postStubWithBody(t, h.Favorite, `{"channelId":"nonexist"}`, "u1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFavorite_Success(t *testing.T) {
	h, chRepo, favRepo, _ := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	rec := postStubWithBody(t, h.Favorite, `{"channelId":"ch1"}`, "u1")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	exists, _ := favRepo.Exists("u1", "ch1")
	assert.True(t, exists)
}

func TestFavorite_AlreadyFavorited(t *testing.T) {
	h, chRepo, favRepo, _ := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	favRepo.Favorites["u1:ch1"] = &model.ChannelFavorite{ID: "f1", UserID: "u1", ChannelID: "ch1"}
	rec := postStubWithBody(t, h.Favorite, `{"channelId":"ch1"}`, "u1")
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestFavorite_NilRepo(t *testing.T) {
	repo := testutil.NewMockChannelRepository()
	followRepo := testutil.NewMockChannelFollowingRepository()
	noteRepo := testutil.NewMockNoteRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := corechannel.NewService(repo, followRepo, noteRepo, idGen)
	h := NewHandler(svc, idGen)
	// favoriteRepo is nil
	rec := postStubWithBody(t, h.Favorite, `{"channelId":"ch1"}`, "u1")
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// --- Unfavorite ---

func TestUnfavorite_Success(t *testing.T) {
	h, chRepo, favRepo, _ := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	favRepo.Favorites["u1:ch1"] = &model.ChannelFavorite{ID: "f1", UserID: "u1", ChannelID: "ch1"}
	rec := postStubWithBody(t, h.Unfavorite, `{"channelId":"ch1"}`, "u1")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	exists, _ := favRepo.Exists("u1", "ch1")
	assert.False(t, exists)
}

// #1770: 存在しない channel の unfavorite は NO_SUCH_CHANNEL (id 353c68dd)。
func TestUnfavorite_NoSuchChannel(t *testing.T) {
	h, _, _, _ := newStubHandler(t)
	rec := postStubWithBody(t, h.Unfavorite, `{"channelId":"ghost"}`, "u1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_CHANNEL")
	assert.Contains(t, rec.Body.String(), "353c68dd-131a-476c-aa99-88a345e83668")
}

func TestUnfavorite_MissingChannelID(t *testing.T) {
	h, _, _, _ := newStubHandler(t)
	rec := postStubWithBody(t, h.Unfavorite, `{}`, "u1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- MyFavorites ---

func TestMyFavorites_Empty(t *testing.T) {
	h, _, _, _ := newStubHandler(t)
	rec := postStubWithBody(t, h.MyFavorites, `{}`, "u1")
	assert.Equal(t, http.StatusOK, rec.Code)
	var arr []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &arr))
	assert.Empty(t, arr)
}

func TestMyFavorites_WithData(t *testing.T) {
	h, chRepo, favRepo, _ := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	favRepo.Favorites["u1:ch1"] = &model.ChannelFavorite{ID: "f1", UserID: "u1", ChannelID: "ch1"}
	rec := postStubWithBody(t, h.MyFavorites, `{}`, "u1")
	assert.Equal(t, http.StatusOK, rec.Code)
	var arr []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &arr))
	assert.Len(t, arr, 1)
}
