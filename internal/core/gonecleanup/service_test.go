package gonecleanup

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/following"
	coreinstance "github.com/shiroha-a/mk/internal/core/instance"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

type fakeRepo struct {
	gone      []repository.GoneInstanceSummary
	relations []*model.Following
	limit     int
	requests  int64
	remaining int64
	err       error
	delErr    error
	countErr  error
}

func (f *fakeRepo) ListGone() ([]repository.GoneInstanceSummary, error) { return f.gone, f.err }
func (f *fakeRepo) ListRelations(_ string, limit int) ([]*model.Following, error) {
	f.limit = limit
	return f.relations, f.err
}
func (f *fakeRepo) CountRelations(string) (int64, error)       { return f.remaining, f.countErr }
func (f *fakeRepo) DeleteFollowRequests(string) (int64, error) { return f.requests, f.delErr }

type fakeInstances struct {
	inst *model.Instance
	err  error
}

func (f fakeInstances) FindByHost(string) (*model.Instance, error) { return f.inst, f.err }

type fakeUnfollower struct {
	got  [][2]string
	errs map[string]error
}

func (f *fakeUnfollower) UnfollowQuiet(follower, followee string) error {
	f.got = append(f.got, [2]string{follower, followee})
	return f.errs[follower+">"+followee]
}

func gone() fakeInstances {
	return fakeInstances{inst: &model.Instance{Host: "gone.example", SuspensionState: model.SuspensionStateGoneSuspended}}
}

func rel(id, follower, followee string, followerHost, followeeHost *string) *model.Following {
	return &model.Following{ID: id, FollowerID: follower, FolloweeID: followee, FollowerHost: followerHost, FolloweeHost: followeeHost}
}

func TestClean_RemovesBothDirectionsSilently(t *testing.T) {
	h := "gone.example"
	repo := &fakeRepo{
		relations: []*model.Following{
			rel("f1", "r1", "l1", &h, nil),
			rel("f2", "r2", "l1", &h, nil),
			rel("f3", "l1", "r1", nil, &h),
		},
		requests:  2,
		remaining: 5,
	}
	u := &fakeUnfollower{}
	res, err := NewService(repo, gone(), u).Clean(h)
	require.NoError(t, err)
	assert.Equal(t, Result{Host: h, RemovedFollowers: 2, RemovedFollowing: 1, RemovedFollowRequests: 2, Remaining: 5}, res)
	assert.Equal(t, [][2]string{{"r1", "l1"}, {"r2", "l1"}, {"l1", "r1"}}, u.got)
	assert.Equal(t, MaxPerRun, repo.limit)
}

// goneSuspended でないホストは消さない (戻した後・手動停止・未知)。
func TestClean_RefusesHostsThatAreNotGone(t *testing.T) {
	for name, inst := range map[string]fakeInstances{
		"restored": {inst: &model.Instance{SuspensionState: model.SuspensionStateNone}},
		"manual":   {inst: &model.Instance{SuspensionState: model.SuspensionStateManuallySuspended}},
		"auto":     {inst: &model.Instance{SuspensionState: model.SuspensionStateAutoSuspendedForNotResponding}},
		"unknown":  {err: coreinstance.ErrInstanceNotFound},
		"repo nf":  {err: repository.ErrNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			h := "x.example"
			repo := &fakeRepo{relations: []*model.Following{rel("f1", "r1", "l1", &h, nil)}}
			u := &fakeUnfollower{}
			_, err := NewService(repo, inst, u).Clean(h)
			assert.ErrorIs(t, err, ErrNotGone)
			assert.Empty(t, u.got, "nothing is removed")
		})
	}
}

func TestClean_LookupFailureIsNotNotGone(t *testing.T) {
	_, err := NewService(&fakeRepo{}, fakeInstances{err: errors.New("db down")}, &fakeUnfollower{}).Clean("x.example")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotGone, "a DB failure is not reported as 'not gone'")
}

// 並行して解除済みの関係は数えずに進み、それ以外の失敗は止める。
func TestClean_UnfollowErrors(t *testing.T) {
	h := "gone.example"
	repo := &fakeRepo{relations: []*model.Following{rel("f1", "r1", "l1", &h, nil), rel("f2", "r2", "l1", &h, nil)}}
	u := &fakeUnfollower{errs: map[string]error{"r1>l1": following.ErrNotFollowing}}
	res, err := NewService(repo, gone(), u).Clean(h)
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.RemovedFollowers)

	u = &fakeUnfollower{errs: map[string]error{"r1>l1": errors.New("db down")}}
	res, err = NewService(repo, gone(), u).Clean(h)
	require.Error(t, err)
	assert.Zero(t, res.RemovedFollowers)
	assert.Len(t, u.got, 1, "stops at the first real failure")
}

