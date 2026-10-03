package processors_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/following"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/shiroha-a/mk/internal/queue/processors"
)

type followCall struct {
	followerID, followeeID string
	opts                   following.FollowOptions
}

type fakeFollower struct {
	calls []followCall
	err   error
}

func (f *fakeFollower) Follow(followerID, followeeID string, opts following.FollowOptions) (*following.FollowResult, error) {
	f.calls = append(f.calls, followCall{followerID, followeeID, opts})
	if f.err != nil {
		return nil, f.err
	}
	return &following.FollowResult{}, nil
}

func TestFollowProcessor_Success(t *testing.T) {
	ff := &fakeFollower{}
	p := processors.NewFollowProcessor(ff)
	task := queue.NewFollowTask(queue.FollowPayload{
		FollowerID: "localA", FolloweeID: "rA", WithReplies: true,
	})
	require.NoError(t, p.Handle(context.Background(), task))
	require.Len(t, ff.calls, 1)
	assert.Equal(t, "localA", ff.calls[0].followerID)
	assert.Equal(t, "rA", ff.calls[0].followeeID)
	assert.True(t, ff.calls[0].opts.WithReplies, "payload の withReplies を FollowOptions に引き継ぐ")
}

// 本家 processFollow は job の silent / withReplies を follow に渡す。
func TestFollowProcessor_PassesSilent(t *testing.T) {
	ff := &fakeFollower{}
	p := processors.NewFollowProcessor(ff)
	task := queue.NewFollowTask(queue.FollowPayload{FollowerID: "a", FolloweeID: "b", Silent: true})
	require.NoError(t, p.Handle(context.Background(), task))
	require.Len(t, ff.calls, 1)
	assert.True(t, ff.calls[0].opts.Silent)
}

// TS 版から引き継いだ job ({from: {id}, to: {id}, silent, withReplies}) も処理する。
// mk-go の鍵があればそちらを優先する。
func TestFollowProcessor_UpstreamJobShape(t *testing.T) {
	ff := &fakeFollower{}
	p := processors.NewFollowProcessor(ff)
	task := driver.RawTask{TypeName: queue.TaskTypeFollow, Body: []byte(`{"from":{"id":"localA"},"to":{"id":"rA"},"silent":true,"withReplies":true,"requestId":"https://remote.example/follows/1"}`)}
	require.NoError(t, p.Handle(context.Background(), task))
	require.Len(t, ff.calls, 1)
	assert.Equal(t, "localA", ff.calls[0].followerID)
	assert.Equal(t, "rA", ff.calls[0].followeeID)
	assert.True(t, ff.calls[0].opts.Silent)
	assert.True(t, ff.calls[0].opts.WithReplies)

	ff = &fakeFollower{}
	p = processors.NewFollowProcessor(ff)
	task = driver.RawTask{TypeName: queue.TaskTypeFollow, Body: []byte(`{"followerId":"a","followeeId":"b","from":{"id":"x"},"to":{"id":"y"}}`)}
	require.NoError(t, p.Handle(context.Background(), task))
	require.Len(t, ff.calls, 1)
	assert.Equal(t, "a", ff.calls[0].followerID)
	assert.Equal(t, "b", ff.calls[0].followeeID)
}

// 既に follow 済 / follow request 送信済は望む終状態なので成功扱い。
func TestFollowProcessor_AlreadyFollowing_Success(t *testing.T) {
	for _, sentinel := range []error{following.ErrAlreadyFollowing, following.ErrAlreadyRequested} {
		ff := &fakeFollower{err: sentinel}
		p := processors.NewFollowProcessor(ff)
		task := queue.NewFollowTask(queue.FollowPayload{FollowerID: "a", FolloweeID: "b"})
		assert.NoError(t, p.Handle(context.Background(), task), sentinel.Error())
	}
}

// block 関係 / 自己 follow / 対象不在は retry 不能の恒久エラー。
func TestFollowProcessor_PermanentErrors_SkipRetry(t *testing.T) {
	for _, sentinel := range []error{following.ErrBlocked, following.ErrSelfFollow, following.ErrFolloweeNotFound} {
		ff := &fakeFollower{err: sentinel}
		p := processors.NewFollowProcessor(ff)
		task := queue.NewFollowTask(queue.FollowPayload{FollowerID: "a", FolloweeID: "b"})
		err := p.Handle(context.Background(), task)
		require.Error(t, err, sentinel.Error())
		assert.True(t, errors.Is(err, driver.ErrSkipRetry), sentinel.Error())
	}
}

func TestFollowProcessor_GenericError_Retries(t *testing.T) {
	ff := &fakeFollower{err: errors.New("network down")}
	p := processors.NewFollowProcessor(ff)
	task := queue.NewFollowTask(queue.FollowPayload{FollowerID: "a", FolloweeID: "b"})
	err := p.Handle(context.Background(), task)
	assert.Error(t, err)
	assert.False(t, errors.Is(err, driver.ErrSkipRetry), "transient error は retry させる")
}

func TestFollowProcessor_MissingFields_SkipsRetry(t *testing.T) {
	ff := &fakeFollower{}
	p := processors.NewFollowProcessor(ff)
	task := queue.NewFollowTask(queue.FollowPayload{FolloweeID: "rA"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, driver.ErrSkipRetry))
	assert.Empty(t, ff.calls)
}

func TestFollowProcessor_NoFollower_SkipsRetry(t *testing.T) {
	p := processors.NewFollowProcessor(nil)
	task := queue.NewFollowTask(queue.FollowPayload{FollowerID: "a", FolloweeID: "b"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, driver.ErrSkipRetry))
}

func TestFollowProcessor_BadPayload_SkipsRetry(t *testing.T) {
	p := processors.NewFollowProcessor(&fakeFollower{})
	task := driver.RawTask{TypeName: queue.TaskTypeFollow, Body: []byte("not json")}
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, driver.ErrSkipRetry))
}
