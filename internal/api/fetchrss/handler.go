// Package fetchrss implements the /api/fetch-rss endpoint, used by the
// RSS / RSSTicker widgets on the Misskey frontend. Fetches a remote RSS or
// Atom feed and translates it into the Misskey-compatible response shape
// produced by the upstream TS implementation (which wraps the rss-parser
// npm package).
package fetchrss

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/safehttp"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
	"github.com/mmcdole/gofeed"
	"golang.org/x/sync/singleflight"
)

// CacheSeconds matches the upstream Misskey `cacheSec: 60 * 3` declaration so
// the frontend's browser cache and any reverse-proxy caches behave the same
// against mk-go and the original TS backend.
const CacheSeconds = 60 * 3

// MaxBodyBytes caps the response read from the upstream feed server.
//
// mk-go は #2106 L27 で先行して 1 MiB に硬化していた。upstream も 2026.7.0 の
// GHSA hardening で `size: MAX_RESPONSE_SIZE (1 MiB)` を指定するようになり、
// 現在は完全一致 (乖離解消)。
const MaxBodyBytes int64 = 1 << 20

// FetchTimeout matches the upstream `timeout: 5000` ms used by Misskey TS.
const FetchTimeout = 5 * time.Second

// MaxURLLength caps the accepted feed URL length (upstream 2026.7.0
// MAX_URL_LENGTH = 8192).
const MaxURLLength = 8192

// MaxConcurrentRequests caps the number of distinct feed URLs being fetched
// at once (upstream 2026.7.0 MAX_CONCURRENT_REQUESTS = 32). 超過は
// FETCH_RSS_UNAVAILABLE (503)。同一 URL への合流 (dedup) は上限に数えない。
const MaxConcurrentRequests = 32

// DefaultCacheMaxEntries は in-memory cache が同時に保持するエントリ数の
// soft cap。攻撃者が大量の異なる URL を送り込むケースで無制限に成長しない
// よう、超過時は最古 expiresAt のエントリを 1 件 evict する。
//
// 1024 は「現実的な widget 設置数 × 同時 actor 数」を超えるが OOM を起こさない
// 値の経験則。TTL=180s なら 1 URL あたり数 KB 想定で max ~数 MB。
const DefaultCacheMaxEntries = 1024

// Handler serves /api/fetch-rss. The HTTP client must be wired with an
// SSRF-safe transport (see internal/server/outbound_http.go); the handler
// itself adds only request-shape validation and gofeed translation. Note
// that safehttp.NewSSRFSafeTransport applies the private-IP guard on every
// dial, so HTTP redirects to private IPs are also blocked — the handler
// can rely on the transport for redirect-chain SSRF defense.
//
// Server-side cache: 同 URL への hit は TTL 期間中 in-memory に保持された
// JSON body を直接書き戻し、upstream へのアクセスは行わない。同じ URL
// に対する concurrent miss は singleflight で 1 fetch に集約する。エラー
// レスポンスは cache しない (短期障害がある feed への retry を許す)。
type Handler struct {
	httpClient *http.Client
	userAgent  string
	parserPool *sync.Pool

	// Cache configuration.
	cacheTTL    time.Duration
	cacheMaxLen int

	// In-memory cache: URL → 既に packFeed 済みの JSON body。
	// hit path で json.Marshal を回避するため pre-serialized で保持する。
	cacheMu sync.Mutex
	cache   map[string]cacheEntry

	// singleflight: 同一 URL への concurrent fetch を 1 つに集約する。
	// upstream への thundering herd を防ぐため、cache miss 中に到着した
	// 後続リクエストは同じ Do 結果を共有する。
	sf singleflight.Group

	// inflight は「現在処理中の distinct URL → 参加リクエスト数」。upstream
	// 2026.7.0 の activeRequestCount 相当で、distinct URL 数が maxConcurrent
	// を超えたら新規 fetch を 503 で拒否する。既に in-flight な URL への
	// 合流は upstream 同様に上限へ数えない。
	inflightMu    sync.Mutex
	inflight      map[string]int
	maxConcurrent int

	// now は cacheGet / cachePut で使う clock。テストで stub する。
	now func() time.Time
}

