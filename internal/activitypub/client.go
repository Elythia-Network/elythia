package activitypub

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/elythia-network/elythia/internal/safehttp"
)

// MaxBodyBytes caps the response body size for FetchJSON / FetchUnsigned to
// prevent memory exhaustion via attacker-controlled remote AP servers (#323).
const MaxBodyBytes = safehttp.DefaultAPBodyLimit

// StatusError carries the HTTP status from a failed Fetch* call so callers
// can react to specific codes (e.g. retry with signed GET on 401/403 for
// authorized-fetch peers like IceShrimp.NET, #419)。
type StatusError struct {
	StatusCode int
	Status     string
	URL        string
}

// ErrInvalidAPContentType is returned by the AP-object fetch paths
// (FetchJSON / FetchUnsigned) when the response Content-Type is not an
// ActivityPub media type. Mirrors upstream validateContentTypeSetAsActivityPub
// (#1828): only `application/activity+json` (any params) or
// `application/ld+json; ...` carrying the ActivityStreams profile is accepted;
// a missing Content-Type is rejected too. これにより誤設定 proxy の HTML
// エラーページや plain JSON API レスポンスを AP object として unmarshal するのを
// 防ぐ。
var ErrInvalidAPContentType = errors.New("invalid AP content-type")

// validateAPContentType replicates upstream validateContentTypeSetAsActivityPub
// (core/activitypub/misc/validator.ts)。getActivityJson (unsigned) / signedGet
// 両経路で必ず適用される本家の防御層を移植したもの。
func validateAPContentType(resp *http.Response) error {
	// 本家同様に lowercase 化のみで trim はしない (startsWith 判定を厳密に揃える)。
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if ct == "" {
		return ErrInvalidAPContentType
	}
	if strings.HasPrefix(ct, "application/activity+json") {
		return nil
	}
	if strings.HasPrefix(ct, "application/ld+json;") && strings.Contains(ct, ContextURL) {
		return nil
	}
	return ErrInvalidAPContentType
}

// Error returns "unexpected status: NNN <text>" — same shape as the legacy
// errors.New("unexpected status: ...") so existing log readers keep working.
func (e *StatusError) Error() string {
	return "unexpected status: " + e.Status
}

// MaxHTMLBodyBytes caps the response body size for FetchHTML. Landing pages
// of real Misskey / Mastodon 等は inline JS/CSS が多く1MiB (AP payload想定)
// だと超えることが多い (#351のDevin指摘)。SPAバンドル込みでも 5MiB あれば
// 実用上 icon 抽出成功率が上がる。AP JSON 側の safety cap はそのまま。
const MaxHTMLBodyBytes = 5 * 1024 * 1024

// Client is a thin wrapper around http.Client that signs outgoing AP requests.
type Client struct {
	httpClient *http.Client
	userAgent  string
}

// NewClient constructs a Client. Pass nil for httpClient to use http.DefaultClient.
func NewClient(httpClient *http.Client, userAgent string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{httpClient: httpClient, userAgent: userAgent}
}

// DisableRedirect configures the HTTP client to reject all redirects.
// meta.allowExternalApRedirect が false のとき呼ぶ。外部 AP サーバーが
// 30x レスポンスを返した場合にオープンリダイレクト攻撃を防ぐ。
func (c *Client) DisableRedirect() {
	c.httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
}

// PostSigned sends a signed POST containing body to url, signed with key.
// 戻り値は呼び出し側で Body.Close() すること。
func (c *Client) PostSigned(url string, body []byte, key *PrivateKey) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", MimeType)
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	digest := SHA256Digest(body)
	if err := SignRequest(req, key, digest, []string{"(request-target)", "date", "host", "digest"}); err != nil {
		return nil, err
	}
	return c.httpClient.Do(req)
}

// GetSigned sends a signed GET to url. acceptOverride may be empty to use the
// default activity+json accept header.
func (c *Client) GetSigned(url string, key *PrivateKey, acceptOverride string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if acceptOverride == "" {
		req.Header.Set("Accept", MimeType+`, `+LDMimeType)
	} else {
		req.Header.Set("Accept", acceptOverride)
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if err := SignRequest(req, key, "", []string{"(request-target)", "date", "host"}); err != nil {
		return nil, err
	}
	return c.httpClient.Do(req)
}

// finalURLOf returns the response's final request URL (after redirects). Go's
// http client points resp.Request at the last request in the redirect chain,
// so this is the URL the body was actually served from — used to bind a fetched
// AP object's id host to the host that served it (#1820)。fallback は要求 URL。
func finalURLOf(resp *http.Response, fallback string) string {
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		return resp.Request.URL.String()
	}
	return fallback
}

// FetchJSONWithURL is FetchJSON but also returns the final response URL (after
// redirects) so callers can verify the fetched object's id host against the
// host that served it (#1820)。
func (c *Client) FetchJSONWithURL(url string, key *PrivateKey) ([]byte, string, error) {
	resp, err := c.GetSigned(url, key, "")
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		drainBody(resp)
		return nil, "", &StatusError{StatusCode: resp.StatusCode, Status: resp.Status, URL: url}
	}
	if err := validateAPContentType(resp); err != nil {
		drainBody(resp)
		return nil, "", err
	}
	body, rerr := safehttp.ReadAllLimit(resp.Body, MaxBodyBytes)
	return body, finalURLOf(resp, url), rerr
}

