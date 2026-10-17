package oauth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// #2682: 起動時の配線検査が見る述語。両側を固定する。
//
// **失効が走るのは攻撃を検出した瞬間** (authorization code の再利用) なので、
// 未配線だと盗んだコードを再利用した相手の token が auth cache の TTL の
// あいだ生き残る (#2682 review M-2)。
func TestHandler_HasAuthInvalidator(t *testing.T) {
	assert.False(t, (&Handler{}).HasAuthInvalidator(), "未配線なら false")

	h := &Handler{}
	h.SetAuthInvalidator(stubOAuthTokenInvalidator{})
	assert.True(t, h.HasAuthInvalidator(), "配線したら true")
}

type stubOAuthTokenInvalidator struct{}

func (stubOAuthTokenInvalidator) InvalidateUserToken(string, string) {}
