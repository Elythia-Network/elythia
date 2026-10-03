package fetchrss

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/mmcdole/gofeed"
	ext "github.com/mmcdole/gofeed/extensions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/testutil"
)

const sampleRSS2 = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:dc="http://purl.org/dc/elements/1.1/">
<channel>
  <title>Example Feed</title>
  <link>https://example.com/</link>
  <description>An example feed</description>
  <image>
    <url>https://example.com/icon.png</url>
    <title>Example Icon</title>
  </image>
  <item>
    <title>First Post</title>
    <link>https://example.com/posts/1</link>
    <guid isPermaLink="false">post-1</guid>
    <pubDate>Fri, 01 May 2026 12:00:00 GMT</pubDate>
    <dc:creator>Alice</dc:creator>
    <category>tech</category>
    <category>golang</category>
    <description>&lt;p&gt;Hello   world&lt;/p&gt;</description>
    <enclosure url="https://example.com/audio.mp3" length="1234" type="audio/mpeg"/>
  </item>
</channel>
</rss>`

const sampleAtom = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Atom Sample</title>
  <link href="https://atom.example.com/"/>
  <updated>2026-05-01T12:00:00Z</updated>
  <id>urn:uuid:atom-feed</id>
  <entry>
    <title>Atom Entry</title>
    <link href="https://atom.example.com/e/1"/>
    <id>urn:uuid:atom-1</id>
    <updated>2026-05-01T12:00:00Z</updated>
    <author><name>Bob</name></author>
    <content type="html">&lt;p&gt;Atom &lt;b&gt;content&lt;/b&gt;&lt;/p&gt;</content>
  </entry>
</feed>`

// testUserAgent is the UA string used for handler-under-test wiring. Static
// so individual tests can assert on it without threading the value around.
const testUserAgent = "mk-go/test (https://example.test)"

// startFeedServer launches an httptest server returning the supplied body and
// returns a Handler whose HTTP client targets that server. We bypass SSRF
// concerns at the transport layer because httptest binds to 127.0.0.1; the
// real wiring uses the SSRF-safe transport from server/outbound_http.go.
func newTestHandler(_ *testing.T, srv *httptest.Server) *Handler {
	return New(&http.Client{Timeout: FetchTimeout, Transport: srv.Client().Transport}, testUserAgent)
}

func newRequestCtx(method, target string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	return c, rec
}

func TestFetchRSS_BasicRSS2(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.Header.Get("Accept"), "application/rss+xml")
		// mk-go/<ver> 相当の UA が付くことを guard する。UA 必須の RSS
		// 配信サーバ (Cloudflare 含む) で 403 にならない契約の regression 検知。
		assert.Equal(t, testUserAgent, r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL))
	require.NoError(t, h.Fetch(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, "Example Feed", resp["title"])
	assert.Equal(t, "https://example.com/", resp["link"])
	assert.Equal(t, "An example feed", resp["description"])

	img, ok := resp["image"].(map[string]any)
	require.True(t, ok, "image must be present")
	assert.Equal(t, "https://example.com/icon.png", img["url"])
	assert.Equal(t, "Example Icon", img["title"])

	items, ok := resp["items"].([]any)
	require.True(t, ok, "items must be array")
	require.Len(t, items, 1)
	item := items[0].(map[string]any)
	assert.Equal(t, "First Post", item["title"])
	assert.Equal(t, "https://example.com/posts/1", item["link"])
	assert.Equal(t, "post-1", item["guid"])
	assert.Equal(t, "Alice", item["creator"])
	assert.Equal(t, "Fri, 01 May 2026 12:00:00 GMT", item["pubDate"])

	// isoDate は RFC3339 UTC で生 pubDate と一致する時刻に正規化される
	iso, ok := item["isoDate"].(string)
	require.True(t, ok)
	parsed, err := time.Parse(time.RFC3339Nano, iso)
	require.NoError(t, err)
	assert.True(t, parsed.UTC().Equal(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)))

	cats, _ := item["categories"].([]any)
	assert.Equal(t, []any{"tech", "golang"}, cats)

	encl, ok := item["enclosure"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "https://example.com/audio.mp3", encl["url"])
	assert.Equal(t, "audio/mpeg", encl["type"])
	// length は Misskey schema では number。json.Unmarshal は float64 になる
	assert.EqualValues(t, 1234, encl["length"])

	// contentSnippet は description の HTML タグ除去 + 連続 whitespace 縮約
	assert.Equal(t, "Hello world", item["contentSnippet"])

	assert.Equal(t, "public, max-age=180", rec.Header().Get("Cache-Control"))
}

func TestFetchRSS_Atom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		fmt.Fprint(w, sampleAtom)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL))
	require.NoError(t, h.Fetch(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "Atom Sample", resp["title"])
	items := resp["items"].([]any)
	require.Len(t, items, 1)
	item := items[0].(map[string]any)
	assert.Equal(t, "Atom Entry", item["title"])
	assert.Equal(t, "Bob", item["creator"])
	assert.Equal(t, "Atom content", item["contentSnippet"])

	// Atom は published を持たないので isoDate は updated から派生する
	iso, ok := item["isoDate"].(string)
	require.True(t, ok)
	_, err := time.Parse(time.RFC3339Nano, iso)
	require.NoError(t, err)
}

