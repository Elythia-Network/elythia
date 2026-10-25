package processors

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/testutil"
)

// 構築子の引数に渡すだけの値。どれも呼ばれないので、interface を埋め込んで型を満たす。
type (
	ctorPushCache struct {
		PushSubscriptionCacheInvalidator
	}
	ctorLock         struct{ ScheduledNoteLock }
	ctorDeliveryGate struct{ DeliveryGate }
	ctorKeySource    struct{ SigningKeySource }
	ctorSuspended    struct{ SuspendedChecker }
	ctorRefresher    struct{ InstanceMetadataRefresher }
)

// 下の構築子は、受け取った依存を processor に持たせる (#3829)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。

func TestNewDeleteAccountProcessorKeepsDependencies(t *testing.T) {
	pages := testutil.NewMockPageRepository()
	cache := &ctorPushCache{}
	p := NewDeleteAccountProcessor(nil, nil, nil, pages, cache)
	assert.Same(t, pages, p.pageRepo)
	assert.Same(t, cache, p.pushCache)
}

func TestNewPostScheduledNoteProcessorKeepsDependencies(t *testing.T) {
	lock := &ctorLock{}
	p := NewPostScheduledNoteProcessor(nil, nil, nil, lock)
	assert.Same(t, lock, p.lock)
}

func TestNewDeliverProcessorKeepsDependencies(t *testing.T) {
	gate := &ctorDeliveryGate{}
	keys := &ctorKeySource{}
	suspended := &ctorSuspended{}
	refresher := &ctorRefresher{}
	called := false
	stats := func() bool { called = true; return false }
	p := NewDeliverProcessor(nil, gate, keys, suspended, refresher, stats)
	assert.Same(t, gate, p.deliveryGate)
	assert.Same(t, keys, p.keySource)
	assert.Same(t, suspended, p.suspendedChecker)
	assert.Same(t, refresher, p.metadataRefresher)
	// func は比べられないので、呼んで渡したものか確かめる。
	if assert.NotNil(t, p.statsEnabled) {
		assert.False(t, p.statsEnabled())
		assert.True(t, called, "statsEnabled が渡した関数でない")
	}
}
