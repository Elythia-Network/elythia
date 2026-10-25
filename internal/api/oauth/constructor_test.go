package oauth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// NewHandler は受け取った依存を handler に持たせる (#3823、#3827)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	inv := &fakeInvalidator{}
	rev := &fakeStreamRevoker{}
	h := NewHandler(nil, nil, nil, nil, nil, "", false, nil, inv, rev)
	assert.Same(t, inv, h.authInvalidator)
	assert.Same(t, rev, h.streamRevoker)
}