// cacheEntry は in-memory cache の値。body は既に packFeed → json.Marshal を
// 通った JSON bytes で、hit 時はこれを直接 ResponseWriter に書き出す。
type cacheEntry struct {
	body      []byte
	expiresAt time.Time
}

// New builds a Handler bound to the given outbound HTTP client and User-Agent
// string. Pass a client whose Transport is safehttp.NewSSRFSafeTransport(...)
// so private IPs and non-http(s) schemes never leak through this endpoint.
// userAgent should normally be cfg.UserAgent so feed servers see the same
// `Elythia/<ver>` identification used everywhere else.
func New(httpClient *http.Client, userAgent string) *Handler {
	return &Handler{
		httpClient: httpClient,
		userAgent:  userAgent,
		// gofeed.Parser の goroutine 安全性は明記されていないため pool 経由で
		// 1 リクエスト 1 インスタンスに分離する。Pool reuse でアロケーションは
		// 実質ゼロに保つ。
		parserPool:    &sync.Pool{New: func() any { return gofeed.NewParser() }},
		cacheTTL:      time.Duration(CacheSeconds) * time.Second,
		cacheMaxLen:   DefaultCacheMaxEntries,
		cache:         make(map[string]cacheEntry),
		inflight:      make(map[string]int),
		maxConcurrent: MaxConcurrentRequests,
		now:           time.Now,
	}
}

// SetMaxConcurrentFetches overrides the distinct-URL concurrency cap.
// Intended for tests; 0 以下は無視。
func (h *Handler) SetMaxConcurrentFetches(n int) {
	if n > 0 {
		h.maxConcurrent = n
	}
}

// SetCacheTTL overrides the server-side cache TTL. Intended for tests; in
// production the default (CacheSeconds * time.Second) matches the
// Cache-Control header so client and server see the same freshness window.
func (h *Handler) SetCacheTTL(d time.Duration) {
	if d > 0 {
		h.cacheTTL = d
	}
}

// SetCacheMaxEntries overrides the soft cap on in-memory cache entries.
// 0 以下は無視 (default を維持)。
func (h *Handler) SetCacheMaxEntries(n int) {
	if n > 0 {
		h.cacheMaxLen = n
	}
}

// SetClock injects a deterministic clock so cache TTL paths can be tested
// without time.Sleep。
func (h *Handler) SetClock(now func() time.Time) {
	if now != nil {
		h.now = now
	}
}

