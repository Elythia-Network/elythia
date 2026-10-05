package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/mediaproxy"
	"github.com/labstack/echo/v4"
)

// statusClientClosedRequest is nginx's non-standard 499, used when the client
// went away before we produced a response.
//
// 標準の status ではないが、本文を書かないので wire 上は問題にならない。
// 500 と混ぜないことが目的 (#3032)。
const statusClientClosedRequest = 499

// MediaProxy is the subset of *mediaproxy.Service this handler needs.
//
// **インターフェースにしてあるのは、失敗の分岐をテストから決定的に踏むため
// (#3032)。** 過負荷や not-found を実サービスで再現しようとすると、枠を
// 埋める側と観測する側の競争になって flaky になる。handler の責務は
// 「error を wire の形に写す」ことなので、そこだけを見る。
type MediaProxy interface {
	Authorize(ctx context.Context, rawURL, sig string) error
	Fetch(ctx context.Context, rawURL string, mode mediaproxy.ProxyMode, out mediaproxy.OutputFormat, animated bool) (*mediaproxy.ProxyResult, error)
}

// Handler handles the /proxy/* media proxy endpoint.
type Handler struct {
	service MediaProxy
	config  *config.Config
}

// NewHandler creates a new proxy Handler.
func NewHandler(service MediaProxy, cfg *config.Config) *Handler {
	return &Handler{service: service, config: cfg}
}

