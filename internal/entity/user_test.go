package entity

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestComputeOnlineStatus(t *testing.T) {
	now := time.Now()
	mk := func(hide bool, last *time.Time) *model.User {
		return &model.User{ID: "u", HideOnlineStatus: hide, LastActiveDate: last}
	}
	online := now.Add(-1 * time.Minute)
	active := now.Add(-1 * time.Hour)
	offline := now.Add(-10 * 24 * time.Hour)

	assert.Equal(t, "unknown", computeOnlineStatus(mk(false, nil)), "lastActiveDate nil → unknown")
	assert.Equal(t, "unknown", computeOnlineStatus(mk(true, &online)), "hideOnlineStatus → unknown")
	assert.Equal(t, "online", computeOnlineStatus(mk(false, &online)))
	assert.Equal(t, "active", computeOnlineStatus(mk(false, &active)))
	assert.Equal(t, "offline", computeOnlineStatus(mk(false, &offline)))

	// 境界: threshold 直前/直後 (< が online、>= が次段階)。
	justOnline := now.Add(-userOnlineThreshold + time.Second)
	justActive := now.Add(-userOnlineThreshold - time.Second)
	justOffline := now.Add(-userActiveThreshold - time.Second)
	assert.Equal(t, "online", computeOnlineStatus(mk(false, &justOnline)))
	assert.Equal(t, "active", computeOnlineStatus(mk(false, &justActive)))
	assert.Equal(t, "offline", computeOnlineStatus(mk(false, &justOffline)))

	// PackUserLite が computeOnlineStatus を使う。
	lite := PackUserLite(mk(false, &online))
	assert.Equal(t, "online", lite.OnlineStatus)
	// HideOnlineStatus は packer 経由でも尊重される。
	assert.Equal(t, "unknown", PackUserLite(mk(true, &online)).OnlineStatus)
}

func TestPackUserDetailed_IsSilenced(t *testing.T) {
	t.Cleanup(func() { SetSilencedLookup(nil) })
	u := &model.User{ID: "silenced-user"}

	// lookup 未配線なら false。
	SetSilencedLookup(nil)
	assert.False(t, PackUserDetailed(u, nil).IsSilenced)

	// lookup が true を返せば反映される。
	SetSilencedLookup(stubSilenced{ids: map[string]bool{"silenced-user": true}})
	assert.True(t, PackUserDetailed(u, nil).IsSilenced)
	// 別ユーザーは false。
	assert.False(t, PackUserDetailed(&model.User{ID: "other"}, nil).IsSilenced)
}

type stubSilenced struct{ ids map[string]bool }

func (s stubSilenced) IsSilenced(userID string) bool { return s.ids[userID] }

func TestPackUserLite(t *testing.T) {
	name := "Test User"
	avatarURL := "https://example.com/avatar.png"
	blurhash := "LEHV6nWB2yk8"
	avatarID := "avatar1"

	u := &model.User{
		ID:                "user1",
		Username:          "testuser",
		Name:              &name,
		AvatarID:          &avatarID,
		AvatarURL:         &avatarURL,
		AvatarBlurhash:    &blurhash,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
		IsBot:             true,
		IsCat:             false,
	}

	lite := PackUserLite(u)

	assert.Equal(t, "user1", lite.ID)
	assert.Equal(t, "testuser", lite.Username)
	assert.Equal(t, &name, lite.Name)
	assert.Equal(t, avatarURL, lite.AvatarURL)
	assert.Equal(t, &blurhash, lite.AvatarBlurhash)
	assert.True(t, lite.IsBot)
	assert.False(t, lite.IsCat)
	assert.Equal(t, "unknown", lite.OnlineStatus)
	assert.NotNil(t, lite.Emojis)
	assert.Empty(t, lite.Emojis)
	assert.NotNil(t, lite.BadgeRoles)
	assert.Empty(t, lite.BadgeRoles)
}

