package processors_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/blocking"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
)

type fakeUnblocker struct {
	calls [][2]string
	err   error
}

func (f *fakeUnblocker) Unblock(blockerID, blockeeID string) error {
	f.calls = append(f.calls, [2]string{blockerID, blockeeID})
	return f.err
}

func TestUnblockProcessor_Success(t *testing.T) {
	fu := &fakeUnblocker{}
	p := processors.NewUnblockProcessor(fu)
	task := queue.NewUnblockTask(queue.UnblockPayload{BlockerID: "localA", BlockeeID: "rA"})
	require.NoError(t, p.Handle(context.Background(), task))
	require.Len(t, fu.calls, 1)
	assert.Equal(t, [2]string{"localA", "rA"}, fu.calls[0])
}

// TS 版から引き継いだ job ({from: {id}, to: {id}}) も処理する。mk-go の鍵が
// あればそちらを優先する。本家 processUnblock は unblock(from, to)。
func TestUnblockProcessor_UpstreamJobShape(t *testing.T) {
	fu := &fakeUnblocker{}
	p := processors.NewUnblockProcessor(fu)
	task := driver.RawTask{TypeName: queue.TaskTypeUnblock, Body: []byte(`{"from":{"id":"blocker1"},"to":{"id":"blockee1"}}`)}
	require.NoError(t, p.Handle(context.Background(), task))
	assert.Equal(t, [][2]string{{"blocker1", "blockee1"}}, fu.calls)

	fu = &fakeUnblocker{}
	p = processors.NewUnblockProcessor(fu)
	task = driver.RawTask{TypeName: queue.TaskTypeUnblock, Body: []byte(`{"blockerId":"a","blockeeId":"b","from":{"id":"x"},"to":{"id":"y"}}`)}
	require.NoError(t, p.Handle(context.Background(), task))
	assert.Equal(t, [][2]string{{"a", "b"}}, fu.calls)
}

// 既に解消済 (row 無し) は望む終状態なので成功扱い。
func TestUnblockProcessor_NotBlocking_Success(t *testing.T) {
	fu := &fakeUnblocker{err: blocking.ErrNotBlocking}
	p := processors.NewUnblockProcessor(fu)
	task := queue.NewUnblockTask(queue.UnblockPayload{BlockerID: "a", BlockeeID: "b"})
	assert.NoError(t, p.Handle(context.Background(), task))
}

// 自己 unblock は retry 不能の恒久エラー。
func TestUnblockProcessor_SelfBlock_SkipRetry(t *testing.T) {
	fu := &fakeUnblocker{err: blocking.ErrSelfBlock}
	p := processors.NewUnblockProcessor(fu)
	task := queue.NewUnblockTask(queue.UnblockPayload{BlockerID: "a", BlockeeID: "b"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, driver.ErrSkipRetry))
}

func TestUnblockProcessor_GenericError_Retries(t *testing.T) {
	fu := &fakeUnblocker{err: errors.New("network down")}
	p := processors.NewUnblockProcessor(fu)
	task := queue.NewUnblockTask(queue.UnblockPayload{BlockerID: "a", BlockeeID: "b"})
	err := p.Handle(context.Background(), task)
	assert.Error(t, err)
	assert.False(t, errors.Is(err, driver.ErrSkipRetry), "transient error は retry させる")
}

func TestUnblockProcessor_MissingFields_SkipsRetry(t *testing.T) {
	fu := &fakeUnblocker{}
	p := processors.NewUnblockProcessor(fu)
	task := queue.NewUnblockTask(queue.UnblockPayload{BlockeeID: "rA"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, driver.ErrSkipRetry))
	assert.Empty(t, fu.calls)
}

func TestUnblockProcessor_NoUnblocker_SkipsRetry(t *testing.T) {
	p := processors.NewUnblockProcessor(nil)
	task := queue.NewUnblockTask(queue.UnblockPayload{BlockerID: "a", BlockeeID: "b"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, driver.ErrSkipRetry))
}

func TestUnblockProcessor_BadPayload_SkipsRetry(t *testing.T) {
	p := processors.NewUnblockProcessor(&fakeUnblocker{})
	task := driver.RawTask{TypeName: queue.TaskTypeUnblock, Body: []byte("not json")}
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, driver.ErrSkipRetry))
}