// Handle processes GET /proxy/* requests.
//
// URL extraction:
//   - Query param ?url=X takes priority
//   - Otherwise, path after /proxy/ is treated as the URL (prefixed with "https://")
//
// Query params: emoji, avatar, static, preview, badge, origin, fallback, sig
func (h *Handler) Handle(c echo.Context) error {
	rawURL := c.QueryParam("url")
	if rawURL == "" {
		// パスからURLを取り出す: /proxy/image.webp → 無効、/proxy/example.com/img.png → https://example.com/img.png
		pathAfterProxy := strings.TrimPrefix(c.Request().URL.Path, "/proxy/")
		// image.webp, preview.webp, static.webp 等のファイル名パターンはスキップ
		if pathAfterProxy != "" && !isProxyFilename(pathAfterProxy) {
			rawURL = "https://" + pathAfterProxy
		}
	}

	if rawURL == "" {
		return c.NoContent(http.StatusBadRequest)
	}

	// 外部プロキシへのリダイレクト
	origin := c.QueryParam("origin")
	if h.config.ExternalMediaProxyEnabled && origin == "" {
		return h.redirectToExternalProxy(c, rawURL)
	}

	// User-Agent検証
	ua := c.Request().Header.Get("User-Agent")
	if ua == "" {
		return c.String(http.StatusBadRequest, "User-Agent is required")
	}
	// #2106 L43: 自身の outbound UA は "Elythia/" 始まり (#3394。それより前は "mk-go/"、
	// #774 で Misskey/ から rename)。upstream の "misskey/" 判定だけだと自身の proxy 経由
	// リクエストを recursive 検出できず loop/増幅防御が効かないため、どれも見る。旧名は
	// 改名より前の版を動かしている相手のために残す。
	lowerUA := strings.ToLower(ua)
	if strings.Contains(lowerUA, "misskey/") ||
		strings.Contains(lowerUA, strings.ToLower(config.UserAgentProduct)+"/") ||
		strings.Contains(lowerUA, config.LegacyUserAgentProduct+"/") {
		return c.String(http.StatusForbidden, "Proxy is recursive")
	}

	// 認可: HMAC署名 or allowlist
	sig := c.QueryParam("sig")
	if err := h.service.Authorize(c.Request().Context(), rawURL, sig); err != nil {
		if errors.Is(err, context.Canceled) {
			return c.NoContent(statusClientClosedRequest)
		}
		if errors.Is(err, mediaproxy.ErrAllowlistUnavailable) {
			// **403 + 1 日にしない (#3036)。** 許可されていないのではなく
			// 判定できなかっただけなので、DB が戻れば同じ URL が通る。
			// 1 日キャッシュすると、瞬断のあいだに見られた**すべての**
			// プロキシ URL が 1 日壊れる。
			//
			// **キャッシュは `no-store`。#3032 の過負荷と同じバケツ** で、
			// #3034 / #3035 の `max-age=300` とは分ける。分ける基準は
			// 「復旧までの長さ」と「再取得のコスト」:
			//
			//   - #3034 は**他人のサーバー**が分単位で落ちている状態で、
			//     再取得は最大 32MiB のダウンロード。叩き続けないために寝かせる
			//   - こちらは**自分の DB** の瞬断で、返すのは本文 0 の 503。
			//     落ちている DB へのクエリは即座に失敗するので、寝かせて
			//     守る相手がいない。逆に 5 分寝かせると、**DB が 3 秒で
			//     戻ってもその 3 秒に見られた全 URL が 5 分壊れたまま**になる
			//
			// `max-age` を付けると `Retry-After` が不活性になる点でも
			// 整合しない (RFC 9111 §3 は明示 `max-age` のある 503 を保存可能
			// とするので、1 秒後に来た要求が残り 299 秒の同じ 503 を受け取る)。
			//
			// **ログは service 側で 1 行出している。** ここで重ねると DB 障害中に
			// 全リクエストが 2 行になる。
			if c.QueryParam("fallback") != "" {
				return h.serveFallbackWithCache(c, "no-store")
			}
			c.Response().Header().Set("Retry-After", "1")
			c.Response().Header().Set("Cache-Control", "no-store")
			return c.NoContent(http.StatusServiceUnavailable)
		}
		if c.QueryParam("fallback") != "" {
			return h.serveFallback(c)
		}
		c.Response().Header().Set("Cache-Control", "max-age=86400")
		return c.NoContent(http.StatusForbidden)
	}

	// 処理モード + 出力フォーマット決定 (#637 M3)
	mode := parseMode(c)
	out := parseOutputFormat(c)

	// Fetch + 画像処理
	result, err := h.service.Fetch(c.Request().Context(), rawURL, mode, out, parseAnimated(c))
	if err != nil {
		if errors.Is(err, mediaproxy.ErrOverloaded) {
			// **枠の枯渇は access log から区別が付かない** ので残す。
			// #2849 の argon2 枠が signin handler で同じことをしている。
			//
			// **流量は signin とは桁が違う。** 1 リクエスト 1 行で、shed は
			// 「到着率 - 排出率」の分だけ出る (排出は 8 core・枠 4 の実測で
			// 63.9 rps)。到着 200 rps なら約 136 行/秒。ローテーションは
			// #2828 で効いているのでディスクは埋まらないが、輻輳時に
			// 他のログが流れることは織り込んでおくこと。
			slog.Warn("mediaproxy: shed request, image pipeline saturated",
				// **`.String()` を明示的に通す。** slog の JSONHandler は
				// `fmt.Stringer` を使わないので、handler を差し替えた瞬間に
				// `"mode":2` に戻る (#2849 の call site も同じ形)。
				"url", rawURL, "mode", mode.String())
			// **過負荷の応答は絶対にキャッシュさせない (#3032)。** 通常の
			// fallback は `max-age=300` だが、瞬間的な輻輳を CDN に載せると
			// その 5 分間ずっと壊れた画像が見える。#2913 (403 が 1 日
			// キャッシュされてアイコンが壊れ続けた) と同型の失敗になる。
			if c.QueryParam("fallback") != "" {
				// **`Retry-After` は付けない。** 返すのは 200 の画像で、
				// RFC 9110 が `Retry-After` を定義しているのは 503 と 3xx。
				return h.serveFallbackWithCache(c, "no-store")
			}
			c.Response().Header().Set("Retry-After", "1")
			c.Response().Header().Set("Cache-Control", "no-store")
			return c.NoContent(http.StatusServiceUnavailable)
		}
		// **`context.Canceled` だけ。`DeadlineExceeded` を混ぜてはいけない
		// (#3032 レビュー)。** `/proxy` のリクエスト context に deadline を
		// 付ける middleware は無い (`grep 'middleware.Timeout\|WithDeadline'
		// internal/server/` が 0 件) ので、利用者の離脱が deadline として
		// 現れることはない。一方 `DeadlineExceeded` は **mk-go 自身の 30 秒
		// `httpClient.Timeout`** から来る — Go 1.26 では `Client.Timeout`
		// 由来のエラーが `errors.Is(err, context.DeadlineExceeded)` を満たし、
		// `fetchRemote` が `%w` でラップしている。これを 499 に倒すと、
		// origin が body を引き延ばしただけの**サーバー側障害**が 4xx にも
		// 5xx にも出ず、しかも `?fallback` が無視される。
		if errors.Is(err, context.Canceled) {
			// 利用者が接続を切っただけ。500 に数えると監視で本物の障害が
			// 埋もれるので、nginx と同じ 499 にして本文は書かない。
			//
			// **`ErrUpstreamUnavailable` より先に見る。** リモート取得中の
			// 切断は両方の条件を満たす (#3034 で `%w` を 2 つにしたため) が、
			// 利用者が自分で切ったものは 502 ではなく 499 が正しい。
			return c.NoContent(statusClientClosedRequest)
		}
		if errors.Is(err, mediaproxy.ErrTargetBlocked) {
			// **502 にしない (#3037)。** 相手が落ちているのではなく、
			// こちらが遮断した。502 だと監視で「相手インスタンスが落ちている」
			// と読める。
			//
			// **キャッシュは 502 のときと同じ 5 分のまま。** SSRF の可否は
			// 毎リクエストの DNS 解決で決まるので恒久的ではない (詳細は
			// `mediaproxy.ErrTargetBlocked` の GoDoc)。変えるのは status とログだけ。
			//
			// **`Authorize` 失敗の 403 と同じ status だが、こちらはログを出す。**
			// 合流させると監視で「allowlist に無い」と「private IP を遮断した」が
			// 区別できない。Error ではなく Warn — 設定どおりに働いた結果で、
			// 運用者が直すべき障害ではない。
			//
			// **未認証の利用者がこのログを任意に生成できる。** `/proxy/*` は
			// `api` グループの外でレートリミットが無く、allowlist 済みの URL が
			// private IP へ 302 するだけでここに来る (allowlist の列は連合相手が
			// 埋められる)。それでも Warn で残すのは、(a) 5xx を作らないので
			// 可用性の監視を汚さない、(b) 1 リクエスト 1 行でローテーションは
			// #2828 で効いている、(c) ここを消すと `Authorize` の 403 と
			// 区別する手段が無くなる、の 3 点による。障害が長引く構成では
			// サンプリングを検討すること (#3032 の shed ログと同じ判断)。
			//
			// **`errors.Unwrap` を使わない。** `fetchRemote` は `%w` を 2 つ
			// 使うので、返る型が実装するのは `Unwrap() []error` のほう。
			// `errors.Unwrap` は `Unwrap() error` しか呼ばないので **本番では
			// nil になり、safehttp 側のメッセージが丸ごと消える** (1 周目の
			// 「message と err が重複する」という指摘に応えて入れたが、
			// 2 周目で実測して戻した)。
			slog.Warn("mediaproxy: blocked target", "url", rawURL, "err", err)
			if c.QueryParam("fallback") != "" {
				return h.serveFallback(c)
			}
			c.Response().Header().Set("Cache-Control", "max-age=300")
			return c.NoContent(http.StatusForbidden)
		}
		if errors.Is(err, mediaproxy.ErrUpstreamUnavailable) {
			// **404 にしない (#3034)。** リモートが「無い」と言ったわけでは
			// なく、こちらが取れなかっただけなので、復旧すれば同じ URL が
			// 引ける。短いキャッシュを付けて gateway 系の status にする。
			//
			// **`no-store` にはしない。** 過負荷 (#3032) は一瞬で復旧するが
			// リモートの障害は分単位で続くので、都度取りに行くと落ちている
			// 相手を叩き続けることになる。5 分は generic な失敗と同じ値で、
			// upstream の `errorHandler` (`max-age=300`) とも揃う。
			//
			// **「繋がらない」と「遅い」を分ける。** 30 秒の
			// `httpClient.Timeout` に当たったものは 504 で、それ以外が 502。
			// 混ぜると監視でどちらか分からない。
			status := http.StatusBadGateway
			if errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			if c.QueryParam("fallback") != "" {
				return h.serveFallback(c)
			}
			c.Response().Header().Set("Cache-Control", "max-age=300")
			return c.NoContent(status)
		}
		if errors.Is(err, mediaproxy.ErrBadRequest) {
			// proxy が取りに行けない URL (相対 URL / 非 http(s) / host 無し /
			// 制御文字入り) と、`/files/` の access key が空のもの。
			// **恒久的なので長期キャッシュでよい (#3034)。** 5 分ごとに
			// 引き直しても結果は変わらない。
			if c.QueryParam("fallback") != "" {
				return h.serveFallback(c)
			}
			c.Response().Header().Set("Cache-Control", "max-age=86400")
			return c.NoContent(http.StatusBadRequest)
		}
		if errors.Is(err, mediaproxy.ErrNotFound) {
			if c.QueryParam("fallback") != "" {
				return h.serveFallback(c)
			}
			c.Response().Header().Set("Cache-Control", "max-age=86400")
			return c.NoContent(http.StatusNotFound)
		}
		if errors.Is(err, mediaproxy.ErrTooLarge) {
			return c.NoContent(http.StatusRequestEntityTooLarge)
		}
		// **原因をログに残す (#3383)。** status だけでは区別できず、本番の 500 の
		// 原因を確かめられなかった。ここに来るのは、取得の失敗 (502 / 504)・不正な
		// URL (400)・404・大きすぎる (413) のどれでもないもの。主にリモートが
		// 404 / 410 以外の非 2xx (401 / 403 / 429 / 5xx) を返したとき
		// (`remote returned %d`)、表示できない MIME が来たとき、ストレージの障害。
		// 画像の変換の失敗は原本かダミーを返すので、ここには来ない。許可確認の
		// DB 障害は 503 で別に返る。
		//
		// **未認証の利用者がこのログを生成できる。** allowlist 済みの URL の相手が
		// 常に 403 を返せば 1 リクエスト 1 行になる。shed / blocked target と同じく、
		// 1 リクエスト 1 行でローテーションは #2828 で効いているので Warn で残す。
		// リモートの 403 が常態的に多いと分かったら、`remote returned` だけ Info に
		// 下げるかサンプリングを検討する。url は shed / blocked と同じ値 (sig は
		// 別のパラメータなので含まれない)。
		slog.Warn("mediaproxy: proxy failed", "url", rawURL, "mode", mode.String(), "err", err)
		if c.QueryParam("fallback") != "" {
			return h.serveFallback(c)
		}
		c.Response().Header().Set("Cache-Control", "max-age=300")
		return c.NoContent(http.StatusInternalServerError)
	}
	defer result.Body.Close()

	// **結果がキャッシュ方針を指定していればそれに従う (#3035)。**
	// 生成に失敗してダミー画像へ倒れた応答は 200 で返るが、原因が一時的な
	// ものを `immutable` で 1 年固定すると、相手が復旧しても直らない。
	cacheControl := "max-age=31536000, immutable"
	if result.CacheControl != "" {
		cacheControl = result.CacheControl
	}
	c.Response().Header().Set("Cache-Control", cacheControl)
	c.Response().Header().Set("Content-Type", result.ContentType)
	// Output format depends on the client's Accept header (image/avif → AVIF,
	// otherwise WebP), so shared caches MUST key on Accept to avoid serving
	// AVIF to a Safari 15 / WebP-only client cached behind a CDN, and
	// vice-versa (#637 review UR-012).
	c.Response().Header().Set("Vary", "Accept")
	setProxyContentSecurityHeaders(c)
	c.Response().WriteHeader(http.StatusOK)
	_, _ = io.Copy(c.Response(), result.Body)
	return nil
}

