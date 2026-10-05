package userrelation

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedRelationMatrix gives each target a different viewer->target relation so
// every field of the block is exercised, and returns the targets (including
// the viewer itself) with their profiles.
func seedRelationMatrix(t *testing.T, r Repos) ([]*model.User, []*model.UserProfile) {
	t.Helper()
	following := r.Following.(*testutil.MockFollowingRepository)
	blocking := r.Blocking.(*testutil.MockBlockingRepository)
	muting := r.Muting.(*testutil.MockMutingRepository)
	renoteMuting := r.RenoteMuting.(*testutil.MockRenoteMutingRepository)
	followReq := r.FollowRequest.(*testutil.MockFollowRequestRepository)
	memo := r.Memo.(*testutil.MockUserMemoRepository)

	notify := "normal"
	require.NoError(t, following.Create(&model.Following{ID: "f1", FollowerID: "viewer", FolloweeID: "t1", Notify: &notify, WithReplies: true}))
	require.NoError(t, following.Create(&model.Following{ID: "f2", FollowerID: "viewer", FolloweeID: "t2"}))
	require.NoError(t, following.Create(&model.Following{ID: "f3", FollowerID: "t3", FolloweeID: "viewer"}))
	require.NoError(t, blocking.Create(&model.Blocking{ID: "b1", BlockerID: "viewer", BlockeeID: "t4"}))
	require.NoError(t, blocking.Create(&model.Blocking{ID: "b2", BlockerID: "t5", BlockeeID: "viewer"}))
	require.NoError(t, muting.Create(&model.Muting{ID: "m1", MuterID: "viewer", MuteeID: "t6"}))
	require.NoError(t, renoteMuting.Create(&model.RenoteMuting{ID: "rm1", MuterID: "viewer", MuteeID: "t7"}))
	require.NoError(t, followReq.Create(&model.FollowRequest{ID: "fr1", FollowerID: "viewer", FolloweeID: "t8"}))
	require.NoError(t, followReq.Create(&model.FollowRequest{ID: "fr2", FollowerID: "t9", FolloweeID: "viewer"}))
	require.NoError(t, memo.CreateOrUpdate(&model.UserMemo{ID: "memo1", UserID: "viewer", TargetUserID: "t2", Memo: "about t2"}))
	require.NoError(t, memo.CreateOrUpdate(&model.UserMemo{ID: "memo2", UserID: "viewer", TargetUserID: "viewer", Memo: "about me"}))

	msg := "welcome"
	var users []*model.User
	var profiles []*model.UserProfile
	for _, id := range []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9", "t10", "viewer", "t1"} {
		users = append(users, &model.User{ID: id})
		p := &model.UserProfile{UserID: id}
		if id == "t1" {
			p.FollowedMessage = &msg
		}
		if id == "t10" {
			p = nil
		}
		profiles = append(profiles, p)
	}
	return users, profiles
}

// ApplyMany writes exactly what Apply writes for each target (self gets only
// the memo, a duplicate target is filled at both indices) and returns the
// same isFollowing.
func TestApplyMany_MatchesApplyPerTarget(t *testing.T) {
	r, _, _ := fullRepos(t)
	users, profiles := seedRelationMatrix(t, r)

	want := make([]entity.UserDetailed, len(users))
	wantFollowing := make([]bool, len(users))
	for i, u := range users {
		wantFollowing[i] = r.Apply(&want[i], "viewer", u, profiles[i])
	}

	got := make([]entity.UserDetailed, len(users))
	details := make([]*entity.UserDetailed, len(users))
	for i := range got {
		details[i] = &got[i]
	}
	gotFollowing := r.ApplyMany("viewer", details, users, profiles)

	assert.Equal(t, wantFollowing, gotFollowing)
	for i, u := range users {
		wj, _ := json.Marshal(want[i])
		gj, _ := json.Marshal(got[i])
		assert.JSONEq(t, string(wj), string(gj), "target %s (index %d)", u.ID, i)
	}
	// 代表値も直接確かめる (Apply 自体が壊れても両方一致してしまわないように)。
	require.NotNil(t, got[0].Notify)
	assert.Equal(t, "normal", *got[0].Notify)
	assert.True(t, *got[0].WithReplies)
	assert.JSONEq(t, `"welcome"`, string(got[0].FollowedMessage))
	assert.Equal(t, "none", *got[1].Notify)
	assert.Equal(t, "about t2", *got[1].Memo)
	assert.True(t, *got[2].IsFollowed)
	assert.True(t, *got[3].IsBlocking)
	assert.True(t, *got[4].IsBlocked)
	assert.True(t, *got[5].IsMuted)
	assert.True(t, *got[6].IsRenoteMuted)
	assert.True(t, *got[7].HasPendingFollowRequestFromYou)
	assert.True(t, *got[8].HasPendingFollowRequestToYou)
	assert.False(t, *got[9].IsFollowing)
	assert.Nil(t, got[10].IsFollowing, "self には関係のブロックを付けない")
	require.NotNil(t, got[10].Memo)
	assert.Equal(t, "about me", *got[10].Memo)
	assert.True(t, gotFollowing[11], "同じ利用者が 2 回出ても両方に書く")
	assert.True(t, *got[11].IsFollowing)
}