func TestFetchRSS_POSTBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	body := fmt.Sprintf(`{"url":%q}`, srv.URL)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/fetch-rss", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, h.Fetch(c))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestFetchRSS_MissingURL(t *testing.T) {
	h := New(&http.Client{Timeout: FetchTimeout}, testUserAgent)
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss")
	require.NoError(t, h.Fetch(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	// url パラメータ欠落は upstream の ajv schema validation (INVALID_PARAM)
	// 相当として扱う (2026.7.0 hardening 追従)。
	assert.Equal(t, "INVALID_PARAM", errObj["code"])
}

func TestFetchRSS_InvalidScheme(t *testing.T) {
	h := New(&http.Client{Timeout: FetchTimeout}, testUserAgent)
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url=file:///etc/passwd")
	require.NoError(t, h.Fetch(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "INVALID_URL", errObj["code"])
	// upstream 2026.7.0 の canonical id。
	assert.Equal(t, "89b7ee05-ccfc-4bdd-9b13-61172fd1e06c", errObj["id"])
}

func TestFetchRSS_EmptyURLAfterTrim(t *testing.T) {
	h := New(&http.Client{Timeout: FetchTimeout}, testUserAgent)
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url=%20%20")
	require.NoError(t, h.Fetch(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFetchRSS_NoHostInURL(t *testing.T) {
	h := New(&http.Client{Timeout: FetchTimeout}, testUserAgent)
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url=http://")
	require.NoError(t, h.Fetch(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFetchRSS_UpstreamErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL))
	require.NoError(t, h.Fetch(c))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "FETCH_RSS_FAILED", errObj["code"])
	assert.Equal(t, "8db5d3d8-31d7-452f-b0cc-ca3b8925de12", errObj["id"])
	assert.Equal(t, "server", errObj["kind"])
}

func TestFetchRSS_UnparseableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "not a valid rss or atom")
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL))
	require.NoError(t, h.Fetch(c))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestFetchRSS_OversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Cap is 1 MiB; send 2 MiB. ReadAllLimit returns ErrMaxBytesExceeded so
		// fetchFeed surfaces FETCH_RSS_FAILED before we ever call gofeed.
		w.Header().Set("Content-Length", fmt.Sprintf("%d", 2<<20))
		_, _ = w.Write(make([]byte, 2<<20))
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL))
	require.NoError(t, h.Fetch(c))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

// TestFetchRSS_DialFails covers the case where the outbound transport itself
// rejects the connection (which is how SSRF blocks surface in production).
func TestFetchRSS_DialFails(t *testing.T) {
	failingClient := &http.Client{
		Timeout: FetchTimeout,
		Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			return nil, errors.New("ssrf block: connection to private IP refused")
		}),
	}
	h := New(failingClient, testUserAgent)
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url=http://10.0.0.1/feed.xml")
	require.NoError(t, h.Fetch(c))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "FETCH_RSS_FAILED", errObj["code"])
	// resolved IP / SSRF 文言が err.Error() 経由で漏れないこと: 静的メッセージ
	// に置き換わっており、敏感な substring を含まない。
	msg, _ := errObj["message"].(string)
	assert.Equal(t, "Failed to fetch RSS.", msg)
	assert.NotContains(t, msg, "ssrf")
	assert.NotContains(t, msg, "10.0.0.1")
}

// TestFetchRSS_NoUserAgent covers the wiring path where userAgent="" — the
// handler must omit the header rather than send "User-Agent: ".
func TestFetchRSS_NoUserAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Go's http server normalizes a missing UA to "Go-http-client/1.1".
		// What we really want to assert is that *we* don't override it with
		// our caller-provided string when it's empty.
		assert.NotEqual(t, testUserAgent, r.Header.Get("User-Agent"))
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)

	h := New(&http.Client{Timeout: FetchTimeout, Transport: srv.Client().Transport}, "")
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL))
	require.NoError(t, h.Fetch(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// --- pack helper unit tests (carve out edge cases that don't surface from
// the full RSS / Atom samples) ---

// --- #683 server-side cache + singleflight tests ---

// TestFetchRSS_CacheHit: 同 URL の 2 回目以降は upstream に到達せず in-memory
// cache から bytes をそのまま返す。upstream hit カウントが 1 のまま増えない
// ことで cache 経路が機能している保証になる。
func TestFetchRSS_CacheHit(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)

	// 1 回目: miss → upstream fetch
	c1, rec1 := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL))
	require.NoError(t, h.Fetch(c1))
	require.Equal(t, http.StatusOK, rec1.Code)

	// 2 回目: hit → upstream は呼ばれないはず
	c2, rec2 := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL))
	require.NoError(t, h.Fetch(c2))
	require.Equal(t, http.StatusOK, rec2.Code)

	assert.EqualValues(t, 1, hits.Load(), "cache hit should not reach upstream")
	// hit / miss どちらも JSON body は同一であるべき
	assert.JSONEq(t, rec1.Body.String(), rec2.Body.String())
	// hit でも Cache-Control は同じ値
	assert.Equal(t, "public, max-age=180", rec2.Header().Get("Cache-Control"))
}

