package activitypub

import (
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

	"github.com/elythia-network/elythia/internal/safehttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestWebFingerClient(t *testing.T, handler http.HandlerFunc) (*WebFingerClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewWebFingerClient(srv.Client(), "test-agent")
	c.SetEndpointOverride(func(_ string) string { return srv.URL + "/.well-known/webfinger" })
	return c, srv
}

func TestWebFingerClient_LookupActorURI_Success(t *testing.T) {
	c, srv := newTestWebFingerClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/.well-known/webfinger", r.URL.Path)
		resource := r.URL.Query().Get("resource")
		assert.Equal(t, "acct:alice@example.com", resource)
		assert.Contains(t, r.Header.Get("Accept"), "application/jrd+json")
		assert.Equal(t, "test-agent", r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/jrd+json")
		fmt.Fprint(w, `{
			"subject": "acct:alice@example.com",
			"links": [
				{"rel": "http://webfinger.net/rel/profile-page", "type": "text/html", "href": "https://example.com/@alice"},
				{"rel": "self", "type": "application/activity+json", "href": "https://example.com/users/alice"}
			]
		}`)
	})
	_ = srv
	uri, err := c.LookupActorURI("alice", "example.com")
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/users/alice", uri)
}

func TestWebFingerClient_LookupActorURI_LDJSONTypeAccepted(t *testing.T) {
	c, _ := newTestWebFingerClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{
			"subject": "acct:alice@example.com",
			"links": [
				{"rel": "self", "type": "application/ld+json; profile=\"https://www.w3.org/ns/activitystreams\"", "href": "https://example.com/users/alice"}
			]
		}`)
	})
	uri, err := c.LookupActorURI("alice", "example.com")
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/users/alice", uri)
}

func TestWebFingerClient_LookupActorURI_NoSelfLink(t *testing.T) {
	c, _ := newTestWebFingerClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{
			"subject": "acct:alice@example.com",
			"links": [
				{"rel": "http://webfinger.net/rel/profile-page", "type": "text/html", "href": "https://example.com/@alice"}
			]
		}`)
	})
	_, err := c.LookupActorURI("alice", "example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no self link")
}

func TestWebFingerClient_LookupActorURI_SelfLinkWithUnrelatedType(t *testing.T) {
	c, _ := newTestWebFingerClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{
			"subject": "acct:alice@example.com",
			"links": [
				{"rel": "self", "type": "text/html", "href": "https://example.com/@alice"}
			]
		}`)
	})
	_, err := c.LookupActorURI("alice", "example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no self link")
}

func TestWebFingerClient_LookupActorURI_SelfLinkEmptyHref(t *testing.T) {
	c, _ := newTestWebFingerClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{
			"subject": "acct:alice@example.com",
			"links": [
				{"rel": "self", "type": "application/activity+json"}
			]
		}`)
	})
	_, err := c.LookupActorURI("alice", "example.com")
	require.Error(t, err)
}

func TestWebFingerClient_LookupActorURI_ResponseTooLarge(t *testing.T) {
	// #323: size cap を超えるレスポンスは ErrResponseTooLarge を伝搬する。
	oversized := make([]byte, int(MaxBodyBytes)+10)
	c, _ := newTestWebFingerClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(oversized)
	})
	_, err := c.LookupActorURI("alice", "example.com")
	require.Error(t, err)
	assert.True(t, errors.Is(err, safehttp.ErrResponseTooLarge))
}

func TestWebFingerClient_LookupActorURI_InvalidJSON(t *testing.T) {
	c, _ := newTestWebFingerClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `not json`)
	})
	_, err := c.LookupActorURI("alice", "example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse response")
}

func TestWebFingerClient_LookupActorURI_Non2xx(t *testing.T) {
	c, _ := newTestWebFingerClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := c.LookupActorURI("ghost", "example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 404")
}