func TestApplyMany_NoOpConditions(t *testing.T) {
	r, target, profile := fullRepos(t)
	d := entity.UserDetailed{}

	// 匿名
	assert.Equal(t, []bool{false}, r.ApplyMany("", []*entity.UserDetailed{&d}, []*model.User{target}, []*model.UserProfile{profile}))
	// 長さが合わない
	assert.Equal(t, []bool{false}, r.ApplyMany("viewer", []*entity.UserDetailed{&d}, nil, []*model.UserProfile{profile}))
	assert.Equal(t, []bool{false}, r.ApplyMany("viewer", []*entity.UserDetailed{&d}, []*model.User{target}, nil))
	// nil の項目は飛ばす
	assert.Equal(t, []bool{false, false}, r.ApplyMany("viewer", []*entity.UserDetailed{nil, &d}, []*model.User{target, nil}, []*model.UserProfile{nil, nil}))
	assert.Empty(t, r.ApplyMany("viewer", nil, nil, nil))
	assert.Nil(t, d.IsFollowing)
}

// erroringRepos implements every batch reader and fails every query, so the
// error branches fall back to "no relation" like Apply does.
type erroringRepos struct{}

var errBatch = errors.New("batch failed")

func (erroringRepos) ListFollowingRowsFromAnchor(string, []string) ([]*model.Following, error) {
	return nil, errBatch
}
func (erroringRepos) FilterBlockingFromAnchor(string, []string) ([]string, error) {
	return nil, errBatch
}
func (erroringRepos) FilterBlockingToAnchor(string, []string) ([]string, error) { return nil, errBatch }
func (erroringRepos) FilterActiveMutingFromAnchor(string, []string) ([]string, error) {
	return nil, errBatch
}
func (erroringRepos) FilterRenoteMutingFromAnchor(string, []string) ([]string, error) {
	return nil, errBatch
}
func (erroringRepos) ListByUserAndTargets(string, []string) ([]*model.UserMemo, error) {
	return nil, errBatch
}

type erroringFollowing struct {
	*testutil.MockFollowingRepository
	erroringRepos
}

func (erroringFollowing) FilterFollowingsToAnchor(string, []string) ([]string, error) {
	return nil, errBatch
}

type erroringBlocking struct {
	*testutil.MockBlockingRepository
	erroringRepos
}
type erroringMuting struct {
	*testutil.MockMutingRepository
	erroringRepos
}
type erroringRenoteMuting struct {
	*testutil.MockRenoteMutingRepository
	erroringRepos
}
type erroringMemo struct {
	*testutil.MockUserMemoRepository
	erroringRepos
}

var (
	_ repository.FollowingRowsFromAnchorReader = erroringFollowing{}
	_ repository.BlockingAnchorFilter          = erroringBlocking{}
	_ repository.MutingAnchorFilter            = erroringMuting{}
	_ repository.RenoteMutingAnchorFilter      = erroringRenoteMuting{}
	_ repository.UserMemoBatchReader           = erroringMemo{}
)

func TestApplyMany_BatchErrorsMeanNoRelation(t *testing.T) {
	base, _, _ := fullRepos(t)
	users, profiles := seedRelationMatrix(t, base)
	r := Repos{
		Following:     erroringFollowing{MockFollowingRepository: base.Following.(*testutil.MockFollowingRepository)},
		Blocking:      erroringBlocking{MockBlockingRepository: base.Blocking.(*testutil.MockBlockingRepository)},
		Muting:        erroringMuting{MockMutingRepository: base.Muting.(*testutil.MockMutingRepository)},
		RenoteMuting:  erroringRenoteMuting{MockRenoteMutingRepository: base.RenoteMuting.(*testutil.MockRenoteMutingRepository)},
		FollowRequest: base.FollowRequest,
		Memo:          erroringMemo{MockUserMemoRepository: base.Memo.(*testutil.MockUserMemoRepository)},
	}
	details := make([]*entity.UserDetailed, len(users))
	for i := range details {
		details[i] = &entity.UserDetailed{}
	}
	following := r.ApplyMany("viewer", details, users, profiles)
	for i, d := range details {
		assert.False(t, following[i])
		if users[i].ID == "viewer" {
			assert.Nil(t, d.Memo)
			continue
		}
		assert.False(t, *d.IsFollowing)
		assert.False(t, *d.IsFollowed)
		assert.False(t, *d.IsBlocking)
		assert.False(t, *d.IsBlocked)
		assert.False(t, *d.IsMuted)
		assert.False(t, *d.IsRenoteMuted)
		assert.Equal(t, "none", *d.Notify)
		assert.Nil(t, d.Memo)
	}
}

// 一部のリポジトリだけを配線した構成でも、配線した項目だけを書く (Apply と同じ)。
func TestApplyMany_PartialWiring(t *testing.T) {
	r := Repos{Following: testutil.NewMockFollowingRepository()}
	require.NoError(t, r.Following.Create(&model.Following{ID: "f1", FollowerID: "viewer", FolloweeID: "t1"}))
	d := &entity.UserDetailed{}
	got := r.ApplyMany("viewer", []*entity.UserDetailed{d}, []*model.User{{ID: "t1"}}, []*model.UserProfile{nil})
	assert.Equal(t, []bool{true}, got)
	assert.True(t, *d.IsFollowing)
	assert.Nil(t, d.IsBlocking)
	assert.Nil(t, d.IsMuted)
	assert.Nil(t, d.HasPendingFollowRequestFromYou)
	assert.Empty(t, d.FollowedMessage, "profile が無ければ followedMessage を出さない")
}