// TestFetchRSS_CacheExpiry: TTL を過ぎると cache が破棄され upstream に再アクセス
// する。SetClock で時計を進めるため Sleep 不要。
func TestFetchRSS_CacheExpiry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	now := time.Now()
	h.SetClock(func() time.Time { return now })
	h.SetCacheTTL(60 * time.Second)

	c1, _ := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL))
	require.NoError(t, h.Fetch(c1))

	// 時計を TTL ぶん超えて進める
	now = now.Add(61 * time.Second)

	c2, _ := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL))
	require.NoError(t, h.Fetch(c2))

	assert.EqualValues(t, 2, hits.Load(), "expired entry should trigger re-fetch")
}

// TestFetchRSS_Singleflight: 同 URL に対する concurrent miss は 1 fetch に
// 集約され、upstream へは 1 回だけ届くこと (thundering herd 防止)。
func TestFetchRSS_Singleflight(t *testing.T) {
	var hits atomic.Int32
	// upstream を意図的に少し遅延させて、N 個の caller が同時に singleflight
	// 待ちに入るウィンドウを作る。
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-gate // gate が close されるまでブロック
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	results := make([]int, N)
	for i := range N {
		go func(idx int) {
			defer wg.Done()
			c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL))
			if err := h.Fetch(c); err != nil {
				t.Errorf("Fetch %d: %v", idx, err)
				return
			}
			results[idx] = rec.Code
		}(i)
	}

	// 1 件目の goroutine が upstream に到達するまで polling で待つ
	// (= sf.Do の closure 実行が始まったことの観測可能シグナル)。固定
	// time.Sleep だと slow CI で前段が走り切らず flaky になり得るため。
	waitForUpstreamHit(t, &hits, 1, 2*time.Second)
	// 残り 19 caller も sf.Do に到達するまでの猶予 (sf.Do 内部で coalesce
	// される時間)。closure 実行は既に始まっているので gate 解放前に到着すれば
	// 全員が同じ result を共有する。
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i, code := range results {
		assert.Equal(t, http.StatusOK, code, "request %d should be 200", i)
	}
	// 重要: upstream に届いた件数は 1 (singleflight が coalesce した)
	assert.EqualValues(t, 1, hits.Load(), "concurrent fetches must coalesce to one upstream call")
}

// waitForUpstreamHit polls until the atomic counter reaches `want` or the
// timeout elapses. fixed time.Sleep だと slow CI で flaky になりがちな
// 「upstream に届いた」の同期を polling で観測する。
func waitForUpstreamHit(t *testing.T, hits *atomic.Int32, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for hits.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("upstream hit did not reach %d within %v (got %d)", want, timeout, hits.Load())
		}
		time.Sleep(time.Millisecond)
	}
}

// TestFetchRSS_Singleflight_CallerCancelDoesNotAbortPeers: caller A の context
// cancel が in-flight の upstream fetch を中断せず、coalesced peer B も成功する
// ことを保証する (Devin review #720 FLAG-1 regression guard)。
//
// 修正前: sf.Do 内 fetch が caller A の request ctx を共有していたため、A の
// disconnect で fetchFeed が ctx canceled error を返し B も 502 を受け取った。
// 修正後: closure は context.Background() + 5s timeout で fetch するので
// caller cancel から切り離される。
func TestFetchRSS_Singleflight_CallerCancelDoesNotAbortPeers(t *testing.T) {
	var hits atomic.Int32
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-gate
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)

	target := "/api/fetch-rss?url=" + url.QueryEscape(srv.URL)

	// Caller A: 途中で cancel する request context を持つ。
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	e := echo.New()
	reqA := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctxA)
	reqA.Header.Set("Content-Type", "application/json")
	recA := httptest.NewRecorder()
	cA := e.NewContext(reqA, recA)

	// Caller B: cancel しない。同 URL なので sf.Do で A に coalesce される。
	cB, recB := newRequestCtx(http.MethodGet, target)

	// 重要: caller A が先に sf.Do に入って closure を実行する状態を保証する
	// (= 後続 B が coalesce されるレースを再現可能にする)。
	// goroutine scheduling だけだと B が先に走ることもあるので、A の upstream
	// 到達 (hits == 1) を確認してから B を起動する。
	var wg sync.WaitGroup
	wg.Add(1)
	var errA error
	go func() { defer wg.Done(); errA = h.Fetch(cA) }()
	waitForUpstreamHit(t, &hits, 1, 2*time.Second)

	// この時点で A が closure 実行中 (fetchFeed で gate 待機)。B を後から起動。
	wg.Add(1)
	var errB error
	go func() { defer wg.Done(); errB = h.Fetch(cB) }()
	// B が sf.Do に到達して coalesce されるための grace。
	time.Sleep(50 * time.Millisecond)

	// ここで caller A をキャンセル。修正前なら fetchCtx に伝播して fetchFeed
	// が ctx canceled で abort、B も 502 を受け取る。
	cancelA()
	// 修正前のコードで cancel 検知が http.Client → fetchFeed → closure に
	// 伝播するのにかかる時間を確保する (regression guard 必須の wait)。
	// 修正後は fetchCtx が caller から切り離されているのでこの 20ms の意味は
	// 無いが、un-fixed code でテストが正しく failing する条件を作るには
	// この sleep が無いと close(gate) が cancel 伝播より先に走って false
	// negative になる。
	time.Sleep(20 * time.Millisecond)

	// gate を開けて upstream を返答させる。fetch が cancel されていなければ
	// body が読めて両 caller に同じ JSON が届く。
	close(gate)
	wg.Wait()

	require.NoError(t, errA)
	require.NoError(t, errB)
	// A (キャンセル側) も 200 で完走するはず: writeCachedJSON は ctx を
	// 観測しないので、キャンセルされていても response は body を書き出して
	// 完了する。「A の cancel が response 自体を壊さない」契約の guard。
	assert.Equal(t, http.StatusOK, recA.Code, "cancelled caller still completes the response")
	// B (キャンセルしていない) は 200 で完走しているはず。これが 502 になっていたら
	// caller cancel が peer に伝播してしまっている (regression)。
	assert.Equal(t, http.StatusOK, recB.Code, "peer caller must not be aborted by sibling cancel")
	assert.EqualValues(t, 1, hits.Load(), "upstream should be hit only once")
}

