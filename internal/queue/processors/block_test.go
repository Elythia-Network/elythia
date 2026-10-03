package processors_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/blocking"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/shiroha-a/mk/internal/queue/processors"
)

type fakeBlocker struct {
	calls  [][2]string
	silent []bool
	err    error
}

func (f *fakeBlocker) Block(blockerID, blockeeID string) (*model.Blocking, error) {
	return f.record(blockerID, blockeeID, false)
}

func (f *fakeBlocker) BlockSilent(blockerID, blockeeID string) (*model.Blocking, error) {
	return f.record(blockerID, blockeeID, true)
}

func (f *fakeBlocker) record(blockerID, blockeeID string, silent bool) (*model.Blocking, error) {
	f.calls = append(f.calls, [2]string{blockerID, blockeeID})
	f.silent = append(f.silent, silent)
	if f.err != nil {
		return nil, f.err
	}
	return &model.Blocking{}, nil
}

// 本家 processBlock は job の silent を block に渡す。silent の job (インポート) は
// BlockSilent、付いていない job (AccountMove の copyBlocking など) は Block を使う。
func TestBlockProcessor_SilentUsesBlockSilent(t *testing.T) {
	for _, tc := range []struct {
		silent bool
	}{{false}, {true}} {
		fb := &fakeBlocker{}
		p := processors.NewBlockProcessor(fb)
		task := queue.NewBlockTask(queue.BlockPayload{BlockerID: "a", BlockeeID: "b", Silent: tc.silent})
		require.NoError(t, p.Handle(context.Background(), task))
		assert.Equal(t, []bool{tc.silent}, fb.silent)
	}
}

// TS 版から引き継いだ job は本家の形 ({from: {id}, to: {id}, silent}) で積まれている。
// from がブロックする側 (本家 processBlock は block(from, to, silent))。
func TestBlockProcessor_UpstreamJobShape(t *testing.T) {
	fb := &fakeBlocker{}
	p := processors.NewBlockProcessor(fb)
	task := driver.RawTask{TypeName: queue.TaskTypeBlock, Body: []byte(`{"from":{"id":"blocker1"},"to":{"id":"blockee1"},"silent":true}`)}
	require.NoError(t, p.Handle(context.Background(), task))
	assert.Equal(t, [][2]string{{"blocker1", "blockee1"}}, fb.calls)
	assert.Equal(t, []bool{true}, fb.silent)

	fb = &fakeBlocker{}
	p = processors.NewBlockProcessor(fb)
	task = driver.RawTask{TypeName: queue.TaskTypeBlock, Body: []byte(`{"blockerId":"a","blockeeId":"b","from":{"id":"x"},"to":{"id":"y"}}`)}
	require.NoError(t, p.Handle(context.Background(), task))
	assert.Equal(t, [][2]string{{"a", "b"}}, fb.calls, "mk-go の鍵を優先する")
}

func TestBlockProcessor_Success(t *testing.T) {
	fb := &fakeBlocker{}
	p := processors.NewBlockProcessor(fb)
	task := queue.NewBlockTask(queue.BlockPayload{BlockerID: "localA", BlockeeID: "rA"})
	require.NoError(t, p.Handle(context.Background(), task))
	require.Len(t, fb.calls, 1)
	assert.Equal(t, [2]string{"localA", "rA"}, fb.calls[0])
}

// 既に block 済は望む終状態なので成功扱い。
func TestBlockProcessor_AlreadyBlocking_Success(t *testing.T) {
	fb := &fakeBlocker{err: blocking.ErrAlreadyBlocking}
	p := processors.NewBlockProcessor(fb)
	task := queue.NewBlockTask(queue.BlockPayload{BlockerID: "a", BlockeeID: "b"})
	assert.NoError(t, p.Handle(context.Background(), task))
}

// 自己 block / 対象不在は retry 不能の恒久エラー。
func TestBlockProcessor_PermanentErrors_SkipRetry(t *testing.T) {
	for _, sentinel := range []error{blocking.ErrSelfBlock, blocking.ErrBlockeeNotFound} {
		fb := &fakeBlocker{err: sentinel}
		p := processors.NewBlockProcessor(fb)
		task := queue.NewBlockTask(queue.BlockPayload{BlockerID: "a", BlockeeID: "b"})
		err := p.Handle(context.Background(), task)
		require.Error(t, err, sentinel.Error())
		assert.True(t, errors.Is(err, driver.ErrSkipRetry), sentinel.Error())
	}
}

func TestBlockProcessor_GenericError_Retries(t *testing.T) {
	fb := &fakeBlocker{err: errors.New("network down")}
	p := processors.NewBlockProcessor(fb)
	task := queue.NewBlockTask(queue.BlockPayload{BlockerID: "a", BlockeeID: "b"})
	err := p.Handle(context.Background(), task)
	assert.Error(t, err)
	assert.False(t, errors.Is(err, driver.ErrSkipRetry), "transient error は retry させる")
}

func TestBlockProcessor_MissingFields_SkipsRetry(t *testing.T) {
	fb := &fakeBlocker{}
	p := processors.NewBlockProcessor(fb)
	task := queue.NewBlockTask(queue.BlockPayload{BlockeeID: "rA"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, driver.ErrSkipRetry))
	assert.Empty(t, fb.calls)
}

func TestBlockProcessor_NoBlocker_SkipsRetry(t *testing.T) {
	p := processors.NewBlockProcessor(nil)
	task := queue.NewBlockTask(queue.BlockPayload{BlockerID: "a", BlockeeID: "b"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, driver.ErrSkipRetry))
}

func TestBlockProcessor_BadPayload_SkipsRetry(t *testing.T) {
	p := processors.NewBlockProcessor(&fakeBlocker{})
	task := driver.RawTask{TypeName: queue.TaskTypeBlock, Body: []byte("not json")}
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, driver.ErrSkipRetry))
}
