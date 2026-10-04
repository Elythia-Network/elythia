package userpack_test

import (
	"context"
	"testing"

	"github.com/shiroha-a/mk/internal/core/userpack"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingMany records the batch call of DetailedMany.
type recordingMany struct {
	calls  int
	viewer *model.User
	ids    []string
}

func (r *recordingMany) FillDetailedExtrasMany(_ context.Context, viewer *model.User, targets []userpack.DetailTarget) {
	r.calls++
	r.viewer = viewer
	for _, t := range targets {
		r.ids = append(r.ids, t.User.ID)
		t.Detailed.PinnedNoteIDs = []string{"pin-" + t.User.ID}
	}
}

func privateProfile(userID string) *model.UserProfile {
	note := "watch"
	return &model.UserProfile{UserID: userID, FollowersVisibility: model.FollowingVisibilityFollowers,
		FollowingVisibility: model.FollowingVisibilityPrivate, ModerationNote: &note, TwoFactorEnabled: true}
}

func TestDetailedMany(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	local := &model.User{ID: "9yyyyyyyyy", Username: "dave", UsernameLower: "dave", FollowersCount: 5, FollowingCount: 4}
	users := []*model.User{target(), local}
	profiles := map[string]*model.UserProfile{target().ID: privateProfile(target().ID), local.ID: privateProfile(local.ID)}

	tests := []struct {
		name          string
		viewer        *model.User
		moderators    fakeModerators
		following     bool
		wantFollowers int
		wantFollowing int
		wantModFields bool
	}{
		{name: "anonymous viewer gets gated counts", viewer: nil, wantFollowers: 0, wantFollowing: 0},
		{name: "follower sees followers-only counts", viewer: &model.User{ID: "v1"}, following: true, wantFollowers: 7, wantFollowing: 0},
		{name: "moderator sees counts and moderator fields", viewer: &model.User{ID: "mod"}, moderators: fakeModerators{"mod": true}, wantFollowers: 7, wantFollowing: 3, wantModFields: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			many := &recordingMany{}
			p := userpack.New(userpack.Lookups{
				Instances: fakeInstances{}, Emojis: fakeEmojis{},
				Relations: fakeRelations{following: tt.following}, Moderators: tt.moderators,
			}, idGen)
			assert.False(t, p.HasDetailExtrasMany())
			p.SetDetailExtrasMany(many)
			assert.True(t, p.HasDetailExtrasMany())

			out := p.DetailedMany(context.Background(), tt.viewer, users, profiles)
			require.Len(t, out, 2)
			assert.Equal(t, target().ID, out[0].ID, "順序は入力どおり")
			assert.EqualValues(t, tt.wantFollowers, out[0].FollowersCount)
			assert.EqualValues(t, tt.wantFollowing, out[0].FollowingCount)
			if tt.wantModFields {
				require.NotNil(t, out[0].ModerationNote)
				assert.Equal(t, "watch", *out[0].ModerationNote)
				require.NotNil(t, out[0].TwoFactorEnabled)
				assert.True(t, *out[0].TwoFactorEnabled)
			} else {
				assert.Nil(t, out[0].ModerationNote)
				assert.Nil(t, out[0].TwoFactorEnabled)
			}
			require.NotNil(t, out[0].IsFollowing, "関係は Relations に任せる (匿名の省略は userrelation 側)")
			assert.Equal(t, 1, many.calls, "一覧の埋め手は 1 回だけ呼ぶ")
			assert.Equal(t, tt.viewer, many.viewer)
			assert.Equal(t, []string{"pin-" + target().ID}, out[0].PinnedNoteIDs)
			assert.Equal(t, []string{"pin-" + local.ID}, out[1].PinnedNoteIDs)
		})
	}
}

// 一覧の埋め手が無い構成でも instance と絵文字は自前で埋める。
func TestDetailedMany_WithoutExtrasManyStillFillsLites(t *testing.T) {
	p := userpack.New(userpack.Lookups{Instances: fakeInstances{}, Emojis: fakeEmojis{}}, nil)
	out := p.DetailedMany(context.Background(), nil, []*model.User{target()}, nil)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Instance)
	assert.Equal(t, "Remote", *out[0].Instance.Name)
	assert.Equal(t, map[string]string{"blobcat": "https://remote.example/blobcat.png"}, out[0].Emojis)
}

// profile が読めない利用者は公開範囲が分からないので、本人とモデレーター以外には
// カウントを伏せる (既定の public に倒して実数を出さない、#3330)。
func TestDetailedMany_MissingProfileHidesCounts(t *testing.T) {
	u := &model.User{ID: "u1", Username: "u1", FollowersCount: 7, FollowingCount: 3}
	tests := []struct {
		name       string
		viewer     *model.User
		moderators fakeModerators
		following  bool
		want       int
	}{
		{name: "anonymous", viewer: nil, want: 0},
		{name: "follower", viewer: &model.User{ID: "v1"}, following: true, want: 0},
		{name: "self", viewer: &model.User{ID: "u1"}, want: 7},
		{name: "moderator", viewer: &model.User{ID: "mod"}, moderators: fakeModerators{"mod": true}, want: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := userpack.New(userpack.Lookups{Relations: fakeRelations{following: tt.following}, Moderators: tt.moderators}, nil)
			out := p.DetailedMany(context.Background(), tt.viewer, []*model.User{u}, nil)
			require.Len(t, out, 1)
			assert.EqualValues(t, tt.want, out[0].FollowersCount)
		})
	}
}

func TestDetailedMany_Empty(t *testing.T) {
	p := userpack.New(userpack.Lookups{}, nil)
	assert.Empty(t, p.DetailedMany(context.Background(), nil, nil, nil))
}

func TestFillLites(t *testing.T) {
	p := userpack.New(userpack.Lookups{Instances: fakeInstances{}, Emojis: fakeEmojis{}}, nil)
	lite := entity.PackUserLite(target())
	p.FillLites([]*model.User{target()}, []*entity.UserLite{&lite})
	require.NotNil(t, lite.Instance)
	assert.Equal(t, "https://remote.example/blobcat.png", lite.Emojis["blobcat"])
}