// TestFetchRSS_ErrorNotCached: upstream 5xx の場合は cache されず、次回呼び出し
// で再度 upstream に到達する (短期障害から復旧した feed が古い error で塞がれない)。
func TestFetchRSS_ErrorNotCached(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)

	for range 3 {
		c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL))
		require.NoError(t, h.Fetch(c))
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	}
	assert.EqualValues(t, 3, hits.Load(), "errors must not be cached")
}

// TestFetchRSS_CacheEvictionAtCapacity: cache 容量超過時に最古 entry を 1 件
// 落とす振る舞いを保証する。SetCacheMaxEntries(2) + 3 URL push で 1 件 evict。
func TestFetchRSS_CacheEvictionAtCapacity(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// クエリで feed 種別を分岐しない (どの URL でも同一 body) — テストは
		// hit カウントだけで evict を観測するため body 内容は重要ではない。
		_ = r
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	h.SetCacheMaxEntries(2)
	now := time.Now()
	h.SetClock(func() time.Time { return now })

	urlA := srv.URL + "/?feed=a"
	urlB := srv.URL + "/?feed=b"
	urlC := srv.URL + "/?feed=c"

	// A 投入 (cache: {A})
	c, _ := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(urlA))
	require.NoError(t, h.Fetch(c))

	// B 投入 (cache: {A, B}). A の expiresAt より B の expiresAt は新しい
	now = now.Add(time.Second)
	c, _ = newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(urlB))
	require.NoError(t, h.Fetch(c))

	// C 投入 (容量 2 超 → 最古 expiresAt の A が evict される。cache: {B, C})
	now = now.Add(time.Second)
	c, _ = newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(urlC))
	require.NoError(t, h.Fetch(c))

	// ここまでで upstream は 3 回叩かれている
	assert.EqualValues(t, 3, hits.Load())

	// B は cache hit (upstream 不変)
	c, _ = newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(urlB))
	require.NoError(t, h.Fetch(c))
	assert.EqualValues(t, 3, hits.Load(), "B should hit cache")

	// A は evict 済み → re-fetch (upstream 4 回目)
	c, _ = newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(urlA))
	require.NoError(t, h.Fetch(c))
	assert.EqualValues(t, 4, hits.Load(), "A should have been evicted and re-fetched")
}

// TestFetchRSS_SetCacheTTL_RejectsNonPositive: SetCacheTTL は 0 / 負値を無視
// する (default を維持) ことを assert。設定ミスで cache が事実上無効化されない
// 安全策。
func TestFetchRSS_SetCacheTTL_RejectsNonPositive(t *testing.T) {
	h := New(&http.Client{Timeout: FetchTimeout}, testUserAgent)
	original := h.cacheTTL
	h.SetCacheTTL(0)
	assert.Equal(t, original, h.cacheTTL)
	h.SetCacheTTL(-1 * time.Second)
	assert.Equal(t, original, h.cacheTTL)
}

