package server

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// 静的アサーション: repository.EmojiRepository が emojiLookup を満たすこと
// を build 時に検証する (avatar.go の同パターンと揃える)。配線時の型
// 不一致 (interface 拡張 / signature 変更) を早期に検出。
var _ emojiLookup = (repository.EmojiRepository)(nil)

// emojiRedirectFallback is the path the frontend treats as the
// "emoji not found" placeholder. Misskey TS upstream redirects to
// the bundled static asset when the request includes a `fallback`
// query string.
const emojiRedirectFallback = "/static-assets/emoji-unknown.png"

// emojiPathRegexp validates the URL component the frontend uses to
// reference custom emoji (`<name>[@<host>].webp`). Mirrors upstream's
// regex (`/^[a-zA-Z0-9\-_@\.]+?\.webp$/`) so requests for unrelated
// or attacker-crafted paths are rejected before hitting the DB.
var emojiPathRegexp = regexp.MustCompile(`^[a-zA-Z0-9\-_@\.]+?\.webp$`)

// emojiLookup is the minimal interface used by the redirect handler.
// Keeping the surface narrow lets handler tests stub without standing
// up the full EmojiRepository.
type emojiLookup interface {
	FindByNameAndHost(name string, host *string) (*model.Emoji, error)
}

// emojiRedirectHandler implements `GET /emoji/:path(.*)` (#468). The
// frontend `MkCustomEmoji` falls back to fetching this endpoint when
// no explicit `emojiUrl` prop is passed — notably the notification
// `MkReactionIcon` for reaction notifications, which would otherwise
// render the fallback image. We mirror upstream Misskey's
// `ServerService.ts:153` semantics:
//
//   - path = "<name>.webp" → local emoji (host == nil)
//   - path = "<name>@.<>.webp" → local emoji (`@.` is the canonical
//     local-suffix sentinel emitted by ReactionService)
//   - path = "<name>@<host>.webp" → remote emoji
//   - row found → 302 redirect to publicUrl (fallback to originalUrl)
//   - row missing → 404, or 302 to /static-assets/emoji-unknown.png
//     when the `fallback` query string is present
//
// Cache-Control header mirrors upstream so the browser stops
// re-fetching reaction images on every notification render.
func emojiRedirectHandler(repo emojiLookup) echo.HandlerFunc {
	return func(c echo.Context) error {
		path := c.Param("path")

		c.Response().Header().Set(echo.HeaderCacheControl, "public, max-age=86400")

		if !emojiPathRegexp.MatchString(path) {
			return c.NoContent(http.StatusNotFound)
		}

		// `.webp` 拡張子を剥がし、`@` で name / host に分解する。
		emojiPath := strings.TrimSuffix(path, ".webp")
		chunks := strings.Split(emojiPath, "@")
		if len(chunks) > 2 {
			return c.NoContent(http.StatusBadRequest)
		}

		name := chunks[0]
		var hostPtr *string
		if len(chunks) == 2 {
			h := chunks[1]
			// `@.` は ReactionService.resolveReaction が永続化する
			// canonical local-suffix。host_NULL と等価に扱う。
			if h != "" && h != "." {
				hostPtr = &h
			}
		}

		emoji, err := repo.FindByNameAndHost(name, hostPtr)
		if err != nil && !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。このファイルは
			// apierr を使わないので echo の HTTPError で返す。
			//
			// **キャッシュを打ち消す。** 上で `public, max-age=86400` を張って
			// いるので、そのままだとブラウザや共有キャッシュが 500 を最大 1 日
			// 保持しうる (RFC 9111 は明示的な max-age があれば 5xx も保存できる)。
			// DB が復旧してもその絵文字だけ壊れて見える。
			c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
			return echo.NewHTTPError(http.StatusInternalServerError)
		}
		if err != nil || emoji == nil {
			if _, hasFallback := c.QueryParams()["fallback"]; hasFallback {
				return c.Redirect(http.StatusFound, emojiRedirectFallback)
			}
			return c.NoContent(http.StatusNotFound)
		}

		// publicUrl が空の場合 originalUrl にフォールバック (upstream の
		// `emoji.publicUrl || emoji.originalUrl` と同じ後方互換)。
		target := emoji.PublicURL
		if target == "" {
			target = emoji.OriginalURL
		}
		if target == "" {
			return c.NoContent(http.StatusNotFound)
		}
		// **`?badge=1` は badge モードへ回す (#2909)。** Service Worker の
		// `create-notification.ts` がリアクションのプッシュ通知で
		// `/emoji/<name>.webp?badge=1` を組み立てる。分岐が無いと badge が無視されて
		// 通常の枝に落ち、96x96 の silhouette PNG ではなく**カラーの絵文字画像**が
		// 返る (リモート絵文字なら `ProxyEmojiURLString` で高さ 128 にリサイズされた
		// もの、ローカル絵文字は同一オリジンなので wrap されず原寸の元ファイル)。
		// **どちらも 200 が返るので SW のエラー処理を素通りし、静かに違うものが出る**。
		//
		// **static より先に見る。** upstream (`ServerService.ts`) の if/else が badge を
		// 先に取り、badge の枝では `static` を一切見ない。badge は 96x96 の silhouette
		// PNG 固定なので、アニメーションの有無を渡しても結果が変わらないため。
		if _, wantsBadge := c.QueryParams()["badge"]; wantsBadge {
			// static と同じく entity 側で組む。`/proxy` を手で組むと sig が付かず、
			// Authorize が HMAC ではなく DB allowlist に落ちる (#2905)。
			if proxied := entity.BadgeEmojiProxyURL(target); proxied != "" {
				c.Response().Header().Set("Content-Security-Policy", assetCSP)
				return c.Redirect(http.StatusFound, proxied)
			}
			// context 未配線 (テスト等) なら従来どおり raw へ 302 する。
		}
		// **`?static=1` は media proxy へ回す (#2905)。** 利用者の
		// 「アニメーション画像を再生しない」設定 (disableShowingAnimatedImages) が
		// frontend からこの形で届く (`getStaticImageUrl` は `/emoji/` を見つけると
		// searchParams を足すだけ)。raw へ 302 するとクエリが落ちて設定が無視される。
		// upstream (`ServerService.ts`) も同じく media proxy へ飛ばしている。
		//
		// **allowlist は通る** — proxy は DB に実在する URL だけを許可し、
		// `emoji.publicUrl` / `originalUrl` はその対象。
		if _, wantsStatic := c.QueryParams()["static"]; wantsStatic {
			// **entity 側で組む。** `/proxy` を手で組むと sig が付かず、
			// Authorize が HMAC ではなく DB allowlist に落ちる。`/emoji/:path`
			// はリアクションアイコンのホットパスなので、毎リクエスト DB を
			// 引くことになる (#3036 で DB 障害は 403 + 1 日ではなく
			// 503 + `no-store` になったが、引く回数そのものは減らない)。
			// media proxy の設定 (external / proxyRemoteFiles) にも従う。
			if proxied := entity.StaticEmojiProxyURL(target); proxied != "" {
				c.Response().Header().Set("Content-Security-Policy", assetCSP)
				return c.Redirect(http.StatusFound, proxied)
			}
			// context 未配線 (テスト等) なら従来どおり raw へ 302 する。
		}
		// upstream (`ServerService.ts` の `/emoji/:path`) はここで
		// `default-src 'none'; style-src 'unsafe-inline'` を付けるので header を
		// 揃える。ただし **この応答は redirect なので CSP は実際には効かない**
		// (ブラウザは最終的な応答の header を適用する)。upstream も redirect の
		// 前に付けており同じ状態。security 上の効果を期待して置くものではなく、
		// header 集合を一致させるためのもの。
		c.Response().Header().Set("Content-Security-Policy", assetCSP)
		// upstream は 301 redirect だが、emoji の publicUrl が変動しうる
		// (リモート instance の URL 更新等) ので 302 で永続キャッシュを
		// 避ける。Cache-Control: max-age=86400 でブラウザ側 1 日キャッシュ
		// と組み合わせる。
		// リモート emoji はメディアプロキシ経由に書き換える (#2425)。upstream も
		// この endpoint では `${mediaProxy}/emoji.webp` へ飛ばしており、生の URL を
		// 返していた mk-go の方が乖離していた。包まないとリアクション 1 つ表示する
		// だけで閲覧者の IP が相手インスタンスへ渡る。
		//
		// API 応答の `emojis` field を server-proxy してはいけない (frontend が
		// client 側で包むので二重になる) のとは別経路。こちらは frontend が包まない。
		return c.Redirect(http.StatusFound, entity.ProxyEmojiURLString(target))
	}
}
