package middleware

import "github.com/labstack/echo/v5"

// referrerPolicyValue is the policy applied to every response.
//
// same-origin へは完全な URL を、cross-origin へは origin だけを送り、
// HTTPS → HTTP のダウングレード時は何も送らない。
//
// 主要ブラウザの現在の既定と同じ値なので、対応ブラウザでは挙動が変わらない。
// 明示する意味は 2 つある。既定が異なる古いブラウザでも path が漏れないこと、
// そして「意図してこの値を選んでいる」ことがレスポンスから読み取れること。
//
// より厳しい `no-referrer` は選ばない。外部サイト側が参照元インスタンスを
// origin 単位で把握できることには実用上の価値があり (連合先からの流入の把握、
// 画像 hotlink の判断など)、upstream も含め Fediverse の慣行から外れる。
const referrerPolicyValue = "strict-origin-when-cross-origin"

// ReferrerPolicy returns a GLOBAL middleware that sets `Referrer-Policy`.
//
// **upstream Misskey には無い。** mk-go 独自の hardening で、意図的な
// divergence として docs/divergence.md に記載している。
//
// これが無いと、ノート本文中の外部リンクを踏んだ際に閲覧していた URL が
// path ごと Referer として送られる。Misskey の URL は
// `/notes/<id>` / `/@user` のように**何を見ていたかがそのまま分かる**形なので、
// 遷移先に閲覧内容が漏れることになる。
//
// path prefix による除外は設けない。Referer を必要とする経路が mk-go には無く、
// cross-origin でも origin は送られるので hotlink 判定なども壊れない。
func ReferrerPolicy() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Response().Header().Set("Referrer-Policy", referrerPolicyValue)
			return next(c)
		}
	}
}