// setProxyContentSecurityHeaders mirrors what `filesHandler` attaches to drive
// responses.
//
// **`/proxy/*` にだけ無かった (#3037)。** `nosniff` は #2782 で全応答に付くように
// なり、Content-Type も `browsersafeMIMEs` に絞ってあるので既知の経路は塞がって
// いるが、**この origin は自分のドメイン**なので、1 つでも取りこぼすとそこから
// 同一オリジンの XSS になる。mk-go は自分のファイル配信で同じ脅威を 3 重に
// 塞いでいるのだから、こちらだけ 1 枚薄いままにしない。
//
// 値は `filesHandler` と同じ。
func setProxyContentSecurityHeaders(c echo.Context) {
	c.Response().Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'")
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	c.Response().Header().Set("Content-Disposition", "inline")
}

// redirectToExternalProxy sends a 301 redirect to the configured external proxy.
func (h *Handler) redirectToExternalProxy(c echo.Context, rawURL string) error {
	pathAfterProxy := strings.TrimPrefix(c.Request().URL.Path, "/proxy/")
	u, err := url.Parse(h.config.MediaProxy + "/" + pathAfterProxy)
	if err != nil {
		return c.NoContent(http.StatusInternalServerError)
	}

	q := u.Query()
	// 元リクエストのクエリパラメータを全て転送
	for key, vals := range c.QueryParams() {
		for _, val := range vals {
			q.Set(key, val)
		}
	}
	// urlが未設定の場合はセット
	if q.Get("url") == "" {
		q.Set("url", rawURL)
	}
	u.RawQuery = q.Encode()

	c.Response().Header().Set("Cache-Control", "public, max-age=259200")
	return c.Redirect(http.StatusMovedPermanently, u.String())
}

