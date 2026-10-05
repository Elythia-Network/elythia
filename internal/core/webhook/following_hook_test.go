package webhook_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/webhook"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeInstanceLookup struct{ rows []*model.Instance }

func (f fakeInstanceLookup) FindManyByHosts(hosts []string) ([]*model.Instance, error) {
	var out []*model.Instance
	for _, r := range f.rows {
		for _, h := range hosts {
			if r.Host == h {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

type fakeEmojiLookup struct{ rows []*model.Emoji }

func (f fakeEmojiLookup) FindManyByNamesAndHost(names []string, host *string) ([]*model.Emoji, error) {
	var out []*model.Emoji
	for _, e := range f.rows {
		if host == nil || e.Host == nil || *e.Host != *host {
			continue
		}
		for _, n := range names {
			if e.Name == n {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

type fakeProfileLookup struct {
	profiles map[string]*model.UserProfile
	err      error
	calls    int
}

func (f *fakeProfileLookup) FindProfileByUserID(userID string) (*model.UserProfile, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.profiles[userID], nil
}

// fakeRelations は viewer -> target のフォロー関係だけを持つ (他の関係は false)。
type fakeRelations struct{ following map[string]bool }

func (f fakeRelations) Apply(d *entity.UserDetailed, viewerID string, target *model.User, _ *model.UserProfile) bool {
	isFollowing := f.following[viewerID+">"+target.ID]
	d.IsFollowing = &isFollowing
	d.EnsureRelationFlags()
	return isFollowing
}

type fakeModerators map[string]bool

func (f fakeModerators) IsModerator(userID string) bool { return f[userID] }

const remoteHost = "remote.example"

func remoteUser() *model.User {
	h := remoteHost
	return &model.User{
		ID: "9zzzzzzzzz", Username: "carol", UsernameLower: "carol", Host: &h,
		Emojis: model.StringArray{"blobcat"}, FollowersCount: 7, FollowingCount: 3,
	}
}

func localUser() *model.User {
	return &model.User{ID: "9yyyyyyyyy", Username: "alice", UsernameLower: "alice"}
}

func followLookups(profiles *fakeProfileLookup, following map[string]bool, mods fakeModerators) webhook.UserLookups {
	host := remoteHost
	name := "Remote"
	return webhook.UserLookups{
		Instances:  fakeInstanceLookup{rows: []*model.Instance{{Host: remoteHost, Name: &name}}},
		Emojis:     fakeEmojiLookup{rows: []*model.Emoji{{Name: "blobcat", Host: &host, PublicURL: "https://remote.example/blobcat.png"}}},
		Profiles:   profiles,
		Relations:  fakeRelations{following: following},
		Moderators: mods,
	}
}

// wireFollowLookups wires lookups as production does, with a profile for each
// of the given user IDs.
func wireFollowLookups(t *testing.T, h *webhook.FollowingHook, userIDs ...string) {
	t.Helper()
	profiles := &fakeProfileLookup{profiles: map[string]*model.UserProfile{}}
	for _, id := range userIDs {
		profiles.profiles[id] = &model.UserProfile{UserID: id}
	}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetUserLookups(followLookups(profiles, nil, nil), idGen)
}

// userOf decodes the `user` of the single enqueued delivery.
func userOf(t *testing.T, enq *fakeEnqueuer) map[string]any {
	t.Helper()
	require.Len(t, enq.userCalls, 1)
	var env struct {
		Body struct {
			User map[string]any `json:"user"`
		} `json:"body"`
	}
	require.NoError(t, json.Unmarshal(enq.userCalls[0].Body, &env))
	require.NotNil(t, env.Body.User)
	return env.Body.User
}

func newFollowingHook(t *testing.T, owner, event string, l webhook.UserLookups) (*webhook.FollowingHook, *fakeEnqueuer) {
	t.Helper()
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h1"] = &model.Webhook{ID: "h1", UserID: owner, Active: true, On: model.StringArray{event}}
	h := webhook.NewFollowingHook(svc)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetUserLookups(l, idGen)
	return h, enq
}

// follow / unfollow の user は本家と同じく UserDetailedNotMe (閲覧者はフォローした側)。
// リモートの相手には instance と解決済みの絵文字が付く (#3269)。
func TestFollowingHook_FollowSendsDetailedNotMe(t *testing.T) {
	follower, followee := localUser(), remoteUser()
	desc := "hello"
	profiles := &fakeProfileLookup{profiles: map[string]*model.UserProfile{
		followee.ID: {UserID: followee.ID, Description: &desc, PublicReactions: true,
			FollowersVisibility: model.FollowingVisibilityFollowers, FollowingVisibility: model.FollowingVisibilityPublic},
	}}
	h, enq := newFollowingHook(t, follower.ID, webhook.EventFollow,
		followLookups(profiles, map[string]bool{follower.ID + ">" + followee.ID: true}, nil))

	h.OnFollow(follower, followee)

	u := userOf(t, enq)
	assert.Equal(t, followee.ID, u["id"])
	// UserDetailed の項目
	assert.Equal(t, "hello", u["description"])
	assert.NotEmpty(t, u["createdAt"])
	assert.Contains(t, u, "pinnedNoteIds")
	// 閲覧者 (フォローした側) から見た関係
	assert.Equal(t, true, u["isFollowing"])
	assert.Equal(t, false, u["isBlocking"])
	// フォロワー限定のカウントは、フォロワーになった閲覧者に見える
	assert.EqualValues(t, 7, u["followersCount"])
	assert.EqualValues(t, 3, u["followingCount"])
	// UserLite のリモート解決
	require.IsType(t, map[string]any{}, u["instance"])
	assert.Equal(t, "Remote", u["instance"].(map[string]any)["name"])
	assert.Equal(t, map[string]any{"blobcat": "https://remote.example/blobcat.png"}, u["emojis"])
	// モデレーターでない閲覧者には出さない
	assert.NotContains(t, u, "moderationNote")
	assert.NotContains(t, u, "twoFactorEnabled")
}

func TestFollowingHook_UnfollowSendsDetailedNotMe(t *testing.T) {
	follower, followee := localUser(), remoteUser()
	profiles := &fakeProfileLookup{profiles: map[string]*model.UserProfile{
		followee.ID: {UserID: followee.ID, PublicReactions: true,
			FollowersVisibility: model.FollowingVisibilityFollowers, FollowingVisibility: model.FollowingVisibilityPublic},
	}}
	h, enq := newFollowingHook(t, follower.ID, webhook.EventUnfollow, followLookups(profiles, nil, nil))

	h.OnUnfollow(follower, followee)

	u := userOf(t, enq)
	assert.Equal(t, followee.ID, u["id"])
	assert.Equal(t, false, u["isFollowing"])
	// unfollow の後はフォロワーでないので、フォロワー限定のカウントは伏せる (本家は 0)
	assert.EqualValues(t, 0, u["followersCount"])
	assert.EqualValues(t, 3, u["followingCount"])
	assert.Equal(t, "Remote", u["instance"].(map[string]any)["name"])
	assert.Equal(t, map[string]any{"blobcat": "https://remote.example/blobcat.png"}, u["emojis"])
}

// followed の user は本家と同じく UserLite (pack の既定)。リモートなら instance と絵文字。
func TestFollowingHook_FollowedSendsUserLite(t *testing.T) {
	follower, followee := remoteUser(), localUser()
	profiles := &fakeProfileLookup{}
	h, enq := newFollowingHook(t, followee.ID, webhook.EventFollowed, followLookups(profiles, nil, nil))

	h.OnFollowed(follower, followee)

	u := userOf(t, enq)
	assert.Equal(t, follower.ID, u["id"])
	assert.Equal(t, "Remote", u["instance"].(map[string]any)["name"])
	assert.Equal(t, map[string]any{"blobcat": "https://remote.example/blobcat.png"}, u["emojis"])
	for _, k := range []string{"description", "createdAt", "followersCount", "isFollowing"} {
		assert.NotContains(t, u, k, "UserLite must not carry %s", k)
	}
	assert.Zero(t, profiles.calls, "UserLite does not read the profile")
}

// 閲覧者がモデレーターなら、本家と同じく moderationNote と 2FA の項目が付く。
func TestFollowingHook_ModeratorViewerGetsModerationFields(t *testing.T) {
	follower, followee := localUser(), remoteUser()
	note := "watch"
	profiles := &fakeProfileLookup{profiles: map[string]*model.UserProfile{
		followee.ID: {UserID: followee.ID, ModerationNote: &note,
			FollowersVisibility: model.FollowingVisibilityPrivate, FollowingVisibility: model.FollowingVisibilityPrivate},
	}}
	h, enq := newFollowingHook(t, follower.ID, webhook.EventUnfollow,
		followLookups(profiles, nil, fakeModerators{follower.ID: true}))

	h.OnUnfollow(follower, followee)

	u := userOf(t, enq)
	assert.Equal(t, "watch", u["moderationNote"])
	assert.Equal(t, false, u["twoFactorEnabled"])
	// モデレーターには private のカウントも見える
	assert.EqualValues(t, 7, u["followersCount"])
}

// profile を読めないときは送らない (本家は findOneByOrFail で例外になる)。
// profile 無しで組むと visibility が public に倒れ、伏せるべきカウントが出る。
func TestFollowingHook_ProfileErrorDropsDelivery(t *testing.T) {
	for name, profiles := range map[string]*fakeProfileLookup{
		"error":   {err: errors.New("db down")},
		"missing": {profiles: map[string]*model.UserProfile{}},
	} {
		t.Run(name, func(t *testing.T) {
			h, enq := newFollowingHook(t, "9yyyyyyyyy", webhook.EventFollow, followLookups(profiles, nil, nil))
			h.OnFollow(localUser(), remoteUser())
			assert.Empty(t, enq.userCalls)
		})
	}
	// 配線が外れて profile の lookup が無いときも、公開範囲を確かめられないので送らない。
	t.Run("unwired", func(t *testing.T) {
		h, enq := newFollowingHook(t, "9yyyyyyyyy", webhook.EventFollow, webhook.UserLookups{})
		h.OnFollow(localUser(), remoteUser())
		assert.Empty(t, enq.userCalls)
	})
}

// 購読していないイベントでは、本文を組み立てない (profile を読まない)。
func TestFollowingHook_NoSubscriberSkipsPacking(t *testing.T) {
	profiles := &fakeProfileLookup{profiles: map[string]*model.UserProfile{}}
	// フォローした側は Webhook を持つが、follow / unfollow は購読していない。
	h, enq := newFollowingHook(t, localUser().ID, webhook.EventFollowed, followLookups(profiles, nil, nil))
	h.OnFollow(localUser(), remoteUser())
	h.OnUnfollow(localUser(), remoteUser())
	assert.Empty(t, enq.userCalls)
	assert.Zero(t, profiles.calls)
}

// 同じ利用者が複数の Webhook で購読していても、本文は 1 回だけ組み立てる。
func TestDispatchUserLazy_BuildsOnceAndHonorsDrop(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["a"] = &model.Webhook{ID: "a", UserID: "u", Active: true, On: model.StringArray{"follow"}}
	userRepo.hooks["b"] = &model.Webhook{ID: "b", UserID: "u", Active: true, On: model.StringArray{"follow"}}
	calls := 0
	svc.DispatchUserLazy("u", "follow", func() (any, bool) {
		calls++
		return map[string]any{"x": 1}, true
	})
	assert.Equal(t, 1, calls)
	assert.Len(t, enq.userCalls, 2)

	enq.userCalls = nil
	svc.DispatchUserLazy("u", "follow", func() (any, bool) { return nil, false })
	assert.Empty(t, enq.userCalls)
}
