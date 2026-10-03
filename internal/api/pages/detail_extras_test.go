package pages

import (
	"context"
	"testing"

	coreuser "github.com/shiroha-a/mk/internal/core/user"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingExtras records the viewer and marks the packed user with a pin.
type recordingExtras struct {
	viewer *model.User
	calls  int
}

func (r *recordingExtras) FillDetailedExtras(_ context.Context, viewer, _ *model.User, _ *model.UserProfile, d *entity.UserDetailed) {
	r.calls++
	r.viewer = viewer
	d.PinnedNoteIDs = []string{"pinned"}
}

// usersByID serves ShowByID per user ID.
type usersByID struct {
	stubUserSource
	users map[string]*model.User
}

func (u *usersByID) ShowByID(id string) (*coreuser.UserWithProfile, error) {
	return &coreuser.UserWithProfile{User: u.users[id]}, nil
}

// 本家 page-push は pack(me.id, {id: page.userId}, {schema: 'UserDetailed'}) で、
// ページの持ち主を閲覧者にしてピン留めと移行先を埋める (#3330)。
func TestPagePush_FillsDetailExtrasAsPageOwner(t *testing.T) {
	h, repo, _ := newHandler(t)
	assert.False(t, h.HasDetailExtras())
	extras := &recordingExtras{}
	h.SetDetailExtras(extras)
	assert.True(t, h.HasDetailExtras())
	pub := &stubMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)
	h.SetUserSource(&usersByID{users: map[string]*model.User{
		"alice": {ID: "alice", Username: "alice"},
		"owner": {ID: "owner", Username: "owner"},
	}})
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "owner", Name: "test", Visibility: model.PageVisibilityPublic}

	c, _ := newReq(t, `{"pageId":"p1","event":"click"}`)
	setUser(c, "alice")
	require.NoError(t, h.PagePush(c))
	require.Len(t, pub.calls, 1)
	body := pub.calls[0].body.(map[string]any)
	user := body["user"].(entity.UserDetailed)
	assert.Equal(t, "alice", user.ID)
	assert.Equal(t, []string{"pinned"}, user.PinnedNoteIDs)
	require.NotNil(t, extras.viewer)
	assert.Equal(t, "owner", extras.viewer.ID)
}