func TestWebFingerClient_LookupActorURI_NetworkError(t *testing.T) {
	c := NewWebFingerClient(http.DefaultClient, "ua")
	c.SetEndpointOverride(func(_ string) string { return "http://127.0.0.1:1/.well-known/webfinger" })
	_, err := c.LookupActorURI("a", "example.com")
	require.Error(t, err)
}

func TestWebFingerClient_LookupActorURI_EmptyArgs(t *testing.T) {
	c := NewWebFingerClient(nil, "ua")
	_, err := c.LookupActorURI("", "example.com")
	require.Error(t, err)
	_, err = c.LookupActorURI("alice", "")
	require.Error(t, err)
}

func TestWebFingerClient_DefaultEndpoint(t *testing.T) {
	// SetEndpointOverride を使わないデフォルト経路の確認。
	// 実ネットワークには行かないが、URL が https://<host>/.well-known/webfinger で
	// 組まれていることを確認する。
	c := NewWebFingerClient(nil, "ua")
	got := c.endpoint("example.com")
	assert.Equal(t, "https://example.com/.well-known/webfinger", got)
	assert.True(t, strings.HasPrefix(got, "https://"))
}

func TestWebFingerClient_ResourceIsURLEncoded(t *testing.T) {
	c, _ := newTestWebFingerClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.RawQuery
		// query parser decodes the value for us, but the raw must contain
		// a percent-encoded "@" as part of a correct resource query.
		decoded, _ := url.ParseQuery(raw)
		assert.Equal(t, "acct:edge@ex.example", decoded.Get("resource"))
		fmt.Fprint(w, `{
			"subject": "acct:edge@ex.example",
			"links": [{"rel": "self", "type": "application/activity+json", "href": "https://ex.example/users/edge"}]
		}`)
	})
	uri, err := c.LookupActorURI("edge", "ex.example")
	require.NoError(t, err)
	assert.Equal(t, "https://ex.example/users/edge", uri)
}

// 同一 acct への並行 lookup は 1 つの HTTP リクエストに collapse される
// (#300 3-7)。サーバ側で in-flight な状態を作って 16 並行呼び出しが全部
// 同じ結果を受け取り、HTTP fetch 数が並行数より大幅に少ないことを担保する。
//
// flake 抑制 (#789): 旧実装は close(gate) 前に追従 goroutine が
// singleflight.Do に到達するのを sleep 無しで期待していたため、CI の
// 高負荷スケジューリング下では leader が gate 解放前に完了して 2 回目以降
// の wave が新たに leader になり HTTP fetch が 3 まで届くケースが稀に
// あった。本テストでは:
//  1. 全 goroutine が c.LookupActorURI を呼び出した瞬間に counter を進める
//  2. counter == N まで wait してから少し息を入れて singleflight 登録の
//     内部処理が完了するのを待つ
//  3. 閾値も「並行数の 1/4 以下」と寛容に取る (= 16 並行なら 4 fetch まで
//     OK)。dedupe が機能していれば余裕で満たせる、scheduling 由来の race
//     では破綻しない値。
func TestWebFingerClient_LookupActorURI_DedupesConcurrentCalls(t *testing.T) {
	var calls atomic.Int64
	gate := make(chan struct{})
	c, _ := newTestWebFingerClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		<-gate // すべての呼び出しが singleflight 集約待ちになるまで blockする
		fmt.Fprint(w, `{
			"subject": "acct:alice@example.com",
			"links": [{"rel": "self", "type": "application/activity+json", "href": "https://example.com/users/alice"}]
		}`)
	})

	const (
		// concurrentCallers は 1 acct への並行 lookup 数。
		concurrentCallers = 16
		// enterTimeout は全 goroutine が LookupActorURI に到達するまでの上限。
		// 通常は ms 以下で完了するが、CI race 環境でも余裕を持たせる。
		enterTimeout = 2 * time.Second
		// singleflightSettle は LookupActorURI に入ってから singleflight.Do の
		// 内部 mutex 取得 + key 登録が完了するまでの内部 race window。CI race
		// 環境でも μs オーダーで終わる想定だが、数倍のマージンを確保する。
		singleflightSettle = 20 * time.Millisecond
		// maxFetches は dedupe が機能している前提での HTTP fetch 数の上限。
		// 完全 collapse なら 1 fetch、scheduling 由来の二段ウェーブで 3-4 まで
		// 届くことがあるので concurrentCallers/4 (= 4) を許容する。dedupe が
		// 完全破綻すると calls == concurrentCallers になるため regression 検出
		// 能力は維持される。
		maxFetches = concurrentCallers / 4
	)
	var wg sync.WaitGroup
	var entered atomic.Int64
	results := make([]string, concurrentCallers)
	errs := make([]error, concurrentCallers)
	for i := 0; i < concurrentCallers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			entered.Add(1)
			results[i], errs[i] = c.LookupActorURI("alice", "example.com")
		}(i)
	}
	// 全 goroutine が LookupActorURI 呼び出し直前まで来たことを確認する
	// (= singleflight.Do に enqueue されるまでもう一息)。spin-wait で
	// 待つが timeout を切って test がハングしないようにする。
	deadline := time.Now().Add(enterTimeout)
	for entered.Load() < int64(concurrentCallers) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d goroutines reached LookupActorURI", entered.Load(), concurrentCallers)
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(singleflightSettle)
	close(gate)
	wg.Wait()

	for i := 0; i < concurrentCallers; i++ {
		require.NoError(t, errs[i], "call %d", i)
		assert.Equal(t, "https://example.com/users/alice", results[i])
	}
	assert.LessOrEqual(t, calls.Load(), int64(maxFetches),
		"singleflight must collapse concurrent same-acct lookups; got %d HTTP fetches for %d callers", calls.Load(), concurrentCallers)
}

