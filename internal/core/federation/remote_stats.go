package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/safehttp"
	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/sync/singleflight"
)

// RemoteUserStats represents counts fetched from a remote Misskey-compatible
// instance via /api/users/show.
type RemoteUserStats struct {
	NotesCount     int
	FollowersCount int
	FollowingCount int
}

// remoteStatsTTL は positive cache 有効期間 (= remote が valid response を返した
// 場合)。counter は数分単位で動くが、毎 request 取りに行くと remote 負荷が
// 大きいので 1 時間で fold (#943)。
const remoteStatsTTL = 1 * time.Hour

// remoteStatsNegativeTTL は negative cache 有効期間 (= remote が 4xx / timeout /
// malformed JSON を返した場合)。一時的な downtime / Mastodon のような互換性無し
// instance を 1h 単位で負キャッシュすると stale 期間が長過ぎるため 5 分に短縮
// (#943 review)。
const remoteStatsNegativeTTL = 5 * time.Minute

// remoteCreatedAtHostDownTTL は、FetchAccountCreatedAt で時間切れか接続失敗に
// なったホストへ取りに行かない期間 (#3465)。404 や別人の応答では止めない。
const remoteCreatedAtHostDownTTL = 5 * time.Minute

// remoteStatsTimeout は remote /api/users/show の単一 fetch timeout。
const remoteStatsTimeout = 5 * time.Second

// remoteStatsCacheSize は LRU cache の最大 entry 数 (#945)。 unique
// (host, username) 組み合わせがこれを超えると最古使用の entry から evict
// される。10000 で typical instance の active remote user 数を十分覆える
// (1 entry あたり ~80 byte → 上限 800KB 程度の memory footprint)。
const remoteStatsCacheSize = 10000

// RemoteStatsFetcher fetches notes/followers/following counts from a remote
// Misskey-compatible instance and caches the result.
//
// Misskey TS の users/show は自インスタンスで観測した範囲のみ集計するため、
// remote user の counts が実体より小さく出る (#943)。本 fetcher は user の
// origin instance の /api/users/show を叩いて公開 counts を取得し、上書き
// 表示することで「リモートサーバー上の実値」を反映する mk-go 独自拡張。
//
// 失敗時は静かに nil を返す (= caller は local 観測値を fallback として使う)。
// 同一 (host, username) への並行 fetch は singleflight で fold する。
//
// SSRF 対策として fetcher は safehttp の SSRF-safe transport を使い、host が
// localhost / private IP / metadata service に解決される場合は接続前に
// reject する (allowedPrivateNetworks で個別許可可)。
//
// cache は size-bounded LRU (hashicorp/golang-lru/v2) で構成し、unique
// (host, username) 組み合わせの増加に対する unbounded growth を防ぐ (#945)。
// TTL は per-entry で持ち、read 時に判定する (LRU library 自体は size
// eviction のみで TTL を扱わない)。
type RemoteStatsFetcher struct {
	client *http.Client
	cache  *lru.Cache[string, cachedRemoteStats]
	// createdCache は FetchAccountCreatedAt の結果 (#3465)。統計とは別に持つ
	// (統計は Mastodon 系へも fallback するが、作成日時は Misskey 系だけに
	// 聞くので、片方の負キャッシュがもう片方を止めないようにする)。
	createdCache *lru.Cache[string, cachedAccountCreatedAt]
	// createdHostDown は FetchAccountCreatedAt で応答しなかった (時間切れ・
	// 接続失敗) ホストと、その時刻 (#3465)。
	createdHostDown *lru.Cache[string, time.Time]
	group           singleflight.Group
	userAgent       string
	// hostAllowed は連合ポリシー (blockedHosts / federation モード) の判定。
	// 未配線なら全ホストへ出す (従来の挙動)。
	hostAllowed func(host string) bool
}

type cachedRemoteStats struct {
	stats   *RemoteUserStats
	fetched time.Time
	ttl     time.Duration
}

type cachedAccountCreatedAt struct {
	createdAt *time.Time
	fetched   time.Time
	ttl       time.Duration
}

// NewRemoteStatsFetcher constructs a fetcher with a SSRF-safe HTTP transport
// built from allowedPrivateNetworks (= config.AllowedPrivateNetworks 互換) と
// optional safehttp.Option (e.g. WithProxy)。
//
// userAgent は outbound request に set する User-Agent header 値で、通常は
// `cfg.UserAgent` (= `Elythia/<ver> (<url>)` 形式) を渡す。空欄なら header は
// 設定しない (= Go default の `Go-http-client/1.1` が送られる、test path)。
//
// urlpreview / mediaproxy と同じく safehttp 経由で組み立てる pattern (#943
// review SSRF guard)。
func NewRemoteStatsFetcher(allowedPrivateNetworks []string, userAgent string, opts ...safehttp.Option) *RemoteStatsFetcher {
	transport := safehttp.NewSSRFSafeTransport(allowedPrivateNetworks, opts...)
	f := newFetcherWithClient(&http.Client{
		Transport: transport,
		Timeout:   remoteStatsTimeout,
	}, remoteStatsCacheSize)
	f.userAgent = userAgent
	return f
}

