package transfer

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

// acct は package 内でしか呼べない非公開ヘルパなので、same-package テストから
// 全分岐を exercise する。
func TestAcct_NilReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", acct(nil))
}

func TestAcct_Local(t *testing.T) {
	u := &model.User{Username: "alice"}
	assert.Equal(t, "alice", acct(u))
}

func TestAcct_LocalWithEmptyHostString(t *testing.T) {
	// host フィールドが空文字ポインタでもローカル扱いになる (nil と同じ)。
	empty := ""
	u := &model.User{Username: "alice", Host: &empty}
	assert.Equal(t, "alice", acct(u))
}

func TestAcct_Remote(t *testing.T) {
	host := "remote.example"
	u := &model.User{Username: "alice", Host: &host}
	assert.Equal(t, "alice@remote.example", acct(u))
}
