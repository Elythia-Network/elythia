package instance_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShouldSkipDelivery_SuspendCache verifies that the suspend decision is
// cached for cacheTTL so repeated deliveries to the same host avoid a
// FindByHost round trip, and that the cache is refreshed once it expires (#1407).
func TestShouldSkipDelivery_SuspendCache(t *testing.T) {
	t.Run("second call within TTL hits cache", func(t *testing.T) {
		svc, repo, _ := newService(t)
		repo.Instances["ok.example"] = &model.Instance{
			ID:              "i1",
			Host:            "ok.example",
			SuspensionState: model.SuspensionStateNone,
		}

		assert.False(t, svc.ShouldSkipDelivery("ok.example"))
		assert.False(t, svc.ShouldSkipDelivery("ok.example"))
		assert.Equal(t, 1, repo.FindCalls, "expected 1 FindByHost call (cache hit on second)")
	})

	t.Run("lookup repeats after TTL expiry", func(t *testing.T) {
		svc, repo, _ := newService(t)
		repo.Instances["ok.example"] = &model.Instance{
			ID:              "i2",
			Host:            "ok.example",
			SuspensionState: model.SuspensionStateNone,
		}

		base := time.Unix(0, 0)
		now := base
		svc.SetClock(func() time.Time { return now })

		assert.False(t, svc.ShouldSkipDelivery("ok.example"))
		assert.Equal(t, 1, repo.FindCalls)

		// 期限超に時刻を進めると再 lookup される
		now = base.Add(6 * time.Minute)
		assert.False(t, svc.ShouldSkipDelivery("ok.example"))
		assert.Equal(t, 2, repo.FindCalls, "expected re-lookup after TTL expiry")
	})

	t.Run("suspend change is reflected after TTL expiry", func(t *testing.T) {
		svc, repo, _ := newService(t)
		repo.Instances["host.example"] = &model.Instance{
			ID:              "i3",
			Host:            "host.example",
			SuspensionState: model.SuspensionStateNone,
		}

		base := time.Unix(0, 0)
		now := base
		svc.SetClock(func() time.Time { return now })

		assert.False(t, svc.ShouldSkipDelivery("host.example"))

		// suspensionState を変えても TTL 内は古い結果がキャッシュされる
		repo.Instances["host.example"].SuspensionState = model.SuspensionStateManuallySuspended
		assert.False(t, svc.ShouldSkipDelivery("host.example"), "stale cached decision within TTL")

		// TTL 経過後は新しい suspensionState が反映される
		now = base.Add(6 * time.Minute)
		assert.True(t, svc.ShouldSkipDelivery("host.example"), "suspension reflected after TTL expiry")
	})
}

// TestShouldSkipDelivery_SuspendInvalidatesCache verifies that calling Suspend
// drops the cached decision so the new suspensionState takes effect immediately
// rather than waiting out the TTL (#1407 review).
func TestShouldSkipDelivery_SuspendInvalidatesCache(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["host.example"] = &model.Instance{
		ID:              "i1",
		Host:            "host.example",
		SuspensionState: model.SuspensionStateNone,
	}

	// 配送可と判定してキャッシュに載せる
	assert.False(t, svc.ShouldSkipDelivery("host.example"))

	// Suspend は cache を invalidate するので、TTL を待たずに次回判定へ即時反映される
	require.NoError(t, svc.Suspend("host.example", model.SuspensionStateManuallySuspended))
	assert.True(t, svc.ShouldSkipDelivery("host.example"), "suspend must take effect immediately")
}

// TestShouldSkipDelivery_EvictsExpiredEntries verifies that a cache write also
// prunes other entries whose TTL has elapsed, bounding map growth (#1407 review).
func TestShouldSkipDelivery_EvictsExpiredEntries(t *testing.T) {
	svc, repo, _ := newService(t)
	for _, h := range []string{"a.example", "b.example"} {
		repo.Instances[h] = &model.Instance{ID: "i-" + h, Host: h, SuspensionState: model.SuspensionStateNone}
	}

	base := time.Unix(0, 0)
	now := base
	svc.SetClock(func() time.Time { return now })

	// a.example を載せる
	assert.False(t, svc.ShouldSkipDelivery("a.example"))
	assert.Equal(t, 1, svc.SuspendCacheLen(), "a cached")

	// TTL 超に進めてから別 host を載せると、期限切れの a が掃除される
	now = base.Add(6 * time.Minute)
	assert.False(t, svc.ShouldSkipDelivery("b.example"))
	assert.Equal(t, 1, svc.SuspendCacheLen(), "expired a evicted, only b remains")
}

// TestShouldSkipDelivery_MetaWarnRateLimit verifies that a persistent meta-fetch
// failure logs at most once per metaWarnEvery window, while still logging the
// first failure and re-logging once the window elapses (#1410).
func TestShouldSkipDelivery_MetaWarnRateLimit(t *testing.T) {
	svc, _, metaRepo := newService(t)
	// metaRepo.Meta == nil で Fetch が error を返し、warn 経路に入る
	metaRepo.Meta = nil

	base := time.Unix(0, 0)
	now := base
	svc.SetClock(func() time.Time { return now })

	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	warnCount := func() int {
		return strings.Count(buf.String(), "could not fetch meta")
	}

	// 初回は必ず warn を出す
	svc.ShouldSkipDelivery("a.example")
	assert.Equal(t, 1, warnCount(), "first failure must warn")

	// 同一 window 内の 2 回目以降は抑制される
	svc.ShouldSkipDelivery("b.example")
	assert.Equal(t, 1, warnCount(), "warn suppressed within window")

	// metaWarnEvery を超えたら再度 warn を出す
	now = base.Add(2 * time.Minute)
	svc.ShouldSkipDelivery("c.example")
	assert.Equal(t, 2, warnCount(), "warn re-emitted after window")
}