// Fetch handles GET/POST /api/fetch-rss. The frontend RSS widget uses GET with
// a query string, so we accept both shapes.
//
// URL 検証と error 体系は upstream 2026.7.0 の GHSA hardening (normalizeUrl +
// INVALID_URL / FETCH_RSS_FAILED / FETCH_RSS_UNAVAILABLE) に揃える。
func (h *Handler) Fetch(c echo.Context) error {
	rawURL, provided, mistyped := h.extractURL(c)
	if mistyped {
		// object でない body (requireObjectBody が先に弾く) か、url が文字列で
		// ない body。本家は ajv の型違いで 400 にする。
		return apierr.JSONInvalidParam(c)
	}
	if !provided {
		// upstream は url 欠落を ajv schema validation の INVALID_PARAM (400)
		// で弾く (info に param/reason を含む)。param が存在して不正な場合は
		// 下の normalize で INVALID_URL。
		return c.JSON(http.StatusBadRequest, apierr.InvalidParamClient("url", "must have required property 'url'"))
	}

	key, ok := normalizeFeedURL(rawURL)
	if !ok {
		// upstream normalizeUrl: 空 / 8192 字超 / parse 失敗 / 非 http(s) /
		// userinfo 付きをすべて INVALID_URL (400) に丸める。
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_URL", "Invalid URL.", "89b7ee05-ccfc-4bdd-9b13-61172fd1e06c"))
	}

	// Cache hit fast path: TTL 内ならパースなしで JSON bytes をそのまま返す。
	if body, ok := h.cacheGet(key); ok {
		return writeCachedJSON(c, body)
	}

	// upstream 2026.7.0 の MAX_CONCURRENT_REQUESTS: distinct URL の同時 fetch
	// が上限を超えたら 503。既存 flight への合流は数えない (upstream の
	// inFlightRequests 共有と同じ)。
	if !h.inflightEnter(key) {
		return c.JSON(http.StatusServiceUnavailable, apierr.ErrorWithKind("FETCH_RSS_UNAVAILABLE", "RSS fetching is temporarily unavailable.", "91e6ff44-c63f-4725-9ad0-b7a40d7f7655", apierr.KindServer))
	}
	// 解放は「fetch 完了時点」で行いたい (レスポンス送信を上限内に含めると、
	// body を読まないクライアントが socket buffer を埋めるだけでスロットを
	// 無期限占有できる) が、singleflight は fn 内の panic を全 caller に再
	// panic させるため defer 無しだと panic 経路でスロットが恒久リークする。
	// OnceFunc で「早期解放 + panic-safe」を両立する。
	leaveOnce := sync.OnceFunc(func() { h.inflightLeave(key) })
	defer leaveOnce()

	// singleflight で同 URL への concurrent miss を 1 fetch に集約する。
	// 後続 caller は同じ result を共有 (upstream に thundering herd を流さない)。
	//
	// 重要: closure 内 fetch の context は caller の request context から切り離す。
	// Misskey TS の rss-parser 互換 timeout (5s) は維持しつつ、最初に sf.Do に
	// 入った caller の disconnect / cancel が後続の coalesced caller を巻き
	// 込まないようにする (Devin review #720 FLAG-1)。caller A の cancel で
	// upstream HTTP を中断すると、その時点で A と一緒に sf.Do を待っている
	// B/C/D も同じエラーを受け取って 502 になり、結果的に A が頻繁に切断する
	// クライアントだと健康な feed でも 502 が返り続ける。
	v, err, _ := h.sf.Do(key, func() (any, error) {
		// singleflight 入場時にキャッシュを再確認: 直前の coalesced 呼び出しが
		// 既に populate していれば再 fetch しない。
		if body, ok := h.cacheGet(key); ok {
			return body, nil
		}
		fetchCtx, cancel := context.WithTimeout(context.Background(), FetchTimeout)
		defer cancel()
		feed, ferr := h.fetchFeed(fetchCtx, key)
		if ferr != nil {
			return nil, ferr
		}
		// json.Encoder.Encode は trailing newline を付ける挙動なので、echo の
		// c.JSON 既定 (json.NewEncoder で書き出し) と body bytes が完全一致
		// する。cache hit / miss の response shape を 1 byte 単位で揃える。
		var buf bytes.Buffer
		if jerr := json.NewEncoder(&buf).Encode(packFeed(feed)); jerr != nil {
			return nil, jerr
		}
		body := buf.Bytes()
		// 成功時のみ cache に積む。エラーレスポンスは cache しない:
		// short-lived な upstream 障害から復旧した直後に古い 502 を引きずらない
		// ようにするのと、攻撃者がエラーを cache 占有に使うのを防ぐ。
		h.cachePut(key, body)
		return body, nil
	})
	// upstream は .finally() を fetch promise に付けており、serialize /
	// レスポンス送信は並行上限の外。ここで先に解放する。
	leaveOnce()
	if err != nil {
		// SSRF block / dial fail / parse fail はすべて upstream 2026.7.0 と同じ
		// FETCH_RSS_FAILED (422, kind server) に丸める。err.Error() を直接
		// client に返すと resolved IP や SSRF 文言が漏れるため static
		// メッセージに置き換え、詳細はサーバ側ログに残す。
		slog.WarnContext(c.Request().Context(), "fetch-rss upstream failed", "url", key, "err", err)
		return c.JSON(http.StatusUnprocessableEntity, apierr.ErrorWithKind("FETCH_RSS_FAILED", "Failed to fetch RSS.", "8db5d3d8-31d7-452f-b0cc-ca3b8925de12", apierr.KindServer))
	}
	return writeCachedJSON(c, v.([]byte))
}