// TestFetchRSS_SetCacheMaxEntries_RejectsNonPositive: 同様に soft cap も
// 0 / 負値で上書きされない。
func TestFetchRSS_SetCacheMaxEntries_RejectsNonPositive(t *testing.T) {
	h := New(&http.Client{Timeout: FetchTimeout}, testUserAgent)
	original := h.cacheMaxLen
	h.SetCacheMaxEntries(0)
	assert.Equal(t, original, h.cacheMaxLen)
	h.SetCacheMaxEntries(-10)
	assert.Equal(t, original, h.cacheMaxLen)
}

// TestFetchRSS_SetClock_NilIgnored: nil clock を渡しても default time.Now が
// 維持されること。
func TestFetchRSS_SetClock_NilIgnored(t *testing.T) {
	h := New(&http.Client{Timeout: FetchTimeout}, testUserAgent)
	h.SetClock(nil)
	// 直接比較不可だが、cache 経路が時計を呼べることで indirect に保証する
	require.NotNil(t, h.now)
}

func TestPackImage_Nil(t *testing.T) {
	assert.Nil(t, packImage(nil))
}

func TestPackEnclosure_BadLength(t *testing.T) {
	// length が parse できない値の場合は length フィールドだけ省略し、url/type は残す
	out := packEnclosure([]*gofeed.Enclosure{{
		URL:    "https://example.com/audio.mp3",
		Type:   "audio/mpeg",
		Length: "not-a-number",
	}})
	require.NotNil(t, out)
	assert.Equal(t, "https://example.com/audio.mp3", out["url"])
	assert.Equal(t, "audio/mpeg", out["type"])
	_, hasLen := out["length"]
	assert.False(t, hasLen, "length must be omitted when not numeric")
}

func TestPackEnclosure_Empty(t *testing.T) {
	assert.Nil(t, packEnclosure(nil))
	assert.Nil(t, packEnclosure([]*gofeed.Enclosure{nil}))
	assert.Nil(t, packEnclosure([]*gofeed.Enclosure{{}}))
}

func TestPackImage_TitleOnly(t *testing.T) {
	// URL 空の Image (本来は schema 違反だが防御的に省略する)
	assert.Nil(t, packImage(&gofeed.Image{Title: "lonely"}))
}

func TestPackImage_NoTitle(t *testing.T) {
	out := packImage(&gofeed.Image{URL: "https://example.com/i.png"})
	require.NotNil(t, out)
	assert.Equal(t, "https://example.com/i.png", out["url"])
	_, hasTitle := out["title"]
	assert.False(t, hasTitle)
}

func TestPackITunes_Nil(t *testing.T) {
	assert.Nil(t, packITunes(&gofeed.Feed{}))
	// extension 全フィールド空 → 結果も nil
	assert.Nil(t, packITunes(&gofeed.Feed{ITunesExt: &ext.ITunesFeedExtension{}}))
}

func TestPackITunes_Full(t *testing.T) {
	feed := &gofeed.Feed{ITunesExt: &ext.ITunesFeedExtension{
		Image:    "https://example.com/cover.jpg",
		Author:   "Show Host",
		Summary:  "All about feeds",
		Explicit: "no",
		Keywords: "tech, golang , , rss",
		Owner: &ext.ITunesOwner{
			Name:  "Owner Name",
			Email: "owner@example.com",
		},
		Categories: []*ext.ITunesCategory{
			{Text: "Technology", Subcategory: &ext.ITunesCategory{Text: "Programming"}},
			{Text: "News"},
			nil,
			{Text: ""},
		},
	}}
	out := packITunes(feed)
	require.NotNil(t, out)
	assert.Equal(t, "https://example.com/cover.jpg", out["image"])
	assert.Equal(t, "Show Host", out["author"])
	assert.Equal(t, "All about feeds", out["summary"])
	assert.Equal(t, "no", out["explicit"])
	assert.Equal(t, []string{"tech", "golang", "rss"}, out["keywords"])
	assert.Equal(t, []string{"Technology", "Programming", "News"}, out["categories"])
	owner := out["owner"].(map[string]any)
	assert.Equal(t, "Owner Name", owner["name"])
	assert.Equal(t, "owner@example.com", owner["email"])
}

func TestPackITunes_KeywordsAllBlank(t *testing.T) {
	out := packITunes(&gofeed.Feed{ITunesExt: &ext.ITunesFeedExtension{
		Keywords: ", ,  ,",
	}})
	// blank-only keywords は categories と同じく省略する
	assert.Nil(t, out)
}

func TestPackITunes_OwnerNameOnly(t *testing.T) {
	out := packITunes(&gofeed.Feed{ITunesExt: &ext.ITunesFeedExtension{
		Owner: &ext.ITunesOwner{Name: "Solo"},
	}})
	require.NotNil(t, out)
	owner := out["owner"].(map[string]any)
	assert.Equal(t, "Solo", owner["name"])
	_, hasEmail := owner["email"]
	assert.False(t, hasEmail)
}

func TestItemCreator_Authors(t *testing.T) {
	got := itemCreator(&gofeed.Item{
		Authors: []*gofeed.Person{{Name: "Atom Author"}},
		Author:  &gofeed.Person{Name: "Deprecated"},
	})
	assert.Equal(t, "Atom Author", got)
}

