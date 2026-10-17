package instance_test

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSuspendInvalidationHook_CalledAfterLocalDrop pins that every path that
// changes a suspension drops this process's decision first and then hands the
// host to the hook that forwards it to the other processes (#3664).
func TestSuspendInvalidationHook_CalledAfterLocalDrop(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["a.example"] = &model.Instance{ID: "i1", Host: "a.example", SuspensionState: model.SuspensionStateNone}
	repo.Instances["b.example"] = &model.Instance{ID: "i2", Host: "b.example", SuspensionState: model.SuspensionStateNone}
	old := time.Now().Add(-8 * 24 * time.Hour)
	repo.Instances["c.example"] = &model.Instance{ID: "i3", Host: "c.example", SuspensionState: model.SuspensionStateNone, IsNotResponding: true, NotRespondingSince: &old}

	var hooked []string
	svc.SetSuspendInvalidationHook(func(host string) {
		// hook が呼ばれた時点で、自プロセスの判定は捨て済みであること。
		assert.Equal(t, 0, svc.SuspendCacheLen(), "local entry must be dropped before the hook runs")
		hooked = append(hooked, host)
	})

	assert.False(t, svc.ShouldSkipDelivery("a.example"))
	require.NoError(t, svc.Suspend("a.example", model.SuspensionStateManuallySuspended))
	assert.True(t, svc.ShouldSkipDelivery("a.example"))

	assert.False(t, svc.ShouldSkipDelivery("b.example"))
	svc.DropSuspendCacheLocally("a.example")
	require.NoError(t, svc.MarkGoneSuspended("b.example"))
	assert.True(t, svc.ShouldSkipDelivery("b.example"))

	// 1 週間以上応答の無い相手の自動停止も、判定のキャッシュを捨てて伝える。
	assert.False(t, svc.ShouldSkipDelivery("c.example"))
	svc.DropSuspendCacheLocally("")
	assert.False(t, svc.ShouldSkipDelivery("c.example"))
	require.NoError(t, svc.RecordResponseError("c.example"))
	assert.True(t, svc.ShouldSkipDelivery("c.example"))

	// 自動の停止の解除 (正当な受信) も、判定のキャッシュを捨てて伝える。
	require.NoError(t, svc.MarkRequestReceived("c.example"))
	assert.False(t, svc.ShouldSkipDelivery("c.example"))

	assert.Equal(t, []string{"a.example", "b.example", "c.example", "c.example"}, hooked)
}

// TestDropSuspendCacheLocally drops one host (with its other ports) or all,
// and never calls the hook, so a received invalidation is not re-published.
func TestDropSuspendCacheLocally(t *testing.T) {
	svc, repo, _ := newService(t)
	svc.SetSuspendInvalidationHook(func(string) { t.Fatal("hook must not be called") })
	for _, h := range []string{"a.example", "a.example:8443", "b.example"} {
		repo.Instances[h] = &model.Instance{ID: h, Host: h, SuspensionState: model.SuspensionStateNone}
		svc.ShouldSkipDelivery(h)
	}
	require.Equal(t, 3, svc.SuspendCacheLen())

	svc.DropSuspendCacheLocally("a.example")
	assert.Equal(t, 1, svc.SuspendCacheLen(), "the same host name on another port goes too")

	svc.DropSuspendCacheLocally("")
	assert.Equal(t, 0, svc.SuspendCacheLen())

	// 空の host の InvalidateSuspendCache は何もしない (hook も呼ばない)。
	svc.InvalidateSuspendCache("")
}

// hookedInstanceRepo runs onFind inside FindByHost, i.e. while the decision is
// being read from the DB.
type hookedInstanceRepo struct {
	*testutil.MockInstanceRepository
	onFind func()
}

func (r *hookedInstanceRepo) FindByHost(host string) (*model.Instance, error) {
	inst, err := r.MockInstanceRepository.FindByHost(host)
	if r.onFind != nil {
		f := r.onFind
		r.onFind = nil
		f()
	}
	return inst, err
}

// TestShouldSkipDelivery_InvalidationDuringReadIsNotCached pins that a
// decision read while an invalidation arrived is not stored: the read may
// predate the suspension, and storing it would keep delivering for the TTL.
func TestShouldSkipDelivery_InvalidationDuringReadIsNotCached(t *testing.T) {
	repo := &hookedInstanceRepo{MockInstanceRepository: testutil.NewMockInstanceRepository()}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{}
	idGen, _ := id.NewGenerator("aidx")
	svc := instance.NewService(repo, metaRepo, idGen)
	repo.Instances["a.example"] = &model.Instance{ID: "i1", Host: "a.example", SuspensionState: model.SuspensionStateNone}

	// 読み込みの最中に、他のプロセスで停止され、その通知が届く。
	repo.onFind = func() {
		repo.Instances["a.example"] = &model.Instance{ID: "i1", Host: "a.example", SuspensionState: model.SuspensionStateManuallySuspended}
		svc.DropSuspendCacheLocally("a.example")
	}
	assert.False(t, svc.ShouldSkipDelivery("a.example"), "the in-flight read returns what it read")
	assert.Equal(t, 0, svc.SuspendCacheLen(), "the possibly stale decision is not cached")
	assert.True(t, svc.ShouldSkipDelivery("a.example"))
}

// TestSuspendInvalidationHook_NotForUnsuspendedHost pins that a request from
// a host that is not suspended does not publish anything: only the automatic
// unsuspension changes the decision.
func TestSuspendInvalidationHook_NotForUnsuspendedHost(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["n.example"] = &model.Instance{ID: "n1", Host: "n.example", SuspensionState: model.SuspensionStateNone, IsNotResponding: true}
	svc.SetSuspendInvalidationHook(func(host string) { t.Fatalf("hook called for %s", host) })
	require.NoError(t, svc.MarkRequestReceived("n.example"))
	assert.False(t, repo.Instances["n.example"].IsNotResponding, "the request was recorded")
}

// TestSuspendInvalidationHook_AfterTheWrite pins, for every path that changes
// a suspension, that the decision is dropped (and published) only after the
// new state is written. Dropping first would let a decision read between the
// drop and the write keep the old state for the cache TTL, in this process
// and in the others.
func TestSuspendInvalidationHook_AfterTheWrite(t *testing.T) {
	svc, repo, _ := newService(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	repo.Instances["m.example"] = &model.Instance{ID: "m1", Host: "m.example", SuspensionState: model.SuspensionStateNone}
	repo.Instances["g.example"] = &model.Instance{ID: "g1", Host: "g.example", SuspensionState: model.SuspensionStateNone}
	repo.Instances["a.example"] = &model.Instance{ID: "a1", Host: "a.example", SuspensionState: model.SuspensionStateNone, IsNotResponding: true, NotRespondingSince: &old}
	seen := map[string][]model.SuspensionState{}
	svc.SetSuspendInvalidationHook(func(host string) {
		// hook の時点で DB はもう新しい値になっていること。
		seen[host] = append(seen[host], repo.Instances[host].SuspensionState)
	})

	require.NoError(t, svc.Suspend("m.example", model.SuspensionStateManuallySuspended))
	require.NoError(t, svc.MarkGoneSuspended("g.example"))
	require.NoError(t, svc.RecordResponseError("a.example"))
	require.NoError(t, svc.MarkRequestReceived("a.example"))

	assert.Equal(t, map[string][]model.SuspensionState{
		"m.example": {model.SuspensionStateManuallySuspended},
		"g.example": {model.SuspensionStateGoneSuspended},
		"a.example": {model.SuspensionStateAutoSuspendedForNotResponding, model.SuspensionStateNone},
	}, seen)
}
