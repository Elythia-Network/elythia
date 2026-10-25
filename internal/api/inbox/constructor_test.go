package inbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// 構築子の引数に渡すだけの値。どれも呼ばれないので、interface を埋め込んで型を満たす。
type (
	ctorEnqueuer    struct{ InboxEnqueuer }
	ctorHostBlocker struct{ HostBlockChecker }
)

// NewHandler は、受け取った依存を handler に持たせる (#3833)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	enq := &ctorEnqueuer{}
	blocker := &ctorHostBlocker{}
	h := NewHandler(nil, nil, "example.test", enq, blocker)
	assert.Equal(t, "example.test", h.expectedHost)
	assert.Same(t, enq, h.enqueuer)
	assert.Same(t, blocker, h.hostBlocker)
	assert.NotNil(t, h.keyCache)
}