func TestItemCreator_DeprecatedAuthor(t *testing.T) {
	got := itemCreator(&gofeed.Item{
		Author: &gofeed.Person{Name: "Old Author"},
	})
	assert.Equal(t, "Old Author", got)
}

func TestItemCreator_DublinCoreFallback(t *testing.T) {
	got := itemCreator(&gofeed.Item{
		DublinCoreExt: &ext.DublinCoreExtension{Creator: []string{"DC Creator"}},
	})
	assert.Equal(t, "DC Creator", got)
}

func TestItemCreator_Empty(t *testing.T) {
	assert.Equal(t, "", itemCreator(&gofeed.Item{}))
	// nil authors / empty arrays
	assert.Equal(t, "", itemCreator(&gofeed.Item{Authors: []*gofeed.Person{nil}}))
	assert.Equal(t, "", itemCreator(&gofeed.Item{Authors: []*gofeed.Person{{}}}))
}

func TestContentSnippet_Empty(t *testing.T) {
	assert.Equal(t, "", contentSnippet(&gofeed.Item{}))
}

func TestContentSnippet_DescriptionFallback(t *testing.T) {
	got := contentSnippet(&gofeed.Item{Description: "<b>Hello</b>"})
	assert.Equal(t, "Hello", got)
}

func TestPackItem_PublishedFallback(t *testing.T) {
	// PublishedParsed が無く UpdatedParsed しか無い場合 isoDate は updated 由来
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	out := packItem(&gofeed.Item{
		Title:         "x",
		UpdatedParsed: &updated,
	})
	iso, ok := out["isoDate"].(string)
	require.True(t, ok)
	parsed, err := time.Parse(time.RFC3339Nano, iso)
	require.NoError(t, err)
	assert.True(t, parsed.UTC().Equal(updated))
}

// #2054: POST と raw-token 付き GET には Cache-Control を付けない (upstream `!token && !user`)。
func TestFetchRSS_CacheControlGate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)
	h := newTestHandler(t, srv)

	// POST → cache 無し。
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/fetch-rss", strings.NewReader(fmt.Sprintf(`{"url":%q}`, srv.URL)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	require.NoError(t, h.Fetch(e.NewContext(req, rec)))
	assert.Empty(t, rec.Header().Get("Cache-Control"), "POST には Cache-Control を付けない (#2054)")

	// raw token 付き GET → cache 無し (auth middleware が積む flag を simulate)。
	c2, rec2 := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL))
	c2.Set("misskeyRawTokenPresent", true)
	require.NoError(t, h.Fetch(c2))
	assert.Empty(t, rec2.Header().Get("Cache-Control"), "raw token GET には Cache-Control を付けない (#2054)")
}

// ── 2026.7.0 GHSA hardening (#2240 item 3) ──────────────────────────

// TestFetchRSS_URLTooLong: 8192 字超の URL は INVALID_URL (400)。
func TestFetchRSS_URLTooLong(t *testing.T) {
	h := New(&http.Client{Timeout: FetchTimeout}, testUserAgent)
	long := "http://example.com/" + strings.Repeat("a", MaxURLLength)
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(long))
	require.NoError(t, h.Fetch(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "89b7ee05-ccfc-4bdd-9b13-61172fd1e06c")
}

// TestFetchRSS_UserinfoRejected: userinfo 付き URL は Basic 認証として外部へ
// 転送されるため INVALID_URL で拒否 (upstream normalizeUrl と同じ)。
func TestFetchRSS_UserinfoRejected(t *testing.T) {
	h := New(&http.Client{Timeout: FetchTimeout}, testUserAgent)
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape("http://user:pass@example.com/feed"))
	require.NoError(t, h.Fetch(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_URL")
}

// TestFetchRSS_FragmentNormalized: fragment 違いの URL は同一 cache entry を
// 共有する (fragment で cache/singleflight を無限にすり抜ける穴の防止)。
func TestFetchRSS_FragmentNormalized(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	for _, frag := range []string{"%23a", "%23b", "%23c"} {
		c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL+"/feed")+frag)
		require.NoError(t, h.Fetch(c))
		require.Equal(t, http.StatusOK, rec.Code)
	}
	assert.EqualValues(t, 1, hits.Load(), "fragment 違いは同一 cache entry に正規化される")
}

// TestFetchRSS_ConcurrencyLimit: distinct URL の同時 fetch が上限を超えたら
// FETCH_RSS_UNAVAILABLE (503, kind server)。
func TestFetchRSS_ConcurrencyLimit(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	h := newTestHandler(t, srv)
	h.SetMaxConcurrentFetches(1)

	started := make(chan struct{})
	go func() {
		c, _ := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL+"/slow"))
		close(started)
		_ = h.Fetch(c)
	}()
	<-started
	// 1 本目が inflight に登録されるのを待つ。
	require.Eventually(t, func() bool {
		h.inflightMu.Lock()
		defer h.inflightMu.Unlock()
		return len(h.inflight) == 1
	}, time.Second, time.Millisecond)

	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL+"/other"))
	require.NoError(t, h.Fetch(c))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "FETCH_RSS_UNAVAILABLE", errObj["code"])
	assert.Equal(t, "91e6ff44-c63f-4725-9ad0-b7a40d7f7655", errObj["id"])
	assert.Equal(t, "server", errObj["kind"])
}

