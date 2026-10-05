package i

import (
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"gorm.io/datatypes"
)

// stubRelationReloadPublisher records published userIDs (#2400).
type stubRelationReloadPublisher struct {
	published []string
}

func (s *stubRelationReloadPublisher) PublishMuteBlockReload(userID string) {
	s.published = append(s.published, userID)
}

// mutedInstances は MuteBlockSnapshot の MutedInstances に載るので、変更時に
// 接続中の snapshot を取り直させる。これが無いとインスタンスミュート直後も
// 対象 host の event が既存の WebSocket に届き続ける。
func TestUpdate_MutedInstancesTriggersRelationReload(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	pub := &stubRelationReloadPublisher{}
	h.SetRelationReloadPublisher(pub)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	rec := post(h.Update, `{"mutedInstances":["bad.example"]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"user1"}, pub.published)
}

// 無関係な field の更新では publish しない。全 update で撒くと無駄な DB 往復になる。
func TestUpdate_OtherFieldDoesNotTriggerRelationReload(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	pub := &stubRelationReloadPublisher{}
	h.SetRelationReloadPublisher(pub)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	rec := post(h.Update, `{"name":"new name"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, pub.published)
}

// publisher 未配線でも動く (既存構成が壊れない)。
func TestUpdate_MutedInstancesWithoutPublisher(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	rec := post(h.Update, `{"mutedInstances":["bad.example"]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

var _ RelationReloadPublisher = (*stubRelationReloadPublisher)(nil)
