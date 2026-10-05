package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

/*
 * 相手が同じプラグインを持っているかを nodeinfo から見る (#2537)。
 *
 * 送る前に確かめるのは、**持っていない相手に接続しないため**。送ってから
 * 404 で知る形にすると、無関係なインスタンスにこちらの都合でリクエストが飛ぶ。
 */

// peerLookupTTL is how long a nodeinfo answer is reused.
//
// プラグインの構成は滅多に変わらないので長めでよい。**プロセス内に持つ**
// (DB に列を足さない) ので、再起動すれば取り直す。
const peerLookupTTL = 6 * time.Hour

// peerLookupNegativeTTL is how long "does not have it" is remembered.
//
// 引けなかった相手に毎回問い合わせると、こちらの都合で相手に負荷をかける。
// ただし導入直後に長く覚えると入れたのに繋がらないので、肯定側より短くする。
const peerLookupNegativeTTL = 30 * time.Minute

// peerLookupTimeout bounds one nodeinfo fetch.
const peerLookupTimeout = 10 * time.Second

// peerLookupMaxBody bounds a nodeinfo document.
const peerLookupMaxBody = 256 << 10

// peerLookupMaxHosts bounds the cache.
//
// **期限切れの entry は消えない実装だった。** 読むときに時刻を比べるだけなので、
// 引いた host の数だけ表が伸び続ける。引く相手を決めるのはプラグインだが、
// リモート利用者のホストを使う作り (authoring.md が勧めている形) だと**外から
// 増やせる**。
const peerLookupMaxHosts = 2048

// nodeInfoPeerLister resolves which plugins a remote host declares.
type nodeInfoPeerLister struct {
	client *http.Client
	// urlFor builds the nodeinfo URL for a host. テストから http の
	// httptest サーバーへ向けられるように関数にしてある。
	urlFor func(host string) string

	// local はこちらが持っている peered プラグイン名。**これに無い名前は
	// 覚えない。** 相手の nodeinfo は相手が書いた値なので、そのまま持つと
	// 1 host あたりの大きさを外から膨らませられる。
	local map[string]struct{}

	mu    sync.Mutex
	cache map[string]peerLookupEntry
}

type peerLookupEntry struct {
	plugins   []string
	expiresAt time.Time
}

func newNodeInfoPeerLister(client *http.Client, local []string) *nodeInfoPeerLister {
	set := make(map[string]struct{}, len(local))
	for _, n := range local {
		set[n] = struct{}{}
	}
	return &nodeInfoPeerLister{
		client: client,
		local:  set,
		// well-known を辿らず 2.1 を直接見る。同じソフト同士でしか使わない経路
		// なので相手の nodeinfo の場所は分かっている。**相手が対応していなければ
		// elythiaPlugins が無いだけ**で、誤って送ることはない。改名 (#3400) より前の
		// 版が出す `mkGoPlugins` は読まない (設計 D8)。
		urlFor: func(host string) string { return "https://" + host + "/nodeinfo/2.1" },
		cache:  map[string]peerLookupEntry{},
	}
}

// Plugins returns the plugin names host declares in nodeinfo metadata.
func (l *nodeInfoPeerLister) Plugins(ctx context.Context, host string) ([]string, error) {
	l.mu.Lock()
	if e, ok := l.cache[host]; ok && time.Now().Before(e.expiresAt) {
		l.mu.Unlock()
		return e.plugins, nil
	}
	l.mu.Unlock()

	names, err := l.fetch(ctx, host)
	if err != nil {
		// **失敗は覚えない。** 一時的な不調で「持っていない」と決めつけると、
		// 相手が復帰しても TTL の間ずっと送れなくなる。
		return nil, err
	}

	// **TTL は取得した一覧そのもので決める。** 絞った後で決めると、相手が
	// 別の peered プラグインだけを持っている場合に 30 分ごとに引き直すことに
	// なり、こちらの都合で相手に負荷をかける。
	ttl := peerLookupTTL
	if len(names) == 0 {
		ttl = peerLookupNegativeTTL
	}
	kept := l.keepLocal(names)

	now := time.Now()
	l.mu.Lock()
	l.evictLocked(now)
	l.cache[host] = peerLookupEntry{plugins: kept, expiresAt: now.Add(ttl)}
	l.mu.Unlock()
	return kept, nil
}

// Forget drops the cached answer for host.
//
// 受け口が無いと分かったときに送信側が呼ぶ。次の問い合わせで引き直す。
func (l *nodeInfoPeerLister) Forget(host string) {
	l.mu.Lock()
	delete(l.cache, host)
	l.mu.Unlock()
}

// keepLocal drops names this instance does not run.
func (l *nodeInfoPeerLister) keepLocal(names []string) []string {
	var kept []string
	for _, n := range names {
		if _, ok := l.local[n]; ok {
			kept = append(kept, n)
		}
	}
	return kept
}

// evictLocked makes room for a new host.
//
// 期限切れを先に落とす。それでも埋まっているときだけ、期限が近い順に捨てる
// (取り直せば済むので、失うのは 1 回分の外向きリクエスト)。
func (l *nodeInfoPeerLister) evictLocked(now time.Time) {
	if len(l.cache) < peerLookupMaxHosts {
		return
	}
	for h, e := range l.cache {
		if !now.Before(e.expiresAt) {
			delete(l.cache, h)
		}
	}
	for len(l.cache) >= peerLookupMaxHosts {
		var oldestHost string
		var oldest time.Time
		for h, e := range l.cache {
			if oldestHost == "" || e.expiresAt.Before(oldest) {
				oldestHost, oldest = h, e.expiresAt
			}
		}
		if oldestHost == "" {
			return
		}
		delete(l.cache, oldestHost)
	}
}

func (l *nodeInfoPeerLister) fetch(ctx context.Context, host string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, peerLookupTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.urlFor(host), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	res, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close() //nolint:errcheck // 読み捨て
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nodeinfo: status %d", res.StatusCode)
	}

	var parsed struct {
		Metadata struct {
			ElythiaPlugins []string `json:"elythiaPlugins"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, peerLookupMaxBody)).Decode(&parsed); err != nil {
		return nil, err
	}
	return parsed.Metadata.ElythiaPlugins, nil
}
