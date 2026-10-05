package federation

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// TestHostFollowLists_PassViewer checks that federation/followers and
// federation/following hand the caller to the repository's owner visibility
// filter, and that only moderators bypass it (upstream skips
// generateFollowingRelationVisibilityQuery for moderators only).
func TestHostFollowLists_PassViewer(t *testing.T) {
	endpoints := []struct {
		name string
		fn   func(h *Handler) echo.HandlerFunc
	}{
		{"followers", func(h *Handler) echo.HandlerFunc { return h.Followers }},
		{"following", func(h *Handler) echo.HandlerFunc { return h.Following }},
	}
	cases := []struct {
		name      string
		viewerID  string
		moderator bool
		want      model.FollowListViewer
	}{
		{"anonymous", "", false, model.FollowListViewer{}},
		{"regular user", "user1", false, model.FollowListViewer{UserID: "user1"}},
		{"moderator bypasses", "mod1", true, model.FollowListViewer{UserID: "mod1", Moderator: true}},
		{"no moderator checker wired", "mod1", false, model.FollowListViewer{UserID: "mod1"}},
	}
	for _, ep := range endpoints {
		for _, tc := range cases {
			t.Run(ep.name+"/"+tc.name, func(t *testing.T) {
				h, _ := newHandler(t)
				repo := testutil.NewMockFollowingRepository()
				h.SetFollowingRepo(repo)
				if tc.moderator {
					h.SetModeratorChecker(fakeModerator{ids: map[string]bool{"mod1": true}})
				}
				var code int
				if tc.viewerID == "" {
					code = postBody(ep.fn(h), `{"host":"remote.example"}`).Code
				} else {
					code = postBodyAs(ep.fn(h), `{"host":"remote.example"}`, tc.viewerID).Code
				}
				require.Equal(t, http.StatusOK, code)
				require.NotNil(t, repo.LastHostListViewer)
				assert.Equal(t, tc.want, *repo.LastHostListViewer)
			})
		}
	}
}

// TestHostFollowLists_NonModeratorNotBypassed checks that a checker which
// rejects the caller keeps the filter on.
func TestHostFollowLists_NonModeratorNotBypassed(t *testing.T) {
	h, _ := newHandler(t)
	repo := testutil.NewMockFollowingRepository()
	h.SetFollowingRepo(repo)
	h.SetModeratorChecker(fakeModerator{ids: map[string]bool{"mod1": true}})

	require.Equal(t, http.StatusOK, postBodyAs(h.Followers, `{"host":"remote.example"}`, "user1").Code)
	require.NotNil(t, repo.LastHostListViewer)
	assert.False(t, repo.LastHostListViewer.Moderator)
	require.Equal(t, http.StatusOK, postBodyAs(h.Following, `{"host":"remote.example"}`, "user1").Code)
	assert.False(t, repo.LastHostListViewer.Moderator)
}
