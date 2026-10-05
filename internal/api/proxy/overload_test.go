package proxy

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/mediaproxy"
	"github.com/elythia-network/elythia/internal/safehttp"
)

// 過負荷で落とした応答の wire 上の形を固定する (#3032)。
//
// **`Cache-Control` が本体。** 通常の fallback は `max-age=300` だが、
// 一時的な輻輳をそれで返すと CDN に 5 分載り、輻輳が去っても画像が壊れた
// ままになる。#2913 が同型 (403 が 1 日キャッシュされてアイコンが 1 日
// 壊れた) なので、ここは必ず `no-store` にする。

// stubProxy returns a fixed error (or a PNG) from Fetch.
type stubProxy struct {
	fetchErr     error
	authErr      error
	cacheControl string
}

func (s stubProxy) Authorize(context.Context, string, string) error { return s.authErr }

func (s stubProxy) Fetch(context.Context, string, mediaproxy.ProxyMode, mediaproxy.OutputFormat, bool) (*mediaproxy.ProxyResult, error) {
	if s.fetchErr != nil {
		return nil, s.fetchErr
	}
	res := mediaproxy.DummyPNG()
	res.CacheControl = s.cacheControl
	return res, nil
}

func stubHandler(t *testing.T, fetchErr error) (*Handler, *echo.Echo) {
	t.Helper()
	cfg := &config.Config{
		URL:              "https://example.com",
		MediaProxy:       "https://example.com/proxy",
		MediaProxySecret: []byte("test-secret"),
		UserAgent:        "Misskey/2026.5.4 (https://example.com)",
	}
	return NewHandler(stubProxy{fetchErr: fetchErr}, cfg), echo.New()
}

func TestHandle_OverloadedReturns503WithoutCaching(t *testing.T) {
	h, e := stubHandler(t, mediaproxy.ErrOverloaded)

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"),
		"過負荷の応答をキャッシュさせてはいけない")
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
}

func TestHandle_OverloadedWithFallbackServesUncachedDummy(t *testing.T) {
	h, e := stubHandler(t, mediaproxy.ErrOverloaded)

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&fallback=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"),
		"fallback でも過負荷の応答はキャッシュさせない")
	assert.NotEmpty(t, rec.Body.Bytes())
	// RFC 9110 が `Retry-After` を定義しているのは 503 と 3xx。200 に
	// 乗せると中間装置の挙動が未定義になる。
	assert.Empty(t, rec.Header().Get("Retry-After"),
		"200 の応答に Retry-After を乗せない")
}

// 恒久的な失敗まで no-store にしてしまっていないこと。
//
// **片方だけ見ると回帰に気付けない。** `serveFallback` を一律 `no-store` に
// 変える修正は、過負荷のテストだけなら緑のまま通る。
func TestHandle_NotFoundFallbackKeepsShortCache(t *testing.T) {
	h, e := stubHandler(t, mediaproxy.ErrNotFound)

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&fallback=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "max-age=300", rec.Header().Get("Cache-Control"))
}

// 利用者の離脱を 500 に数えない (#3032)。
//
// **監視で本物の障害が埋もれる。** モバイル回線ではスクロールのたびに
// 未完了の画像要求が切られるので、ここを 500 にすると 5xx が常時立つ。
func TestHandle_ClientCancelIsNot500(t *testing.T) {
	h, e := stubHandler(t, context.Canceled)

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, statusClientClosedRequest, rec.Code)
}

// **`context.DeadlineExceeded` は 499 にしない (#3032 レビュー)。**
//
// `/proxy` のリクエスト context に deadline を付ける middleware は無いので、
// ここへ届く `DeadlineExceeded` は **mk-go 自身の 30 秒
// `httpClient.Timeout`** から来る。Go 1.26 では `Client.Timeout` 由来の
// エラーが `errors.Is(err, context.DeadlineExceeded)` を満たし、
// `fetchRemote` が `%w` でラップしているので、**リモート origin が body を
// 引き延ばしただけでここに来る**。499 に倒すと、そのサーバー側障害が 4xx
// にも 5xx にも出なくなり、しかも `?fallback` が無視される。
func TestHandle_ServerSideTimeoutIsNotTreatedAsClientCancel(t *testing.T) {
	h, e := stubHandler(t, context.DeadlineExceeded)

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotEqual(t, statusClientClosedRequest, rec.Code)

	// fallback が効くことも見る (499 だと本文ゼロで返っていた)。
	rec = doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&fallback=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
}

