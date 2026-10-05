package following

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubListPacker records its calls and marks the packed users.
type stubListPacker struct {
	viewers   []*model.User
	liteCalls int
}

func (s *stubListPacker) DetailedMany(_ context.Context, viewer *model.User, users []*model.User, profiles map[string]*model.UserProfile) []entity.UserDetailed {
	s.viewers = append(s.viewers, viewer)
	out := make([]entity.UserDetailed, len(users))
	for i, u := range users {
		out[i] = entity.PackUserDetailed(u, profiles[u.ID])
		out[i].PinnedNoteIDs = []string{"pin-" + u.ID}
	}
	return out
}

func (s *stubListPacker) FillLites(users []*model.User, lites []*entity.UserLite) {
	s.liteCalls++
	for i, u := range users {
		name := "inst-" + u.ID
		lites[i].Instance = &entity.InstanceLite{Name: &name}
	}
}

// 本家 following/list は FollowingEntityService.packMany(followings, me,
// {populateFollowee: true}) で followee を packMany で組む (#3330)。
func TestList_UsesListPacker(t *testing.T) {
	h, repo := newTestHandler(t)
	assert.False(t, h.HasListPacker())
	p := &stubListPacker{}
	h.SetListPacker(p)
	assert.True(t, h.HasListPacker())
	addUser(repo, "alice", false)
	addUser(repo, "carol", false)
	bob := addUser(repo, "bob", false)
	require.Equal(t, http.StatusOK, postJSON(h.Create, `{"userId":"alice"}`, bob).Code)
	require.Equal(t, http.StatusOK, postJSON(h.Create, `{"userId":"carol"}`, bob).Code)

	rec := postJSON(h.List, `{}`, bob)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 2)
	for _, row := range out {
		followee := row["followee"].(map[string]any)
		assert.Equal(t, []any{"pin-" + row["followeeId"].(string)}, followee["pinnedNoteIds"])
	}
	require.Len(t, p.viewers, 1, "followee はまとめて 1 回で pack する")
	assert.Equal(t, "bob", p.viewers[0].ID)
}

// 本家 FollowRequestEntityService.packMany は follower / followee を
// packMany (UserLite) で組むので instance と絵文字を埋める (#3330)。
func TestListRequestsAndSent_FillLites(t *testing.T) {
	h, repo := newTestHandler(t)
	p := &stubListPacker{}
	h.SetListPacker(p)
	bob := addUser(repo, "bob", true)
	alice := addUser(repo, "alice", false)
	_, _ = h.followingService.Follow("alice", bob.ID, corefollowing.FollowOptions{})

	rec := postJSON(h.ListRequests, `{}`, bob)
	require.Equal(t, http.StatusOK, rec.Code)
	var items []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
	require.Len(t, items, 1)
	assert.Equal(t, "inst-alice", items[0]["follower"].(map[string]any)["instance"].(map[string]any)["name"])
	assert.Equal(t, "inst-bob", items[0]["followee"].(map[string]any)["instance"].(map[string]any)["name"])

	rec = postJSON(h.RequestsSent, `{}`, alice)
	require.Equal(t, http.StatusOK, rec.Code)
	items = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
	require.Len(t, items, 1)
	assert.Equal(t, "inst-bob", items[0]["followee"].(map[string]any)["instance"].(map[string]any)["name"])
	assert.Equal(t, 2, p.liteCalls, "1 リクエストにつき 1 回でまとめて埋める")
}