// TestFetchRSS_ConcurrencyAllowsJoin: 同一 URL への合流は上限に数えない
// (upstream の inFlightRequests 共有と同じ)。
func TestFetchRSS_ConcurrencyAllowsJoin(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	h.SetMaxConcurrentFetches(1)

	target := srv.URL + "/feed"
	results := make(chan int, 2)
	for range 2 {
		go func() {
			c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(target))
			_ = h.Fetch(c)
			results <- rec.Code
		}()
	}
	// 両方が inflight に入った (= 2 本目が 503 されず合流した) のを確認して解放。
	require.Eventually(t, func() bool {
		h.inflightMu.Lock()
		defer h.inflightMu.Unlock()
		return h.inflight[target] == 2 || len(results) == 2
	}, time.Second, time.Millisecond)
	close(release)
	for range 2 {
		assert.Equal(t, http.StatusOK, <-results)
	}
}

// TestFetchRSS_FinalURLProtocolRechecked: リダイレクト後の最終 URL が
// http(s) 以外なら FETCH_RSS_FAILED (defense-in-depth、upstream 準拠)。
func TestFetchRSS_FinalURLProtocolRechecked(t *testing.T) {
	client := &http.Client{
		Timeout: FetchTimeout,
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			// transport 層で最終 URL を非 http(s) に差し替えた状況を再現する。
			req.URL.Scheme = "ftp"
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(sampleRSS2)),
				Request:    req,
			}, nil
		}),
	}
	h := New(client, testUserAgent)
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape("http://example.com/feed"))
	require.NoError(t, h.Fetch(c))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "FETCH_RSS_FAILED")
}

// TestFetchRSS_SlotReleasedAfterFetch: 並行スロットは fetch 完了時点で解放
// される (レスポンス書き込みまで保持すると、body を読まないクライアントが
// socket buffer を埋めるだけで endpoint 全体を恒久 503 にできる)。
func TestFetchRSS_SlotReleasedAfterFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	// 成功経路。
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL+"/ok"))
	require.NoError(t, h.Fetch(c))
	require.Equal(t, http.StatusOK, rec.Code)
	h.inflightMu.Lock()
	assert.Empty(t, h.inflight, "成功後にスロットが残らない")
	h.inflightMu.Unlock()

	// 失敗経路 (dial error)。
	failing := &http.Client{Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("boom")
	})}
	h2 := New(failing, testUserAgent)
	c2, rec2 := newRequestCtx(http.MethodGet, "/api/fetch-rss?url=http://example.com/feed")
	require.NoError(t, h2.Fetch(c2))
	require.Equal(t, http.StatusUnprocessableEntity, rec2.Code)
	h2.inflightMu.Lock()
	assert.Empty(t, h2.inflight, "失敗後もスロットが残らない")
	h2.inflightMu.Unlock()
}

// TestFetchRSS_SlotReleasedOnPanic: singleflight は fn 内 panic を caller に
// 再 panic させるため、defer 無しで解放するとスロットが恒久リークする。
func TestFetchRSS_SlotReleasedOnPanic(t *testing.T) {
	panicking := &http.Client{Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		panic("boom")
	})}
	h := New(panicking, testUserAgent)
	c, _ := newRequestCtx(http.MethodGet, "/api/fetch-rss?url=http://example.com/feed")
	assert.Panics(t, func() { _ = h.Fetch(c) })
	h.inflightMu.Lock()
	defer h.inflightMu.Unlock()
	assert.Empty(t, h.inflight, "panic 経路でもスロットは解放される")
}

// TestFetchRSS_SlotFreeDuringResponseWrite: スロットは fetch 完了時点で解放
// され、レスポンス書き込み中は占有していない (書き込みでブロックする
// クライアントが endpoint 全体を 503 にできないことの回帰テスト)。
func TestFetchRSS_SlotFreeDuringResponseWrite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	inflightDuringWrite := -1
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(srv.URL+"/feed"), nil)
	rec := &slotObservingRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		onWrite: func() {
			h.inflightMu.Lock()
			inflightDuringWrite = len(h.inflight)
			h.inflightMu.Unlock()
		},
	}
	require.NoError(t, h.Fetch(e.NewContext(req, rec)))
	assert.Equal(t, 0, inflightDuringWrite, "レスポンス書き込み中はスロットを保持していない")
}

// slotObservingRecorder は Write の瞬間に hook を呼ぶ ResponseWriter。
type slotObservingRecorder struct {
	*httptest.ResponseRecorder
	onWrite func()
}

func (r *slotObservingRecorder) Write(b []byte) (int, error) {
	if r.onWrite != nil {
		r.onWrite()
	}
	return r.ResponseRecorder.Write(b)
}

