package mediaproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/safehttp"
)

// リモートから取れなかったことと「無い」ことを混ぜない (#3034)。
//
// **潰すと handler が 404 + `Cache-Control: max-age=86400` で返す。**
// DNS 失敗・接続拒否・TLS エラー・タイムアウトはどれもリモート側の一時障害
// なので、それを 1 日キャッシュさせると復旧しても画像が壊れたままになる。
// #2913 (403 が 1 日キャッシュされてアイコンが 1 日壊れた) と同型で、
// #2792 の「lookup error を種別を見ずに 4xx へ潰さない」にも反する。

// deadAddr returns an address nothing is listening on.
func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// **DNS 失敗は入れていない。** `httpClient.Do` の同じ 1 行を通るので変異
// 検出力は増えず、名前解決はネットワーク依存で flaky 側に倒れる。TLS 側は
// 下の別テストが踏む。
func TestFetchRemote_TransportFailureIsNotNotFound(t *testing.T) {
	// 接続拒否 (誰も listen していないポート)。
	url := "http://" + deadAddr(t) + "/a.png"
	s := testService(map[string]bool{url: true})

	_, err := s.Fetch(context.Background(), url, ModeDefault, FormatWebP, true)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamUnavailable)
	assert.NotErrorIs(t, err, ErrNotFound,
		"リモート取得の失敗を 404 に潰している")
}

// handler の 504 分岐が前提にしている stdlib の挙動を固定する (#3034)。
//
// **`http.Client.Timeout` に当たったエラーが `context.DeadlineExceeded` を
// 満たすことは、Go のバージョンに依存する。** go1.26 では `*url.Error` の
// 中身が `*http.timeoutError` で、その `Is` が真を返す。go1.22 では
// `*http.httpError` になり **偽**だった (実測)。toolchain が下がると
// `handler.go` の 504 分岐が黙って 502 に退化し、他のどのテストも落ちない。
//
// **`context.Canceled` には偽であることも見る** — 真だと handler の判定順
// (Canceled を先に見る) で timeout が 499 に化ける。
func TestClientTimeoutSatisfiesDeadlineExceeded(t *testing.T) {
	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
		w.(http.Flusher).Flush()
		<-block
	}))
	defer ts.Close()
	defer close(block)

	c := &http.Client{Timeout: 100 * time.Millisecond}
	resp, err := c.Get(ts.URL)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	require.Error(t, err)

	// `fetchRemote` と同じ包み方をしても判定が生きること。
	wrapped := fmt.Errorf("%w: read remote: %w", ErrUpstreamUnavailable, err)
	assert.ErrorIs(t, wrapped, context.DeadlineExceeded,
		"Client.Timeout が DeadlineExceeded を満たさない (504 分岐が効かない)")
	assert.NotErrorIs(t, wrapped, context.Canceled,
		"timeout が Canceled も満たすと 499 に化ける")
	assert.ErrorIs(t, wrapped, ErrUpstreamUnavailable)
}

// TLS を話さない相手に https で行った場合も同じ扱い。
//
// **平文の HTTP サーバーに https で繋ぐと handshake で落ちる。** 実運用では
// 証明書切れ・自己署名・SNI 不一致がこの枝に来る。どれも「無い」ではない。
func TestFetchRemote_TLSFailureIsNotNotFound(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(makePNG())
	}))
	defer plain.Close()

	url := "https" + plain.URL[len("http"):]
	s := testService(map[string]bool{url: true})

	_, err := s.Fetch(context.Background(), url, ModeDefault, FormatWebP, true)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamUnavailable)
	assert.NotErrorIs(t, err, ErrNotFound)
}

// proxy が取りに行けない URL は「リモートの障害」ではない (#3034)。
//
// **相対 URL がこの経路に来る。** `/identicon/<id>` は `user.avatarUrl` 列に
// 入るので DB allowlist を通り、同一オリジン判定 (`instanceURL + "/files/"`)
// を外れて `fetchRemote` に落ちる。`httpClient.Do` に渡せば
// `unsupported protocol scheme` で失敗するが、それを 502 + 5 分キャッシュに
// すると (a) 監視で「相手が落ちている」と読め、(b) 恒久的に直らないものを
// 5 分ごとに引き直し続ける。
func TestFetchRemote_UnfetchableURLIsBadRequest(t *testing.T) {
	for name, raw := range map[string]string{
		"相対 URL":    "/identicon/alice",
		"scheme 無し": "//example.com/a.png",
		"非 http(s)": "data:image/png;base64,iVBORw0KGgo=",
		"host 無し":   "http:///a.png",
		// **`http.NewRequestWithContext` より前に判定している証拠。**
		// 後ろに置くと parse 失敗が `create request` 側で返り、この形は
		// generic 500 + `max-age=300` に落ちる。
		"制御文字入り": "http://exa\x00mple.com/a.png",
	} {
		t.Run(name, func(t *testing.T) {
			s := testService(map[string]bool{raw: true})
			_, err := s.Fetch(context.Background(), raw, ModeDefault, FormatWebP, true)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrBadRequest)
			assert.NotErrorIs(t, err, ErrUpstreamUnavailable,
				"取りに行けない URL をリモートの障害に混ぜている")
			assert.NotErrorIs(t, err, ErrNotFound)
		})
	}
}