// HasHostAllowedChecker reports whether the federation gate is wired.
//
// 未配線でも統計は取れてしまうので、外れても利用者からは見えない。起動時の
// critical wiring 検査で落とす。
func (f *RemoteStatsFetcher) HasHostAllowedChecker() bool {
	return f != nil && f.hostAllowed != nil
}

// SetHostAllowedChecker wires the federation gate.
//
// 未配線なら従来どおり (= 全ホストへ出す)。production では必ず配線する。
func (f *RemoteStatsFetcher) SetHostAllowedChecker(fn func(host string) bool) {
	if f != nil {
		f.hostAllowed = fn
	}
}

// newRemoteStatsFetcherWithTransport はテスト専用 constructor。redirectTransport
// 等で httptest server に向け直したい場合に使う。production では
// NewRemoteStatsFetcher を使うこと。
func newRemoteStatsFetcherWithTransport(rt http.RoundTripper) *RemoteStatsFetcher {
	return newRemoteStatsFetcherWithCacheSize(rt, remoteStatsCacheSize)
}

// newRemoteStatsFetcherWithCacheSize はテスト専用 constructor で cache cap
// を override する。production で eviction を起こすには 10000 entry 必要で
// 単体テスト上現実的でないため、size を override して LRU 挙動を verify する。
func newRemoteStatsFetcherWithCacheSize(rt http.RoundTripper, cacheSize int) *RemoteStatsFetcher {
	return newFetcherWithClient(&http.Client{
		Transport: rt,
		Timeout:   remoteStatsTimeout,
	}, cacheSize)
}

// newFetcherWithClient は cache 構築まで含めた共通 constructor。
// production の remoteStatsCacheSize は compile-time const (10000 > 0) のため
// lru.New が error を返す経路には到達しない (lru.New は size <= 0 でのみ
// error)。test 経由で size <= 0 が渡された場合は 1 entry で fallback。
func newFetcherWithClient(client *http.Client, cacheSize int) *RemoteStatsFetcher {
	if cacheSize <= 0 {
		cacheSize = 1
	}
	cache, _ := lru.New[string, cachedRemoteStats](cacheSize)
	createdCache, _ := lru.New[string, cachedAccountCreatedAt](cacheSize)
	createdHostDown, _ := lru.New[string, time.Time](cacheSize)
	return &RemoteStatsFetcher{client: client, cache: cache, createdCache: createdCache, createdHostDown: createdHostDown}
}

// Fetch returns cached stats if available and fresh, otherwise fetches from
// the remote /api/users/show endpoint. Returns nil if the host or username is
// empty / malformed, or if the remote call fails / returns malformed payload.
func (f *RemoteStatsFetcher) Fetch(ctx context.Context, host, username string) *RemoteUserStats {
	if f == nil || host == "" || username == "" {
		return nil
	}
	if !isValidHost(host) {
		// host に / ? # @ space などが混入した URL injection 攻撃を防ぐ
		// (#943 review)。federation の webfinger 経由で sanitize されるはず
		// だが、念のため二重 check。
		return nil
	}
	// **連合を切った相手へは出さない。**
	//
	// この経路は `/api/users/show` (未認証) から呼ばれるので、`blockedHosts` に
	// 入れた相手や `federation: none` の構成でも、そのホストの利用者の
	// プロフィールが描画されるたびに `POST https://<host>/api/users/show` が
	// 出る。**defederate したはずの相手に「誰をいつ見たか」が漏れる。**
	if f.hostAllowed != nil && !f.hostAllowed(host) {
		return nil
	}
	key := host + "|" + username
	if entry, ok := f.cache.Get(key); ok {
		if time.Since(entry.fetched) < entry.ttl {
			return entry.stats
		}
		// expired entry は明示的に Remove して LRU の age 順序を更新しない。
		// (Get で hot 化しないようにする)。
		f.cache.Remove(key)
	}
	v, _, _ := f.group.Do(key, func() (any, error) {
		stats := f.fetchRemote(ctx, host, username)
		ttl := remoteStatsTTL
		if stats == nil {
			ttl = remoteStatsNegativeTTL
		}
		f.cache.Add(key, cachedRemoteStats{stats: stats, fetched: time.Now(), ttl: ttl})
		return stats, nil
	})
	if stats, ok := v.(*RemoteUserStats); ok {
		return stats
	}
	return nil
}

