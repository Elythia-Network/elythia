package pages

import (
	"context"
	"net/http"
	"testing"

	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingPacker records the target / viewer and marks the packed user.
type recordingPacker struct {
	target, viewer *model.User
	calls          int
	fail           bool
}

func (r *recordingPacker) DetailedNotMe(_ context.Context, target, viewer *model.User) (entity.UserDetailed, bool) {
	r.calls++
	r.target, r.viewer = target, viewer
	if r.fail {
		return entity.UserDetailed{}, false
	}
	d := entity.PackUserDetailed(target, nil)
	d.PinnedNoteIDs = []string{"pinned"}
	return d, true
}

// usersByID serves ShowByID per user ID.
type usersByID struct {
	stubUserSource
	users map[string]*model.User
}

func (u *usersByID) ShowByID(id string) (*coreuser.UserWithProfile, error) {
	return &coreuser.UserWithProfile{User: u.users[id]}, nil
}

func pagePushFixture(t *testing.T, p *recordingPacker) (*Handler, *stubMainStreamPublisher) {
	t.Helper()
	h, repo, _ := newHandler(t)
	if p != nil {
		assert.False(t, h.HasUserPacker())
		h.SetUserPacker(p)
		assert.True(t, h.HasUserPacker())
	}
	pub := &stubMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)
	h.SetUserSource(&usersByID{users: map[string]*model.User{
		"alice": {ID: "alice", Username: "alice"},
		"owner": {ID: "owner", Username: "owner"},
	}})
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "owner", Name: "test", Visibility: model.PageVisibilityPublic}
	return h, pub
}

// 本家 page-push は pack(me.id, {id: page.userId}, {schema: 'UserDetailed'}) で、
// ページの持ち主を閲覧者にして押した人を組む (#3330)。
func TestPagePush_PacksPusherSeenByPageOwner(t *testing.T) {
	p := &recordingPacker{}
	h, pub := pagePushFixture(t, p)

	c, _ := newReq(t, `{"pageId":"p1","event":"click"}`)
	setUser(c, "alice")
	require.NoError(t, h.PagePush(c))
	require.Len(t, pub.calls, 1)
	body := pub.calls[0].body.(map[string]any)
	user, ok := body["user"].(entity.UserDetailed)
	require.True(t, ok, "他人のページなら UserDetailed")
	assert.Equal(t, "alice", user.ID)
	assert.Equal(t, []string{"pinned"}, user.PinnedNoteIDs)
	assert.Equal(t, "alice", p.target.ID)
	assert.Equal(t, "owner", p.viewer.ID, "閲覧者はページの持ち主")
}

// 自分のページなら本家の pack は isMe なので MeDetailed になる (#3330)。
func TestPagePush_OwnPageIsMeDetailed(t *testing.T) {
	p := &recordingPacker{}
	h, pub := pagePushFixture(t, p)

	c, _ := newReq(t, `{"pageId":"p1","event":"click"}`)
	setUser(c, "owner")
	require.NoError(t, h.PagePush(c))
	require.Len(t, pub.calls, 1)
	body := pub.calls[0].body.(map[string]any)
	user, ok := body["user"].(map[string]any)
	require.True(t, ok, "自分のページなら MeDetailed (map)")
	assert.Contains(t, user, "avatarId")
	assert.Equal(t, "owner", user["id"])
	assert.Equal(t, "owner", p.viewer.ID)
}

// profile が読めないとき本家は例外で送らない。
func TestPagePush_PackFailureDoesNotPublish(t *testing.T) {
	h, pub := pagePushFixture(t, &recordingPacker{fail: true})

	c, rec := newReq(t, `{"pageId":"p1","event":"click"}`)
	setUser(c, "alice")
	require.NoError(t, h.PagePush(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, pub.calls)
}

// 未配線なら素の UserDetailed を送る。
func TestPagePush_WithoutPackerSendsBareUser(t *testing.T) {
	h, pub := pagePushFixture(t, nil)

	c, _ := newReq(t, `{"pageId":"p1","event":"click"}`)
	setUser(c, "alice")
	require.NoError(t, h.PagePush(c))
	require.Len(t, pub.calls, 1)
	user := pub.calls[0].body.(map[string]any)["user"].(entity.UserDetailed)
	assert.Equal(t, "alice", user.ID)
	assert.Empty(t, user.PinnedNoteIDs)
}
