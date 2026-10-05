package following

import (
	"net/http"
	"testing"

	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestRequestsSent_Empty(t *testing.T) {
	h, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, postJSON(h.RequestsSent, `{}`, &model.User{ID: "u1"}).Code)
}

func TestRequestsSent_PopulatesFollowerFollowee(t *testing.T) {
	h, userRepo := newTestHandler(t)
	// me が remote-locked を follow リクエストして pending な状態を作る
	me := &model.User{ID: "me", Username: "me", UsernameLower: "me"}
	locked := &model.User{ID: "locked", Username: "locked", UsernameLower: "locked", IsLocked: true}
	userRepo.Users[me.ID] = me
	userRepo.Users[locked.ID] = locked
	_, _ = h.followingService.Follow(me.ID, locked.ID, corefollowing.FollowOptions{}) // ends in FollowRequest

	rec := postJSON(h.RequestsSent, `{}`, me)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "me")
}
