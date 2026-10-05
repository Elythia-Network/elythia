package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLister(t *testing.T, handler http.HandlerFunc, local ...string) (*nodeInfoPeerLister, *atomic.Int32) {
	t.Helper()
	if len(local) == 0 {
		local = []string{"demo", "other"}
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	l := newNodeInfoPeerLister(srv.Client(), local)
	l.urlFor = func(string) string { return srv.URL + "/nodeinfo/2.1" }
	return l, &hits
}

func TestNodeInfoPeerLister_ReadsDeclaredPlugins(t *testing.T) {
	l, hits := testLister(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/nodeinfo/2.1", r.URL.Path)
		_, _ = w.Write([]byte(`{"metadata":{"elythiaPlugins":["demo","other"]}}`))
	})

	got, err := l.Plugins(context.Background(), "other.example")
	require.NoError(t, err)
	assert.Equal(t, []string{"demo", "other"}, got)

	// **毎回は引かない。** 相手に負荷をかけないよう答えを覚える。
	got, err = l.Plugins(context.Background(), "other.example")
	require.NoError(t, err)
	assert.Equal(t, []string{"demo", "other"}, got)
	assert.Equal(t, int32(1), hits.Load(), "2 回目はキャッシュから返す")
}

// elythiaPlugins を持たない相手 (Misskey TS など) は「持っていない」。
func TestNodeInfoPeerLister_NonMkGoInstance(t *testing.T) {
	l, _ := testLister(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"software":{"name":"misskey"},"metadata":{"nodeName":"x"}}`))
	})

	got, err := l.Plugins(context.Background(), "ts.example")
	require.NoError(t, err)
	assert.Empty(t, got)
}

// 改名 (#3400) より前の版が出す `mkGoPlugins` は読まない (設計 D8)。相手が上げる
// までプラグインの連合は止まるが、旧名を読み続ける猶予は設けない。
func TestNodeInfoPeerLister_IgnoresLegacyKey(t *testing.T) {
	l, _ := testLister(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"software":{"name":"mk-go"},"metadata":{"mkGoPlugins":["demo"]}}`))
	})

	got, err := l.Plugins(context.Background(), "old.example")
	require.NoError(t, err)
	assert.Empty(t, got)
}

// **失敗は覚えない。** 一時的な不調で「持っていない」と決めつけると、
// 相手が復帰しても TTL の間ずっと送れなくなる。
func TestNodeInfoPeerLister_DoesNotCacheFailures(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	l, hits := testLister(t, func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"metadata":{"elythiaPlugins":["demo"]}}`))
	})

	_, err := l.Plugins(context.Background(), "flaky.example")
	require.Error(t, err)

	fail.Store(false)
	got, err := l.Plugins(context.Background(), "flaky.example")
	require.NoError(t, err)
	assert.Equal(t, []string{"demo"}, got)
	assert.Equal(t, int32(2), hits.Load(), "失敗後は取り直す")
}

// 否定の答えは肯定より短く覚える (入れた直後に繋がらない時間を短くする)。
func TestNodeInfoPeerLister_NegativeTTLIsShorter(t *testing.T) {
	assert.Less(t, peerLookupNegativeTTL, peerLookupTTL)

	l, _ := testLister(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"metadata":{}}`))
	})
	_, err := l.Plugins(context.Background(), "none.example")
	require.NoError(t, err)

	l.mu.Lock()
	e := l.cache["none.example"]
	l.mu.Unlock()
	assert.WithinDuration(t, time.Now().Add(peerLookupNegativeTTL), e.expiresAt, time.Minute)
}

// 相手の nodeinfo は **相手が書いた値**。こちらが持っていない名前まで覚えると、
// 1 host あたりの大きさを外から膨らませられる。
func TestNodeInfoPeerLister_KeepsOnlyLocalPlugins(t *testing.T) {
	l, _ := testLister(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"metadata":{"elythiaPlugins":["demo","junk1","junk2","junk3"]}}`))
	}, "demo")

	got, err := l.Plugins(context.Background(), "other.example")
	require.NoError(t, err)
	assert.Equal(t, []string{"demo"}, got, "こちらが持っている名前だけ返す")

	l.mu.Lock()
	cached := l.cache["other.example"]
	l.mu.Unlock()
	assert.Equal(t, []string{"demo"}, cached.plugins, "覚えるのも絞った後")
}

// **寿命は絞る前の一覧で決める。** 絞った後で決めると、相手が別の peered
// プラグインだけを持っている場合に 30 分ごとに引き直すことになり、こちらの
// 都合で相手に負荷をかける。
func TestNodeInfoPeerLister_TTLUsesRawList(t *testing.T) {
	l, _ := testLister(t, func(w http.ResponseWriter, _ *http.Request) {
		// 相手は peered プラグインを持っているが、こちらのものは 1 つも無い。
		_, _ = w.Write([]byte(`{"metadata":{"elythiaPlugins":["theirs1","theirs2"]}}`))
	}, "demo")

	got, err := l.Plugins(context.Background(), "other.example")
	require.NoError(t, err)
	assert.Empty(t, got)

	l.mu.Lock()
	cached := l.cache["other.example"]
	l.mu.Unlock()
	assert.Empty(t, cached.plugins)
	assert.Greater(t, time.Until(cached.expiresAt), peerLookupNegativeTTL,
		"相手が mk-go で peered プラグインを持つ以上、寿命は肯定側 (6 時間)")
}

// 期限切れの entry は読むときに無視されるだけで消えないので、引いた host の数
// だけ表が伸びる。**外から増やせる**ので上限を置く。
func TestNodeInfoPeerLister_BoundsHosts(t *testing.T) {
	l, _ := testLister(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"metadata":{"elythiaPlugins":["demo"]}}`))
	}, "demo")

	// **毎回見る。** ループの後で 1 回だけ数えると、`<` / `<=` の取り違えが
	// 回数の偶奇で当たり外れになる (実測で `<=` への変異が生き残った)。
	for i := 0; i < peerLookupMaxHosts+64; i++ {
		_, err := l.Plugins(context.Background(), fmt.Sprintf("h%d.example", i))
		require.NoError(t, err)
		l.mu.Lock()
		n := len(l.cache)
		l.mu.Unlock()
		require.LessOrEqual(t, n, peerLookupMaxHosts, "i=%d", i)
	}
}
