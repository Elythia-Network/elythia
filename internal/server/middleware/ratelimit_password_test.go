package middleware

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
)

// TestDefaultEndpointLimits_TokenHolderCannotExhaustRecoveryEndpoints guards
// the endpoints that verify the current password against route limits.
//
// limiter は route の RequireAuth / RequireSecure より前に走り、成否に関係なく
// user bucket を消費する。ここに上限を置くと、被害者の token を持つだけの
// 第三者が `i/regenerate-token` (token 漏洩時の唯一の対処) を使い切れる。
// 総当たりは handler 側の passwordguard が照合失敗だけを数えて止める。
func TestDefaultEndpointLimits_TokenHolderCannotExhaustRecoveryEndpoints(t *testing.T) {
	paths := []string{
		"/api/i/change-password",
		"/api/i/delete-account",
		"/api/i/regenerate-token",
		"/api/i/2fa/register",
		"/api/i/2fa/unregister",
		"/api/i/2fa/register-key",
		"/api/i/2fa/key-done",
		"/api/i/2fa/remove-key",
	}
	user := &model.User{ID: "01hpasswordbruteforce00000"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			_, ok := DefaultEndpointLimits[strings.TrimPrefix(path, "/api/")]
			assert.False(t, ok, "%s に route の上限がある。token を持つ第三者が使い切れる", path)

			// store が「尽きた」を返しても limiter はこの endpoint を数えない。
			store := &mockLimitStore{results: []mockResult{
				{Info: LimitInfo{Remaining: 0, ResetMs: time.Now().Add(time.Hour).UnixMilli()}},
			}}
			rl := NewRateLimiter(store, true, DefaultEndpointLimits)
			e, h := setupEcho(rl)
			rec := doRequest(e, rl.Middleware(), h, path, user)
			assert.NotEqual(t, http.StatusTooManyRequests, rec.Code)
			assert.Empty(t, store.calls, "%s で bucket を消費している", path)
		})
	}
}
