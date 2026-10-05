package processors

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withRedis sets up a fresh testcontainers Redis per test. Reusing
// SetupRedis is overkill here (= we only need 1 ms-scoped test), so we
// spin a per-test container and tear it down with t.Cleanup.
func withRedis(t *testing.T) *testutil.TestRedis {
	t.Helper()
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { tr.Teardown(ctx) })
	return tr
}

func TestRedisScheduledNoteLock_TryAcquireOncePerDraft(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	tr := withRedis(t)
	lock := NewRedisScheduledNoteLock(tr.Client, "test:", time.Minute)

	ok, err := lock.TryAcquire(context.Background(), "draft-A")
	require.NoError(t, err)
	assert.True(t, ok, "初回 TryAcquire は占有成功")

	// 同 draft へ 2 度目の取得 → false (= 他 retry が保持中の状態と同じ)
	ok, err = lock.TryAcquire(context.Background(), "draft-A")
	require.NoError(t, err)
	assert.False(t, ok, "占有中の draft は再 acquire できない")

	// 別 draft は別 key なので独立して acquire 可能
	ok, err = lock.TryAcquire(context.Background(), "draft-B")
	require.NoError(t, err)
	assert.True(t, ok)
}

// ttl<=0 を渡したときの default fallback を確認する (= keyPrefix 設定と TTL
// 既定値を seal)。production の wire 側で 0 を渡しても 5 分 default が効く。
func TestRedisScheduledNoteLock_DefaultTTL(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	tr := withRedis(t)
	lock := NewRedisScheduledNoteLock(tr.Client, "test:", 0)
	assert.Equal(t, defaultScheduledNoteLockTTL, lock.ttl)
	assert.Equal(t, "test:scheduled-note-lock:", lock.keyPrefix)
}