// FetchJSON performs a signed GET and returns the response body. Non-2xx
// responses produce a *StatusError so callers can branch on status (e.g.
// authorized-fetch fallback on 401/403, #419)。
func (c *Client) FetchJSON(url string, key *PrivateKey) ([]byte, error) {
	body, _, err := c.FetchJSONWithURL(url, key)
	return body, err
}

// FetchUnsignedWithURL is FetchUnsigned but also returns the final response URL
// (after redirects) for object-host verification (#1820)。
func (c *Client) FetchUnsignedWithURL(url string) ([]byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", MimeType+`, `+LDMimeType)
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		drainBody(resp)
		return nil, "", &StatusError{StatusCode: resp.StatusCode, Status: resp.Status, URL: url}
	}
	if err := validateAPContentType(resp); err != nil {
		drainBody(resp)
		return nil, "", err
	}
	body, rerr := safehttp.ReadAllLimit(resp.Body, MaxBodyBytes)
	return body, finalURLOf(resp, url), rerr
}

// FetchUnsigned performs a plain GET without HTTP signing. 多くのAPサーバーは
// アクター取得を未署名で許可するため、resolver の初回 fetch 用に使う。
// Non-2xx responses produce a *StatusError so callers can branch on status
// (e.g. authorized-fetch fallback on 401/403, #419)。
func (c *Client) FetchUnsigned(url string) ([]byte, error) {
	body, _, err := c.FetchUnsignedWithURL(url)
	return body, err
}

// FetchUnsignedJSON performs an unsigned GET with `Accept: application/json, */*`.
// Used for non-AP discovery endpoints that speak plain JSON (notably
// `/.well-known/nodeinfo` and the nodeinfo 2.x documents themselves).
//
// Sending the AP-only Accept (`application/activity+json, application/ld+json`)
// works for Misskey TS but is rejected by stricter implementations like
// Iceshrimp.NET, which return a 406 Not Acceptable JSON envelope (#474).
// Splitting this into a separate method keeps FetchUnsigned (= AP object
// fetch) free of fallback logic and makes the request semantics explicit
// at the call site.
func (c *Client) FetchUnsignedJSON(url string) ([]byte, error) {
	body, _, err := c.FetchUnsignedJSONWithURL(url)
	return body, err
}

// FetchUnsignedJSONWithURL is FetchUnsignedJSON but also returns the final
// response URL (after redirects).
//
// **nodeinfo の取得元を呼び出し側が縛れるようにするため。** discovery が返す
// `links[].href` の host を検証しても、client が redirect を追従するなら
// **302 一回で任意の host へ飛べる** ので、href の検証だけでは足りない。
//
// **飛び先そのものは止めない。** host を跨ぐ redirect を `CheckRedirect` で
// 弾く形も試したが、(a) `.well-known/*` を別 host へ委譲する構成を落とす、
// (b) `meta.allowExternalApRedirect` の設定を上書きしてしまう、(c) hop ごとに
// 方針を分けると配線を取り違えてもテストで気付けない、が重なった。
// **呼び出し側が最終 URL を見て「読み戻すか」を決める**形にしてある。残る面は
// 「任意の public host へ GET が出る」ことだけで、到達先は SSRF-safe transport
// が private IP を落とす。
func (c *Client) FetchUnsignedJSONWithURL(url string) ([]byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json, */*")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		drainBody(resp)
		return nil, "", &StatusError{StatusCode: resp.StatusCode, Status: resp.Status, URL: url}
	}
	body, rerr := safehttp.ReadAllLimit(resp.Body, MaxBodyBytes)
	return body, finalURLOf(resp, url), rerr
}

// drainBodyLimit caps how many bytes `drainBody` is willing to read from
// a non-2xx response before giving up on connection reuse. Most error
// payloads (Misskey/Mastodon JSON error envelope, plain-text 4xx page) fit
// in well under 16 KiB, so 64 KiB leaves margin for verbose stack traces
// without giving an adversarial peer a 1 MiB read budget per error
// response (#419 Devin review)。
const drainBodyLimit = 64 * 1024

// drainBody discards remaining bytes (up to drainBodyLimit) on a non-2xx
// response so the underlying TCP connection can be reused by the http
// transport pool。authorized-fetch fallback (#419) で signed→unsigned の
// 二段階 GET を同じ host に投げる際の connection reuse 効率を上げる。
func drainBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainBodyLimit))
}

// FetchHTML performs a plain GET with Accept: text/html. リモートインスタンスの
// トップページを取得して <link rel="icon"> 等を抽出するための用途を想定。
// 同じhttpClient (SSRF guard / timeout / redirect policy) を使うので nodeinfo
// 取得と同水準の安全策が効く。
func (c *Client) FetchHTML(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.5")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("unexpected status: " + resp.Status)
	}
	// Content-Type が明示的に text/html(または xhtml)以外なら 5 MiB まで
	// 読み込まず早期 error。相手が JSON / binaryを返すケースで帯域と
	// メモリを無駄にしない (Devin #4 指摘)。Content-Type 未設定は許容する
	// (一部の古いサーバーが含めないため)。
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		lower := strings.ToLower(ct)
		if !strings.Contains(lower, "text/html") && !strings.Contains(lower, "application/xhtml") {
			return nil, errors.New("unexpected content-type: " + ct)
		}
	}
	return safehttp.ReadAllLimit(resp.Body, MaxHTMLBodyBytes)
}
