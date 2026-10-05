// Package emojimeta fetches custom emoji metadata from the origin server.
//
// **AP では足りない** (#2698)。AP の Emoji tag が運ぶのは `name` / `icon` /
// `_misskey_license.freeText` だけで、カテゴリ・エイリアス・センシティブは入らない。
// 本番の実測でも、リモート絵文字 19,129 件のうち `category` / `aliases` /
// `isSensitive` は **1 件も** 連合で入っていなかった (`license` は 8,386 件)。
// インポート時にそれらを埋めるには相手の REST API を直接叩くしかない。
package emojimeta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/safehttp"
)

// ErrUnsupported is returned when the origin server has no per-name endpoint we
// can use.
//
// **Mastodon 系がこれに当たる。** per-name の endpoint が無く、1 件のために
// `/api/v1/custom_emojis` の全件 (fedibird.com で実測 2.1 MB / 6,060 件) を取ることに
// なる。しかも vanilla Mastodon が返すのは `shortcode` / `category` / `url` だけで、
// 得られるのは実質 `category` 1 項目。本番のリモート絵文字に占める Mastodon 系は
// 3.7% (717 / 19,129) なので、一覧取得もキャッシュ機構も作らない。呼び出し側は
// これを「取得できなかった」として扱い、手入力に倒すこと。
var ErrUnsupported = errors.New("emojimeta: origin has no per-name emoji endpoint")

// ErrNotFound is returned when the origin responded but has no such emoji.
var ErrNotFound = errors.New("emojimeta: no such emoji on origin")

const fetchTimeout = 10 * time.Second

// maxBodyBytes caps the response we read.
//
// Misskey の `/api/emoji` は 1 件 410 B (実測) なので 1 MiB あれば十分に余裕がある。
// **上限を置かないと相手が無限に流し込める** (取得先は絵文字の host = 相手が決める値)。
const maxBodyBytes = 1 << 20

// Meta is the subset of emoji metadata we can fill in from the origin.
//
// **ポインタで持つのは「取得できなかった」と「空だった」を区別するため。** nil は
// 未取得なので UI 側で既存値を残し、空文字は相手が空を返したことを意味する。
type Meta struct {
	Category    *string
	Aliases     []string
	License     *string
	IsSensitive *bool
}

// Fetcher retrieves emoji metadata from origin servers.
type Fetcher struct {
	client    *http.Client
	userAgent string
}

// NewFetcher builds a fetcher with an SSRF-safe transport.
//
// **ホストは相手が決める値**なので必ず safehttp を通す (`internal/safehttp`)。
// urlpreview / mediaproxy / remote_stats と同じ組み立て。
func NewFetcher(allowedPrivateNetworks []string, userAgent string, opts ...safehttp.Option) *Fetcher {
	return &Fetcher{
		client: &http.Client{
			Transport: safehttp.NewSSRFSafeTransport(allowedPrivateNetworks, opts...),
			Timeout:   fetchTimeout,
		},
		userAgent: userAgent,
	}
}

// NewFetcherWithClient is a test-only constructor.
func NewFetcherWithClient(c *http.Client) *Fetcher { return &Fetcher{client: c} }

// misskeyEmojiResponse mirrors the shape of Misskey's `GET /api/emoji`.
type misskeyEmojiResponse struct {
	Name        string   `json:"name"`
	Category    *string  `json:"category"`
	Aliases     []string `json:"aliases"`
	License     *string  `json:"license"`
	IsSensitive *bool    `json:"isSensitive"`
}

// SupportsHost reports whether the given software name has a per-name endpoint.
//
// 判定は `instance.softwareName` (nodeinfo 由来) で行う。**大文字混じりの値が
// 実在する** (`Iceshrimp.NET` など) ので、比較の前に lowercase する。
// 本番の実測では Misskey 系 (misskey / yojo-art / cherrypick / sharkey / sakurasato) が
// リモート絵文字の 88% を占める。
func SupportsHost(softwareName string) bool {
	switch strings.ToLower(strings.TrimSpace(softwareName)) {
	// **mk-go 自身を落とさないこと。** mk-go の nodeinfo は `software.name = "mk-go"`
	// (`internal/api/nodeinfo/handler.go`) で、`/api/emoji` は実装済み。落とすと
	// mk-go 同士のインポートが常に unsupported になる。
	case "misskey", "mk-go", "cherrypick", "sharkey", "yojo-art", "cluckey", "firefish", "iceshrimp":
		return true
	default:
		return false
	}
}

// Fetch retrieves metadata for `name` from `host`.
//
// softwareName は `instance.softwareName`。空や未知のときは ErrUnsupported を返す
// (**推測で叩かない** — 相手が per-name endpoint を持たないなら 404 が返るだけで、
// こちらが待つ時間と相手の負荷が無駄になる)。
func (f *Fetcher) Fetch(ctx context.Context, host, name, softwareName string) (*Meta, error) {
	if host == "" || name == "" {
		return nil, ErrNotFound
	}
	if !SupportsHost(softwareName) {
		return nil, ErrUnsupported
	}

	// **文字列連結で組まない。** `emoji.host` は今は `hostFromURI` 由来 (URL の
	// host、punycode 正規化済み) なので `/` や `@` は入らないが、連結だと由来が
	// 増えたときにここが最初に壊れる。`url.URL` に組ませて、不正な host は
	// 組み立ての時点で落とす。
	if strings.ContainsAny(host, "/?#@\\") {
		return nil, ErrNotFound
	}
	endpoint := (&url.URL{
		Scheme:   "https",
		Host:     host,
		Path:     "/api/emoji",
		RawQuery: url.Values{"name": {name}}.Encode(),
	}).String()
	return f.fetchFrom(ctx, endpoint, name)
}

// fetchFrom performs the request against an already-built endpoint.
//
// **URL の組み立てと分けてある。** `Fetch` は host から https:// を組むので、
// テストから httptest サーバーへ向けられない。ここを切り出すことで、応答の
// 解釈 (name の一致・本文上限・status) を実サーバー相手に検査できる。
func (f *Fetcher) fetchFrom(ctx context.Context, endpoint, name string) (*Meta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("emojimeta: build request: %w", err)
	}
	if f.userAgent != "" {
		req.Header.Set("User-Agent", f.userAgent)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("emojimeta: fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("emojimeta: unexpected status %d", resp.StatusCode)
	}

	var parsed misskeyEmojiResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("emojimeta: decode: %w", err)
	}

	// **name の一致を確かめる。** `?name=` を無視して別の絵文字を返す実装が
	// あった場合、取り違えた値でローカルの絵文字を作ることになる。
	if parsed.Name != "" && parsed.Name != name {
		return nil, ErrNotFound
	}

	return &Meta{
		Category:    parsed.Category,
		Aliases:     parsed.Aliases,
		License:     parsed.License,
		IsSensitive: parsed.IsSensitive,
	}, nil
}