// fallback 付きでも利用者の離脱は 499 のまま。
//
// **ダミー画像を 200 で返すと、切れた接続に書こうとするうえ、実装として
// 「切断」と「処理失敗」の区別が消える。**
func TestHandle_ClientCancelIgnoresFallback(t *testing.T) {
	h, e := stubHandler(t, context.Canceled)

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&fallback=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, statusClientClosedRequest, rec.Code)
}

// リモート取得の失敗は 404 ではなく 502 で、長期キャッシュさせない (#3034)。
//
// **404 + `max-age=86400` で返していたのが元のバグ。** リモートの一時障害が
// CDN に「この画像は存在しない」として 1 日焼き付き、復旧しても壊れたままに
// なっていた (#2913 と同型)。
func TestHandle_UpstreamUnavailableIsBadGatewayWithShortCache(t *testing.T) {
	h, e := stubHandler(t, fmt.Errorf("%w: dial tcp: connection refused", mediaproxy.ErrUpstreamUnavailable))

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Equal(t, "max-age=300", rec.Header().Get("Cache-Control"))
	assert.NotEqual(t, "max-age=86400", rec.Header().Get("Cache-Control"),
		"リモートの一時障害を 1 日キャッシュさせている")
}

// **このテストが唯一検出するのは「502 分岐の中の `?fallback` だけを消す」変異。**
// 応答は generic 経路と byte 単位で同じ (200 + dummy PNG + `max-age=300`) なので、
// 分岐を丸ごと消す変異はここでは落ちない — そちらは
// TestHandle_UpstreamUnavailableIsBadGatewayWithShortCache と
// TestHandle_UpstreamTimeoutIsGatewayTimeout が落とす (実測)。
// 消すと `?fallback=1` で 502 が返るようになるので、残すこと。
func TestHandle_UpstreamUnavailableWithFallbackIsShortCached(t *testing.T) {
	h, e := stubHandler(t, fmt.Errorf("%w: dial tcp: connection refused", mediaproxy.ErrUpstreamUnavailable))

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&fallback=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Equal(t, "max-age=300", rec.Header().Get("Cache-Control"))
}

// 30 秒の httpClient.Timeout に当たったものは 504、それ以外が 502 (#3034)。
//
// **混ぜると監視で「繋がらない」と「遅い」が分離できない。**
func TestHandle_UpstreamTimeoutIsGatewayTimeout(t *testing.T) {
	h, e := stubHandler(t, fmt.Errorf("%w: %w", mediaproxy.ErrUpstreamUnavailable, context.DeadlineExceeded))

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusGatewayTimeout, rec.Code)
	assert.NotEqual(t, http.StatusBadGateway, rec.Code)
	assert.Equal(t, "max-age=300", rec.Header().Get("Cache-Control"))
}

// 取りに行けない URL は 400 + 長期キャッシュ (#3034)。
//
// **恒久的なので短期キャッシュにしない。** 5 分ごとに引き直しても結果は
// 変わらないし、監視で「相手が落ちている」と読まれてもいけない。
func TestHandle_UnfetchableURLIsBadRequestWithLongCache(t *testing.T) {
	h, e := stubHandler(t, mediaproxy.ErrBadRequest)

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=%2Fidenticon%2Falice",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "max-age=86400", rec.Header().Get("Cache-Control"))
	assert.NotEqual(t, http.StatusBadGateway, rec.Code)
}

// 本当に「無い」ときは従来どおり 404 + 1 日キャッシュのまま。
//
// **片側だけ見ると回帰に気付けない。** 「全部 502 にする」実装は上の 2 本なら
// 緑で通るが、消えた画像まで 5 分ごとに取りに行くことになる。
func TestHandle_NotFoundKeepsLongCache(t *testing.T) {
	h, e := stubHandler(t, mediaproxy.ErrNotFound)

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "max-age=86400", rec.Header().Get("Cache-Control"))
}

// 取得中の離脱は 502 ではなく 499。
//
// **`%w` を 2 つにしたので、離脱したエラーは `ErrUpstreamUnavailable` と
// `context.Canceled` の両方を満たす。** handler の判定順が入れ替わると、
// 利用者が自分で切っただけのものが 502 として監視に積み上がる。
func TestHandle_CancelDuringFetchWinsOverUpstreamUnavailable(t *testing.T) {
	h, e := stubHandler(t, fmt.Errorf("%w: %w", mediaproxy.ErrUpstreamUnavailable, context.Canceled))

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, statusClientClosedRequest, rec.Code)
	assert.NotEqual(t, http.StatusBadGateway, rec.Code)
}

