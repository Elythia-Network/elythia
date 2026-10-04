package entity

import (
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/require"
)

func profileWith(followers, following model.FollowingVisibility) *model.UserProfile {
	return &model.UserProfile{UserID: "u", FollowersVisibility: followers, FollowingVisibility: following}
}

// **ゲートを呼び忘れても非公開のカウントが漏れないこと。**
//
// `PackUserDetailed` の呼び出しは 30 箇所あり、実際に複数の経路が
// `GateCountVisibility` を呼び忘れて `followersVisibility: "private"` と実数を
// 並べて返していた。packer 側で既定を伏せてあれば、呼び忘れても漏れない。
func TestPackUserDetailed_HidesRestrictedCountsWithoutGate(t *testing.T) {
	t.Parallel()

	u := &model.User{ID: "u", Username: "u", FollowersCount: 42, FollowingCount: 7}

	for _, c := range []struct {
		name       string
		vis        model.FollowingVisibility
		wantHidden bool
	}{
		{"public は出す", model.FollowingVisibilityPublic, false},
		{"followers は伏せる", model.FollowingVisibilityFollowers, true},
		{"private は伏せる", model.FollowingVisibilityPrivate, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := PackUserDetailed(u, profileWith(c.vis, c.vis))
			if c.wantHidden {
				require.Equal(t, 0, d.FollowersCount, "ゲートを呼ぶ前は伏せておくこと")
				require.Equal(t, 0, d.FollowingCount)
			} else {
				require.Equal(t, 42, d.FollowersCount)
				require.Equal(t, 7, d.FollowingCount)
			}
		})
	}
}

// ゲートを通した閲覧者には実数が戻ること。
func TestGateCountVisibility_RestoresForPermittedViewer(t *testing.T) {
	t.Parallel()

	u := &model.User{ID: "u", Username: "u", FollowersCount: 42, FollowingCount: 7}
	prof := profileWith(model.FollowingVisibilityFollowers, model.FollowingVisibilityFollowers)

	d := PackUserDetailed(u, prof)
	GateCountVisibility(&d, false, false, true) // follower
	require.Equal(t, 42, d.FollowersCount, "follower には見せること")
	require.Equal(t, 7, d.FollowingCount)

	d2 := PackUserDetailed(u, prof)
	GateCountVisibility(&d2, false, false, false) // 非 follower
	require.Equal(t, 0, d2.FollowersCount)

	d3 := PackUserDetailed(u, prof)
	GateCountVisibility(&d3, true, false, false) // self
	require.Equal(t, 42, d3.FollowersCount, "self には常に見せること")

	d4 := PackUserDetailed(u, prof)
	GateCountVisibility(&d4, false, true, false) // moderator
	require.Equal(t, 42, d4.FollowersCount, "moderator には見せること")
}

// リモート統計の上書きがゲートと噛み合うこと (#943 の退行防止)。
func TestOverrideRemoteCounts_SurvivesGate(t *testing.T) {
	t.Parallel()

	host := "remote.example"
	u := &model.User{ID: "u", Username: "u", Host: &host, FollowersCount: 5, FollowingCount: 3}
	d := PackUserDetailed(u, profileWith(model.FollowingVisibilityPublic, model.FollowingVisibilityPublic))
	OverrideRemoteCounts(&d, 1000, 250)
	require.Equal(t, 1000, d.FollowersCount)

	// ゲートを通しても上書きした値が残ること (表示値だけ書くと元の値に戻る)。
	GateCountVisibility(&d, false, false, false)
	require.Equal(t, 1000, d.FollowersCount, "ゲートが元の値で上書きしないこと")
	require.Equal(t, 250, d.FollowingCount)
}

// 空文字の visibility は列の default ('public') として扱うこと。
func TestPackUserDetailed_EmptyVisibilityIsPublic(t *testing.T) {
	t.Parallel()

	u := &model.User{ID: "u", Username: "u", FollowersCount: 11, FollowingCount: 2}
	d := PackUserDetailed(u, &model.UserProfile{UserID: "u"})
	require.Equal(t, 11, d.FollowersCount, "未設定は既定 (public) として扱う")
}

// profile 無しで組んだ利用者は公開範囲が分からないので、GateCountVisibility は
// 本人とモデレーター以外にカウントを伏せる。visibility が空のままだと public
// 扱いに倒れ、DB 障害のあいだ非公開のカウントが一覧から漏れていた (#3330)。
func TestGateCountVisibility_ProfileMissingIsClosed(t *testing.T) {
	t.Parallel()

	u := &model.User{ID: "u", Username: "u", FollowersCount: 42, FollowingCount: 7}
	for _, c := range []struct {
		name                     string
		isMe, isMod, isFollowing bool
		wantFollowers            int
		wantFollowing            int
	}{
		{"anonymous / stranger", false, false, false, 0, 0},
		{"follower", false, false, true, 0, 0},
		{"self", true, false, false, 42, 7},
		{"moderator", false, true, false, 42, 7},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := PackUserDetailed(u, nil)
			GateCountVisibility(&d, c.isMe, c.isMod, c.isFollowing)
			require.Equal(t, c.wantFollowers, d.FollowersCount)
			require.Equal(t, c.wantFollowing, d.FollowingCount)
		})
	}
}

// profile 無しの利用者は、リモートの統計で上書きしても伏せたままにする。
func TestOverrideRemoteCounts_ProfileMissingStaysClosed(t *testing.T) {
	t.Parallel()

	host := "remote.example"
	u := &model.User{ID: "u", Username: "u", Host: &host, FollowersCount: 5, FollowingCount: 3}
	d := PackUserDetailed(u, nil)
	OverrideRemoteCounts(&d, 1000, 250)
	require.Equal(t, 0, d.FollowersCount)
	require.Equal(t, 0, d.FollowingCount)
	GateCountVisibility(&d, false, true, false) // moderator
	require.Equal(t, 1000, d.FollowersCount, "見てよい閲覧者には上書きした値を見せる")
	require.Equal(t, 250, d.FollowingCount)
}
