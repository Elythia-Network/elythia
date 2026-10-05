package urlpreview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/misc/keyword"
	"github.com/elythia-network/elythia/internal/safehttp"
	"github.com/redis/go-redis/v9"
	"golang.org/x/net/html/charset"
)

var (
	ErrDisabled    = errors.New("urlpreview: feature disabled")
	ErrTooLarge    = errors.New("urlpreview: content too large")
	ErrFetchFailed = errors.New("urlpreview: fetch failed")
	// ErrPrivateIP is returned when the target host resolves to a private /
	// reserved IP. 互換性のため公開していた sentinel は維持しつつ、実体は
	// safehttp.ErrSSRFBlocked と同じ値にして AP / mediaproxy と統一する
	// (#638)。
	ErrPrivateIP = safehttp.ErrSSRFBlocked
)

const cachePrefix = "url-preview:"
const cacheTTL = 24 * time.Hour

// Config holds URL preview settings read from Meta.
type Config struct {
	Enabled              bool
	AllowRedirect        bool
	TimeoutMs            int
	MaxContentLength     int64
	RequireContentLength bool
	SummaryProxyURL      string
	UserAgent            string
}

// Fetcher fetches and parses URL previews with caching and SSRF protection.
type Fetcher struct {
	cfg         Config
	redis       *redis.Client
	keyPrefix   string // TS drop-in互換用 `<host>:` prefix
	client      *http.Client
	proxyClient *http.Client

	// sensitiveList は meta.urlPreviewSensitiveList を返す provider
	// (upstream 2026.7.0 #17635)。admin が随時変更するため、起動時
	// snapshot の Config ではなく都度評価する (metaRepo は cached なので
	// per-request コストは無視できる)。nil なら keyword 上書きを skip。
	sensitiveList func() []string
}

// SetSensitiveListProvider wires the live meta.urlPreviewSensitiveList
// source used to force Sensitive=true on keyword-matching preview URLs.
func (f *Fetcher) SetSensitiveListProvider(p func() []string) {
	f.sensitiveList = p
}

// SetHTTPClient replaces the HTTP client (for tests that need to bypass
// the SSRF dialer or use a custom transport).
func (f *Fetcher) SetHTTPClient(c *http.Client) {
	f.client = c
}

// NewFetcher creates a URL preview Fetcher.
//
// keyPrefix (通常は `cfg.Redis.KeyPrefix()` = `<host>:`) は TS 本家と同じ
// キー名前空間の下にキャッシュキーを置くために前置される。空文字列なら
// prefix 無し。
//
// allowedPrivateNetworks は SSRF guard で例外的に許可する CIDR (config の
// allowedPrivateNetworks をそのまま渡す)。transportOpts は forward proxy /
// outgoing address / address family などの outbound 設定で、AP fetch や
// media proxy と挙動を揃えるため `safehttp.WithProxy(...)` などを渡す
// (#638)。
func NewFetcher(cfg Config, rdb *redis.Client, keyPrefix string, allowedPrivateNetworks []string, transportOpts ...safehttp.Option) *Fetcher {
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	client := &http.Client{
		Timeout:   timeout,
		Transport: safehttp.NewSSRFSafeTransport(allowedPrivateNetworks, transportOpts...),
	}
	if !cfg.AllowRedirect {
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	return &Fetcher{
		cfg:       cfg,
		redis:     rdb,
		keyPrefix: keyPrefix,
		client:    client,
		// proxyClient は管理者が設定した summalyProxy への呼び出し用。
		// operator-trusted endpoint (社内 summaly や localhost に立てた
		// proxy) を指定するケースがあるため SSRF guard は適用しない。
		// ここに forward proxy を掛けると 127.0.0.1 への内向き呼び出しまで
		// proxy 経由になって壊れるので、明示的に裸の Client を使う。
		proxyClient: &http.Client{Timeout: timeout},
	}
}

// Fetch returns the URL preview, using Redis cache when available.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Result, error) {
	if !f.cfg.Enabled {
		return nil, ErrDisabled
	}

	// 外部summarizer proxyが設定されていればそちらに委譲する。
	if f.cfg.SummaryProxyURL != "" {
		result, err := f.fetchViaProxy(ctx, rawURL)
		if err != nil {
			return nil, err
		}
		// upstream は fetcher クロージャで直取得/プロキシ両経路を検証する。
		// プロキシは operator-trusted だが応答内容までは信頼しない
		// (javascript: 等の scheme 混入で frontend の href/iframe に流れる)。
		if err := validateResultSchemes(result); err != nil {
			return nil, err
		}
		f.applySensitiveList(result)
		return result, nil
	}

	// Redisキャッシュ確認
	cacheKey := f.keyPrefix + cachePrefix + hashURL(rawURL)
	if f.redis != nil {
		if cached, err := f.redis.Get(ctx, cacheKey).Bytes(); err == nil {
			var result Result
			// 旧バージョン期に検証前の値が入った可能性に備え、cache 読み出し
			// にも scheme 検証を掛ける (不正 entry は miss 扱いで再取得)。
			if json.Unmarshal(cached, &result) == nil && validateResultSchemes(&result) == nil {
				// sensitive 上書きは cache 読み出し後に評価する (upstream 同様、
				// リスト変更が cache 済み entry にも即時反映される)。
				f.applySensitiveList(&result)
				return &result, nil
			}
		}
	}

	result, err := f.fetchAndParse(ctx, rawURL)
	if err != nil {
		return nil, err
	}

	// upstream 2026.7.0: summaly 結果の url / player.url が http(s) 以外なら
	// 不正としてキャッシュせず失敗させる (javascript: 等の scheme 混入防止)。
	if err := validateResultSchemes(result); err != nil {
		return nil, err
	}

	// Redisキャッシュ保存 (keyword 由来の sensitive 上書き前の生値を保存し、
	// リスト変更が過去 entry へ遡って効くようにする)。
	if f.redis != nil {
		if data, err := json.Marshal(result); err == nil {
			f.redis.Set(ctx, cacheKey, data, cacheTTL)
		}
	}

	f.applySensitiveList(result)
	return result, nil
}

