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
//
// guard は i の handler の構築子の必須の引数 (#3827)。渡し忘れはコンパイルで落ち、nil を
// 渡す形は TestBuiltHandlersHaveNoNilDependencies が落とす。ただ、同じ型の別の実装
// (数えない guard) を渡す形はどちらでも捕まらないので、Redis で実装した guard を作る行と、
// それが setupRoutes → buildHandlers → i.NewHandler へ渡る段を照合する。
func TestTwoFAPendingGuardIsWired(t *testing.T) {
	const symptom = "i/2fa/register の仮の秘密に期限と試行回数の上限が掛からない"
	assertWired(t, serverPkg,
		`twoFAPendingGuard := coretwofactor.NewRedisPendingGuard(s.redis.Default)`, symptom)
	assertWired(t, serverPkg,
		`hSvcs := s.buildHandlers(w, coreSvcs, fedSvcs, idGen, noteHider, entityPacker, sharedUserPacker, metaRepo, `+
			`abuseNotifStates, abuseReportRepoForNotif.CountUnresolved, signupApplicationNotifLookup, `+
			`twoFAPendingGuard, streamRevokePublisher)`, symptom)
	assertWired(t, serverPkg,
		`i.NewHandler(userService, idGen, noteHider, entityPacker, roleService, totpReplayGuard, `+
			`twoFAPendingGuard, s.auth, accessTokenRepo, streamRevokePublisher, followingService, s.background)`, symptom)
}
