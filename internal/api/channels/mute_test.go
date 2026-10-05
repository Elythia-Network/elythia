package channels

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- MuteCreate ---

func TestMuteCreate_Success(t *testing.T) {
	h, chRepo, _, mutRepo := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	rec := postStubWithBody(t, h.MuteCreate, `{"channelId":"ch1"}`, "u1")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	exists, _ := mutRepo.Exists("u1", "ch1")
	assert.True(t, exists)
}

func TestMuteCreate_WithFutureExpiresAt(t *testing.T) {
	h, chRepo, _, mutRepo := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	future := time.Now().Add(24 * time.Hour).UnixMilli()
	rec := postStubWithBody(t, h.MuteCreate, fmt.Sprintf(`{"channelId":"ch1","expiresAt":%d}`, future), "u1")
	require.Equal(t, http.StatusNoContent, rec.Code)
	mut := mutRepo.Mutings["u1:ch1"]
	require.NotNil(t, mut)
	require.NotNil(t, mut.ExpiresAt, "expiresAt が保存される")
	assert.Equal(t, future, mut.ExpiresAt.UnixMilli())
}

func TestMuteCreate_PastExpiresAtRejected(t *testing.T) {
	h, chRepo, _, mutRepo := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	past := time.Now().Add(-time.Hour).UnixMilli()
	rec := postStubWithBody(t, h.MuteCreate, fmt.Sprintf(`{"channelId":"ch1","expiresAt":%d}`, past), "u1")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "EXPIRES_AT_IS_PAST", errObj["code"])
	assert.Equal(t, "42b32236-df2c-a45f-fdbf-def67268f749", errObj["id"])
	exists, _ := mutRepo.Exists("u1", "ch1")
	assert.False(t, exists, "past expiresAt では mute を作らない")
}

func TestMuteCreate_ZeroExpiresAtIsIndefinite(t *testing.T) {
	h, chRepo, _, mutRepo := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	// 本家は expiresAt=0 を falsy として無期限扱いにする (過去弾きしない)。
	rec := postStubWithBody(t, h.MuteCreate, `{"channelId":"ch1","expiresAt":0}`, "u1")
	require.Equal(t, http.StatusNoContent, rec.Code)
	mut := mutRepo.Mutings["u1:ch1"]
	require.NotNil(t, mut)
	assert.Nil(t, mut.ExpiresAt, "expiresAt=0 は無期限 (nil) として保存")
}

func TestMuteCreate_AlreadyMutedPastExpiresAtReturnsAlreadyMuting(t *testing.T) {
	h, chRepo, _, mutRepo := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	mutRepo.Mutings["u1:ch1"] = &model.ChannelMuting{ID: "m1", UserID: "u1", ChannelID: "ch1"}
	// 既ミュート済みなら過去 expiresAt でも EXPIRES_AT_IS_PAST ではなく
	// ALREADY_MUTING_CHANNEL (本家順序: alreadyMuting を先にチェック, #1540)。
	past := time.Now().Add(-time.Hour).UnixMilli()
	rec := postStubWithBody(t, h.MuteCreate, fmt.Sprintf(`{"channelId":"ch1","expiresAt":%d}`, past), "u1")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "ALREADY_MUTING_CHANNEL", resp["error"].(map[string]any)["code"])
}

func TestMuteCreate_MissingChannelID(t *testing.T) {
	h, _, _, _ := newStubHandler(t)
	rec := postStubWithBody(t, h.MuteCreate, `{}`, "u1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMuteCreate_ChannelNotFound(t *testing.T) {
	h, _, _, _ := newStubHandler(t)
	rec := postStubWithBody(t, h.MuteCreate, `{"channelId":"nonexist"}`, "u1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMuteCreate_AlreadyMuted(t *testing.T) {
	h, chRepo, _, mutRepo := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	mutRepo.Mutings["u1:ch1"] = &model.ChannelMuting{ID: "m1", UserID: "u1", ChannelID: "ch1"}
	rec := postStubWithBody(t, h.MuteCreate, `{"channelId":"ch1"}`, "u1")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "ALREADY_MUTING_CHANNEL", errObj["code"])
	assert.Equal(t, "5a251978-769a-da44-3e89-3931e43bb592", errObj["id"])
}

// --- MuteDelete ---

func TestMuteDelete_Success(t *testing.T) {
	h, chRepo, _, mutRepo := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	mutRepo.Mutings["u1:ch1"] = &model.ChannelMuting{ID: "m1", UserID: "u1", ChannelID: "ch1"}
	rec := postStubWithBody(t, h.MuteDelete, `{"channelId":"ch1"}`, "u1")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	exists, _ := mutRepo.Exists("u1", "ch1")
	assert.False(t, exists)
}

func TestMuteDelete_MissingChannelID(t *testing.T) {
	h, _, _, _ := newStubHandler(t)
	rec := postStubWithBody(t, h.MuteDelete, `{}`, "u1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #1540: delete は channel 不在で NO_SUCH_CHANNEL。
func TestMuteDelete_ChannelNotFound(t *testing.T) {
	h, _, _, mutRepo := newStubHandler(t)
	mutRepo.Mutings["u1:ch1"] = &model.ChannelMuting{ID: "m1", UserID: "u1", ChannelID: "ch1"}
	rec := postStubWithBody(t, h.MuteDelete, `{"channelId":"ch1"}`, "u1")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_CHANNEL", errObj["code"])
	assert.Equal(t, "e7998769-6e94-d9c2-6b8f-94a527314aba", errObj["id"])
}

// #1540: delete は未ミュートで NOT_MUTING_CHANNEL。
func TestMuteDelete_NotMuting(t *testing.T) {
	h, chRepo, _, _ := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	rec := postStubWithBody(t, h.MuteDelete, `{"channelId":"ch1"}`, "u1")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "NOT_MUTING_CHANNEL", errObj["code"])
	assert.Equal(t, "14d55962-6ea8-d990-1333-d6bef78dc2ab", errObj["id"])
}

// --- MuteList ---

func TestMuteList_Empty(t *testing.T) {
	h, _, _, _ := newStubHandler(t)
	rec := postStubWithBody(t, h.MuteList, `{}`, "u1")
	assert.Equal(t, http.StatusOK, rec.Code)
	var arr []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &arr))
	assert.Empty(t, arr)
}

func TestMuteList_WithData(t *testing.T) {
	h, chRepo, _, mutRepo := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	mutRepo.Mutings["u1:ch1"] = &model.ChannelMuting{ID: "m1", UserID: "u1", ChannelID: "ch1"}
	rec := postStubWithBody(t, h.MuteList, `{}`, "u1")
	assert.Equal(t, http.StatusOK, rec.Code)
	var arr []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &arr))
	assert.Len(t, arr, 1)
}