// FetchAccountCreatedAt returns the account creation time (`createdAt`) of a
// local user of a Misskey-compatible server via its `/api/users/show` (#3465).
// It returns nil when the host or username is empty / malformed, the host is
// not allowed by the federation gate, or the remote call fails / returns no
// readable `createdAt`. Results are cached with the same positive / negative
// TTLs as Fetch.
//
// 呼び出し側 (AccountCreatedAtFiller) が保存済みのソフトウェア名で Misskey 系
// だと確かめてから呼ぶ前提で、Mastodon 系の API へは fallback しない。
func (f *RemoteStatsFetcher) FetchAccountCreatedAt(ctx context.Context, host, username string) *time.Time {
	if f == nil || host == "" || username == "" || !isValidHost(host) {
		return nil
	}
	// Fetch と同じく、連合を切った相手へは出さない。
	if f.hostAllowed != nil && !f.hostAllowed(host) {
		return nil
	}
	// 応答しなかったホストへは、しばらく誰の分も取りに行かない。人ごとの
	// 負キャッシュだけだと、Misskey を名乗る相手が知られている人の数だけ
	// フォローの処理を 5 秒ずつ待たせられる。
	if downAt, ok := f.createdHostDown.Get(host); ok {
		if time.Since(downAt) < remoteCreatedAtHostDownTTL {
			return nil
		}
		f.createdHostDown.Remove(host)
	}
	key := host + "|" + username
	if entry, ok := f.createdCache.Get(key); ok {
		if time.Since(entry.fetched) < entry.ttl {
			return entry.createdAt
		}
		f.createdCache.Remove(key)
	}
	v, _, _ := f.group.Do("createdAt|"+key, func() (any, error) {
		createdAt, unreachable := f.fetchMisskeyCreatedAt(ctx, host, username)
		if unreachable {
			f.createdHostDown.Add(host, time.Now())
		}
		ttl := remoteStatsTTL
		if createdAt == nil {
			ttl = remoteStatsNegativeTTL
		}
		f.createdCache.Add(key, cachedAccountCreatedAt{createdAt: createdAt, fetched: time.Now(), ttl: ttl})
		return createdAt, nil
	})
	if createdAt, ok := v.(*time.Time); ok {
		return createdAt
	}
	return nil
}

// fetchMisskeyCreatedAt reads `createdAt` from `/api/users/show`.
//
// 返ってきた人の username が頼んだ人と違うなら採らない (別の人の作成日時を
// 保存しないため)。Misskey の username の照合は大文字小文字を区別しない。
func (f *RemoteStatsFetcher) fetchMisskeyCreatedAt(ctx context.Context, host, username string) (createdAt *time.Time, unreachable bool) {
	var payload struct {
		Username  string `json:"username"`
		CreatedAt string `json:"createdAt"`
	}
	ok, unreachable := f.postMisskeyUsersShow(ctx, host, username, &payload)
	if !ok {
		return nil, unreachable
	}
	if !strings.EqualFold(payload.Username, username) {
		return nil, false
	}
	t, err := time.Parse(time.RFC3339Nano, payload.CreatedAt)
	if err != nil {
		return nil, false
	}
	return &t, false
}

// isValidHost checks that host is a plain hostname (optionally with port) and
// does not contain URL-meaningful characters. Reject `/`, `?`, `#`, `@`, ` `
// 等が混入した host で URL を組むと別 path / 別 host が叩かれて SSRF / spoof
// になるため、url.Parse 経由で host parse 結果が input と一致することを確認する。
func isValidHost(host string) bool {
	if host == "" {
		return false
	}
	parsed, err := url.Parse("https://" + host + "/")
	if err != nil {
		return false
	}
	// Host は parser が optional port 込みで保持。input host 全体が parser
	// の Host と一致しない場合 (= path / query / userinfo が混入していた場合) は reject。
	return parsed.Host == host && parsed.Path == "/"
}

// fetchRemote retrieves public stats for a remote user via a fallback chain:
//  1. Misskey-compat: POST `/api/users/show` with `{username, host:null}`
//  2. Mastodon-compat: GET `/api/v1/accounts/lookup?acct=<username>`
//
// 各経路は 4xx/404/parse-fail で nil を返し、次の fallback に進む。全経路
// 失敗時は nil で local 観測値 fallback (#1154)。本 fetcher は mk-go 独自
// 拡張 (#943) なので upstream Misskey TS に対応 endpoint なく、
// Mastodon 系 instance (= Mastodon / Pleroma / Akkoma / Iceshrimp 等)
// との相互運用は本 chain で確保する。
//
// 将来拡張: 3rd fallback として ActivityPub actor の `outbox.totalItems` /
// `followers.totalItems` / `following.totalItems` を採用すれば、本 chain で
// 拾えない fediverse 実装も覆える (= universal だが追加 3 round-trip 必要)。
// 本 PR scope 外、需要が出たら別 issue で扱う。
func (f *RemoteStatsFetcher) fetchRemote(ctx context.Context, host, username string) *RemoteUserStats {
	if stats := f.fetchMisskey(ctx, host, username); stats != nil {
		return stats
	}
	if stats := f.fetchMastodon(ctx, host, username); stats != nil {
		return stats
	}
	return nil
}

