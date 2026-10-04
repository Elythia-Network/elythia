package move_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/core/move"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/testutil"
)

type movedAwayCall struct {
	oldID     string
	followers []string
}

type fakeMoveChartHook struct {
	calls []movedAwayCall
}

func (f *fakeMoveChartHook) OnFollowersMovedAway(old *model.User, followerIDs []string) {
	f.calls = append(f.calls, movedAwayCall{oldID: old.ID, followers: append([]string(nil), followerIDs...)})
}

type failingInstanceCounter struct{ calls int }

func (f *failingInstanceCounter) IncrementFollowersCount(string, int) error {
	f.calls++
	return errors.New("instance counter boom")
}

// newStatsService wires a Service for PostMoveProcess with the instance stats
// dependencies.
func newStatsService(followingRepo repository.FollowingRepository, counter move.InstanceFollowersCounter, enabled func() bool, chart move.ChartHook) *move.Service {
	userRepo := testutil.NewMockUserRepository()
	urls := activitypub.NewURLBuilder("https://local.example")
	svc := move.NewService(userRepo, followingRepo, urls, activitypub.NewRenderer(urls), &fakeResolver{}, &fakeDeliverer{})
	svc.SetFollowQueue(&fakeFollowQueue{})
	svc.SetInstanceStats(counter, enabled, chart)
	return svc
}

// TestPostMoveProcess_AdjustsInstanceFollowers pins the tail of upstream
// AccountMoveService.adjustFollowingCounts (#3330): when the old account is
// remote and enableStatsForFederatedInstances is on, its instance loses
// followersCount by the number of local followers, and the chart hook gets
// the follower list.
func TestPostMoveProcess_AdjustsInstanceFollowers(t *testing.T) {
	host := "old.example"
	cases := []struct {
		name        string
		oldHost     *string
		enabled     func() bool
		wantCount   int
		wantChartOn bool
	}{
		{name: "remote old account", oldHost: &host, wantCount: 8, wantChartOn: true},
		{name: "remote old account, gate on", oldHost: &host, enabled: func() bool { return true }, wantCount: 8, wantChartOn: true},
		{name: "stats disabled", oldHost: &host, enabled: func() bool { return false }, wantCount: 10, wantChartOn: true},
		{name: "local old account", oldHost: nil, wantCount: 10, wantChartOn: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			instanceRepo := testutil.NewMockInstanceRepository()
			instanceRepo.Instances[host] = &model.Instance{Host: host, FollowersCount: 10}
			followingRepo := testutil.NewMockFollowingRepository()
			putLocalFollowing(followingRepo, "localA", "old")
			putLocalFollowing(followingRepo, "localB", "old")
			putRemoteFollowing(followingRepo, "remoteC", "old", "third.example")
			chart := &fakeMoveChartHook{}
			svc := newStatsService(followingRepo, instanceRepo, tc.enabled, chart)

			old := &model.User{ID: "old", Host: tc.oldHost}
			svc.PostMoveProcess(old, &model.User{ID: "dst"})

			assert.Equal(t, tc.wantCount, instanceRepo.Instances[host].FollowersCount)
			// chart の stats / charts の判定は charthook 側が持つので、hook には
			// 常に渡す。
			require.Len(t, chart.calls, 1)
			assert.Equal(t, "old", chart.calls[0].oldID)
			assert.ElementsMatch(t, []string{"localA", "localB"}, chart.calls[0].followers)
		})
	}
}

// TestPostMoveProcess_InstanceFollowersSkipped pins the cases that must not
// touch the instance: no local followers (upstream returns early), and a
// failed followee listing (upstream throws before reaching the stats part).
func TestPostMoveProcess_InstanceFollowersSkipped(t *testing.T) {
	host := "old.example"
	t.Run("no local followers", func(t *testing.T) {
		instanceRepo := testutil.NewMockInstanceRepository()
		instanceRepo.Instances[host] = &model.Instance{Host: host, FollowersCount: 10}
		followingRepo := testutil.NewMockFollowingRepository()
		putRemoteFollowing(followingRepo, "remoteC", "old", "third.example")
		chart := &fakeMoveChartHook{}
		svc := newStatsService(followingRepo, instanceRepo, nil, chart)

		svc.PostMoveProcess(&model.User{ID: "old", Host: &host}, &model.User{ID: "dst"})
		assert.Equal(t, 10, instanceRepo.Instances[host].FollowersCount)
		assert.Empty(t, chart.calls)
	})
	t.Run("followee listing fails", func(t *testing.T) {
		instanceRepo := testutil.NewMockInstanceRepository()
		instanceRepo.Instances[host] = &model.Instance{Host: host, FollowersCount: 10}
		inner := testutil.NewMockFollowingRepository()
		putLocalFollowing(inner, "localA", "old")
		chart := &fakeMoveChartHook{}
		svc := newStatsService(&listFailFollowingRepo{MockFollowingRepository: inner}, instanceRepo, nil, chart)

		svc.PostMoveProcess(&model.User{ID: "old", Host: &host}, &model.User{ID: "dst"})
		assert.Equal(t, 10, instanceRepo.Instances[host].FollowersCount)
		assert.Empty(t, chart.calls)
	})
}

// TestPostMoveProcess_InstanceFollowersBestEffort pins that a failing counter
// is logged and does not stop the chart hook or the move, and that an
// unwired counter / hook is a no-op.
func TestPostMoveProcess_InstanceFollowersBestEffort(t *testing.T) {
	host := "old.example"
	followingRepo := testutil.NewMockFollowingRepository()
	putLocalFollowing(followingRepo, "localA", "old")
	counter := &failingInstanceCounter{}
	chart := &fakeMoveChartHook{}
	svc := newStatsService(followingRepo, counter, nil, chart)

	svc.PostMoveProcess(&model.User{ID: "old", Host: &host}, &model.User{ID: "dst"})
	assert.Equal(t, 1, counter.calls)
	assert.Len(t, chart.calls, 1)

	unwired := newStatsService(followingRepo, nil, nil, nil)
	assert.NotPanics(t, func() {
		unwired.PostMoveProcess(&model.User{ID: "old", Host: &host}, &model.User{ID: "dst"})
	})
}
