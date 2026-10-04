package activitypub

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/shiroha-a/mk/internal/safehttp"
	"golang.org/x/sync/singleflight"
)

// WebFingerLink models a single link entry in an RFC 7033 response.
type WebFingerLink struct {
	Rel      string `json:"rel"`
	Type     string `json:"type,omitempty"`
	Href     string `json:"href,omitempty"`
	Template string `json:"template,omitempty"`
}

// WebFingerResponse models the minimal subset of an RFC 7033 WebFinger
// response needed to discover remote actor URIs.
type WebFingerResponse struct {
	Subject string          `json:"subject"`
	Links   []WebFingerLink `json:"links"`
}

// WebFingerClient performs outbound WebFinger queries used to discover remote
// ActivityPub actor URIs from `(username, host)` pairs.
type WebFingerClient struct {
	httpClient *http.Client
	userAgent  string
	// endpointOverride is used by tests to redirect the default
	// https://<host>/.well-known/webfinger URL to an httptest server.
	endpointOverride func(host string) string
	// lookupGroup は同一 (username, host) への並行 LookupActorURI 呼び出しを
	// 1 つの HTTP リクエストに collapse する (#300 3-7)。
	lookupGroup singleflight.Group
}

// NewWebFingerClient constructs a WebFingerClient. Pass nil for httpClient to
// get a fresh `&http.Client{}` dedicated to WebFinger.
//
// http.DefaultClient をデフォルトにしないのは、別所で `DisableRedirect()` 等で
// グローバル DefaultClient が汚染されるのを拾うのを防ぐため。WebFinger は
// RFC 7033 §4.2 で redirect を追う必要があるので分離しておきたい。
func NewWebFingerClient(httpClient *http.Client, userAgent string) *WebFingerClient {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &WebFingerClient{httpClient: httpClient, userAgent: userAgent}
}

// SetEndpointOverride replaces the default `https://<host>/.well-known/webfinger`
// URL with the function's return value. Intended for tests only.
func (c *WebFingerClient) SetEndpointOverride(fn func(host string) string) {
	c.endpointOverride = fn
}

