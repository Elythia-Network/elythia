package signup_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/stretchr/testify/assert"
)

func TestValidUsernameFormat(t *testing.T) {
	assert.True(t, signup.ValidUsernameFormat("alice"))
	assert.True(t, signup.ValidUsernameFormat("a_1"))
	assert.True(t, signup.ValidUsernameFormat("ABCdef123_"))
	assert.True(t, signup.ValidUsernameFormat("a")) // length 1 OK

	assert.False(t, signup.ValidUsernameFormat(""))                      // 空
	assert.False(t, signup.ValidUsernameFormat("has space"))             // 空白
	assert.False(t, signup.ValidUsernameFormat("dash-not-allowed"))      // ハイフン不可
	assert.False(t, signup.ValidUsernameFormat("dot.not.allowed"))       // ドット不可
	assert.False(t, signup.ValidUsernameFormat("aaaaaaaaaaaaaaaaaaaaa")) // 21 文字 (>20)
	assert.False(t, signup.ValidUsernameFormat("ünïcode"))               // 非 ASCII
}

func TestIsReservedUsername(t *testing.T) {
	reserved := []string{"admin", "Root", " system "}
	assert.True(t, signup.IsReservedUsername("admin", reserved))
	assert.True(t, signup.IsReservedUsername("root", reserved))   // case-insensitive
	assert.True(t, signup.IsReservedUsername("system", reserved)) // trim 済比較
	assert.False(t, signup.IsReservedUsername("alice", reserved))
	assert.False(t, signup.IsReservedUsername("admin", nil))
}
