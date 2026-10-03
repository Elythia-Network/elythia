package userpack_test

import (
	"context"
	"errors"
	"testing"

	"github.com/shiroha-a/mk/internal/core/userpack"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const remoteHost = "remote.example"

type fakeInstances struct{}

func (fakeInstances) FindManyByHosts(hosts []string) ([]*model.Instance, error) {
	name := "Remote"
	for _, h := range hosts {
		if h == remoteHost {
			return []*model.Instance{{Host: remoteHost, Name: &name}}, nil
		}
	}
	return nil, nil
}

type fakeEmojis struct{}

func (fakeEmojis) FindManyByNamesAndHost(names []string, host *string) ([]*model.Emoji, error) {
	if host == nil || *host != remoteHost {
		return nil, nil
	}
	h := remoteHost
	return []*model.Emoji{{Name: "blobcat", Host: &h, PublicURL: "https://remote.example/blobcat.png"}}, nil
}

type fakeProfiles struct {
	profiles map[string]*model.UserProfile
	err      error
}

func (f fakeProfiles) FindProfileByUserID(userID string) (*model.UserProfile, error) {
	return f.profiles[userID], f.err
}

type fakeRelations struct{ following bool }

func (f fakeRelations) Apply(d *entity.UserDetailed, _ string, _ *model.User, _ *model.UserProfile) bool {
	v := f.following
	d.IsFollowing = &v
	d.EnsureRelationFlags()
	return v
}

type fakeModerators map[string]bool

func (f fakeModerators) IsModerator(userID string) bool { return f[userID] }

// fakeExtras records the call and fills a pinned note id and movedTo.
type fakeExtras struct {
	viewerID, targetID string
	profile            *model.UserProfile
}

func (f *fakeExtras) FillDetailedExtras(_ context.Context, viewer, u *model.User, profile *model.UserProfile, d *entity.UserDetailed) {
	f.viewerID, f.targetID, f.profile = viewer.ID, u.ID, profile
	d.PinnedNoteIDs = []string{"pinned"}
	moved := "dest"
	d.MovedTo = &moved
}

func target() *model.User {
	h := remoteHost
	return &model.User{ID: "9zzzzzzzzz", Username: "carol", UsernameLower: "carol", Host: &h,
		Emojis: model.StringArray{"blobcat"}, FollowersCount: 7, FollowingCount: 3}
}

func viewer() *model.User { return &model.User{ID: "9yyyyyyyyy", Username: "alice"} }

func newPacker(t *testing.T, l userpack.Lookups) *userpack.Packer {
	t.Helper()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return userpack.New(l, idGen)
}

func followersOnlyProfile(u *model.User) *model.UserProfile {
	note := "watch"
	return &model.UserProfile{UserID: u.ID, ModerationNote: &note,
		FollowersVisibility: model.FollowingVisibilityFollowers, FollowingVisibility: model.FollowingVisibilityPublic}
}

func TestLite_ResolvesInstanceAndEmojis(t *testing.T) {
	p := newPacker(t, userpack.Lookups{Instances: fakeInstances{}, Emojis: fakeEmojis{}})
	lite := p.Lite(target())
	require.NotNil(t, lite.Instance)
	assert.Equal(t, "Remote", *lite.Instance.Name)
	assert.Equal(t, map[string]string{"blobcat": "https://remote.example/blobcat.png"}, lite.Emojis)
}

func TestDetailedNotMe_FullShape(t *testing.T) {
	u := target()
	extras := &fakeExtras{}
	profile := followersOnlyProfile(u)
	p := newPacker(t, userpack.Lookups{
		Instances: fakeInstances{}, Emojis: fakeEmojis{},
		Profiles:  fakeProfiles{profiles: map[string]*model.UserProfile{u.ID: profile}},
		Relations: fakeRelations{following: true},
	})
	p.SetDetailExtras(extras)

	d, ok := p.DetailedNotMe(context.Background(), u, viewer())
	require.True(t, ok)
	assert.Equal(t, "Remote", *d.Instance.Name)
	assert.Equal(t, map[string]string{"blobcat": "https://remote.example/blobcat.png"}, d.Emojis)
	assert.NotEmpty(t, d.CreatedAt)
	// ピン留めと移行先は閲覧者を渡して埋める。
	assert.Equal(t, viewer().ID, extras.viewerID)
	assert.Equal(t, u.ID, extras.targetID)
	assert.Same(t, profile, extras.profile)
	assert.Equal(t, []string{"pinned"}, d.PinnedNoteIDs)
	assert.Equal(t, "dest", *d.MovedTo)
	// フォロワーにはフォロワー限定のカウントが見える。
	assert.True(t, *d.IsFollowing)
	assert.Equal(t, 7, d.FollowersCount)
	// モデレーターでない閲覧者には出さない。
	assert.Nil(t, d.ModerationNote)
}

// unfollow の直後のように閲覧者がフォロワーでなければ、フォロワー限定のカウントを伏せる。
func TestDetailedNotMe_HidesFollowersOnlyCountsForNonFollower(t *testing.T) {
	u := target()
	p := newPacker(t, userpack.Lookups{
		Profiles:  fakeProfiles{profiles: map[string]*model.UserProfile{u.ID: followersOnlyProfile(u)}},
		Relations: fakeRelations{following: false},
	})
	d, ok := p.DetailedNotMe(context.Background(), u, viewer())
	require.True(t, ok)
	assert.False(t, *d.IsFollowing)
	assert.Equal(t, 0, d.FollowersCount)
	assert.Equal(t, 3, d.FollowingCount)
}

func TestDetailedNotMe_ModeratorViewer(t *testing.T) {
	u := target()
	v := viewer()
	for name, note := range map[string]*string{"with note": new("watch"), "without note": nil} {
		t.Run(name, func(t *testing.T) {
			profile := followersOnlyProfile(u)
			profile.ModerationNote = note
			p := newPacker(t, userpack.Lookups{
				Profiles:   fakeProfiles{profiles: map[string]*model.UserProfile{u.ID: profile}},
				Moderators: fakeModerators{v.ID: true},
			})
			d, ok := p.DetailedNotMe(context.Background(), u, v)
			require.True(t, ok)
			require.NotNil(t, d.ModerationNote)
			want := ""
			if note != nil {
				want = *note
			}
			assert.Equal(t, want, *d.ModerationNote)
			require.NotNil(t, d.TwoFactorEnabled)
			// モデレーターにはフォロワー限定のカウントも見える。
			assert.Equal(t, 7, d.FollowersCount)
		})
	}
}

func TestPacker_HasDetailExtras(t *testing.T) {
	p := newPacker(t, userpack.Lookups{})
	assert.False(t, p.HasDetailExtras())
	p.SetDetailExtras(&fakeExtras{})
	assert.True(t, p.HasDetailExtras())
}

// profile を読めないときは組まない (本家は findOneByOrFail で例外)。
func TestDetailedNotMe_DropsWithoutProfile(t *testing.T) {
	for name, l := range map[string]userpack.Lookups{
		"unwired": {},
		"error":   {Profiles: fakeProfiles{err: errors.New("db down")}},
		"missing": {Profiles: fakeProfiles{profiles: map[string]*model.UserProfile{}}},
	} {
		t.Run(name, func(t *testing.T) {
			extras := &fakeExtras{}
			l.Extras = extras
			_, ok := newPacker(t, l).DetailedNotMe(context.Background(), target(), viewer())
			assert.False(t, ok)
			assert.Empty(t, extras.targetID)
		})
	}
}