// LookupActorURI performs a WebFinger lookup for `acct:<username>@<host>` and
// returns the href of the first `rel="self"` link whose type matches an
// ActivityPub-compatible content type (application/activity+json or
// application/ld+json).
//
// 呼び出し側は返却 URI を activitypub.Resolver.ResolveActor に渡すことで
// リモート actor を取り込める。
func (c *WebFingerClient) LookupActorURI(username, host string) (string, error) {
	if username == "" || host == "" {
		return "", errors.New("webfinger: username and host are required")
	}
	// **host は無検証で URL に連結されていた。** `endpoint` は
	// `"https://" + host + "/.well-known/webfinger"` を組むだけなので、
	// `a.example/x` ならパス注入、`a.example@b.example` なら url.Parse の
	// host が `b.example` になって authority がすり替わる。ここに来る host は
	// `POST /api/users/show` (host 指定) のような**未認証で叩ける経路**から渡るので、
	// 呼び出し側の `idnhost.Puny` (punycode プロファイル。ASCII は素通し) を
	// 唯一の砦にできない。
	//
	// 規則は `internal/core/federation/remote_stats.go` の `isValidHost` と
	// 同じ (あちらのコメントは「webfinger 経由で sanitize されるはず」と
	// 書いているが、その前提がここで初めて成立する)。パッケージを跨いで
	// 共有すると `activitypub` → `core` の逆向き依存になるので規則だけ揃える。
	if !isPlainHost(host) {
		return "", fmt.Errorf("webfinger: invalid host %q", host)
	}
	// 同一 acct への並行 lookup は singleflight で collapse (#300 3-7)。
	// inbox / 検索画面で同じ remote handle が連続して resolve されるケース
	// で WebFinger HTTP fan-out を抑える。
	resource := "acct:" + username + "@" + host
	v, err, _ := c.lookupGroup.Do(resource, func() (any, error) {
		return c.lookupActorURIOnce(host, resource)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// lookupActorURIOnce is the body of LookupActorURI, invoked once per
// resource by singleflight.Do.
func (c *WebFingerClient) lookupActorURIOnce(host, resource string) (string, error) {
	endpoint := c.endpoint(host) + "?resource=" + url.QueryEscape(resource)

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("webfinger: build request: %w", err)
	}
	// RFC 7033 §4.2 では application/jrd+json が優先、application/json は互換
	// として受け入れるサーバーが多い。両方を提示しておく。
	req.Header.Set("Accept", "application/jrd+json, application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("webfinger: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("webfinger: unexpected status %d", resp.StatusCode)
	}
	body, err := safehttp.ReadAllLimit(resp.Body, MaxBodyBytes)
	if err != nil {
		return "", fmt.Errorf("webfinger: read body: %w", err)
	}
	var wf WebFingerResponse
	if err := json.Unmarshal(body, &wf); err != nil {
		return "", fmt.Errorf("webfinger: parse response: %w", err)
	}
	for _, link := range wf.Links {
		if link.Rel != "self" {
			continue
		}
		if link.Href == "" {
			continue
		}
		// **href は相手サーバーが自由に決められる値**で、そのまま
		// `ResolveActor` へ渡る。http(s) 以外の scheme を弾いておく
		// (`internal/core/federation` の `remoteNoteURL` と同じ方針で、
		// scheme は RFC 3986 に従い case-insensitive に見る)。
		if !isHTTPScheme(link.Href) {
			continue
		}
		if isActivityPubLinkType(link.Type) {
			return link.Href, nil
		}
	}
	return "", errors.New("webfinger: no self link with ActivityPub type")
}

// endpoint returns the WebFinger endpoint URL for a host. The default is
// `https://<host>/.well-known/webfinger`. テストから httptest server に向けた
// い場合のみ SetEndpointOverride で差し替える。
func (c *WebFingerClient) endpoint(host string) string {
	if c.endpointOverride != nil {
		if o := c.endpointOverride(host); o != "" {
			return o
		}
	}
	return "https://" + host + "/.well-known/webfinger"
}

// isPlainHost reports whether host is a bare hostname (optionally with a
// port) that can be concatenated into a URL without changing its meaning.
//
// url.Parse に解かせて、入力がまるごと authority に収まっていることを確かめる。
// `/` `?` `#` `@` 空白などが混ざっていると Host / Path が input と食い違うので
// 落ちる。自前で文字集合を列挙すると必ず取りこぼす。
//
// **port は許す。** `example.com:3000` のような host は fediverse に実在し、
// mk-go も `idnhost.Puny` が port 付きを通す前提で書かれている。ここで
// 落とすと、そうした相手との連合が解決できなくなる (upstream WebfingerService
// も acct の host 部分をそのまま使うので port を許す)。
func isPlainHost(host string) bool {
	if host == "" {
		return false
	}
	parsed, err := url.Parse("https://" + host + "/")
	if err != nil {
		return false
	}
	return parsed.Host == host && parsed.Path == "/" && parsed.User == nil &&
		parsed.RawQuery == "" && parsed.Fragment == ""
}

// isHTTPScheme reports whether raw uses the http or https scheme.
// scheme は RFC 3986 に従い case-insensitive に判定する。
func isHTTPScheme(raw string) bool {
	lower := strings.ToLower(raw)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// isActivityPubLinkType reports whether t is an ActivityPub-compatible link
// type. Matches `application/activity+json` と `application/ld+json` のベース
// 部分 (profile パラメータ付き含む)。
func isActivityPubLinkType(t string) bool {
	base := strings.TrimSpace(strings.SplitN(t, ";", 2)[0])
	return base == MimeType || base == "application/ld+json"
}
