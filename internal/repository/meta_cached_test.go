package repository_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingMetaRepo counts calls to each method for cache verification.
type countingMetaRepo struct {
	fetchCount       atomic.Int32
	updateCount      atomic.Int32
	ensureCount      atomic.Int32
	meta             *model.Meta
	fetchErr         error
	updateErr        error
	ensureInitialErr error
}

func (r *countingMetaRepo) Fetch() (*model.Meta, error) {
	r.fetchCount.Add(1)
	if r.fetchErr != nil {
		return nil, r.fetchErr
	}
	return r.meta, nil
}

func (r *countingMetaRepo) Update(fields map[string]any) error {
	r.updateCount.Add(1)
	return r.updateErr
}

func (r *countingMetaRepo) EnsureInitial(_ string) error {
	r.ensureCount.Add(1)
	return r.ensureInitialErr
}

func TestCachedMetaRepository_CacheHit(t *testing.T) {
	inner := &countingMetaRepo{meta: &model.Meta{ID: "m1"}}
	cached := repository.NewCachedMetaRepositoryWithTTL(inner, 1*time.Minute)

	m1, err := cached.Fetch()
	require.NoError(t, err)
	assert.Equal(t, "m1", m1.ID)

	m2, err := cached.Fetch()
	require.NoError(t, err)
	assert.Equal(t, "m1", m2.ID)

	// inner.Fetchは1回だけ呼ばれる
	assert.Equal(t, int32(1), inner.fetchCount.Load())
}

func TestCachedMetaRepository_TTLExpiry(t *testing.T) {
	inner := &countingMetaRepo{meta: &model.Meta{ID: "m1"}}
	cached := repository.NewCachedMetaRepositoryWithTTL(inner, 10*time.Millisecond)

	_, err := cached.Fetch()
	require.NoError(t, err)
	assert.Equal(t, int32(1), inner.fetchCount.Load())

	// TTL超過を待つ
	time.Sleep(20 * time.Millisecond)

	_, err = cached.Fetch()
	require.NoError(t, err)
	assert.Equal(t, int32(2), inner.fetchCount.Load())
}

func TestCachedMetaRepository_UpdateInvalidates(t *testing.T) {
	inner := &countingMetaRepo{meta: &model.Meta{ID: "m1"}}
	cached := repository.NewCachedMetaRepositoryWithTTL(inner, 1*time.Minute)

	_, err := cached.Fetch()
	require.NoError(t, err)
	assert.Equal(t, int32(1), inner.fetchCount.Load())

	err = cached.Update(map[string]any{"name": "new"})
	require.NoError(t, err)

	// Update後のFetchは再取得する
	_, err = cached.Fetch()
	require.NoError(t, err)
	assert.Equal(t, int32(2), inner.fetchCount.Load())
}

// SetInvalidationHook は Update / EnsureInitial 成功時に呼ばれる (#1740)。
func TestCachedMetaRepository_InvalidationHookFires(t *testing.T) {
	inner := &countingMetaRepo{meta: &model.Meta{ID: "m1"}}
	cached := repository.NewCachedMetaRepositoryWithTTL(inner, 1*time.Minute)
	var hookCount atomic.Int32
	cached.SetInvalidationHook(func() { hookCount.Add(1) })

	require.NoError(t, cached.Update(map[string]any{"name": "new"}))
	assert.Equal(t, int32(1), hookCount.Load(), "Update success fires hook")

	require.NoError(t, cached.EnsureInitial("m1"))
	assert.Equal(t, int32(2), hookCount.Load(), "EnsureInitial success fires hook")
}

// Update 失敗時は hook を呼ばない (#1740)。
func TestCachedMetaRepository_InvalidationHookNotFiredOnError(t *testing.T) {
	inner := &countingMetaRepo{meta: &model.Meta{ID: "m1"}, updateErr: errors.New("boom")}
	cached := repository.NewCachedMetaRepositoryWithTTL(inner, 1*time.Minute)
	var hookCount atomic.Int32
	cached.SetInvalidationHook(func() { hookCount.Add(1) })

	require.Error(t, cached.Update(map[string]any{"name": "new"}))
	assert.Equal(t, int32(0), hookCount.Load(), "failed Update must not fire hook")
}