// serveFallback returns a 1x1 transparent PNG with short cache.
func (h *Handler) serveFallback(c echo.Context) error {
	return h.serveFallbackWithCache(c, "max-age=300")
}

// serveFallbackWithCache is serveFallback with an explicit Cache-Control.
//
// **一時的な失敗と恒久的な失敗でキャッシュ時間を分けるために要る (#3032)。**
// 過負荷で落とした応答が CDN に載ると、輻輳が去っても壊れたままになる。
func (h *Handler) serveFallbackWithCache(c echo.Context, cacheControl string) error {
	dummy := mediaproxy.DummyPNG()
	defer dummy.Body.Close()
	c.Response().Header().Set("Cache-Control", cacheControl)
	setProxyContentSecurityHeaders(c)
	data, _ := io.ReadAll(dummy.Body)
	return c.Blob(http.StatusOK, dummy.ContentType, data)
}

// parseMode determines the processing mode from query parameters.
func parseMode(c echo.Context) mediaproxy.ProxyMode {
	if c.QueryParam("emoji") != "" {
		return mediaproxy.ModeEmoji
	}
	if c.QueryParam("avatar") != "" {
		return mediaproxy.ModeAvatar
	}
	if c.QueryParam("static") != "" {
		return mediaproxy.ModeStatic
	}
	if c.QueryParam("preview") != "" {
		return mediaproxy.ModePreview
	}
	if c.QueryParam("badge") != "" {
		return mediaproxy.ModeBadge
	}
	return mediaproxy.ModeDefault
}

