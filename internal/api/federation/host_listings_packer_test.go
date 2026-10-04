package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/shiroha-a/mk/internal/api/userrelation"
	"github.com/shiroha-a/mk/internal/core/userpack"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setListPacker wires a real userpack.Packer with the given relation repos and
// moderator checker, plus a recording batch filler.
func setListPacker(h *Handler, rel userrelation.Repos, mod userpack.ModeratorChecker) *recordingManyExtras {
	idGen, _ := id.NewGenerator("aidx")
	p := userpack.New(userpack.Lookups{Relations: rel, Moderators: mod}, idGen)
	extras := &recordingManyExtras{}
	p.SetDetailExtrasMany(extras)
	h.SetListPacker(p)
	return extras
}

// recordingManyExtras marks every packed user with a pin and records the
// viewer it was called with.
type recordingManyExtras struct {
	calls   int
	viewers []*model.User
	ids     [][]string
}

func (r *recordingManyExtras) FillDetailedExtrasMany(_ context.Context, viewer *model.User, targets []userpack.DetailTarget) {
	r.calls++
	r.viewers = append(r.viewers, viewer)
	var ids []string
	for _, t := range targets {
		ids = append(ids, t.User.ID)
		t.Detailed.PinnedNoteIDs = []string{"pin-" + t.User.ID}
	}
	r.ids = append(r.ids, ids)
}

func TestHasListPacker(t *testing.T) {
	h, _ := newHandler(t)
	assert.False(t, h.HasListPacker())
	setListPacker(h, userrelation.Repos{}, nil)
	assert.True(t, h.HasListPacker())
}

// 本家 federation/users は packMany(users, me) なので、ピン留めと移行先を
// 閲覧者付きでまとめて埋める (#3330)。
func TestUsers_FillsDetailExtrasInOneBatch(t *testing.T) {
	h, _ := newHandler(t)
	userRepo := testutil.NewMockUserRepository()
	remote := "remote.example"
	userRepo.Users["r1"] = &model.User{ID: "r1", Username: "r1", Host: &remote}
	userRepo.Users["r2"] = &model.User{ID: "r2", Username: "r2", Host: &remote}
	h.SetUserRepo(userRepo)
	extras := setListPacker(h, userrelation.Repos{}, nil)

	rec := postBodyAs(h.Users, `{"host":"remote.example","limit":10}`, "viewer1")
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, []any{"pin-" + row["id"].(string)}, row["pinnedNoteIds"])
	}
	require.Equal(t, 1, extras.calls, "一覧は 1 回でまとめて埋める")
	require.NotNil(t, extras.viewers[0])
	assert.Equal(t, "viewer1", extras.viewers[0].ID)
}

// 本家 federation/following の followee はローカルの利用者なので閲覧者自身が
// 混ざりうる。pack は isMe なら MeDetailed を返す (#3330)。
func TestFollowing_ViewerFolloweeIsMeDetailed(t *testing.T) {
	h, _ := newHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	h.SetIDGen(idGen)
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["me"] = &model.User{ID: "me", Username: "me"}
	userRepo.Users["other"] = &model.User{ID: "other", Username: "other"}
	h.SetUserRepo(userRepo)
	followingRepo := testutil.NewMockFollowingRepository()
	remote := "remote.example"
	followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "r1", FollowerHost: &remote, FolloweeID: "me"}
	followingRepo.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "r1", FollowerHost: &remote, FolloweeID: "other"}
	followingRepo.Followings["f3"] = &model.Following{ID: "f3", FollowerID: "r2", FollowerHost: &remote, FolloweeID: "other"}
	h.SetFollowingRepo(followingRepo)
	extras := setListPacker(h, userrelation.Repos{Following: followingRepo}, nil)

	rec := postBodyAs(h.Following, `{"host":"remote.example","limit":10}`, "me")
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 3)
	seen := 0
	for _, row := range rows {
		followee, ok := row["followee"].(map[string]any)
		require.True(t, ok)
		_, isMe := followee["avatarId"]
		if row["followeeId"] == "me" {
			assert.True(t, isMe, "閲覧者自身は MeDetailed")
		} else {
			assert.False(t, isMe, "他人は UserDetailed")
		}
		assert.Equal(t, []any{"pin-" + row["followeeId"].(string)}, followee["pinnedNoteIds"])
		seen++
	}
	assert.Equal(t, 3, seen)
	require.Equal(t, 1, extras.calls)
	assert.ElementsMatch(t, []string{"me", "other"}, extras.ids[0], "同じ followee は 1 回だけ pack する")
}

// 未配線なら素の UserDetailed を返し、伏せるべきカウントは伏せたまま。
func TestUsers_WithoutPackerKeepsCountsHidden(t *testing.T) {
	h, _ := newHandler(t)
	userRepo := testutil.NewMockUserRepository()
	remote := "remote.example"
	userRepo.Users["r1"] = &model.User{ID: "r1", Username: "r1", Host: &remote, FollowersCount: 7}
	userRepo.Profiles["r1"] = &model.UserProfile{UserID: "r1", FollowersVisibility: model.FollowingVisibilityPrivate, FollowingVisibility: model.FollowingVisibilityPublic}
	h.SetUserRepo(userRepo)

	rec := postBodyAs(h.Users, `{"host":"remote.example","limit":10}`, "viewer1")
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.EqualValues(t, 0, rows[0]["followersCount"])
}