// Invalidate() は cache を drop し次の Fetch を再取得させる (cross-worker
// subscriber が remote metaUpdated で呼ぶ経路、#1740)。
func TestCachedMetaRepository_InvalidateDropsCache(t *testing.T) {
	inner := &countingMetaRepo{meta: &model.Meta{ID: "m1"}}
	cached := repository.NewCachedMetaRepositoryWithTTL(inner, 1*time.Minute)

	_, err := cached.Fetch()
	require.NoError(t, err)
	assert.Equal(t, int32(1), inner.fetchCount.Load())

	cached.Invalidate()

	_, err = cached.Fetch()
	require.NoError(t, err)
	assert.Equal(t, int32(2), inner.fetchCount.Load(), "Invalidate forces re-fetch")
}

func TestCachedMetaRepository_EnsureInitialInvalidates(t *testing.T) {
	inner := &countingMetaRepo{meta: &model.Meta{ID: "m1"}}
	cached := repository.NewCachedMetaRepositoryWithTTL(inner, 1*time.Minute)

	_, err := cached.Fetch()
	require.NoError(t, err)

	err = cached.EnsureInitial("m1")
	require.NoError(t, err)

	_, err = cached.Fetch()
	require.NoError(t, err)
	assert.Equal(t, int32(2), inner.fetchCount.Load())
}

func TestCachedMetaRepository_ErrorNotCached(t *testing.T) {
	fetchErr := errors.New("db error")
	inner := &countingMetaRepo{fetchErr: fetchErr}
	cached := repository.NewCachedMetaRepositoryWithTTL(inner, 1*time.Minute)

	_, err := cached.Fetch()
	require.ErrorIs(t, err, fetchErr)

	// エラー時はキャッシュされないので次のFetchも再取得する
	_, err = cached.Fetch()
	require.ErrorIs(t, err, fetchErr)
	assert.Equal(t, int32(2), inner.fetchCount.Load())
}

func TestCachedMetaRepository_UpdateErrorDoesNotInvalidate(t *testing.T) {
	inner := &countingMetaRepo{
		meta:      &model.Meta{ID: "m1"},
		updateErr: errors.New("update failed"),
	}
	cached := repository.NewCachedMetaRepositoryWithTTL(inner, 1*time.Minute)

	_, err := cached.Fetch()
	require.NoError(t, err)

	err = cached.Update(map[string]any{"name": "new"})
	require.Error(t, err)

	// Update失敗時はキャッシュを保持する
	_, err = cached.Fetch()
	require.NoError(t, err)
	assert.Equal(t, int32(1), inner.fetchCount.Load())
}

func TestCachedMetaRepository_ConcurrentFetch(t *testing.T) {
	inner := &countingMetaRepo{meta: &model.Meta{ID: "m1"}}
	cached := repository.NewCachedMetaRepositoryWithTTL(inner, 1*time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := cached.Fetch()
			assert.NoError(t, err)
			assert.Equal(t, "m1", m.ID)
		}()
	}
	wg.Wait()

	// ダブルチェックロックにより1〜数回のFetchで済む
	assert.LessOrEqual(t, inner.fetchCount.Load(), int32(3))
}

// #739: NewCachedMetaRepository (default 5-min TTL constructor) を coverage 化。
// 中身は WithTTL と同じだが default TTL 経路が踏まれていなかった。
func TestNewCachedMetaRepository_DefaultTTL(t *testing.T) {
	inner := &countingMetaRepo{
		meta: &model.Meta{ID: "m1"},
	}
	cached := repository.NewCachedMetaRepository(inner)
	m, err := cached.Fetch()
	require.NoError(t, err)
	assert.Equal(t, "m1", m.ID)
}