func TestIsActivityPubLinkType(t *testing.T) {
	cases := map[string]bool{
		"application/activity+json": true,
		"application/ld+json":       true,
		"application/ld+json; profile=\"https://www.w3.org/ns/activitystreams\"": true,
		"application/jrd+json": false,
		"text/html":            false,
		"":                     false,
	}
	for input, want := range cases {
		assert.Equal(t, want, isActivityPubLinkType(input), input)
	}
}

// --- host validation ---

// recordingTransport captures the outbound URL without ever dialing.
type recordingTransport struct {
	urls []string
	body string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.urls = append(r.urls, req.URL.String())
	body := r.body
	if body == "" {
		body = `{"subject":"acct:alice@a.example","links":[{"rel":"self","type":"application/activity+json","href":"https://a.example/users/alice"}]}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/jrd+json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

// host は URL に素で連結されるので、authority 以外に化ける入力は
// **HTTP を 1 本も出さずに** 弾く。
func TestWebFingerClient_RejectsMalformedHost(t *testing.T) {
	for _, host := range []string{
		"a.example/x",         // path injection
		"a.example/../evil",   // path traversal
		"a.example@b.example", // authority すり替え (url.Parse の host は b.example)
		"a.example?x=1",       // query injection
		"a.example#frag",      // fragment injection
		"a.example/.well-known/nodeinfo",
		"a.example ",              // trailing space
		"a b.example",             // 空白
		"a.example\\evil.example", // backslash
		"",                        // 空文字 (既存の guard)
	} {
		t.Run(host, func(t *testing.T) {
			rt := &recordingTransport{}
			c := NewWebFingerClient(&http.Client{Transport: rt}, "ua")
			_, err := c.LookupActorURI("alice", host)
			require.Error(t, err, "host %q must be rejected", host)
			assert.Empty(t, rt.urls, "rejected host must not produce an outbound request")
		})
	}
}

// 正常な host は従来どおり `https://<host>/.well-known/webfinger` を叩く。
// ゲートが広すぎないことの確認 (port 付きも通す)。
func TestWebFingerClient_AcceptsPlainHost(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"a.example", "https://a.example/.well-known/webfinger?resource=acct%3Aalice%40a.example"},
		{"a.example:8443", "https://a.example:8443/.well-known/webfinger?resource=acct%3Aalice%40a.example%3A8443"},
		{"xn--eckve.example", "https://xn--eckve.example/.well-known/webfinger?resource=acct%3Aalice%40xn--eckve.example"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			rt := &recordingTransport{}
			c := NewWebFingerClient(&http.Client{Transport: rt}, "ua")
			uri, err := c.LookupActorURI("alice", tc.host)
			require.NoError(t, err)
			assert.Equal(t, "https://a.example/users/alice", uri)
			require.Len(t, rt.urls, 1)
			assert.Equal(t, tc.want, rt.urls[0])
		})
	}
}