// extractURL pulls the url parameter and reports whether it was provided at
// all (ajv の required 相当の判定に使う)。POST は upstream 同様 body だけを
// 見て、frontend の RSS widget が使う GET は query から取る。
//
// mistyped is true when a POST body could not be read as {url: string}; the
// caller answers it with INVALID_PARAM like upstream's ajv.
func (h *Handler) extractURL(c echo.Context) (rawURL string, provided, mistyped bool) {
	if c.Request().Method != http.MethodGet {
		// 本家は POST では body しか params にしない (ApiCallService.ts の
		// handleRequest)。bind に失敗したときや url が文字列でないときに
		// query の url へ流れると、本家が 400 にする要求を通してしまう (#3330)。
		var body struct {
			URL json.RawMessage `json:"url"`
		}
		if err := c.Bind(&body); err != nil {
			return "", false, true
		}
		if body.URL == nil {
			return "", false, false
		}
		var s string
		if string(body.URL) == "null" || json.Unmarshal(body.URL, &s) != nil {
			return "", false, true
		}
		return strings.TrimSpace(s), true, false
	}
	if qp := c.QueryParams(); len(qp["url"]) > 0 {
		return strings.TrimSpace(qp.Get("url")), true, false
	}
	return "", false, false
}

// normalizeFeedURL validates and canonicalizes the feed URL following
// upstream 2026.7.0 normalizeUrl: 空 / 8192 字超 / parse 不能 / 非 http(s) /
// userinfo 付きを拒否し、fragment を除去した canonical 形を返す。
//
// 加えて WHATWG URL (upstream の `new URL()`) が行う正規化のうち、host の
// 小文字化と default port の除去を再現する。これが無いと `example.com` /
// `EXAMPLE.com` / `example.com:80` が別キーになり、DNS は case-insensitive
// なので同一実体に対していくらでも cache / 並行スロットを食い潰せる。
func normalizeFeedURL(raw string) (string, bool) {
	if raw == "" || len(raw) > MaxURLLength {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	// Go の http.Client は URL の userinfo を Basic 認証ヘッダとして外部に
	// 送出するため、upstream 同様に拒否する (credential 転送防止)。
	if u.User != nil {
		return "", false
	}
	// host は case-insensitive。hostname だけ小文字化し port は保持する。
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && !isDefaultPort(u.Scheme, port) {
		// 非 canonical な数値表記 (`:08080`) も畳んでキー変種を防ぐ。
		if n, err := strconv.Atoi(port); err == nil {
			port = strconv.Itoa(n)
		}
		u.Host = net.JoinHostPort(host, port)
	} else {
		u.Host = host
		// IPv6 リテラルは JoinHostPort を通さないと角括弧が落ちる。
		if strings.Contains(host, ":") {
			u.Host = "[" + host + "]"
		}
	}
	// port だけの URL (`http://:80/x`) は Hostname() が空になる。WHATWG の
	// `new URL()` は throw するので、こちらも INVALID_URL に倒す (素通しすると
	// 壊れた cache key を作った上で 422 になり error shape が upstream とずれる)。
	if host == "" {
		return "", false
	}
	// WHATWG URL は空 path を "/" に正規化する。
	if u.Path == "" {
		u.Path = "/"
	}
	u.Fragment = ""
	u.RawFragment = ""
	return u.String(), true
}

// isDefaultPort reports whether port is the scheme's default (WHATWG URL は
// default port を href から落とす)。`:080` のような非 canonical 表記も同じ
// 番号として畳む (文字列比較だと port 表記を変えるだけでキー変種を無限に
// 作れてしまう)。
func isDefaultPort(scheme, port string) bool {
	n, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	return (scheme == "http" && n == 80) || (scheme == "https" && n == 443)
}

// inflightEnter registers the request against the concurrency cap. Returns
// false if starting a fetch for a new distinct URL would exceed the cap.
func (h *Handler) inflightEnter(key string) bool {
	h.inflightMu.Lock()
	defer h.inflightMu.Unlock()
	if h.inflight[key] == 0 && len(h.inflight) >= h.maxConcurrent {
		return false
	}
	h.inflight[key]++
	return true
}

func (h *Handler) inflightLeave(key string) {
	h.inflightMu.Lock()
	defer h.inflightMu.Unlock()
	if h.inflight[key] <= 1 {
		delete(h.inflight, key)
	} else {
		h.inflight[key]--
	}
}

// writeCachedJSON は echo.Context.JSON 相当の応答を pre-serialized body から
// 書き出す。Cache-Control / Content-Type を echo の既定挙動と揃えることで、
// cache hit / miss どちらの経路でもクライアントから見た response shape は
// 同一になる (TS upstream とも整合)。
func writeCachedJSON(c echo.Context, body []byte) error {
	resp := c.Response()
	// upstream fetch-rss.ts は cacheSec:180 + allowGet:true。ApiCallService は
	// `GET && cacheSec && !token && !user` のときだけ Cache-Control を付ける。
	// POST / 認証済み / raw token 付き GET には付けない (charts/bubble-game と同
	// convention、#2054)。token は HasRawToken (解決可否に依らない) で判定する。
	if c.Request().Method == http.MethodGet && middleware.GetUser(c) == nil && !middleware.HasRawToken(c) {
		resp.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", CacheSeconds))
	}
	resp.Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	resp.WriteHeader(http.StatusOK)
	_, err := resp.Write(body)
	return err
}

// cacheGet returns a cached body and true if a fresh entry exists for the
// key, else nil/false。Lazy expiration: 期限切れ entry は読んだタイミングで
// map から落とす (定期 sweeper を持たないので、再度同 URL が来た時に自然
// 整理される)。
func (h *Handler) cacheGet(key string) ([]byte, bool) {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	e, ok := h.cache[key]
	if !ok {
		return nil, false
	}
	if h.now().After(e.expiresAt) {
		delete(h.cache, key)
		return nil, false
	}
	return e.body, true
}

// cachePut stores the body keyed by url with TTL = h.cacheTTL。容量超過時は
// 最古 expiresAt の entry を 1 件 evict する (LRU ではなく LFRU 近似で十分)。
// O(N) sweep だが N <= cacheMaxLen で実用上問題なし。
func (h *Handler) cachePut(key string, body []byte) {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	if _, exists := h.cache[key]; !exists && len(h.cache) >= h.cacheMaxLen {
		h.evictOldestLocked()
	}
	h.cache[key] = cacheEntry{
		body:      body,
		expiresAt: h.now().Add(h.cacheTTL),
	}
}

// evictOldestLocked removes the cache entry with the earliest expiresAt.
// Caller must hold cacheMu。同点時は map iteration 順序のため非決定だが、
// 実害はない (どちらを落としても TTL 終了は秒オーダーで同じ)。
func (h *Handler) evictOldestLocked() {
	var oldestKey string
	var oldestT time.Time
	for k, v := range h.cache {
		if oldestKey == "" || v.expiresAt.Before(oldestT) {
			oldestKey = k
			oldestT = v.expiresAt
		}
	}
	if oldestKey != "" {
		delete(h.cache, oldestKey)
	}
}

// fetchFeed performs the GET + body cap + gofeed parse. Kept separate so
// tests can stub at a coarser layer.
func (h *Handler) fetchFeed(ctx context.Context, feedURL string) (*gofeed.Feed, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Misskey TS は `Accept: application/rss+xml, */*` を送る。Atom 専用 server
	// 互換のため */* も付ける。
	req.Header.Set("Accept", "application/rss+xml, */*")
	if h.userAgent != "" {
		// UA 必須の RSS 配信サーバ (Cloudflare 含む) で 403 にならないように
		// するため、他の outbound 経路と同じ Elythia/<ver> UA を送る。
		req.Header.Set("User-Agent", h.userAgent)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	// upstream 2026.7.0: リダイレクト追跡後の最終 URL の protocol を再検証する。
	// Go transport は非 http(s) への redirect を自力で拒否するが、明示チェックを
	// upstream に揃えて defense-in-depth にする。
	if resp.Request != nil && resp.Request.URL != nil {
		if s := resp.Request.URL.Scheme; s != "http" && s != "https" {
			return nil, fmt.Errorf("invalid final URL protocol: %s", s)
		}
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	body, err := safehttp.ReadAllLimit(resp.Body, MaxBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	parser := h.parserPool.Get().(*gofeed.Parser)
	defer h.parserPool.Put(parser)

	feed, err := parser.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse feed: %w", err)
	}
	if feed == nil {
		return nil, errors.New("parser returned nil feed")
	}
	return feed, nil
}

// packFeed maps gofeed.Feed onto the Misskey-compatible response shape. Empty
// fields are omitted entirely (Misskey TS marks them `optional: true` and
// rss-parser leaves them undefined). `items` is always present as an array,
// matching the `optional: false` declaration in the upstream meta.
func packFeed(feed *gofeed.Feed) map[string]any {
	out := map[string]any{}
	if feed.Title != "" {
		out["title"] = feed.Title
	}
	if feed.Description != "" {
		out["description"] = feed.Description
	}
	if feed.Link != "" {
		out["link"] = feed.Link
	}
	if feed.FeedLink != "" {
		out["feedUrl"] = feed.FeedLink
	}
	if img := packImage(feed.Image); img != nil {
		out["image"] = img
	}
	if itunes := packITunes(feed); itunes != nil {
		out["itunes"] = itunes
	}

	items := make([]map[string]any, 0, len(feed.Items))
	for _, it := range feed.Items {
		items = append(items, packItem(it))
	}
	out["items"] = items

	// upstream は RFC5005 の atom:link rel=self/first/next/last/prev を
	// paginationLinks (optional) として返すが、gofeed の universal Feed は
	// link の rel 属性を unified model に surface しない (Feed.Links は href のみ)。
	// 取得には atom 専用 re-parse が必要だが、(1) optional かつ paging を advertise
	// する feed は稀、(2) drop-in frontend は paginationLinks を消費しない
	// (misskey-js autogen 型に存在するのみ) ため、本フィールドは意図的に omit する
	// (#1778)。

	return out
}

func packImage(img *gofeed.Image) map[string]any {
	if img == nil || img.URL == "" {
		return nil
	}
	out := map[string]any{"url": img.URL}
	if img.Title != "" {
		out["title"] = img.Title
	}
	return out
}

func packITunes(feed *gofeed.Feed) map[string]any {
	ext := feed.ITunesExt
	if ext == nil {
		return nil
	}
	out := map[string]any{}
	if ext.Image != "" {
		out["image"] = ext.Image
	}
	if ext.Author != "" {
		out["author"] = ext.Author
	}
	if ext.Summary != "" {
		out["summary"] = ext.Summary
	}
	if ext.Explicit != "" {
		out["explicit"] = ext.Explicit
	}
	if len(ext.Categories) > 0 {
		// gofeed の Categories は階層 ([{Text, Subcategory}]) だが、Misskey は
		// flat string[] を期待するので flatten する。
		cats := make([]string, 0, len(ext.Categories))
		for _, cat := range ext.Categories {
			if cat == nil || cat.Text == "" {
				continue
			}
			cats = append(cats, cat.Text)
			if cat.Subcategory != nil && cat.Subcategory.Text != "" {
				cats = append(cats, cat.Subcategory.Text)
			}
		}
		if len(cats) > 0 {
			out["categories"] = cats
		}
	}
	if ext.Keywords != "" {
		// gofeed は keywords を生のまま string で持つ (RSS itunes は CSV)。
		// Misskey schema は array なので rss-parser 互換に split + trim する。
		parts := strings.Split(ext.Keywords, ",")
		kws := make([]string, 0, len(parts))
		for _, p := range parts {
			if s := strings.TrimSpace(p); s != "" {
				kws = append(kws, s)
			}
		}
		if len(kws) > 0 {
			out["keywords"] = kws
		}
	}
	if ext.Owner != nil {
		owner := map[string]any{}
		if ext.Owner.Name != "" {
			owner["name"] = ext.Owner.Name
		}
		if ext.Owner.Email != "" {
			owner["email"] = ext.Owner.Email
		}
		if len(owner) > 0 {
			out["owner"] = owner
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func packItem(it *gofeed.Item) map[string]any {
	out := map[string]any{}
	if it.Title != "" {
		out["title"] = it.Title
	}
	if it.Link != "" {
		out["link"] = it.Link
	}
	if it.GUID != "" {
		out["guid"] = it.GUID
	}
	if it.Published != "" {
		out["pubDate"] = it.Published
	}
	if t := it.PublishedParsed; t != nil {
		out["isoDate"] = t.UTC().Format(time.RFC3339Nano)
	} else if t := it.UpdatedParsed; t != nil {
		// Atom feed では published より updated しか持たないことが多い。
		// rss-parser も isoDate fallback として updated を使う。
		out["isoDate"] = t.UTC().Format(time.RFC3339Nano)
	}
	if creator := itemCreator(it); creator != "" {
		out["creator"] = creator
	}
	if it.Description != "" {
		out["summary"] = it.Description
	}
	if it.Content != "" {
		out["content"] = it.Content
	}
	if snippet := contentSnippet(it); snippet != "" {
		out["contentSnippet"] = snippet
	}
	if len(it.Categories) > 0 {
		out["categories"] = it.Categories
	}
	if encl := packEnclosure(it.Enclosures); encl != nil {
		out["enclosure"] = encl
	}
	return out
}

func itemCreator(it *gofeed.Item) string {
	// Atom は <author><name>...</name></author>、RSS 2.0 は <dc:creator>...</dc:creator>
	// が一般的。両系統を rss-parser と同じ優先順で見る。
	if len(it.Authors) > 0 && it.Authors[0] != nil && it.Authors[0].Name != "" {
		return it.Authors[0].Name
	}
	if it.Author != nil && it.Author.Name != "" {
		return it.Author.Name
	}
	if it.DublinCoreExt != nil && len(it.DublinCoreExt.Creator) > 0 {
		return it.DublinCoreExt.Creator[0]
	}
	return ""
}

func packEnclosure(encs []*gofeed.Enclosure) map[string]any {
	if len(encs) == 0 {
		return nil
	}
	e := encs[0]
	if e == nil || e.URL == "" {
		return nil
	}
	out := map[string]any{"url": e.URL}
	if e.Type != "" {
		out["type"] = e.Type
	}
	if e.Length != "" {
		// rss-parser は length を Number に変換するため schema は number。
		// gofeed は string で持つので int 変換、失敗時は省略する。
		if n, err := strconv.ParseInt(e.Length, 10, 64); err == nil {
			out["length"] = n
		}
	}
	return out
}

var (
	htmlTagRE  = regexp.MustCompile(`<[^>]*>`)
	whitespace = regexp.MustCompile(`\s+`)
)

// contentSnippet mirrors rss-parser の contentSnippet 算出: content または
// description から HTML タグを除去し、whitespace を 1 個に正規化、前後 trim。
// frontend MFM レンダラに直接渡されることはないが、ticker のテキスト表示で
// 使われる場合があるため形だけ揃える。
func contentSnippet(it *gofeed.Item) string {
	src := it.Content
	if src == "" {
		src = it.Description
	}
	if src == "" {
		return ""
	}
	stripped := htmlTagRE.ReplaceAllString(src, "")
	stripped = whitespace.ReplaceAllString(stripped, " ")
	return strings.TrimSpace(stripped)
}
