package channels

import (
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingReloadPublisher records notified users (#2400).
type recordingReloadPublisher struct{ users []string }

func (r *recordingReloadPublisher) PublishMuteBlockReload(userID string) {
	r.users = append(r.users, userID)
}

// channel mute は MuteBlockSnapshot の MutingChannels に載るので、変更したら
// 接続中の snapshot を取り直させる。これが無いとミュート直後もそのチャンネルの
// 投稿が既存の WebSocket に流れ続ける。
func TestMuteCreate_PublishesRelationReload(t *testing.T) {
	h, chRepo, _, _ := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	pub := &recordingReloadPublisher{}
	h.SetRelationReloadPublisher(pub)

	rec := postStubWithBody(t, h.MuteCreate, `{"channelId":"ch1"}`, "u1")
	require.Equal(t, http.StatusNoContent, rec.Code)

	assert.Equal(t, []string{"u1"}, pub.users)
}

func TestMuteDelete_PublishesRelationReload(t *testing.T) {
	h, chRepo, _, _ := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	require.Equal(t, http.StatusNoContent,
		postStubWithBody(t, h.MuteCreate, `{"channelId":"ch1"}`, "u1").Code)

	pub := &recordingReloadPublisher{}
	h.SetRelationReloadPublisher(pub)
	rec := postStubWithBody(t, h.MuteDelete, `{"channelId":"ch1"}`, "u1")
	require.Equal(t, http.StatusNoContent, rec.Code)

	assert.Equal(t, []string{"u1"}, pub.users)
}

// エラー応答では通知しない。無駄な reload を撒かないため。
func TestMuteCreate_NoPublishOnError(t *testing.T) {
	h, _, _, _ := newStubHandler(t)
	pub := &recordingReloadPublisher{}
	h.SetRelationReloadPublisher(pub)

	// 存在しない channel は NO_SUCH_CHANNEL。
	rec := postStubWithBody(t, h.MuteCreate, `{"channelId":"missing"}`, "u1")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, pub.users)
}

func TestMuteDelete_NoPublishWhenNotMuting(t *testing.T) {
	h, chRepo, _, _ := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}
	pub := &recordingReloadPublisher{}
	h.SetRelationReloadPublisher(pub)

	rec := postStubWithBody(t, h.MuteDelete, `{"channelId":"ch1"}`, "u1")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, pub.users)
}

// publisher 未配線でも動く (既存構成が壊れない)。
func TestMuteCreate_WithoutPublisher(t *testing.T) {
	h, chRepo, _, _ := newStubHandler(t)
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "test"}

	rec := postStubWithBody(t, h.MuteCreate, `{"channelId":"ch1"}`, "u1")
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

var _ RelationReloadPublisher = (*recordingReloadPublisher)(nil)
