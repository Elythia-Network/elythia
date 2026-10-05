package processors_test

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeOrphanCleaner records the grace period it was called with.
type fakeOrphanCleaner struct {
	remaining int64
	calls     int
	lastGrace int
	err       error
}

func (f *fakeOrphanCleaner) DeleteOrphanRemoteUsers(graceDays, batchSize int) (int64, error) {
	f.calls++
	f.lastGrace = graceDays
	if f.err != nil {
		return 0, f.err
	}
	n := int64(batchSize)
	if f.remaining < n {
		n = f.remaining
	}
	f.remaining -= n
	return n, nil
}

func cfgFn(enabled bool, grace int) func() processors.OrphanUserCleanerConfig {
	return func() processors.OrphanUserCleanerConfig {
		return processors.OrphanUserCleanerConfig{Enabled: enabled, GraceDays: grace}
	}
}

func TestOrphanUserCleanup_DeletesInBatches(t *testing.T) {
	repo := &fakeOrphanCleaner{remaining: 450}
	p := processors.NewOrphanUserCleanupProcessor(repo, cfgFn(true, 30))

	require.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
	assert.Zero(t, repo.remaining, "全件削除されること")
	assert.Equal(t, 3, repo.calls, "バッチで回ること")
}

// 既定無効なら何もしない。
func TestOrphanUserCleanup_DisabledIsNoop(t *testing.T) {
	repo := &fakeOrphanCleaner{remaining: 100}
	p := processors.NewOrphanUserCleanupProcessor(repo, cfgFn(false, 30))

	require.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
	assert.Zero(t, repo.calls)
}

// 猶予が短すぎる設定は下限で丸める。
// ResolveActor は actorTTL (24 時間) で lastFetchedAt を更新するため、短いと
// 活動中の著者が「削除 -> 再フェッチ」を繰り返すチャーンになる。
func TestOrphanUserCleanup_ClampsGraceToMinimum(t *testing.T) {
	for _, grace := range []int{0, 1, 3} {
		repo := &fakeOrphanCleaner{remaining: 1}
		p := processors.NewOrphanUserCleanupProcessor(repo, cfgFn(true, grace))
		require.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
		assert.Equal(t, processors.MinOrphanUserGraceDays, repo.lastGrace,
			"設定 %d 日は下限に丸められること", grace)
	}
}

func TestOrphanUserCleanup_KeepsConfiguredGraceWhenLongEnough(t *testing.T) {
	repo := &fakeOrphanCleaner{remaining: 1}
	p := processors.NewOrphanUserCleanupProcessor(repo, cfgFn(true, 60))
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
	assert.Equal(t, 60, repo.lastGrace)
}

func TestOrphanUserCleanup_DeleteError(t *testing.T) {
	repo := &fakeOrphanCleaner{err: errors.New("db down")}
	p := processors.NewOrphanUserCleanupProcessor(repo, cfgFn(true, 30))
	assert.Error(t, p.Handle(context.Background(), driver.RawTask{}))
}

func TestOrphanUserCleanup_CancelledContext(t *testing.T) {
	repo := &fakeOrphanCleaner{remaining: 1000}
	p := processors.NewOrphanUserCleanupProcessor(repo, cfgFn(true, 30))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, p.Handle(ctx, driver.RawTask{}))
	assert.Zero(t, repo.calls)
}

func TestOrphanUserCleanup_NilSafe(t *testing.T) {
	var p *processors.OrphanUserCleanupProcessor
	assert.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
	assert.NoError(t, processors.NewOrphanUserCleanupProcessor(nil, nil).
		Handle(context.Background(), driver.RawTask{}))
}