// applySensitiveList forces Sensitive=true when the preview URL matches
// meta.urlPreviewSensitiveList keywords (upstream UrlPreviewService)。
func (f *Fetcher) applySensitiveList(result *Result) {
	if result == nil || result.Sensitive || f.sensitiveList == nil {
		return
	}
	if keyword.IsKeyWordIncluded(result.URL, f.sensitiveList()) {
		result.Sensitive = true
	}
}

// validateResultSchemes rejects previews whose resolved url / player.url is
// not http(s) (upstream 2026.7.0 は cache 前に unsupported schema を弾く)、
// および非 http(s) の thumbnail / icon を落とす。
//
// upstream は thumbnail / icon を無条件に mediaProxy URL へ書き換えるので
// 非 http(s) 値がクライアントに出ることが構造上ありえないが、mk-go の
// `entity.ProxyMediaURL` は host を持たない URL (`javascript:…` / `data:…`) を
// proxy 対象外として素通しするため、ここで落とす必要がある。
func validateResultSchemes(result *Result) error {
	if !isHTTPScheme(result.URL) {
		return fmt.Errorf("%w: unsupported schema in result url", ErrFetchFailed)
	}
	if result.Player.URL != nil && *result.Player.URL != "" && !isHTTPScheme(*result.Player.URL) {
		return fmt.Errorf("%w: unsupported schema in player url", ErrFetchFailed)
	}
	// thumbnail / icon は preview 全体を落とすほどではないので、値だけ捨てる
	// (upstream も画像が壊れているだけで preview 自体は返す)。
	dropNonHTTP(&result.Thumbnail)
	dropNonHTTP(&result.Icon)
	return nil
}

// dropNonHTTP clears an optional URL field when it is not http(s).
func dropNonHTTP(field **string) {
	if *field != nil && **field != "" && !isHTTPScheme(**field) {
		*field = nil
	}
}

// isHTTPScheme reports whether the URL uses http(s)。
//
// upstream (`UrlPreviewService.ts`) は生文字列の case-sensitive な
// `startsWith('http://')` で判定するが、URL scheme は RFC 3986 上
// case-insensitive なので `HTTP://…` を返す summaly proxy / ページを
// 誤って弾かないよう mk-go は大文字小文字を無視する。許可方向の乖離で
// (upstream が弾くものを mk-go は通す)、非 http(s) の遮断力は同じ。
func isHTTPScheme(raw string) bool {
	lower := strings.ToLower(raw)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

func (f *Fetcher) fetchAndParse(ctx context.Context, rawURL string) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	ua := f.cfg.UserAgent
	if ua == "" {
		ua = "Misskey URL Preview"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,*/*;q=0.5")

	resp, err := f.client.Do(req)
	if err != nil {
		if errors.Is(err, ErrPrivateIP) {
			return nil, ErrPrivateIP
		}
		return nil, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: status %d", ErrFetchFailed, resp.StatusCode)
	}

	// Content-Lengthチェック
	if f.cfg.RequireContentLength {
		cl := resp.Header.Get("Content-Length")
		if cl == "" {
			return nil, ErrTooLarge
		}
	}
	if f.cfg.MaxContentLength > 0 {
		if cl, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil && cl > f.cfg.MaxContentLength {
			return nil, ErrTooLarge
		}
	}

	// HTMLのみ解析する。それ以外はURLだけ返す。
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") && !strings.Contains(ct, "application/xhtml") {
		return &Result{URL: rawURL, Player: PlayerResult{Allow: []string{}}}, nil
	}

	// charset.NewReader が Content-Type の charset と HTML <meta charset> を
	// 自動判定して UTF-8 に正規化する。Shift_JIS / EUC-JP / ISO-2022-JP の
	// 日本語ページで title / description が文字化けする drift を解消 (#942)。
	limited := io.LimitReader(resp.Body, f.cfg.MaxContentLength)
	body, err := charset.NewReader(limited, ct)
	if err != nil {
		// charset 判定不能 / unsupported encoding は raw bytes でフォールバック。
		// ParseHTML は UTF-8 仮定だが、ASCII / UTF-8 互換ページなら問題なく動く。
		body = limited
	}
	result := ParseHTML(body, rawURL)

	// oEmbed discovery: HTML 中に <link rel="alternate" type=
	// "application/json+oembed"> があれば追加で fetch して PlayerResult を
	// 埋める (#639)。失敗は preview 全体を壊さない (best-effort)。
	if result.oEmbedURL != "" {
		if player, err := f.fetchOEmbed(ctx, result.oEmbedURL); err == nil {
			result.Player = player
		}
	}
	return result, nil
}

// fetchViaProxy delegates to an external summarizer service.
//
// proxyは管理者が設定した信頼済みURLなのでSSRFチェックを適用しないが、
// rawURLはクエリパラメータとして安全にエスケープする。
func (f *Fetcher) fetchViaProxy(ctx context.Context, rawURL string) (*Result, error) {
	proxyURL := f.cfg.SummaryProxyURL + "?url=" + url.QueryEscape(rawURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	resp, err := f.proxyClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	defer resp.Body.Close()

	var result Result
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("%w: parse proxy response: %v", ErrFetchFailed, err)
	}
	return &result, nil
}

func hashURL(u string) string {
	h := sha256.Sum256([]byte(u))
	return hex.EncodeToString(h[:])
}