func TestPackUserLite_NilFields(t *testing.T) {
	u := &model.User{
		ID:                "user2",
		Username:          "minimal",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	lite := PackUserLite(u)

	assert.Equal(t, "user2", lite.ID)
	assert.Nil(t, lite.Name)
	assert.Nil(t, lite.Host)
	// avatarUrlがnullの場合、identiconにフォールバック
	assert.Equal(t, "/identicon/minimal", lite.AvatarURL)
	assert.Nil(t, lite.AvatarBlurhash)
}

func TestPackUserLite_IdenticonWithHost(t *testing.T) {
	host := "remote.example"
	u := &model.User{
		ID:                "user3",
		Username:          "alice",
		Host:              &host,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	lite := PackUserLite(u)
	assert.Equal(t, "/identicon/alice@remote.example", lite.AvatarURL)
}

func TestPackUserLite_ExistingAvatarURL(t *testing.T) {
	avatar := "https://example.com/avatar.png"
	u := &model.User{
		ID:                "user4",
		Username:          "bob",
		AvatarURL:         &avatar,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	lite := PackUserLite(u)
	assert.Equal(t, "https://example.com/avatar.png", lite.AvatarURL)
}

func TestPackUserDetailed(t *testing.T) {
	name := "Detailed User"
	bannerURL := "https://example.com/banner.png"
	bannerID := "banner1"
	desc := "A test user"
	location := "Tokyo"
	birthday := "2000-01-01"
	lang := "ja-JP"

	u := &model.User{
		ID:                "user3",
		Username:          "detailed",
		Name:              &name,
		BannerID:          &bannerID,
		BannerURL:         &bannerURL,
		IsLocked:          true,
		IsSuspended:       false,
		FollowersCount:    100,
		FollowingCount:    50,
		NotesCount:        1000,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	profile := &model.UserProfile{
		UserID:      "user3",
		Description: &desc,
		Location:    &location,
		Birthday:    &birthday,
		Lang:        &lang,
		Fields:      datatypes.JSON([]byte("[]")),
	}

	detailed := PackUserDetailed(u, profile)

	assert.Equal(t, "user3", detailed.ID)
	assert.Equal(t, "detailed", detailed.Username)
	assert.Equal(t, &bannerURL, detailed.BannerURL)
	assert.True(t, detailed.IsLocked)
	assert.False(t, detailed.IsSuspended)
	assert.Equal(t, 100, detailed.FollowersCount)
	assert.Equal(t, 50, detailed.FollowingCount)
	assert.Equal(t, 1000, detailed.NotesCount)
	assert.Equal(t, &desc, detailed.Description)
	assert.Equal(t, &location, detailed.Location)
	assert.Equal(t, &birthday, detailed.Birthday)
	assert.Equal(t, &lang, detailed.Lang)
}

// #1781: moderator が他ユーザーを見るとき twoFactorEnabled /
// usePasswordLessLogin / securityKeys を emit する。非 moderator には omit。
func TestApplyModeratorSecurityFields(t *testing.T) {
	profile := &model.UserProfile{
		UserID:                "u1",
		TwoFactorEnabled:      true,
		UsePasswordLessLogin:  false,
		SecurityKeysAvailable: true,
	}

	t.Run("moderator gets the trio", func(t *testing.T) {
		d := UserDetailed{}
		ApplyModeratorSecurityFields(&d, true, profile)
		require.NotNil(t, d.TwoFactorEnabled)
		assert.True(t, *d.TwoFactorEnabled)
		require.NotNil(t, d.UsePasswordLessLogin)
		assert.False(t, *d.UsePasswordLessLogin)
		require.NotNil(t, d.SecurityKeys)
		assert.True(t, *d.SecurityKeys)
		// JSON では key が emit される。
		b, err := json.Marshal(d)
		require.NoError(t, err)
		assert.Contains(t, string(b), "\"twoFactorEnabled\":true")
		assert.Contains(t, string(b), "\"usePasswordLessLogin\":false")
		assert.Contains(t, string(b), "\"securityKeys\":true")
	})

	t.Run("non-moderator omits the trio", func(t *testing.T) {
		d := UserDetailed{}
		ApplyModeratorSecurityFields(&d, false, profile)
		assert.Nil(t, d.TwoFactorEnabled)
		assert.Nil(t, d.UsePasswordLessLogin)
		assert.Nil(t, d.SecurityKeys)
		b, err := json.Marshal(d)
		require.NoError(t, err)
		assert.NotContains(t, string(b), "twoFactorEnabled")
		assert.NotContains(t, string(b), "usePasswordLessLogin")
		assert.NotContains(t, string(b), "securityKeys")
	})

	t.Run("nil profile is a no-op", func(t *testing.T) {
		d := UserDetailed{}
		ApplyModeratorSecurityFields(&d, true, nil)
		assert.Nil(t, d.TwoFactorEnabled)
	})

	// #1921: 2FA 無効なら securityKeys は SecurityKeysAvailable に関わらず false。
	t.Run("securityKeys false when 2FA disabled", func(t *testing.T) {
		off := &model.UserProfile{UserID: "u1", TwoFactorEnabled: false, SecurityKeysAvailable: true}
		d := UserDetailed{}
		ApplyModeratorSecurityFields(&d, true, off)
		require.NotNil(t, d.TwoFactorEnabled)
		assert.False(t, *d.TwoFactorEnabled)
		require.NotNil(t, d.SecurityKeys)
		assert.False(t, *d.SecurityKeys, "2FA 無効なら securityKeys は false")
	})
}

func TestPackUserDetailed_MoveFields(t *testing.T) {
	fetched := time.Date(2026, 5, 25, 1, 2, 3, 0, time.UTC)
	movedURI := "https://remote.example/users/moved"
	aka := "https://a.example/users/x,https://b.example/users/y"
	u := &model.User{
		ID:            "user_move",
		Username:      "mover",
		LastFetchedAt: &fetched,
		MovedToURI:    &movedURI,
		AlsoKnownAs:   &aka,
	}

	d := PackUserDetailed(u, nil)

	// lastFetchedAt は ISO8601(ms) で埋まる。
	require.NotNil(t, d.LastFetchedAt)
	assert.Equal(t, "2026-05-25T01:02:03.000Z", *d.LastFetchedAt)

	// movedTo / alsoKnownAs は URI→ID 解決を要するため pure packer では null。
	// (upstream の解決不能時 fallback と同じ。完全解決は follow-up)
	assert.Nil(t, d.MovedTo)
	assert.Nil(t, d.AlsoKnownAs)

	// JSON 上は 3 field とも present で、movedTo / alsoKnownAs / (未取得時の)
	// lastFetchedAt は null になる (golden: string|null / string[]|null)。
	b, err := json.Marshal(d)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	for _, k := range []string{"movedTo", "alsoKnownAs", "lastFetchedAt"} {
		_, present := m[k]
		assert.Truef(t, present, "%q must be present in JSON", k)
	}
	assert.Nil(t, m["movedTo"])
	assert.Nil(t, m["alsoKnownAs"])
	assert.Equal(t, "2026-05-25T01:02:03.000Z", m["lastFetchedAt"])
}

func TestPackUserDetailed_LastFetchedAtNil(t *testing.T) {
	u := &model.User{ID: "u_local", Username: "local"}
	d := PackUserDetailed(u, nil)
	assert.Nil(t, d.LastFetchedAt, "local user without lastFetchedAt -> null")
}

func TestResolveMoveTargets(t *testing.T) {
	movedURI := "https://remote.example/users/dest"
	aka := "https://a.example/users/x, https://unknown.example/users/z ,https://b.example/users/y"
	u := &model.User{
		ID:          "u1",
		MovedToURI:  &movedURI,
		AlsoKnownAs: &aka,
	}
	// known URIs -> local IDs; unknown -> dropped.
	known := map[string]string{
		movedURI:                    "dest_id",
		"https://a.example/users/x": "x_id",
		"https://b.example/users/y": "y_id",
	}
	resolve := func(uri string) (string, bool) { id, ok := known[uri]; return id, ok }

	d := PackUserDetailed(u, nil)
	d.ResolveMoveTargets(u, resolve)

	require.NotNil(t, d.MovedTo)
	assert.Equal(t, "dest_id", *d.MovedTo)
	// unknown.example は解決できず除外され、空白も trim される。
	assert.Equal(t, []string{"x_id", "y_id"}, d.AlsoKnownAs)
}

// #2027: alsoKnownAs に URI はあるが全て解決不能なら、null ではなく [] を返す
// (upstream xs.length===0?null は URI 皆無のときだけ true)。
func TestResolveMoveTargets_AllUnresolvableYieldsEmptyArray(t *testing.T) {
	aka := "https://unknown.example/users/a, https://unknown.example/users/b"
	u := &model.User{ID: "u1", AlsoKnownAs: &aka}
	resolve := func(string) (string, bool) { return "", false } // 全て解決不能

	d := PackUserDetailed(u, nil)
	d.ResolveMoveTargets(u, resolve)
	require.NotNil(t, d.AlsoKnownAs, "URI があれば全解決不能でも [] (#2027)")
	assert.Empty(t, d.AlsoKnownAs)

	// URI が皆無 (空文字列) のときは null のまま。
	empty := ""
	u2 := &model.User{ID: "u2", AlsoKnownAs: &empty}
	d2 := PackUserDetailed(u2, nil)
	d2.ResolveMoveTargets(u2, resolve)
	assert.Nil(t, d2.AlsoKnownAs, "URI 皆無は null")
}

func TestResolveMoveTargets_NilAndUnresolvable(t *testing.T) {
	// nil resolve / nil user は no-op。
	d := UserDetailed{}
	d.ResolveMoveTargets(&model.User{}, nil)
	assert.Nil(t, d.MovedTo)
	assert.Nil(t, d.AlsoKnownAs)

	// alsoKnownAs に URI があれば全解決不能でも [] (upstream xs.length>=1 → filter→[]、
	// #2027)。movedTo は解決不能なら null のまま。
	movedURI := "https://x/u"
	aka := "https://y/u"
	u := &model.User{MovedToURI: &movedURI, AlsoKnownAs: &aka}
	d2 := UserDetailed{}
	d2.ResolveMoveTargets(u, func(string) (string, bool) { return "", false })
	assert.Nil(t, d2.MovedTo)
	require.NotNil(t, d2.AlsoKnownAs)
	assert.Empty(t, d2.AlsoKnownAs, "URI があれば全解決不能でも [] (#2027)")
}

func TestPackUserDetailed_MemoNullNotOmitted(t *testing.T) {
	u := &model.User{ID: "u_memo", Username: "memo"}
	d := PackUserDetailed(u, nil)
	b, err := json.Marshal(d)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))

	// memo は golden で required(string|null)なので、未設定でも present かつ null。
	v, present := m["memo"]
	assert.True(t, present, "memo must be present (golden: string|null required)")
	assert.Nil(t, v, "memo is null when the viewer has no memo")

	// 一方 relation bool 群は golden optional なので未設定なら omit のまま
	// (null を出すと misskey_dart が `as bool` で crash する #1228)。
	for _, k := range []string{"isFollowing", "isFollowed", "isMuted", "withReplies"} {
		_, p := m[k]
		assert.Falsef(t, p, "%q must stay omitempty (golden optional)", k)
	}
}

func TestPackUserDetailed_ProfileVisibility(t *testing.T) {
	u := &model.User{
		ID:                "user5",
		Username:          "visible",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	profile := &model.UserProfile{
		UserID:              "user5",
		FollowersVisibility: model.FollowingVisibilityFollowers,
		FollowingVisibility: model.FollowingVisibilityPrivate,
		Fields:              datatypes.JSON([]byte("[]")),
	}

	detailed := PackUserDetailed(u, profile)

	assert.Equal(t, "followers", detailed.FollowersVisibility)
	assert.Equal(t, "private", detailed.FollowingVisibility)
}

// #1558 avatarId / bannerId が null のとき blurhash / bannerUrl も null にする。
func TestPackUser_BlurhashGatedByMediaID(t *testing.T) {
	blur := "LEHV6nWB2yk8"
	bannerURL := "https://example.com/banner.png"

	t.Run("avatarId nil → avatarBlurhash nil", func(t *testing.T) {
		u := &model.User{ID: "u", Username: "u", AvatarBlurhash: &blur, AvatarDecorations: datatypes.JSON([]byte("[]"))}
		lite := PackUserLite(u)
		assert.Nil(t, lite.AvatarBlurhash, "avatarId が無ければ blurhash も出さない")
	})

	t.Run("avatarId set → avatarBlurhash kept", func(t *testing.T) {
		aid := "a1"
		u := &model.User{ID: "u", Username: "u", AvatarID: &aid, AvatarBlurhash: &blur, AvatarDecorations: datatypes.JSON([]byte("[]"))}
		lite := PackUserLite(u)
		assert.Equal(t, &blur, lite.AvatarBlurhash)
	})

	t.Run("bannerId nil → bannerUrl / bannerBlurhash nil", func(t *testing.T) {
		u := &model.User{ID: "u", Username: "u", BannerURL: &bannerURL, BannerBlurhash: &blur}
		d := PackUserDetailed(u, nil)
		assert.Nil(t, d.BannerURL, "bannerId が無ければ bannerUrl も出さない")
		assert.Nil(t, d.BannerBlurhash, "bannerId が無ければ bannerBlurhash も出さない")
	})

	t.Run("bannerId set → banner kept", func(t *testing.T) {
		bid := "b1"
		u := &model.User{ID: "u", Username: "u", BannerID: &bid, BannerURL: &bannerURL, BannerBlurhash: &blur}
		d := PackUserDetailed(u, nil)
		require.NotNil(t, d.BannerURL)
		assert.Equal(t, bannerURL, *d.BannerURL)
		assert.Equal(t, &blur, d.BannerBlurhash)
	})
}

// #1558 GateCountVisibility は visibility / viewer 関係に応じて count を 0 化する。
func TestGateCountVisibility(t *testing.T) {
	cases := []struct {
		name                           string
		followersVis, followingVis     string
		isMe, isModerator, isFollowing bool
		wantFollowers, wantFollowing   int
	}{
		{"public always shown", "public", "public", false, false, false, 10, 20},
		{"private hidden from non-follower", "private", "private", false, false, false, 0, 0},
		{"private shown to self", "private", "private", true, false, false, 10, 20},
		{"private shown to moderator", "private", "private", false, true, false, 10, 20},
		{"followers hidden from non-follower", "followers", "followers", false, false, false, 0, 0},
		{"followers shown to follower", "followers", "followers", false, false, true, 10, 20},
		{"private hidden even from follower", "private", "private", false, false, true, 0, 0},
		{"mixed: public followers + private following", "public", "private", false, false, false, 10, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// **packer を通して組む。** packer は既定でカウントを伏せ、
			// `GateCountVisibility` が見せてよい閲覧者へ入れ直す設計なので、
			// 手組みの UserDetailed では「入れ直す元の値」が無い。
			u := &model.User{ID: "u", Username: "u", FollowersCount: 10, FollowingCount: 20}
			prof := &model.UserProfile{
				UserID:              "u",
				FollowersVisibility: model.FollowingVisibility(c.followersVis),
				FollowingVisibility: model.FollowingVisibility(c.followingVis),
			}
			packed := PackUserDetailed(u, prof)
			d := &packed
			GateCountVisibility(d, c.isMe, c.isModerator, c.isFollowing)
			assert.Equal(t, c.wantFollowers, d.FollowersCount)
			assert.Equal(t, c.wantFollowing, d.FollowingCount)
		})
	}
}

func TestPackUserDetailed_VerifiedLinks(t *testing.T) {
	u := &model.User{
		ID:                "user6",
		Username:          "verified",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	profile := &model.UserProfile{
		UserID:        "user6",
		VerifiedLinks: model.StringArray{"https://example.com", "https://blog.example.com"},
		Fields:        datatypes.JSON([]byte("[]")),
	}

	detailed := PackUserDetailed(u, profile)

	assert.Equal(t, []string{"https://example.com", "https://blog.example.com"}, detailed.VerifiedLinks)
}

func TestPackUserDetailed_DefaultValues(t *testing.T) {
	u := &model.User{
		ID:                "user7",
		Username:          "defaults",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	// nilプロフィールのデフォルト
	detailed := PackUserDetailed(u, nil)

	assert.Equal(t, "public", detailed.FollowersVisibility)
	assert.Equal(t, "public", detailed.FollowingVisibility)
	assert.NotNil(t, detailed.VerifiedLinks)
	assert.Empty(t, detailed.VerifiedLinks)
}

// #692: chatScope を含めることで FE の /settings/privacy が `i/update` の
// レスポンスから現在値を取り戻せる。expose を忘れると DB は更新されている
// のに UI が "保存されない" 表示になる退行を防ぐ。
func TestPackUserDetailed_ExposesChatScope(t *testing.T) {
	cases := []string{"everyone", "followers", "following", "mutual", "none"}
	for _, scope := range cases {
		t.Run(scope, func(t *testing.T) {
			u := &model.User{
				ID:                "u",
				Username:          "u",
				ChatScope:         scope,
				AvatarDecorations: datatypes.JSON([]byte("[]")),
			}
			detailed := PackUserDetailed(u, nil)
			assert.Equal(t, scope, detailed.ChatScope)
		})
	}
}

func TestPackUserDetailed_NilProfile(t *testing.T) {
	u := &model.User{
		ID:                "user4",
		Username:          "noprofile",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	detailed := PackUserDetailed(u, nil)

	assert.Equal(t, "user4", detailed.ID)
	assert.Nil(t, detailed.Description)
	assert.Nil(t, detailed.Location)
	assert.Nil(t, detailed.Birthday)
	assert.Nil(t, detailed.Lang)
}

// --- Phase 7-5a (#247): UserLite 追加 optional フィールド ---

func TestPackUserLite_RequireSigninToViewContents_TrueExposed(t *testing.T) {
	u := &model.User{
		ID: "u1", Username: "x", AvatarDecorations: datatypes.JSON([]byte("[]")),
		RequireSigninToViewContents: true,
	}
	lite := PackUserLite(u)
	require.NotNil(t, lite.RequireSigninToViewContents)
	assert.Equal(t, true, *lite.RequireSigninToViewContents)
}

func TestPackUserLite_RequireSigninToViewContents_FalseOmitted(t *testing.T) {
	u := &model.User{
		ID: "u1", Username: "x", AvatarDecorations: datatypes.JSON([]byte("[]")),
		RequireSigninToViewContents: false,
	}
	lite := PackUserLite(u)
	// false → nil (TS で undefined、JSONはomit)
	assert.Nil(t, lite.RequireSigninToViewContents)

	// JSONマーシャル結果でもキーが出ないこと
	b, err := json.Marshal(lite)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "requireSigninToViewContents")
}

func TestPackUserLite_MakeNotesBefore(t *testing.T) {
	// makeNotes*Before は integer カラム (TS MiUser.ts:208) で「秒単位、
	// マイナスで相対時間」と定義されている。int32 (~2.1B) 範囲内の値を使う
	// こと (ms timestamp 等の大きい値は DB round-trip で overflow する)。
	followersSec := 1700000000 // 2023-11-14T22:13:20Z 相当の UNIX 秒
	hiddenRelSec := -86400     // 過去 1 日 (相対時間の例)
	u := &model.User{
		ID: "u1", Username: "x", AvatarDecorations: datatypes.JSON([]byte("[]")),
		MakeNotesFollowersOnlyBefore: &followersSec,
		MakeNotesHiddenBefore:        &hiddenRelSec,
	}
	lite := PackUserLite(u)
	require.NotNil(t, lite.MakeNotesFollowersOnlyBefore)
	assert.Equal(t, 1700000000, *lite.MakeNotesFollowersOnlyBefore)
	require.NotNil(t, lite.MakeNotesHiddenBefore)
	assert.Equal(t, -86400, *lite.MakeNotesHiddenBefore)
}

func TestPackUserLite_MakeNotesBefore_NilOmitted(t *testing.T) {
	u := &model.User{
		ID: "u1", Username: "x", AvatarDecorations: datatypes.JSON([]byte("[]")),
		// MakeNotes*Before は nil
	}
	lite := PackUserLite(u)
	assert.Nil(t, lite.MakeNotesFollowersOnlyBefore)
	assert.Nil(t, lite.MakeNotesHiddenBefore)

	b, err := json.Marshal(lite)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "makeNotesFollowersOnlyBefore")
	assert.NotContains(t, string(b), "makeNotesHiddenBefore")
}

func TestPackUserLite_Instance_NilByDefault(t *testing.T) {
	u := &model.User{
		ID: "u1", Username: "x", AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	lite := PackUserLite(u)
	// PackUserLite 自体は Instance を設定しない (caller がpre-fetchしてセット)
	assert.Nil(t, lite.Instance)

	b, err := json.Marshal(lite)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "\"instance\"")
}

func TestUserLite_Instance_WhenSet(t *testing.T) {
	u := &model.User{
		ID: "u1", Username: "x", AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	lite := PackUserLite(u)
	name := "example.com"
	iconURL := "https://example.com/icon.png"
	lite.Instance = &InstanceLite{Name: &name, IconURL: &iconURL}

	b, err := json.Marshal(lite)
	require.NoError(t, err)
	assert.Contains(t, string(b), "\"instance\"")
	assert.Contains(t, string(b), "example.com")
}

// UserDetailed は UserLite を埋め込むため、Instance 等もそこに含まれる
// ことを確認する (移動に伴う regression guard)。
func TestUserDetailed_EmbedsUserLiteOptionalFields(t *testing.T) {
	req := true
	u := &model.User{
		ID: "u1", Username: "x", AvatarDecorations: datatypes.JSON([]byte("[]")),
		RequireSigninToViewContents: req,
	}
	detailed := PackUserDetailed(u, nil)
	require.NotNil(t, detailed.RequireSigninToViewContents)
	assert.True(t, *detailed.RequireSigninToViewContents)

	// Instance 設定も embed 経由で動く
	host := "remote.example"
	detailed.Instance = &InstanceLite{Name: &host}
	b, err := json.Marshal(detailed)
	require.NoError(t, err)
	assert.Contains(t, string(b), "\"instance\"")
}

// profile が nil (リモートユーザーで user_profile レコードが未生成) の場合でも
// fields は空配列 `[]` として marshal されるべき。null だとフロントが
// user.fields.length で TypeError を起こし、ユーザーページの概要タブが
// 真っ白になる。
func TestPackUserDetailed_FieldsDefaultWhenProfileNil(t *testing.T) {
	u := &model.User{ID: "u1", Username: "x", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	detailed := PackUserDetailed(u, nil)
	b, err := json.Marshal(detailed)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"fields":[]`)
	assert.NotContains(t, string(b), `"fields":null`)
}

func TestPackUserForFollowStreamEvent(t *testing.T) {
	idGen := newTestIDGen(t)
	uid := idGen.Generate(time.Now())
	u := &model.User{ID: uid, Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}

	t.Run("follow event sets isFollowing=true and hasPending=false", func(t *testing.T) {
		out := PackUserForFollowStreamEvent(u, true, false, idGen)
		require.NotNil(t, out.IsFollowing)
		assert.True(t, *out.IsFollowing)
		require.NotNil(t, out.HasPendingFollowRequestFromYou)
		assert.False(t, *out.HasPendingFollowRequestFromYou)
		assert.Equal(t, uid, out.ID)
		assert.Equal(t, "alice", out.Username)
		// idGen から復元した createdAt が空でないこと (misskey_dart の
		// DateTimeConverter が FormatException で落ちないため)。
		assert.NotEmpty(t, out.CreatedAt)
		// EnsureRelationFlags が WithRelations parse 用の関係 bool を埋めること。
		require.NotNil(t, out.IsFollowed)
		assert.False(t, *out.IsFollowed)
		require.NotNil(t, out.IsBlocking)
		assert.False(t, *out.IsBlocking)
		require.NotNil(t, out.IsMuted)
		assert.False(t, *out.IsMuted)
	})

	t.Run("unfollow event sets isFollowing=false and hasPending=false", func(t *testing.T) {
		out := PackUserForFollowStreamEvent(u, false, false, idGen)
		require.NotNil(t, out.IsFollowing)
		assert.False(t, *out.IsFollowing)
		require.NotNil(t, out.HasPendingFollowRequestFromYou)
		assert.False(t, *out.HasPendingFollowRequestFromYou)
	})

	t.Run("pending request sets isFollowing=false and hasPending=true", func(t *testing.T) {
		out := PackUserForFollowStreamEvent(u, false, true, idGen)
		require.NotNil(t, out.IsFollowing)
		assert.False(t, *out.IsFollowing)
		require.NotNil(t, out.HasPendingFollowRequestFromYou)
		assert.True(t, *out.HasPendingFollowRequestFromYou)
	})

	t.Run("serialized JSON exposes the viewer fields", func(t *testing.T) {
		out := PackUserForFollowStreamEvent(u, true, false, idGen)
		b, err := json.Marshal(out)
		require.NoError(t, err)
		assert.Contains(t, string(b), `"isFollowing":true`)
		assert.Contains(t, string(b), `"hasPendingFollowRequestFromYou":false`)
	})
}

// TestUserDetailed_OmitsAvatarIDKey は #1251 の core 修正を検証する。misskey_dart の
// UserDetailed union dispatch は `avatarId` key の存在で MeDetailed を選ぶため、
// 他人ビュー (UserDetailed / users explore) では avatarId / bannerId key を一切
// 出してはならない。出すと NotMe ユーザーが MeDetailed として誤 parse され、
// createdAt="" 等で fromJson が落ちる。
func TestUserDetailed_OmitsAvatarIDKey(t *testing.T) {
	idGen := newTestIDGen(t)
	uid := idGen.Generate(time.Now())
	avatarID := "avatar-id-1"
	bannerID := "banner-id-1"
	u := &model.User{
		ID:                uid,
		Username:          "other",
		AvatarID:          &avatarID,
		BannerID:          &bannerID,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	detailed := PackUserDetailed(u, nil, idGen)
	b, err := json.Marshal(detailed)
	require.NoError(t, err)

	// avatarId / bannerId key 自体が出てはいけない (MeDetailed discriminator のため)。
	assert.NotContains(t, string(b), `"avatarId"`)
	assert.NotContains(t, string(b), `"bannerId"`)
	// idGen から復元した createdAt が非空であること (NotMe parse が落ちない)。
	assert.NotEmpty(t, detailed.CreatedAt)
}

// TestMeDetailed_EmitsAvatarIDKey は self-view では avatarId / bannerId key が
// 出ることを検証する。これが MeDetailed dispatch の discriminator であり、
// avatar 未設定でも `"avatarId":null` の key が存在する必要がある (#1251)。
func TestMeDetailed_EmitsAvatarIDKey(t *testing.T) {
	idGen := newTestIDGen(t)
	uid := idGen.Generate(time.Now())

	t.Run("with avatar set", func(t *testing.T) {
		avatarID := "avatar-id-1"
		u := &model.User{
			ID:                uid,
			Username:          "me",
			AvatarID:          &avatarID,
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		}
		me := PackMeDetailed(u, nil, idGen)
		b, err := json.Marshal(me)
		require.NoError(t, err)
		assert.Contains(t, string(b), `"avatarId":"avatar-id-1"`)
		assert.Contains(t, string(b), `"bannerId"`)
		require.NotNil(t, me.AvatarID)
		assert.Equal(t, "avatar-id-1", *me.AvatarID)
	})

	t.Run("without avatar still emits null key for dispatch", func(t *testing.T) {
		u := &model.User{
			ID:                uid,
			Username:          "me",
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		}
		me := PackMeDetailed(u, nil, idGen)
		b, err := json.Marshal(me)
		require.NoError(t, err)
		// avatar 未設定でも key 自体は出ること (dispatch discriminator)。
		assert.Contains(t, string(b), `"avatarId":null`)
		assert.Contains(t, string(b), `"bannerId":null`)
	})
}

// #968: PackMeDetailed が User / UserProfile から self-view-only field を
// 全て transfer することを確認する。i/update の response はフロントの
// updateCurrentAccountPartial にそのまま流れ込むので、欠損すると session
// state が stale なまま残る。
func TestPackMeDetailed_TransfersSelfViewFields(t *testing.T) {
	u := &model.User{
		ID:                "me1",
		Username:          "me",
		IsExplorable:      false,
		IsDeleted:         true,
		HideOnlineStatus:  true,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	profile := &model.UserProfile{
		UserID:                   "me1",
		NoCrawle:                 true,
		PreventAiLearning:        false,
		AutoSensitive:            true,
		CarefulBot:               true,
		AutoAcceptFollowed:       true,
		AlwaysMarkNsfw:           true,
		ReceiveAnnouncementEmail: false,
		InjectFeaturedNote:       false,
		Fields:                   datatypes.JSON([]byte("[]")),
	}

	me := PackMeDetailed(u, profile)

	assert.False(t, me.IsExplorable)
	assert.True(t, me.IsDeleted)
	assert.True(t, me.HideOnlineStatus)
	assert.True(t, me.NoCrawle)
	assert.False(t, me.PreventAiLearning)
	assert.True(t, me.AutoSensitive)
	assert.True(t, me.CarefulBot)
	assert.True(t, me.AutoAcceptFollowed)
	assert.True(t, me.AlwaysMarkNsfw)
	assert.False(t, me.ReceiveAnnouncementEmail)
	assert.False(t, me.InjectFeaturedNote)

	// UserDetailed embed が機能していることも確認する。
	assert.Equal(t, "me1", me.ID)
	assert.Equal(t, "me", me.Username)
}

// #968: profile が nil でも panic せず、User 由来の self-view field のみ
// transfer されること。Profile fetch が失敗した fallback path で必要。
func TestPackMeDetailed_NilProfile(t *testing.T) {
	u := &model.User{
		ID:                "me2",
		Username:          "noprof",
		IsExplorable:      true,
		HideOnlineStatus:  true,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	me := PackMeDetailed(u, nil)

	assert.True(t, me.IsExplorable)
	assert.True(t, me.HideOnlineStatus)
	// profile が nil の fallback path では profile 由来 field は Go zero
	// (= bool false) になる。DB の column default (例: preventAiLearning は
	// `default:true`) とは一致しないが、これは Profile fetch が失敗した
	// 例外パスでのみ発火する。frontend は次の /api/i fetch で正しい値を
	// 取り直す前提なので、ここで DB default 相当に倒す必要はない。
	assert.False(t, me.NoCrawle)
	assert.False(t, me.PreventAiLearning)
	assert.False(t, me.InjectFeaturedNote)
	assert.False(t, me.ReceiveAnnouncementEmail)
}

// #1249: EnsureRelationFlags は IsFollowing がセット済みのとき、未計算の
// relation bool を false で埋める (misskey_dart の WithRelations variant 互換)。
func TestEnsureRelationFlags(t *testing.T) {
	t.Run("fills nil flags when isFollowing set", func(t *testing.T) {
		tr := true
		d := UserDetailed{IsFollowing: &tr}
		d.EnsureRelationFlags()
		for name, p := range map[string]*bool{
			"isFollowed": d.IsFollowed, "isBlocking": d.IsBlocking,
			"isBlocked": d.IsBlocked, "isMuted": d.IsMuted, "isRenoteMuted": d.IsRenoteMuted,
			"hasPendingFollowRequestFromYou": d.HasPendingFollowRequestFromYou,
			"hasPendingFollowRequestToYou":   d.HasPendingFollowRequestToYou,
		} {
			require.NotNil(t, p, "%s must be filled", name)
			assert.False(t, *p)
		}
	})
	t.Run("no-op when isFollowing nil", func(t *testing.T) {
		d := UserDetailed{}
		d.EnsureRelationFlags()
		assert.Nil(t, d.IsBlocking)
		assert.Nil(t, d.IsFollowed)
	})
	t.Run("does not overwrite already-set flags", func(t *testing.T) {
		tr, fa := true, false
		d := UserDetailed{IsFollowing: &tr, IsBlocking: &tr, IsMuted: &fa}
		d.EnsureRelationFlags()
		assert.True(t, *d.IsBlocking, "existing true must be preserved")
		assert.False(t, *d.IsMuted)
	})
}

// #1240: misskey_dart の MeDetailed.fromJson が非null List/num として cast する
// mutedWords / mutedInstances / achievements / loggedInDays が profile から
// 正しく埋まること。
func TestPackMeDetailed_ListFields(t *testing.T) {
	u := &model.User{ID: "me4", Username: "lists", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	profile := &model.UserProfile{
		UserID:         "me4",
		Fields:         datatypes.JSON([]byte("[]")),
		MutedWords:     datatypes.JSON([]byte(`[["foo","bar"],"baz"]`)),
		MutedInstances: datatypes.JSON([]byte(`["evil.example"]`)),
		Achievements:   datatypes.JSON([]byte(`[{"name":"a","unlockedAt":1}]`)),
		LoggedInDates:  model.StringArray{"2026-05-01", "2026-05-02"},
	}
	me := PackMeDetailed(u, profile)
	assert.Len(t, me.MutedWords, 2)
	assert.Equal(t, []string{"evil.example"}, me.MutedInstances)
	assert.Len(t, me.Achievements, 1)
	assert.Equal(t, 2, me.LoggedInDays)
}

// 空 / nil profile でも非null 空配列を保つこと (#1240)。
func TestPackMeDetailed_ListFieldsEmptyAreNonNull(t *testing.T) {
	u := &model.User{ID: "me5", Username: "empty", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	me := PackMeDetailed(u, nil)
	assert.Equal(t, []any{}, me.MutedWords)
	assert.Equal(t, []string{}, me.MutedInstances)
	assert.Equal(t, []any{}, me.Achievements)
	assert.Equal(t, 0, me.LoggedInDays)
}

// #1258 fu: MeDetailedOnly の HIGH 5 field が struct から非null で出ること。
func TestPackMeDetailed_MeDetailedOnlyHighFields(t *testing.T) {
	u := &model.User{ID: "me_high", Username: "high", AvatarDecorations: datatypes.JSON([]byte("[]"))}

	// nil profile -> 非null default (users/show self の best-effort と同じ)。
	me := PackMeDetailed(u, nil)
	assert.Equal(t, []any{}, me.UnreadAnnouncements)
	assert.Equal(t, []any{}, me.HardMutedWords)
	assert.False(t, me.HasUnreadChatMessages)
	assert.Equal(t, 0, me.UnreadNotificationsCount)
	assert.Equal(t, "none", me.TwoFactorBackupCodesStock)

	// JSON 上 5 field とも present (golden MeDetailedOnly は全て required)。
	b, err := json.Marshal(me)
	require.NoError(t, err)
	for _, k := range []string{"unreadAnnouncements", "hardMutedWords", "hasUnreadChatMessages", "unreadNotificationsCount", "twoFactorBackupCodesStock"} {
		assert.Containsf(t, string(b), `"`+k+`"`, "%s must be present", k)
	}
}

// #1258 fu: profile 由来の hardMutedWords / twoFactorBackupCodesStock は
// AsMeDetailed が正しく計算する (= meUpdated/i-update でも clobber しない)。
func TestPackMeDetailed_ProfileDerivedHighFields(t *testing.T) {
	u := &model.User{ID: "me_pd", Username: "pd", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	profile := &model.UserProfile{
		UserID:                "me_pd",
		Fields:                datatypes.JSON([]byte("[]")),
		HardMutedWords:        datatypes.JSON([]byte(`[["foo","bar"],["baz"]]`)),
		TwoFactorEnabled:      true,
		SecurityKeysAvailable: true,
		TwoFactorBackupSecret: model.StringArray{"a", "b"}, // 2 codes -> partial
	}
	me := PackMeDetailed(u, profile)
	assert.Len(t, me.HardMutedWords, 2, "hardMutedWords は jsonb から展開")
	assert.Equal(t, "partial", me.TwoFactorBackupCodesStock)
	// #1921: 2FA 有効 + key 有り → securityKeys true。
	assert.True(t, me.SecurityKeys, "2FA 有効 + key 有りで securityKeys true")

	profile.TwoFactorBackupSecret = model.StringArray{"a", "b", "c", "d", "e"} // >=5 -> full
	assert.Equal(t, "full", PackMeDetailed(u, profile).TwoFactorBackupCodesStock)

	profile.TwoFactorEnabled = false // 2FA off -> none
	off := PackMeDetailed(u, profile)
	assert.Equal(t, "none", off.TwoFactorBackupCodesStock)
	// #1921: 2FA 無効なら SecurityKeysAvailable=true でも securityKeys false。
	assert.False(t, off.SecurityKeys, "2FA 無効なら securityKeys false")
}

// #1240: PackMeDetailed 単体 (policies override 無し) は policies を **出さない**
// こと。i/update / meUpdated はこれを直接 frontend の updateCurrentAccountPartial
// に流すため、`policies:null` を出すと `$i.policies` を null 上書きして policy
// 判定が壊れる。omitempty で省略されることを保証する。
func TestPackMeDetailed_OmitsPoliciesWhenUnset(t *testing.T) {
	u := &model.User{ID: "me6", Username: "nopol", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	me := PackMeDetailed(u, &model.UserProfile{UserID: "me6", Fields: datatypes.JSON([]byte("[]"))})
	assert.Nil(t, me.Policies)
	b, err := json.Marshal(me)
	require.NoError(t, err)
	assert.NotContains(t, string(b), `"policies"`, "PackMeDetailed must omit policies (not emit null) so i/update merge does not null out $i.policies")
}

// #968: serialized JSON が drop-in 互換 key 名 (isExplorable / noCrawle 等)
// を出すこと。フロントの updateCurrentAccountPartial が key 名で merge
// するので、key drift があると session には反映されない。
func TestPackMeDetailed_SerializedJSON(t *testing.T) {
	u := &model.User{
		ID:                "me3",
		Username:          "json",
		IsExplorable:      true,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	profile := &model.UserProfile{
		UserID:            "me3",
		NoCrawle:          true,
		PreventAiLearning: true,
		Fields:            datatypes.JSON([]byte("[]")),
	}

	me := PackMeDetailed(u, profile)
	b, err := json.Marshal(me)
	require.NoError(t, err)
	s := string(b)
	assert.Contains(t, s, `"isExplorable":true`)
	assert.Contains(t, s, `"noCrawle":true`)
	assert.Contains(t, s, `"preventAiLearning":true`)
	assert.Contains(t, s, `"hideOnlineStatus":false`)
	assert.Contains(t, s, `"isDeleted":false`)
}

// #985: notification 3 field (emailNotificationTypes /
// mutingNotificationTypes / notificationRecieveConfig) のデフォルト値が
// JSON に乗ること + emailNotificationTypes の default が upstream と一致。
func TestPackMeDetailed_NotificationDefaults(t *testing.T) {
	u := &model.User{
		ID:                "me4",
		Username:          "default-notif",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	profile := &model.UserProfile{
		UserID: "me4",
		Fields: datatypes.JSON([]byte("[]")),
	}

	me := PackMeDetailed(u, profile)

	assert.Equal(t, []string{"follow", "receiveFollowRequest"}, me.EmailNotificationTypes)
	assert.Equal(t, []string{}, me.MutingNotificationTypes)
	assert.Equal(t, map[string]any{}, me.NotificationRecieveConfig)

	b, err := json.Marshal(me)
	require.NoError(t, err)
	s := string(b)
	assert.Contains(t, s, `"emailNotificationTypes":["follow","receiveFollowRequest"]`)
	assert.Contains(t, s, `"mutingNotificationTypes":[]`)
	assert.Contains(t, s, `"notificationRecieveConfig":{}`)
}

// #985: profile JSON column に値が入っているときに override されることを
// 確認 (emailNotificationTypes / notificationRecieveConfig の path をカバー)。
func TestPackMeDetailed_NotificationOverride(t *testing.T) {
	u := &model.User{
		ID:                "me5",
		Username:          "override-notif",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	profile := &model.UserProfile{
		UserID:                    "me5",
		EmailNotificationTypes:    datatypes.JSON([]byte(`["mention"]`)),
		NotificationRecieveConfig: datatypes.JSON([]byte(`{"mention":{"type":"following"}}`)),
		Fields:                    datatypes.JSON([]byte("[]")),
	}

	me := PackMeDetailed(u, profile)
	assert.Equal(t, []string{"mention"}, me.EmailNotificationTypes)
	require.Contains(t, me.NotificationRecieveConfig, "mention")
}

// #985: profile が nil の fallback path でも 3 field は default で埋まる。
func TestPackMeDetailed_NotificationNilProfile(t *testing.T) {
	u := &model.User{
		ID:                "me6",
		Username:          "nilprof-notif",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	me := PackMeDetailed(u, nil)
	assert.Equal(t, []string{"follow", "receiveFollowRequest"}, me.EmailNotificationTypes)
	assert.Equal(t, []string{}, me.MutingNotificationTypes)
	assert.Equal(t, map[string]any{}, me.NotificationRecieveConfig)
}

// #970: AsMeDetailed が pre-built UserDetailed の embed 値を保持しつつ
// MeDetailed-only field を layering する direct entry test。users/show の
// self-view branch が build 済の detailed (= remote stats / instance /
// emojis / pinned / viewer-dependent fields を含む) を渡しても上書きされず、
// 11 self-view field + notification 3 field のみが追加されること。
func TestAsMeDetailed_PreservesEmbeddedUserDetailed(t *testing.T) {
	u := &model.User{
		ID:                "as1",
		Username:          "asuser",
		IsExplorable:      true,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	profile := &model.UserProfile{
		UserID:   "as1",
		NoCrawle: true,
		Fields:   datatypes.JSON([]byte("[]")),
	}
	// pre-built UserDetailed に external な後付け修正 (= users/show が行う
	// remote stats / pinned 反映に相当) を加えたものを base として渡す。
	d := PackUserDetailed(u, profile)
	d.NotesCount = 42
	pinnedIDs := []string{"note1", "note2"}
	d.PinnedNoteIDs = pinnedIDs

	me := AsMeDetailed(d, u, profile)

	// 後付け修正値は保持されている
	assert.Equal(t, 42, me.NotesCount)
	assert.Equal(t, pinnedIDs, me.PinnedNoteIDs)
	// MeDetailed-only field が layering される
	assert.True(t, me.IsExplorable)
	assert.True(t, me.NoCrawle)
	assert.Equal(t, []string{"follow", "receiveFollowRequest"}, me.EmailNotificationTypes)
}

// #985: profile column の JSON が壊れているときは default に倒し silent
// fallback する (parse error は ignore)。
func TestPackMeDetailed_NotificationMalformed(t *testing.T) {
	u := &model.User{
		ID:                "me7",
		Username:          "malformed-notif",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	profile := &model.UserProfile{
		UserID:                    "me7",
		EmailNotificationTypes:    datatypes.JSON([]byte(`not-json`)),
		NotificationRecieveConfig: datatypes.JSON([]byte(`not-json`)),
		Fields:                    datatypes.JSON([]byte("[]")),
	}
	me := PackMeDetailed(u, profile)
	assert.Equal(t, []string{"follow", "receiveFollowRequest"}, me.EmailNotificationTypes)
	assert.Equal(t, map[string]any{}, me.NotificationRecieveConfig)
}

// remote user (host != nil) は upstream UserEntityService と同じく
// publicReactions を常に false にする (issue 12964)。これが true だと
// frontend が reactions タブを表示し、users/reactions が IS_REMOTE_USER で
// error になる (リモートユーザーをローカルと誤認するバグの回帰防止)。
func TestPackUserDetailed_RemoteUserPublicReactionsFalse(t *testing.T) {
	host := "remote.example.com"
	u := &model.User{
		ID:                "ruser",
		Username:          "alice",
		Host:              &host,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	// profile が publicReactions=true でも remote なら false に上書きされる。
	profile := &model.UserProfile{UserID: "ruser", PublicReactions: true, Fields: datatypes.JSON([]byte("[]"))}

	detailed := PackUserDetailed(u, profile)
	assert.False(t, detailed.PublicReactions, "remote user must report publicReactions=false")
}

// local user は profile の publicReactions をそのまま反映する (remote 上書きが
// local を巻き込まない回帰防止)。
func TestPackUserDetailed_LocalUserPublicReactionsPreserved(t *testing.T) {
	u := &model.User{ID: "luser", Username: "bob", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	profile := &model.UserProfile{UserID: "luser", PublicReactions: true, Fields: datatypes.JSON([]byte("[]"))}

	detailed := PackUserDetailed(u, profile)
	assert.True(t, detailed.PublicReactions, "local user with publicReactions=true must stay true")
}
