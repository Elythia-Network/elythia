package emojiapplication

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// NewService は受け取った依存を service に持たせる (#3823)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewServiceKeepsDependencies(t *testing.T) {
	policies := &stubPolicies{}
	resets := &fakeResets{}
	svc := NewService(nil, nil, nil, nil, nil, nil, policies, resets)
	assert.Same(t, policies, svc.policies)
	assert.Same(t, resets, svc.resets)
}
