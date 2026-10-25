package processors

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/testutil"
)

// 構築子の引数に渡すだけの値。どれも呼ばれないので、interface を埋め込んで型を満たす。
type (
	ctorPushCache struct{ PushSubscriptionCacheInvalidator }
	ctorLock      struct{ ScheduledNoteLock }
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