// 結果が指定したキャッシュ方針を handler が尊重すること (#3035)。
//
// **service 側だけ直しても wire には出ない。** #3034 では実際にそれをやって、
// service に写像を足したのに handler に受け口が無く、doc の主張と食い違う
// 状態を作った。ここは必ず handler まで通して見る。
func TestHandle_ResultCacheControlOverridesDefault(t *testing.T) {
	h, e := stubHandler(t, nil)
	h.service = stubProxy{cacheControl: "max-age=300"}

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/preview.webp?preview=1&url=https%3A%2F%2Fremote.example%2Fv.mp4",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "max-age=300", rec.Header().Get("Cache-Control"))
	assert.NotContains(t, rec.Header().Get("Cache-Control"), "immutable",
		"一時的な失敗のダミー画像を 1 年 immutable で配っている")
}

// 指定が無ければ従来どおり 1 年 immutable。
//
// **片側だけ見ると回帰に気付けない。** 「常に短期」にする実装は上のテスト
// だけなら緑で通るが、正常な画像まで 5 分ごとに取り直すことになる。
func TestHandle_DefaultCacheControlIsUnchanged(t *testing.T) {
	h, e := stubHandler(t, nil)

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
}

// allowlist を引けなかったときは 403 + 1 日ではなく 503 + `no-store` (#3036)。
//
// **障害中の全 URL が同時に焼き付くのを防ぐ。** #3034 / #3035 と違い、
// こちらは 1 URL ずつではなく `sig` を持たない要求すべてが一斉に落ちる。
func TestHandle_AllowlistUnavailableIsServiceUnavailable(t *testing.T) {
	h, e := stubHandler(t, nil)
	h.service = stubProxy{authErr: fmt.Errorf("%w: db connection failed", mediaproxy.ErrAllowlistUnavailable)}

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"),
		"DB の瞬断を CDN に焼き付けている")
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
}

// **#3034 の同型テストとは射程が違う。** あちらは fallback の応答が generic
// 経路と完全に同じ (`max-age=300`) なので「分岐を丸ごと消す」変異を検出
// できないが、こちらは `no-store` で generic (`max-age=300`) と違うため
// **丸ごと削除も検出する** (実測)。#3034 のコメントをそのまま写さないこと。
func TestHandle_AllowlistUnavailableWithFallbackIsNotStored(t *testing.T) {
	h, e := stubHandler(t, nil)
	h.service = stubProxy{authErr: fmt.Errorf("%w: db connection failed", mediaproxy.ErrAllowlistUnavailable)}

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&fallback=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

// 本当に許可されていないときは従来どおり 403 + 1 日。
//
// **既存テストとほぼ重複している。** 「認可の失敗を全部 503 にする」変異も
// 「403 のまま `no-store` にする」変異も、既存の `TestHandle_UnauthorizedURL` /
// `TestHandle_CacheHeaders` が落とす (実測)。それでも残すのは、#3036 が
// **何を変えていないか**をこのファイル内で示すため — 503 側の 2 本と
// 並べて読めないと、片方だけ見た人が「認可の失敗は全部 503」と読む。
func TestHandle_UnauthorizedKeepsForbiddenAndLongCache(t *testing.T) {
	h, e := stubHandler(t, nil)
	h.service = stubProxy{authErr: mediaproxy.ErrUnauthorized}

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "max-age=86400", rec.Header().Get("Cache-Control"))
}

// 認可中の離脱は 503 ではなく 499 (#3036)。
func TestHandle_CancelDuringAuthorizeIsClientClosed(t *testing.T) {
	h, e := stubHandler(t, nil)
	h.service = stubProxy{authErr: fmt.Errorf("%w: %w", mediaproxy.ErrAllowlistUnavailable, context.Canceled)}

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, statusClientClosedRequest, rec.Code)
	assert.NotEqual(t, http.StatusServiceUnavailable, rec.Code)
}

// blockedErr builds the error shape `fetchRemote` actually produces.
//
// **`%w` を 2 つ使うのが要点。** stub を 1 つで作ると `errors.Unwrap` が
// 動いてしまい、本番では nil になる書き方を検出できない (2 周目で実測)。
func blockedErr() error {
	cause := fmt.Errorf("Get %q: %w", "http://10.0.0.1/a.png", safehttp.ErrSSRFBlocked)
	return fmt.Errorf("%w: %w", mediaproxy.ErrTargetBlocked, cause)
}