// SSRF ガードの拒否を「相手の障害」と分ける (#3037)。
//
// **こちらが意図的に遮断した事実**なので、`ErrUpstreamUnavailable` (502) に
// 混ぜると監視で「相手インスタンスが落ちている」と読める。**キャッシュ時間は
// 変えていない** — SSRF の可否は毎リクエストの DNS 解決で決まるので恒久的では
// なく、1 日にすると #2913 の窓を 288 倍に広げる (1 周目で実測)。
//
// #3034 では service 側だけ分けて handler に受け口を作らず、500 に落ちる
// 状態を作って巻き戻した。**写像を足すときは handler のテストと対で入れる** —
// `internal/api/proxy` の `TestHandle_BlockedTargetIsForbidden` が対。
func TestFetchRemote_SSRFBlockedIsNotUpstreamUnavailable(t *testing.T) {
	// testService は 127.0.0.0/8 を許可しているので、許可していない
	// プライベート帯を使う。
	s := NewService(
		"https://example.com", "Misskey/2026.5.1 (https://example.com)",
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{"http://10.0.0.1/a.png": true}},
		[]byte("test-secret"), nil,
	)

	_, err := s.Fetch(context.Background(), "http://10.0.0.1/a.png", ModeDefault, FormatWebP, true)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTargetBlocked)
	assert.NotErrorIs(t, err, ErrUpstreamUnavailable,
		"SSRF 拒否を相手の障害に混ぜている")
	// #3034 の本題 (404 + 1 日キャッシュ) にも戻っていないこと。
	assert.NotErrorIs(t, err, ErrNotFound)
	// **元の原因を捨てない。** 捨てるとログから遮断の理由が消える。
	assert.ErrorIs(t, err, safehttp.ErrSSRFBlocked)
}

// リモートの一時障害は従来どおり `ErrUpstreamUnavailable` のまま (#3037)。
//
// **既存テストと重複している。** 「全部 `ErrTargetBlocked` にする」変異は
// `TestFetchRemote_TransportFailureIsNotNotFound` /
// `TestFetchRemote_TLSFailureIsNotNotFound` も落とす (実測)。それでも残すのは、
// SSRF 側の 1 本と並べて分類の境界が読めるようにするため。
func TestFetchRemote_TransientFailureIsNotBlocked(t *testing.T) {
	url := "http://" + deadAddr(t) + "/a.png"
	s := testService(map[string]bool{url: true})

	_, err := s.Fetch(context.Background(), url, ModeDefault, FormatWebP, true)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamUnavailable)
	assert.NotErrorIs(t, err, ErrTargetBlocked)
}

// リモートが本当に「無い」と言ったときは従来どおり ErrNotFound。
//
// **これが無いと「全部 502 にする」実装が緑で通る。** それをやると本当に
// 消えた画像まで 5 分ごとに取りに行くことになり、長期キャッシュが効かなくなる。
func TestFetchRemote_404And410StayNotFound(t *testing.T) {
	for name, code := range map[string]int{
		"404": http.StatusNotFound,
		"410": http.StatusGone,
	} {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer ts.Close()

			s := testService(map[string]bool{ts.URL + "/x.png": true})
			_, err := s.Fetch(context.Background(), ts.URL+"/x.png", ModeDefault, FormatWebP, true)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrNotFound)
			assert.NotErrorIs(t, err, ErrUpstreamUnavailable)
		})
	}
}

// 取得中に利用者が切った場合、`context.Canceled` が生き残ること。
//
// **`%w` を 2 つ使っている理由がこれ。** 潰していた頃は `ErrNotFound` に
// なって 404 + 1 日キャッシュが返っていた。ラップを 1 つに減らすと
// handler が離脱を 499 に振り分けられなくなる。
func TestFetchRemote_ClientCancelSurvivesAsContextCanceled(t *testing.T) {
	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
	}))
	defer ts.Close()
	defer close(block)

	s := testService(map[string]bool{ts.URL + "/slow.png": true})

	ctx, cancel := context.WithCancel(context.Background())
	go cancel()

	_, err := s.Fetch(ctx, ts.URL+"/slow.png", ModeDefault, FormatWebP, true)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled,
		"利用者の離脱が context.Canceled として届いていない")
	assert.NotErrorIs(t, err, ErrNotFound,
		"利用者の離脱を 404 に潰している")
}

// 途中で切れた転送も「取れなかった」側。
//
// Content-Length を宣言しておいて途中で接続を落とすと、`io.ReadAll` が
// `unexpected EOF` を返す。これを generic error のままにすると 500 になり、
// 502 を出す理由 (リモート側の障害だと運用者に分かる) が失われる。
func TestFetchRemote_TruncatedBodyIsUpstreamUnavailable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write([]byte("partial"))
		// hijack して黙って切る。
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/cut.png": true})
	_, err := s.Fetch(context.Background(), ts.URL+"/cut.png", ModeDefault, FormatWebP, true)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamUnavailable)
	assert.NotErrorIs(t, err, ErrNotFound)
	// **元のエラーを捨てない。** sentinel だけにすると、ログに
	// 「upstream fetch failed」としか出ず原因が追えなくなる。
	assert.Contains(t, err.Error(), "read remote")
	assert.NotEqual(t, ErrUpstreamUnavailable.Error(), err.Error(),
		"元のエラーが失われている")
}
