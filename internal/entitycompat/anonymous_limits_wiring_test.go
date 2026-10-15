package entitycompat

import "testing"

// i/2fa/register の仮の秘密の期限と i/2fa/done の失敗回数は、router が guard を
// 渡さないと数えられない (handler は nil なら数えない)。
func TestTwoFAPendingGuardIsWired(t *testing.T) {
	assertWired(t, routerGo,
		`iHandler.SetTwoFAPendingGuard(coretwofactor.NewRedisPendingGuard(s.redis.Default))`,
		"i/2fa/register の仮の秘密に期限と試行回数の上限が掛からない")
}