// SSRF ガードが遮断した要求は 403 + 5 分 (#3037)。
//
// **#3034 で service 側だけ分けて handler に受け口を作らず、500 に落ちる
// 状態を作った。** その失敗を繰り返さないために、写像と対でここに置く。
func TestHandle_BlockedTargetIsForbidden(t *testing.T) {
	h, e := stubHandler(t, blockedErr())

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=http%3A%2F%2F10.0.0.1%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.NotEqual(t, http.StatusBadGateway, rec.Code,
		"こちらが遮断した事実を相手の障害として返している")
	// **502 のときと同じ 5 分。** SSRF の可否は毎リクエストの DNS 解決で
	// 決まるので恒久的ではない — 1 日にすると #2913 (403 が 1 日キャッシュ
	// されてアイコンが 1 日壊れた) の窓を 288 倍に広げる。
	assert.Equal(t, "max-age=300", rec.Header().Get("Cache-Control"))
	assert.NotEqual(t, "max-age=86400", rec.Header().Get("Cache-Control"))
}

func TestHandle_BlockedTargetWithFallbackIsShortCached(t *testing.T) {
	h, e := stubHandler(t, blockedErr())

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&fallback=1&url=http%3A%2F%2F10.0.0.1%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	// **名乗っている性質を固定する。** これが無いと `no-store` に変える変異が
	// 素通りする (レビューで実測)。
	assert.Equal(t, "max-age=300", rec.Header().Get("Cache-Control"))
}

// 遮断の事実がログに残ること (#3037)。
//
// **`Authorize` 失敗の 403 と同じ status なので、ログが唯一の区別手段。**
// 合流させると監視で「allowlist に無い」と「private IP を遮断した」が
// 区別できない。Error ではなく Warn — 設定どおりに働いた結果で、運用者が
// 直すべき障害ではない。
func TestHandle_BlockedTargetIsLogged(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	h, e := stubHandler(t, blockedErr())
	doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=http%3A%2F%2F10.0.0.1%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	out := buf.String()
	// **`msg=` まで含めて固定する。** 部分一致だと `err` の中身に同じ文字列が
	// 入っているせいで、message を変える変異が素通りする (2 周目で実測)。
	assert.Contains(t, out, `msg="mediaproxy: blocked target"`)
	// **属性の形まで固定する。** `"err", err` に戻したことで err の文字列にも
	// URL が入るようになり、`"url", rawURL` を丸ごと落とす変異が素通りする
	// ようになっていた (3 周目で実測)。構造化ログのクエリは err の中身では
	// なく `url` キーを引くので、属性を固定する価値がある。
	assert.Contains(t, out, "url=http://10.0.0.1/a.png",
		"遮断した URL が url 属性に出ていない")
	assert.Contains(t, out, "level=WARN", "Error で出している (設定どおりの動作なので Warn)")
	// **原因が本番でも残ること。** `errors.Unwrap` を使うと `%w` 2 つの
	// error では nil になり、safehttp 側のメッセージが丸ごと消える。
	assert.Contains(t, out, "connection to private IP blocked",
		"遮断の理由がログから消えている")

	// **`Authorize` 失敗の 403 はログを出さない** (区別できること)。
	buf.Reset()
	h2, e2 := stubHandler(t, nil)
	h2.service = stubProxy{authErr: mediaproxy.ErrUnauthorized}
	doRequest(e2, h2, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=http%3A%2F%2F10.0.0.1%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})
	assert.NotContains(t, buf.String(), "blocked target",
		"allowlist 外の 403 が遮断として記録されている")
}

// リモートの一時障害は従来どおり 502 (#3037)。
//
// **既存テストと重複している。** 「全部 403 にする」変異は
// `TestHandle_PathBasedURL` / `...IsBadGatewayWithShortCache` /
// `...UpstreamTimeoutIsGatewayTimeout` の 3 本も落とす (実測)。それでも残すのは、
// #3037 が**何を変えていないか**を `ErrTargetBlocked` の 3 本と並べて読めるようにするため。
func TestHandle_UpstreamUnavailableIsNotForbidden(t *testing.T) {
	h, e := stubHandler(t, fmt.Errorf("%w: dial tcp: connection refused", mediaproxy.ErrUpstreamUnavailable))

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?avatar=1&url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "Mozilla/5.0"})

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Equal(t, "max-age=300", rec.Header().Get("Cache-Control"))
}

// Service が MediaProxy を満たしていること。
//
// **インターフェースを足したので、production の型がずれても handler の
// テストだけは緑で通る。** router.go の配線と同じ代入をここで固定する。
func TestServiceSatisfiesMediaProxy(t *testing.T) {
	svc := mediaproxy.NewService(
		"https://example.com", "Misskey/2026.5.4 (https://example.com)",
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{}},
		[]byte("test-secret"), testAllowedCIDRs,
	)
	var _ MediaProxy = svc
	require.NotNil(t, NewHandler(svc, &config.Config{}))
}