func TestIsPlainHost(t *testing.T) {
	cases := map[string]bool{
		"a.example":           true,
		"a.example:8443":      true,
		"xn--eckve.example":   true,
		"[::1]":               true,
		"":                    false,
		"a.example/x":         false,
		"a.example@b.example": false,
		"a.example?x":         false,
		"a.example#f":         false,
		"a.example/":          false,
		"a%2Eexample":         false,
		"https://a.example":   false,
	}
	for input, want := range cases {
		assert.Equal(t, want, isPlainHost(input), input)
	}
}

// --- link href scheme validation ---

// self link の href は相手サーバーが自由に決められ、そのまま ResolveActor に
// 渡る。http(s) 以外の scheme は採用しない。
func TestWebFingerClient_RejectsNonHTTPHref(t *testing.T) {
	for _, href := range []string{
		"javascript:alert(1)",
		"file:///etc/passwd",
		"data:application/activity+json,{}",
		"ftp://a.example/users/alice",
		"/users/alice",
		"users/alice",
	} {
		t.Run(href, func(t *testing.T) {
			c, _ := newTestWebFingerClient(t, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"subject":"acct:alice@a.example","links":[{"rel":"self","type":"application/activity+json","href":%q}]}`, href)
			})
			_, err := c.LookupActorURI("alice", "a.example")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "no self link")
		})
	}
}

// http:// は通す。https 限定にすると http のみの開発用インスタンスと
// 連合できなくなるうえ、本体の他の scheme 判定 (remoteNoteURL) とも揃わない。
func TestWebFingerClient_AcceptsHTTPHref(t *testing.T) {
	for _, href := range []string{"http://a.example/users/alice", "HTTPS://a.example/users/alice"} {
		t.Run(href, func(t *testing.T) {
			c, _ := newTestWebFingerClient(t, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"subject":"acct:alice@a.example","links":[{"rel":"self","type":"application/activity+json","href":%q}]}`, href)
			})
			uri, err := c.LookupActorURI("alice", "a.example")
			require.NoError(t, err)
			assert.Equal(t, href, uri)
		})
	}
}

// 使えない href を持つ self link があっても、後ろに正当な self link があれば
// そちらを採る (最初の 1 件で打ち切らない)。
func TestWebFingerClient_SkipsBadHrefAndKeepsScanning(t *testing.T) {
	c, _ := newTestWebFingerClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"subject":"acct:alice@a.example","links":[
			{"rel":"self","type":"application/activity+json","href":"javascript:alert(1)"},
			{"rel":"self","type":"application/activity+json","href":"https://a.example/users/alice"}
		]}`)
	})
	uri, err := c.LookupActorURI("alice", "a.example")
	require.NoError(t, err)
	assert.Equal(t, "https://a.example/users/alice", uri)
}

func TestIsHTTPScheme(t *testing.T) {
	cases := map[string]bool{
		"http://a.example":    true,
		"https://a.example":   true,
		"HTTPS://a.example":   true,
		"HtTp://a.example":    true,
		"javascript:alert(1)": false,
		"file:///etc/passwd":  false,
		"//a.example/x":       false,
		"":                    false,
		" https://a.example":  false,
	}
	for input, want := range cases {
		assert.Equal(t, want, isHTTPScheme(input), input)
	}
}
