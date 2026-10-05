package users

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestGetFrequentlyRepliedUsers_InvalidAndMissing(t *testing.T) {
	h, _ := newTestHandler(t)
	// userId 欠落は 400
	assert.Equal(t, http.StatusBadRequest, postStub(h.GetFrequentlyRepliedUsers, `{}`, nil).Code)
	// 存在しない user は NO_SUCH_USER (404)
	assert.Equal(t, http.StatusNotFound, postStub(h.GetFrequentlyRepliedUsers, `{"userId":"ghost"}`, nil).Code)
}

func TestGetFollowingUsersByBirthday_InvalidParams(t *testing.T) {
	h, _ := newTestHandler(t)
	// birthday 未指定は 400
	assert.Equal(t, http.StatusBadRequest, postStub(h.GetFollowingUsersByBirthday, `{}`, &model.User{ID: "u1"}).Code)
	// 単発指定で month/day が範囲外は 400
	assert.Equal(t, http.StatusBadRequest, postStub(h.GetFollowingUsersByBirthday, `{"birthday":{"month":13,"day":1}}`, &model.User{ID: "u1"}).Code)
	// begin/end いずれかが無効
	assert.Equal(t, http.StatusBadRequest, postStub(h.GetFollowingUsersByBirthday, `{"birthday":{"begin":{"month":1,"day":1},"end":{"month":2,"day":40}}}`, &model.User{ID: "u1"}).Code)
}

func TestUserRecommendation_EmptyDefault(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postStub(h.UserRecommendation, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

// --- UsersBulk ---

func TestUsersBulk_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON("[]")}
	rec := postStub(h.UsersBulk, `{"userIds":["u1"]}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "alice")

	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	shapetest.Assert(t, "UserLite", out[0]) // L3 (#1314)
}

func TestUsersBulk_Empty(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postStub(h.UsersBulk, `{"userIds":[]}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUsersBulk_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postStub(h.UsersBulk, `invalid`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUsersBulk_LimitsTo100(t *testing.T) {
	h, userRepo := newTestHandler(t)
	// 150 人の user を仕込む
	for i := 0; i < 150; i++ {
		id := "u" + timeSuffix(i)
		userRepo.Users[id] = &model.User{ID: id, Username: id, UsernameLower: id}
	}
	ids := make([]string, 150)
	for i := 0; i < 150; i++ {
		ids[i] = "u" + timeSuffix(i)
	}
	body, _ := json.Marshal(map[string]any{"userIds": ids})
	rec := postStub(h.UsersBulk, string(body), nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.LessOrEqual(t, len(rows), 100)
}

func timeSuffix(i int) string {
	s := time.Date(2025, 1, 1, 0, 0, i, 0, time.UTC).Format("20060102150405")
	return s
}