// TestFetchRSS_HostNormalization: host の大文字小文字 / default port 違いは
// 同一 cache entry に正規化される (WHATWG URL 相当)。
func TestFetchRSS_HostNormalization(t *testing.T) {
	u, ok := normalizeFeedURL("http://ExAmPle.COM:80/Feed?a=B#frag")
	require.True(t, ok)
	assert.Equal(t, "http://example.com/Feed?a=B", u, "host 小文字化 + default port 除去 + fragment 除去、path/query は保持")

	u, ok = normalizeFeedURL("http://example.com:080/feed")
	require.True(t, ok)
	assert.Equal(t, "http://example.com/feed", u, "非 canonical な default port 表記も畳む")

	u, ok = normalizeFeedURL("http://example.com")
	require.True(t, ok)
	assert.Equal(t, "http://example.com/", u, "空 path は / に正規化")

	u, ok = normalizeFeedURL("http://example.com:08080/x")
	require.True(t, ok)
	assert.Equal(t, "http://example.com:8080/x", u, "非 canonical な非 default port も数値正規化")

	u, ok = normalizeFeedURL("https://Example.com:8443/x")
	require.True(t, ok)
	assert.Equal(t, "https://example.com:8443/x", u, "非 default port は保持")

	u, ok = normalizeFeedURL("http://[2001:DB8::1]:80/feed")
	require.True(t, ok)
	assert.Equal(t, "http://[2001:db8::1]/feed", u, "IPv6 リテラルの角括弧を保つ")
}

// TestFetchRSS_URLLengthBoundary: 8192 字ちょうどは通り、超過は弾く。
func TestFetchRSS_URLLengthBoundary(t *testing.T) {
	base := "http://example.com/"
	exact := base + strings.Repeat("a", MaxURLLength-len(base))
	require.Len(t, exact, MaxURLLength)
	_, ok := normalizeFeedURL(exact)
	assert.True(t, ok, "8192 字ちょうどは許可")

	_, ok = normalizeFeedURL(exact + "a")
	assert.False(t, ok, "8193 字は拒否")
}

// TestFetchRSS_PostPrefersBody: POST は upstream 同様 body を優先する。
func TestFetchRSS_PostPrefersBody(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/fetch-rss?url="+url.QueryEscape(srv.URL+"/from-query"),
		strings.NewReader(`{"url":"`+srv.URL+`/from-body"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Fetch(e.NewContext(req, rec)))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "/from-body", gotPath)
}

// TestFetchRSS_PostIgnoresQuery pins that a POST reads only the body, as
// upstream's ApiCallService does: an unbindable body or a url that is not a
// string is 400 INVALID_PARAM, and a body without url is the required error,
// even when the query carries a url (#3330).
func TestFetchRSS_PostIgnoresQuery(t *testing.T) {
	fetched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched = true
		fmt.Fprint(w, sampleRSS2)
	}))
	t.Cleanup(srv.Close)
	h := newTestHandler(t, srv)

	cases := []struct {
		name, body, wantParam string
	}{
		{"unbindable body", `[]`, ""},
		{"url is a number", `{"url":1}`, ""},
		{"url is null", `{"url":null}`, ""},
		{"url missing", `{}`, "url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequest(http.MethodPost, "/api/fetch-rss?url="+url.QueryEscape(srv.URL), strings.NewReader(tc.body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			require.NoError(t, h.Fetch(e.NewContext(req, rec)))
			testutil.AssertInvalidParam(t, rec)
			if tc.wantParam != "" {
				assert.Contains(t, rec.Body.String(), `"param":"`+tc.wantParam+`"`)
			} else {
				// 型違いを「url が無い」と取り違えていないこと。
				assert.NotContains(t, rec.Body.String(), `"param":"url"`)
			}
		})
	}
	assert.False(t, fetched, "query の url を取りに行ってはいけない")
}

// IPv6 リテラル (zone id 付き含む) が正規化で壊れないこと。
func TestFetchRSS_IPv6ZoneNormalization(t *testing.T) {
	u, ok := normalizeFeedURL("http://[fe80::1%25eth0]/feed")
	require.True(t, ok)
	assert.Equal(t, "http://[fe80::1%25eth0]/feed", u, "zone id の %25 エンコードを保つ")
	parsed, err := url.Parse(u)
	require.NoError(t, err)
	assert.Equal(t, "fe80::1%eth0", parsed.Hostname())
}

// host が空の URL (`http://:80/x`) は WHATWG の new URL() が throw するので
// INVALID_URL (400) に倒す。素通しすると壊れた cache key を作った上で 422 に
// なり error shape が upstream とずれる。
func TestFetchRSS_EmptyHostWithPortRejected(t *testing.T) {
	for _, raw := range []string{"http://:80/feed", "http://:8080/x"} {
		_, ok := normalizeFeedURL(raw)
		assert.False(t, ok, "host 空の URL は拒否: %s", raw)
	}
}
