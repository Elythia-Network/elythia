package entitycompat

import "testing"

// `GET /oauth/authorize` は匿名で client_id の文書を取りに行き、承認の待ちを
// Redis に置く。`/api` の外なので limiter の表に載せただけでは掛からず、route に
// RequireIPRateLimit を付ける必要がある。`internal/server` はカバレッジ対象外で
// router を組み立てるテストも無いので、ここで固定する。
func TestOAuthAuthorizeIPRateLimitIsWired(t *testing.T) {
	assertWired(t, serverPkg,
		`s.echo.GET("/oauth/authorize", oauthHandler.Authorize, `+
			`middleware.RequireIPRateLimit(rateLimiter, middleware.OAuthAuthorizeRateLimitKey))`,
		"/oauth/authorize に接続元ごとの回数の上限が掛からない")
}

// i/2fa/register の仮の秘密の期限と i/2fa/done の失敗回数は、router が guard を
// 渡さないと数えられない (handler は nil なら数えない)。
func TestTwoFAPendingGuardIsWired(t *testing.T) {
	assertWired(t, serverPkg,
		`iHandler.SetTwoFAPendingGuard(coretwofactor.NewRedisPendingGuard(s.redis.Default))`,
		"i/2fa/register の仮の秘密に期限と試行回数の上限が掛からない")
}