// postMisskeyUsersShow calls the Misskey-compat `/api/users/show` endpoint and
// decodes a 200 response into payload. ok reports whether that succeeded;
// unreachable reports that the host did not answer in time or the connection
// failed (as opposed to an HTTP error status or an unreadable body).
// `{username, host:null}` で origin instance のローカル user lookup を要求する
// shape は upstream Misskey TS の paramDef と一致。
func (f *RemoteStatsFetcher) postMisskeyUsersShow(ctx context.Context, host, username string, payload any) (ok, unreachable bool) {
	endpoint := fmt.Sprintf("https://%s/api/users/show", host)
	body, _ := json.Marshal(map[string]any{
		"username": username,
		"host":     nil,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if f.userAgent != "" {
		req.Header.Set("User-Agent", f.userAgent)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		slog.Debug("remoteStats: misskey fetch failed", "host", host, "username", username, "err", err)
		return false, true
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, false
	}
	limited := io.LimitReader(resp.Body, 1<<20)
	if err := json.NewDecoder(limited).Decode(payload); err != nil {
		// ヘッダーだけ返して本文を止める相手も、待たされる点では応答しない
		// 相手と同じなので、時間切れは unreachable に数える。
		return false, isTimeout(err)
	}
	return true, false
}

// isTimeout reports whether err is a deadline / timeout error.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// fetchMisskey reads the public counts from the Misskey-compat
// `/api/users/show` endpoint.
func (f *RemoteStatsFetcher) fetchMisskey(ctx context.Context, host, username string) *RemoteUserStats {
	var payload struct {
		NotesCount     *int `json:"notesCount"`
		FollowersCount *int `json:"followersCount"`
		FollowingCount *int `json:"followingCount"`
	}
	if ok, _ := f.postMisskeyUsersShow(ctx, host, username, &payload); !ok {
		return nil
	}
	if payload.NotesCount == nil && payload.FollowersCount == nil && payload.FollowingCount == nil {
		return nil
	}
	stats := &RemoteUserStats{}
	if payload.NotesCount != nil {
		stats.NotesCount = *payload.NotesCount
	}
	if payload.FollowersCount != nil {
		stats.FollowersCount = *payload.FollowersCount
	}
	if payload.FollowingCount != nil {
		stats.FollowingCount = *payload.FollowingCount
	}
	return stats
}

// fetchMastodon calls the Mastodon-compat `/api/v1/accounts/lookup` endpoint.
// `?acct=<username>` で local account の数値統計を取得する (= 同一 host の
// local user lookup なので host suffix 不要)。Pleroma / Akkoma / Iceshrimp
// 等の Mastodon API 互換実装でも動作する (#1154)。
//
// response field mapping (Mastodon → mk-go):
//   - statuses_count   → NotesCount
//   - followers_count  → FollowersCount
//   - following_count  → FollowingCount
func (f *RemoteStatsFetcher) fetchMastodon(ctx context.Context, host, username string) *RemoteUserStats {
	endpoint := fmt.Sprintf("https://%s/api/v1/accounts/lookup?acct=%s", host, url.QueryEscape(username))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	if f.userAgent != "" {
		req.Header.Set("User-Agent", f.userAgent)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		slog.Debug("remoteStats: mastodon fetch failed", "host", host, "username", username, "err", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	limited := io.LimitReader(resp.Body, 1<<20)
	var payload struct {
		StatusesCount  *int `json:"statuses_count"`
		FollowersCount *int `json:"followers_count"`
		FollowingCount *int `json:"following_count"`
	}
	if err := json.NewDecoder(limited).Decode(&payload); err != nil {
		return nil
	}
	if payload.StatusesCount == nil && payload.FollowersCount == nil && payload.FollowingCount == nil {
		return nil
	}
	stats := &RemoteUserStats{}
	if payload.StatusesCount != nil {
		stats.NotesCount = *payload.StatusesCount
	}
	if payload.FollowersCount != nil {
		stats.FollowersCount = *payload.FollowersCount
	}
	if payload.FollowingCount != nil {
		stats.FollowingCount = *payload.FollowingCount
	}
	return stats
}