// parseAnimated reports whether animated formats may be returned as-is.
//
// **mode と直交する (#2905)。** `?emoji=1&static=1` は「emoji のサイズで、ただし
// 静止画」を意味する。parseMode は emoji を先に見るので mode は ModeEmoji のままで、
// 静止画かどうかはここで別に判定する (upstream の
// `animated: !('static' in query)` と同じ)。
func parseAnimated(c echo.Context) bool {
	// **存在で見る (値は問わない)。** upstream は fastify の `'static' in query`
	// なので `?static=` (空値) でも静止画になる。値の非空で判定すると
	// 同じリポジトリ内の emoji_redirect.go (存在判定) と食い違い、
	// `/emoji/x.webp?static=` は静止画・`/proxy/...?static=` はアニメ、という
	// 矛盾が生まれる。
	_, present := c.QueryParams()["static"]
	return !present
}

// parseOutputFormat picks the encoder format from `?avif=1` (explicit opt-in,
// matches the existing `?static=1` style flags) or the Accept header (treats
// `image/avif` as a non-zero quality preference). Falls back to WebP.
//
// AVIF は CPU 重めなので「ブラウザが本当に使う」ケースだけに絞る。Misskey
// TS の sharp().avif() 経路と同じ semantics。
func parseOutputFormat(c echo.Context) mediaproxy.OutputFormat {
	if c.QueryParam("avif") != "" {
		return mediaproxy.FormatAVIF
	}
	if acceptsAVIF(c.Request().Header.Get("Accept")) {
		return mediaproxy.FormatAVIF
	}
	return mediaproxy.FormatWebP
}

// acceptsAVIF returns true when the client signalled non-zero preference for
// `image/avif` in its Accept header.
func acceptsAVIF(accept string) bool {
	for _, part := range strings.Split(accept, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		mt, params := splitAcceptEntry(entry)
		if !strings.EqualFold(mt, "image/avif") {
			continue
		}
		if params["q"] == "0" || params["q"] == "0.0" || params["q"] == "0.00" {
			return false
		}
		return true
	}
	return false
}

func splitAcceptEntry(entry string) (string, map[string]string) {
	parts := strings.Split(entry, ";")
	mt := strings.TrimSpace(parts[0])
	params := map[string]string{}
	for _, p := range parts[1:] {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) == 2 {
			params[strings.ToLower(strings.TrimSpace(kv[0]))] = strings.TrimSpace(kv[1])
		}
	}
	return mt, params
}

// isProxyFilename returns true if the path segment looks like a proxy output
// filename (e.g., "image.webp", "preview.webp", "static.webp", "emoji.webp").
func isProxyFilename(path string) bool {
	for _, name := range []string{
		"image.webp", "preview.webp", "static.webp",
		"emoji.webp", "emoji.png", "avatar.webp",
	} {
		if path == name {
			return true
		}
	}
	return false
}
