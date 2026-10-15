package repository

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// hostVisUser inserts a user (remote when host != "") and, when withProfile,
// its user_profile with the given visibilities. Cleanup is registered on t.
func hostVisUser(t *testing.T, id, host string, withProfile bool, followers, following model.FollowingVisibility) {
	t.Helper()
	if host == "" {
		insertTestUser(t, id, id)
	} else {
		insertRemoteTestUser(t, id, id, host)
	}
	t.Cleanup(func() { cleanupUser(t, id) })
	if !withProfile {
		return
	}
	p := &model.UserProfile{UserID: id, FollowersVisibility: followers, FollowingVisibility: following}
	if host != "" {
		h := host
		p.UserHost = &h
	}
	require.NoError(t, testDB.Create(p).Error)
}

// hostVisFollow inserts a following row. Cleanup is registered on t (it runs
// before the user cleanups registered earlier).
func hostVisFollow(t *testing.T, id, followerID, followeeID string, followerHost, followeeHost string) {
	t.Helper()
	row := &model.Following{ID: id, FollowerID: followerID, FolloweeID: followeeID}
	if followerHost != "" {
		h := followerHost
		row.FollowerHost = &h
	}
	if followeeHost != "" {
		h := followeeHost
		row.FolloweeHost = &h
	}
	require.NoError(t, testDB.Create(row).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "following" WHERE id = ?`, id) })
}

func hostVisIDs(rows []*model.Following) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

// TestFollowingRepository_HostListVisibility checks that federation/followers
// and federation/following only return rows whose owner's list visibility
// allows the viewer (upstream generateFollowingRelationVisibilityQuery).
func TestFollowingRepository_HostListVisibility(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	const (
		pub  = model.FollowingVisibilityPublic
		fol  = model.FollowingVisibilityFollowers
		priv = model.FollowingVisibilityPrivate
	)

	t.Run("followers", func(t *testing.T) {
		// 持ち主は followee (hostA のリモート)。判定は followersVisibility。
		// followingVisibility は逆の値にして、列の取り違えを検出できるようにする。
		const host = "hvfollowers.example"
		hostVisUser(t, "hvf_local", "", true, pub, pub)
		hostVisUser(t, "hvf_stranger", "", true, pub, pub)
		hostVisUser(t, "hvf_o_pub", host, true, pub, priv)
		hostVisUser(t, "hvf_o_fol", host, true, fol, pub)
		hostVisUser(t, "hvf_o_priv", host, true, priv, pub)
		hostVisUser(t, "hvf_o_none", host, false, "", "")
		// hvf_local は 4 人全員をフォローしている (= どの持ち主にとってもフォロワー)。
		hostVisFollow(t, "hvf_r_pub", "hvf_local", "hvf_o_pub", "", host)
		hostVisFollow(t, "hvf_r_fol", "hvf_local", "hvf_o_fol", "", host)
		hostVisFollow(t, "hvf_r_priv", "hvf_local", "hvf_o_priv", "", host)
		hostVisFollow(t, "hvf_r_none", "hvf_local", "hvf_o_none", "", host)

		cases := []struct {
			name   string
			viewer model.FollowListViewer
			want   []string
		}{
			{"anonymous sees public only", model.FollowListViewer{}, []string{"hvf_r_pub"}},
			{"non-follower sees public only", model.FollowListViewer{UserID: "hvf_stranger"}, []string{"hvf_r_pub"}},
			{"follower also sees followers-only", model.FollowListViewer{UserID: "hvf_local"}, []string{"hvf_r_fol", "hvf_r_pub"}},
			{"owner sees own private list", model.FollowListViewer{UserID: "hvf_o_priv"}, []string{"hvf_r_priv", "hvf_r_pub"}},
			{"owner sees own followers-only list", model.FollowListViewer{UserID: "hvf_o_fol"}, []string{"hvf_r_fol", "hvf_r_pub"}},
			{"owner without profile is excluded even for self", model.FollowListViewer{UserID: "hvf_o_none"}, []string{"hvf_r_pub"}},
			{"moderator bypasses the filter", model.FollowListViewer{UserID: "hvf_stranger", Moderator: true}, []string{"hvf_r_fol", "hvf_r_none", "hvf_r_priv", "hvf_r_pub"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rows, err := repo.ListFollowersByHostCursor(host, "", "", 100, tc.viewer)
				require.NoError(t, err)
				assert.Equal(t, tc.want, hostVisIDs(rows))
			})
		}
	})

	t.Run("following", func(t *testing.T) {
		// 持ち主は follower (hostB のリモート)。判定は followingVisibility。
		const host = "hvfollowing.example"
		hostVisUser(t, "hvg_local", "", true, pub, pub)
		hostVisUser(t, "hvg_viewer", "", true, pub, pub)
		hostVisUser(t, "hvg_stranger", "", true, pub, pub)
		hostVisUser(t, "hvg_o_pub", host, true, priv, pub)
		hostVisUser(t, "hvg_o_fol", host, true, pub, fol)
		hostVisUser(t, "hvg_o_priv", host, true, pub, priv)
		hostVisUser(t, "hvg_o_none", host, false, "", "")
		// 一覧の行: 持ち主 (リモート) → hvg_local。
		hostVisFollow(t, "hvg_r_pub", "hvg_o_pub", "hvg_local", host, "")
		hostVisFollow(t, "hvg_r_fol", "hvg_o_fol", "hvg_local", host, "")
		hostVisFollow(t, "hvg_r_priv", "hvg_o_priv", "hvg_local", host, "")
		hostVisFollow(t, "hvg_r_none", "hvg_o_none", "hvg_local", host, "")
		// hvg_viewer は持ち主全員をフォローしている。この行は followeeHost 側
		// なので following 一覧には載らない。
		for _, o := range []string{"hvg_o_pub", "hvg_o_fol", "hvg_o_priv", "hvg_o_none"} {
			hostVisFollow(t, "hvg_v_"+o, "hvg_viewer", o, "", host)
		}

		cases := []struct {
			name   string
			viewer model.FollowListViewer
			want   []string
		}{
			{"anonymous sees public only", model.FollowListViewer{}, []string{"hvg_r_pub"}},
			{"non-follower sees public only", model.FollowListViewer{UserID: "hvg_stranger"}, []string{"hvg_r_pub"}},
			// hvg_local は持ち主にフォローされているだけで、フォローはしていない。
			{"followee of the owner is not a follower", model.FollowListViewer{UserID: "hvg_local"}, []string{"hvg_r_pub"}},
			{"follower also sees followers-only", model.FollowListViewer{UserID: "hvg_viewer"}, []string{"hvg_r_fol", "hvg_r_pub"}},
			{"owner sees own private list", model.FollowListViewer{UserID: "hvg_o_priv"}, []string{"hvg_r_priv", "hvg_r_pub"}},
			{"owner without profile is excluded even for self", model.FollowListViewer{UserID: "hvg_o_none"}, []string{"hvg_r_pub"}},
			{"moderator bypasses the filter", model.FollowListViewer{Moderator: true}, []string{"hvg_r_fol", "hvg_r_none", "hvg_r_priv", "hvg_r_pub"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rows, err := repo.ListFollowingByHostCursor(host, "", "", 100, tc.viewer)
				require.NoError(t, err)
				assert.Equal(t, tc.want, hostVisIDs(rows))
			})
		}
	})
}

// TestFollowingRepository_HostListVisibilityBeforeLimit checks that the
// visibility filter is applied in SQL before LIMIT, so hidden rows neither
// shorten a page nor stall the cursor.
func TestFollowingRepository_HostListVisibilityBeforeLimit(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	const (
		pub  = model.FollowingVisibilityPublic
		priv = model.FollowingVisibilityPrivate
	)

	t.Run("followers", func(t *testing.T) {
		const host = "hvlimit-followers.example"
		hostVisUser(t, "hvl_f_local", "", true, pub, pub)
		hostVisUser(t, "hvl_f_pub1", host, true, pub, pub)
		hostVisUser(t, "hvl_f_pub2", host, true, pub, pub)
		hostVisUser(t, "hvl_f_priv", host, true, priv, priv)
		// id DESC の先頭 (z*) は全て非公開の持ち主。公開は a / b。
		hostVisFollow(t, "hvl_f_a", "hvl_f_local", "hvl_f_pub1", "", host)
		hostVisFollow(t, "hvl_f_b", "hvl_f_local", "hvl_f_pub2", "", host)
		hostVisFollow(t, "hvl_f_z", "hvl_f_local", "hvl_f_priv", "", host)

		rows, err := repo.ListFollowersByHostCursor(host, "", "", 1, model.FollowListViewer{})
		require.NoError(t, err)
		assert.Equal(t, []string{"hvl_f_b"}, hostVisIDs(rows), "first page must be filled with a visible row")
		rows, err = repo.ListFollowersByHostCursor(host, "", "hvl_f_b", 1, model.FollowListViewer{})
		require.NoError(t, err)
		assert.Equal(t, []string{"hvl_f_a"}, hostVisIDs(rows), "next page continues past hidden rows")
	})

	t.Run("following", func(t *testing.T) {
		const host = "hvlimit-following.example"
		hostVisUser(t, "hvl_g_local", "", true, pub, pub)
		hostVisUser(t, "hvl_g_pub1", host, true, pub, pub)
		hostVisUser(t, "hvl_g_pub2", host, true, pub, pub)
		hostVisUser(t, "hvl_g_priv1", host, true, priv, priv)
		hostVisUser(t, "hvl_g_priv2", host, true, priv, priv)
		hostVisFollow(t, "hvl_g_a", "hvl_g_pub1", "hvl_g_local", host, "")
		hostVisFollow(t, "hvl_g_b", "hvl_g_pub2", "hvl_g_local", host, "")
		hostVisFollow(t, "hvl_g_y", "hvl_g_priv1", "hvl_g_local", host, "")
		hostVisFollow(t, "hvl_g_z", "hvl_g_priv2", "hvl_g_local", host, "")

		rows, err := repo.ListFollowingByHostCursor(host, "", "", 2, model.FollowListViewer{})
		require.NoError(t, err)
		assert.Equal(t, []string{"hvl_g_a", "hvl_g_b"}, hostVisIDs(rows))
		// sinceId (ASC) でも同じ。
		rows, err = repo.ListFollowingByHostCursor(host, "hvl_g_a", "", 1, model.FollowListViewer{})
		require.NoError(t, err)
		assert.Equal(t, []string{"hvl_g_b"}, hostVisIDs(rows))
	})
}

// TestFollowingRepository_HostListOtherSideVisibility checks that a row is
// also gated by the other side's list visibility: a federation/followers row
// is a row of the follower's following list, and a federation/following row
// is a row of the followee's followers list. Upstream only checks the owner.
func TestFollowingRepository_HostListOtherSideVisibility(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	const (
		pub  = model.FollowingVisibilityPublic
		fol  = model.FollowingVisibilityFollowers
		priv = model.FollowingVisibilityPrivate
	)

	t.Run("followers", func(t *testing.T) {
		// 持ち主 (followee) は公開。相手側 (follower) の followingVisibility を見る。
		// followersVisibility は逆の値にして、列の取り違えを検出できるようにする。
		const host = "hvother-followers.example"
		hostVisUser(t, "hof_owner", host, true, pub, pub)
		hostVisUser(t, "hof_l_pub", "", true, priv, pub)
		hostVisUser(t, "hof_l_fol", "", true, priv, fol)
		hostVisUser(t, "hof_l_priv", "", true, pub, priv)
		hostVisUser(t, "hof_l_none", "", false, "", "")
		hostVisUser(t, "hof_r_priv", "other-followers.example", true, pub, priv)
		hostVisUser(t, "hof_viewer", "", true, pub, pub)
		hostVisFollow(t, "hof_x_pub", "hof_l_pub", "hof_owner", "", host)
		hostVisFollow(t, "hof_x_fol", "hof_l_fol", "hof_owner", "", host)
		hostVisFollow(t, "hof_x_priv", "hof_l_priv", "hof_owner", "", host)
		hostVisFollow(t, "hof_x_none", "hof_l_none", "hof_owner", "", host)
		hostVisFollow(t, "hof_x_rpriv", "hof_r_priv", "hof_owner", "other-followers.example", host)
		// hof_viewer は hof_l_fol のフォロワー。この行の followeeHost は NULL なので一覧には載らない。
		hostVisFollow(t, "hof_v", "hof_viewer", "hof_l_fol", "", "")

		cases := []struct {
			name   string
			viewer model.FollowListViewer
			want   []string
		}{
			{"anonymous sees rows whose follower lists are public", model.FollowListViewer{}, []string{"hof_x_none", "hof_x_pub"}},
			{"stranger sees rows whose follower lists are public", model.FollowListViewer{UserID: "hof_owner"}, []string{"hof_x_none", "hof_x_pub"}},
			{"follower of the follower sees followers-only", model.FollowListViewer{UserID: "hof_viewer"}, []string{"hof_x_fol", "hof_x_none", "hof_x_pub"}},
			{"follower sees own private following", model.FollowListViewer{UserID: "hof_l_priv"}, []string{"hof_x_none", "hof_x_priv", "hof_x_pub"}},
			{"moderator bypasses the filter", model.FollowListViewer{Moderator: true}, []string{"hof_x_fol", "hof_x_none", "hof_x_priv", "hof_x_pub", "hof_x_rpriv"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rows, err := repo.ListFollowersByHostCursor(host, "", "", 100, tc.viewer)
				require.NoError(t, err)
				assert.Equal(t, tc.want, hostVisIDs(rows))
			})
		}
	})

	t.Run("following", func(t *testing.T) {
		// 持ち主 (follower) は公開。相手側 (followee) の followersVisibility を見る。
		const host = "hvother-following.example"
		hostVisUser(t, "hog_owner", host, true, pub, pub)
		hostVisUser(t, "hog_l_pub", "", true, pub, priv)
		hostVisUser(t, "hog_l_fol", "", true, fol, priv)
		hostVisUser(t, "hog_l_priv", "", true, priv, pub)
		hostVisUser(t, "hog_l_none", "", false, "", "")
		hostVisUser(t, "hog_viewer", "", true, pub, pub)
		hostVisFollow(t, "hog_x_pub", "hog_owner", "hog_l_pub", host, "")
		hostVisFollow(t, "hog_x_fol", "hog_owner", "hog_l_fol", host, "")
		hostVisFollow(t, "hog_x_priv", "hog_owner", "hog_l_priv", host, "")
		hostVisFollow(t, "hog_x_none", "hog_owner", "hog_l_none", host, "")
		hostVisFollow(t, "hog_v", "hog_viewer", "hog_l_fol", "", "")

		cases := []struct {
			name   string
			viewer model.FollowListViewer
			want   []string
		}{
			{"anonymous sees rows whose followee lists are public", model.FollowListViewer{}, []string{"hog_x_none", "hog_x_pub"}},
			{"follower of the followee sees followers-only", model.FollowListViewer{UserID: "hog_viewer"}, []string{"hog_x_fol", "hog_x_none", "hog_x_pub"}},
			{"followee sees own private followers", model.FollowListViewer{UserID: "hog_l_priv"}, []string{"hog_x_none", "hog_x_priv", "hog_x_pub"}},
			{"moderator bypasses the filter", model.FollowListViewer{Moderator: true}, []string{"hog_x_fol", "hog_x_none", "hog_x_priv", "hog_x_pub"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rows, err := repo.ListFollowingByHostCursor(host, "", "", 100, tc.viewer)
				require.NoError(t, err)
				assert.Equal(t, tc.want, hostVisIDs(rows))
			})
		}
	})

	t.Run("hidden rows do not break paging", func(t *testing.T) {
		const host = "hvother-limit.example"
		hostVisUser(t, "hol_owner", host, true, pub, pub)
		hostVisUser(t, "hol_l_pub1", "", true, pub, pub)
		hostVisUser(t, "hol_l_pub2", "", true, pub, pub)
		hostVisUser(t, "hol_l_priv", "", true, pub, priv)
		// id DESC の先頭 (z) は相手側が非公開の行。
		hostVisFollow(t, "hol_a", "hol_l_pub1", "hol_owner", "", host)
		hostVisFollow(t, "hol_b", "hol_l_pub2", "hol_owner", "", host)
		hostVisFollow(t, "hol_z", "hol_l_priv", "hol_owner", "", host)

		rows, err := repo.ListFollowersByHostCursor(host, "", "", 1, model.FollowListViewer{})
		require.NoError(t, err)
		assert.Equal(t, []string{"hol_b"}, hostVisIDs(rows), "first page must be filled with a visible row")
		rows, err = repo.ListFollowersByHostCursor(host, "", "hol_b", 1, model.FollowListViewer{})
		require.NoError(t, err)
		assert.Equal(t, []string{"hol_a"}, hostVisIDs(rows), "next page continues past hidden rows")
		rows, err = repo.ListFollowersByHostCursor(host, "hol_a", "", 5, model.FollowListViewer{})
		require.NoError(t, err)
		assert.Equal(t, []string{"hol_b"}, hostVisIDs(rows), "sinceId skips the hidden row too")
	})
}
