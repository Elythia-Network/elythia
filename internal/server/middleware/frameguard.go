package middleware

import (
	"strings"

	"github.com/labstack/echo/v4"
)

// frameGuardSkipPrefixes lists path prefixes that must stay embeddable.
//
//   - /embed/ は iframe に埋め込まれること自体が目的。upstream も
//     ClientServerService で一律 DENY を付けたうえで、embed route だけ
//     `reply.removeHeader('X-Frame-Options')` して外している
//   - /files/ と /proxy/ は画像・動画・PDF などを配る。upstream の
//     FileServerService は X-Frame-Options を付けておらず、PDF を iframe で
//     開く利用を壊さないためにも合わせる
//
// /embed/ は `internal/server/wiring_frontend_routes.go` で配線済み (#2389)。**除外はここ 1 箇所で管理する** —
// frontend CSP に `frame-ancestors` を入れると同じ除外を 2 箇所で持つことになり、
// 片方だけ更新して埋め込みが死ぬ (#2789 で embed に CSP を付けたときも
// `frame-ancestors` は入れていない)。
var frameGuardSkipPrefixes = []string{
	"/embed/",
	"/files/",
	"/proxy/",
}

// FrameGuard returns a GLOBAL middleware that sets `X-Frame-Options: DENY`
// so the UI cannot be framed by a third-party page.
//
// upstream は ClientServerService の onRequest hook で同じ header を付けている
// (「クリックジャッキング防止のためiFrameの中に入れられないようにする」)。
// mk-go にはこれが無く、**upstream にあって mk-go に無い**状態だった。
// 単なる hardening ではなく互換性の欠落でもある。
//
// 特に守りたいのは OAuth の同意画面。認可プロンプトを透明な iframe で重ねて
// クリックを盗む攻撃はクリックジャッキングの典型で、SPA shell と同じ経路で
// 配信されている以上ここで一括して塞ぐのが確実。
//
// JSON を返す /api/* にも付くが、JSON document を frame する用途は無いので
// 実害はない。「どこに付けるか」を細かく列挙するより、外すべき経路
// (frameGuardSkipPrefixes) を明示する方が、route が増えたときに守り漏れない。
func FrameGuard() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// **マッチしたルートで判定する。**
			//
			// 生のリクエストパスで前方一致を取ると、`/files/` (キー無し) の
			// ように**除外の接頭辞に当たるが実際には SPA へ落ちる**パスで
			// ヘッダが外れる。`/files/:accessKey` は空セグメントにマッチせず
			// catchall (`/*`) に落ちるので、SPA シェルが `X-Frame-Options`
			// 無しで返っていた。`c.Path()` はルーティング後のパターン
			// (`/files/:accessKey` / `/*`) を返すので、実際に返すものと
			// 判定が揃う。
			// ルーティングを通っていない (= 直接呼ばれた) ときは生パスに
			// 落とす。本番では `e.Use` なので必ずパターンが取れる。
			pattern := c.Path()
			if pattern == "" {
				pattern = c.Request().URL.Path
			}
			if !frameGuardSkipped(pattern) {
				c.Response().Header().Set("X-Frame-Options", "DENY")
			}
			return next(c)
		}
	}
}

// frameGuardSkipped reports whether p is exempt from the frame guard.
func frameGuardSkipped(p string) bool {
	for _, prefix := range frameGuardSkipPrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}
