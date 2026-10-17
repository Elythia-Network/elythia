package server

import (
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

func newTickWriter(t *testing.T, buffering bool) *corereaction.SwitchableWriter {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return corereaction.NewSwitchableWriter(rdb, testutil.NewMockNoteRepository(), buffering)
}

// pub/sub を取りこぼしたプロセスでも、queue の tick で meta から読み直して揃う。
func TestReactionFlushTick_RereadsBufferingFromMeta(t *testing.T) {
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", EnableReactionsBuffering: true}
	w := newTickWriter(t, false)
	enqueued := 0
	enqueue := func() error { enqueued++; return nil }

	require.NoError(t, reactionFlushTick(metaRepo, w, enqueue))
	assert.True(t, w.Buffering(), "a missed enable must be picked up on the next tick")
	assert.Equal(t, 1, enqueued)

	metaRepo.Meta.EnableReactionsBuffering = false
	require.NoError(t, reactionFlushTick(metaRepo, w, enqueue))
	assert.False(t, w.Buffering(), "a missed disable must be picked up on the next tick")
	assert.Equal(t, 2, enqueued, "the leftover buffer is still flushed after turning off")
}

// meta を読めなければ据え置き、積むかは今の状態で決める。
func TestReactionFlushTick_KeepsModeWhenMetaUnavailable(t *testing.T) {
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.FetchErr = errors.New("db down")
	w := newTickWriter(t, true)
	enqueueErr := errors.New("queue down")
	err := reactionFlushTick(metaRepo, w, func() error { return enqueueErr })
	assert.ErrorIs(t, err, enqueueErr)
	assert.True(t, w.Buffering())
}

// 無効のまま窓を過ぎたら積まない。積むと、無効の間も 30 秒ごとに flush の
// ジョブと SCAN が走り続ける。
func TestReactionFlushTick_DoesNotEnqueueOutsideTheWindow(t *testing.T) {
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", EnableReactionsBuffering: false}
	w := newTickWriter(t, false)
	w.SetClock(func() time.Time { return time.Now().Add(corereaction.BufferDrainWindow + time.Minute) })
	enqueued := 0
	require.NoError(t, reactionFlushTick(metaRepo, w, func() error { enqueued++; return nil }))
	assert.Zero(t, enqueued)
}

// queue の役割でないプロセス (enqueue が nil) も、tick で設定を読み直す。
func TestReactionFlushTick_ReloadsWithoutQueueRole(t *testing.T) {
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", EnableReactionsBuffering: true}
	w := newTickWriter(t, false)
	require.NoError(t, reactionFlushTick(metaRepo, w, nil))
	assert.True(t, w.Buffering())
}
