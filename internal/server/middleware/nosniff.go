package middleware

import "github.com/labstack/echo/v5"

// NoSniff returns a GLOBAL middleware that sets `X-Content-Type-Options: nosniff`.
//
// ブラウザが `Content-Type` を無視して中身から MIME を推測するのを止める。
// 推測されると、こちらが `text/plain` のつもりで返したものが `text/html` として
// 解釈され、埋め込まれた script が origin 上で動く。
//
// **upstream には無い mk-go 独自の hardening** (#2782)。upstream の backend が
// 明示的に付ける箇所は無い (`git grep -n nosniff -- packages/backend/src/` が
// 0 件。`built/` に出る hit は `@fastify/static` の依存で、static route の
// 404 / 301 応答にだけ付く)。`Referrer-Policy` (#2404) と同じく、互換性の欠落では
// なく上乗せ。
//
// **全レスポンスに付ける。** 「どこに付けるか」を列挙する形だと route が増えた
// ときに漏れる。実際 #2782 まで `X-Content-Type-Options` は drive のファイル配信
// (`internal/server/files.go`) とプラグインの proxy (`plugin_wiring.go`) にしか
// 付いておらず、SPA shell も API も素通しだった。
//
// 除外は設けていない。`nosniff` が壊すのは「`Content-Type` が間違っているのに
// 推測で救われていた」応答だけで、それは直すべき側。
func NoSniff() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Response().Header().Set("X-Content-Type-Options", "nosniff")
			return next(c)
		}
	}
}