func TestList(t *testing.T) {
	repo := &fakeRepo{gone: []repository.GoneInstanceSummary{{Host: "gone.example", Followers: 3}}}
	list, err := NewService(repo, gone(), &fakeUnfollower{}).List()
	require.NoError(t, err)
	assert.Equal(t, repo.gone, list)
}

// どの段で失敗してもエラーを返す (消した件数を黙って成功にしない)。
func TestClean_RepoErrors(t *testing.T) {
	boom := errors.New("db down")
	for name, repo := range map[string]*fakeRepo{
		"list":   {err: boom},
		"delete": {delErr: boom},
		"count":  {countErr: boom},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewService(repo, gone(), &fakeUnfollower{}).Clean("gone.example")
			assert.ErrorIs(t, err, boom)
		})
	}
}

// scriptedInstances answers FindByHost from a list of states, one per call
// (the last one repeats).
type scriptedInstances struct {
	states []model.SuspensionState
	calls  int
}

func (f *scriptedInstances) FindByHost(string) (*model.Instance, error) {
	i := min(f.calls, len(f.states)-1)
	f.calls++
	return &model.Instance{SuspensionState: f.states[i]}, nil
}

func manyRelations(h string, n int) []*model.Following {
	out := make([]*model.Following, n)
	for i := range out {
		out[i] = rel(fmt.Sprintf("f%d", i), fmt.Sprintf("r%d", i), "l1", &h, nil)
	}
	return out
}

// 片付けている途中で戻されたら、残りは消さない。
func TestClean_StopsWhenRestoredMidway(t *testing.T) {
	h := "gone.example"
	repo := &fakeRepo{relations: manyRelations(h, 250), requests: 3}
	inst := &scriptedInstances{states: []model.SuspensionState{
		model.SuspensionStateGoneSuspended, // 開始時
		model.SuspensionStateGoneSuspended, // 100 件目
		model.SuspensionStateNone,          // 200 件目で戻された
	}}
	u := &fakeUnfollower{}
	res, err := NewService(repo, inst, u).Clean(h)
	assert.ErrorIs(t, err, ErrNotGone)
	assert.Equal(t, int64(200), res.RemovedFollowers, "stops at the recheck")
	assert.Len(t, u.got, 200)
	assert.Zero(t, res.RemovedFollowRequests, "follow requests are not deleted either")
}

// フォローリクエストを消す前にも読み直す。
func TestClean_RechecksBeforeDeletingRequests(t *testing.T) {
	h := "gone.example"
	repo := &fakeRepo{relations: manyRelations(h, 1), requests: 3}
	inst := &scriptedInstances{states: []model.SuspensionState{model.SuspensionStateGoneSuspended, model.SuspensionStateNone}}
	res, err := NewService(repo, inst, &fakeUnfollower{}).Clean(h)
	assert.ErrorIs(t, err, ErrNotGone)
	assert.Equal(t, int64(1), res.RemovedFollowers)
	assert.Zero(t, res.RemovedFollowRequests)
}

// 同じホストを並行して片付けない (数が二重に減る)。別のホストは並行してよい。
func TestClean_RefusesConcurrentCleanOfTheSameHost(t *testing.T) {
	h := "gone.example"
	block := make(chan struct{})
	entered := make(chan struct{})
	u := &blockingUnfollower{block: block, entered: entered}
	svc := NewService(&fakeRepo{relations: manyRelations(h, 1)}, gone(), u)

	done := make(chan error)
	go func() {
		_, err := svc.Clean(h)
		done <- err
	}()
	<-entered
	_, err := svc.Clean(h)
	assert.ErrorIs(t, err, ErrInProgress)
	_, err = svc.Clean("other.example")
	assert.NotErrorIs(t, err, ErrInProgress, "another host is not blocked")
	close(block)
	require.NoError(t, <-done)

	_, err = svc.Clean(h)
	assert.NoError(t, err, "the host can be cleaned again once the first run ends")
}

// blockingUnfollower blocks only the first call (the other callers return at
// once; sync.Once would make them wait for the blocked first call).
type blockingUnfollower struct {
	block   chan struct{}
	entered chan struct{}
	first   atomic.Bool
}

func (b *blockingUnfollower) UnfollowQuiet(string, string) error {
	if b.first.CompareAndSwap(false, true) {
		close(b.entered)
		<-b.block
	}
	return nil
}
