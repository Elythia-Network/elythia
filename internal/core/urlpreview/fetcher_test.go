package urlpreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetcher_Disabled(t *testing.T) {
	f := NewFetcher(Config{Enabled: false}, nil)
	_, err := f.Fetch(context.Background(), "https://example.com")
	assert.ErrorIs(t, err, ErrDisabled)
}

func newTestFetcher(cfg Config) *Fetcher {
	f := NewFetcher(cfg, nil)
	f.SetHTTPClient(&http.Client{Timeout: f.client.Timeout})
	return f
}

func TestFetcher_FetchAndParse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head>
			<meta property="og:title" content="Hello">
			<meta property="og:description" content="World">
		</head><body></body></html>`))
	}))
	defer srv.Close()

	f := newTestFetcher(Config{Enabled: true, AllowRedirect: true, TimeoutMs: 5000, MaxContentLength: 1 << 20})
	result, err := f.Fetch(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "Hello", *result.Title)
	assert.Equal(t, "World", *result.Description)
}

// Shift_JIS の HTML response が UTF-8 に正規化されて parse される (#942)。
// Content-Type header に charset=shift_jis を載せて charset.NewReader が
// 自動判定することを確認する。
func TestFetcher_ShiftJISCharset(t *testing.T) {
	// 「こんにちは」を Shift_JIS で encode したバイト列。
	sjisHello := []byte{0x82, 0xb1, 0x82, 0xf1, 0x82, 0xc9, 0x82, 0xbf, 0x82, 0xcd}
	body := []byte(`<html><head><meta property="og:title" content="`)
	body = append(body, sjisHello...)
	body = append(body, []byte(`"></head></html>`)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=shift_jis")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	f := newTestFetcher(Config{Enabled: true, AllowRedirect: true, TimeoutMs: 5000, MaxContentLength: 1 << 20})
	result, err := f.Fetch(context.Background(), srv.URL)
	require.NoError(t, err)
	require.NotNil(t, result.Title)
	assert.Equal(t, "こんにちは", *result.Title)
}

// charset=euc-jp も同様に正規化される。
func TestFetcher_EUCJPCharset(t *testing.T) {
	// 「テスト」を EUC-JP で encode。
	eucHello := []byte{0xa5, 0xc6, 0xa5, 0xb9, 0xa5, 0xc8}
	body := []byte(`<html><head><meta property="og:title" content="`)
	body = append(body, eucHello...)
	body = append(body, []byte(`"></head></html>`)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=euc-jp")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	f := newTestFetcher(Config{Enabled: true, AllowRedirect: true, TimeoutMs: 5000, MaxContentLength: 1 << 20})
	result, err := f.Fetch(context.Background(), srv.URL)
	require.NoError(t, err)
	require.NotNil(t, result.Title)
	assert.Equal(t, "テスト", *result.Title)
}

func TestFetcher_NonHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("PNG data"))
	}))
	defer srv.Close()

	f := newTestFetcher(Config{Enabled: true, AllowRedirect: true, TimeoutMs: 5000, MaxContentLength: 1 << 20})
	result, err := f.Fetch(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, srv.URL, result.URL)
	assert.Nil(t, result.Title)
}

func TestFetcher_TooLargeContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", "999999999")
		_, _ = w.Write([]byte("<html></html>"))
	}))
	defer srv.Close()

	f := newTestFetcher(Config{Enabled: true, AllowRedirect: true, TimeoutMs: 5000, MaxContentLength: 1000})
	_, err := f.Fetch(context.Background(), srv.URL)
	assert.ErrorIs(t, err, ErrTooLarge)
}

func TestFetcher_RequireContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// chunked 転送 (Content-Length なし) にするため Flush を使う。
		w.Header().Del("Content-Length")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("<html></html>"))
		flusher.Flush()
	}))
	defer srv.Close()

	f := newTestFetcher(Config{Enabled: true, AllowRedirect: true, TimeoutMs: 5000, MaxContentLength: 1 << 20, RequireContentLength: true})
	_, err := f.Fetch(context.Background(), srv.URL)
	assert.ErrorIs(t, err, ErrTooLarge)
}

func TestFetcher_PrivateIP(t *testing.T) {
	// SSRF チェック付きの Fetcher を使う (newTestFetcher ではなく NewFetcher)
	f := NewFetcher(Config{Enabled: true, AllowRedirect: true, TimeoutMs: 5000, MaxContentLength: 1 << 20}, nil)
	_, err := f.Fetch(context.Background(), "http://127.0.0.1:9999/test")
	assert.ErrorIs(t, err, ErrPrivateIP)
}

func TestFetcher_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := newTestFetcher(Config{Enabled: true, AllowRedirect: true, TimeoutMs: 5000, MaxContentLength: 1 << 20})
	_, err := f.Fetch(context.Background(), srv.URL)
	assert.ErrorIs(t, err, ErrFetchFailed)
}

func TestFetcher_ViaProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.RawQuery, "url=")
		_ = json.NewEncoder(w).Encode(Result{
			URL:    "https://example.com",
			Player: PlayerResult{Allow: []string{}},
		})
	}))
	defer proxy.Close()

	f := NewFetcher(Config{Enabled: true, SummaryProxyURL: proxy.URL, TimeoutMs: 5000, MaxContentLength: 1 << 20}, nil)
	result, err := f.Fetch(context.Background(), "https://example.com")
	require.NoError(t, err)
	assert.Equal(t, "https://example.com", result.URL)
}

func TestFetcher_NoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, "https://example.com", http.StatusFound)
	}))
	defer srv.Close()

	// AllowRedirect=false のとき 302 で止まる。SSRF チェックを外すために
	// newTestFetcher は使えないので直接 client を差し替える。
	f := NewFetcher(Config{Enabled: true, AllowRedirect: false, TimeoutMs: 5000, MaxContentLength: 1 << 20}, nil)
	f.SetHTTPClient(&http.Client{
		Timeout:       f.client.Timeout,
		CheckRedirect: f.client.CheckRedirect,
	})
	_, err := f.Fetch(context.Background(), srv.URL)
	assert.ErrorIs(t, err, ErrFetchFailed)
}

// SSRF 判定は safehttp 側で完結するため、ここではテストしない (#638)。
// urlpreview レイヤは「private IP アクセス時に ErrPrivateIP が伝播するか」
// だけを TestFetcher_PrivateIP で確認する。

func TestHashURL(t *testing.T) {
	h1 := hashURL("https://example.com")
	h2 := hashURL("https://example.com")
	h3 := hashURL("https://other.com")
	assert.Equal(t, h1, h2)
	assert.NotEqual(t, h1, h3)
	assert.Len(t, h1, 64)
}

// #739: SummaryProxyURL が設定されていれば fetchViaProxy 経由で取得する経路。
// proxy が JSON で Result 相当を返すと正しく decode される。
func TestFetcher_FetchViaProxy_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "https://example.com/page", r.URL.Query().Get("url"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"title":"From Proxy","url":"https://example.com/page"}`))
	}))
	defer srv.Close()

	f := newTestFetcher(Config{Enabled: true, SummaryProxyURL: srv.URL})
	res, err := f.Fetch(context.Background(), "https://example.com/page")
	require.NoError(t, err)
	require.NotNil(t, res.Title)
	assert.Equal(t, "From Proxy", *res.Title)
}

// proxy が malformed JSON を返した場合は ErrFetchFailed。
func TestFetcher_FetchViaProxy_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()
	f := newTestFetcher(Config{Enabled: true, SummaryProxyURL: srv.URL})
	_, err := f.Fetch(context.Background(), "https://example.com/page")
	assert.ErrorIs(t, err, ErrFetchFailed)
}

// ── 2026.7.0 #17635 urlPreviewSensitiveList / scheme 検証 ─────────────

func TestFetcher_SensitiveListMatchForcesSensitive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><meta property="og:title" content="x"></head></html>`))
	}))
	defer srv.Close()

	f := newTestFetcher(Config{Enabled: true, AllowRedirect: true, TimeoutMs: 5000, MaxContentLength: 1 << 20})
	f.SetSensitiveListProvider(func() []string { return []string{"127.0.0.1"} })
	result, err := f.Fetch(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.True(t, result.Sensitive, "URL が keyword 一致したら sensitive=true に上書き")
}

func TestFetcher_SensitiveListNoMatchKeepsFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><meta property="og:title" content="x"></head></html>`))
	}))
	defer srv.Close()

	f := newTestFetcher(Config{Enabled: true, AllowRedirect: true, TimeoutMs: 5000, MaxContentLength: 1 << 20})
	f.SetSensitiveListProvider(func() []string { return []string{"no-such-keyword"} })
	result, err := f.Fetch(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.False(t, result.Sensitive)
}

func TestValidateResultSchemes(t *testing.T) {
	httpsURL := "https://example.com"
	jsURL := "javascript:alert(1)"
	tests := []struct {
		name    string
		result  *Result
		wantErr bool
	}{
		{"http url ok", &Result{URL: "http://example.com"}, false},
		{"https url + player ok", &Result{URL: "https://example.com", Player: PlayerResult{URL: &httpsURL}}, false},
		{"non-http url rejected", &Result{URL: "javascript:alert(1)"}, true},
		{"non-http player rejected", &Result{URL: "https://example.com", Player: PlayerResult{URL: &jsURL}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateResultSchemes(tt.result)
			if tt.wantErr {
				assert.ErrorIs(t, err, ErrFetchFailed)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// summaly proxy の応答も scheme 検証を通す (operator-trusted な proxy でも
// 応答内容までは信頼しない。javascript: が frontend の href/iframe に流れる)。
func TestFetcher_ViaProxy_RejectsNonHTTPScheme(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Result{URL: "javascript:alert(1)"})
	}))
	defer proxy.Close()

	f := NewFetcher(Config{Enabled: true, SummaryProxyURL: proxy.URL, TimeoutMs: 5000, MaxContentLength: 1 << 20}, nil)
	_, err := f.Fetch(context.Background(), "https://example.com")
	assert.ErrorIs(t, err, ErrFetchFailed)
}

// proxy 経路でも urlPreviewSensitiveList の keyword 一致は効く。
func TestFetcher_ViaProxy_AppliesSensitiveList(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Result{URL: "https://nsfw.example/x"})
	}))
	defer proxy.Close()

	f := NewFetcher(Config{Enabled: true, SummaryProxyURL: proxy.URL, TimeoutMs: 5000, MaxContentLength: 1 << 20}, nil)
	f.SetSensitiveListProvider(func() []string { return []string{"nsfw.example"} })
	result, err := f.Fetch(context.Background(), "https://nsfw.example/x")
	require.NoError(t, err)
	assert.True(t, result.Sensitive)
}

// scheme は case-insensitive (HTTP:// を返す proxy/ページを誤って弾かない)。
func TestValidateResultSchemes_CaseInsensitive(t *testing.T) {
	assert.NoError(t, validateResultSchemes(&Result{URL: "HTTPS://example.com"}))
	upper := "HTTP://example.com/p"
	assert.NoError(t, validateResultSchemes(&Result{URL: "https://example.com", Player: PlayerResult{URL: &upper}}))
}

// thumbnail / icon が非 http(s) なら値を落とす (preview 自体は返す)。
// mk-go の ProxyMediaURL は host 無し URL を素通しするため、ここで落とさないと
// javascript:/data: がクライアントに出る。
func TestValidateResultSchemes_DropsNonHTTPThumbnailAndIcon(t *testing.T) {
	js := "javascript:alert(1)"
	data := "data:image/png;base64,AAAA"
	ok := "https://example.com/i.png"

	r := &Result{URL: "https://example.com", Thumbnail: &js, Icon: &data}
	require.NoError(t, validateResultSchemes(r))
	assert.Nil(t, r.Thumbnail, "javascript: thumbnail は落とす")
	assert.Nil(t, r.Icon, "data: icon は落とす")

	r2 := &Result{URL: "https://example.com", Thumbnail: &ok, Icon: &ok}
	require.NoError(t, validateResultSchemes(r2))
	assert.Equal(t, &ok, r2.Thumbnail, "http(s) の thumbnail は保持")
	assert.Equal(t, &ok, r2.Icon)
}

// scheme が http(s) でも、host を持たない thumbnail / icon は落とす。メディア
// プロキシは host を持つ絶対 URL しか包まないので、包まれずに画面へ出てしまう。
// 括弧や空白は符号化した値にする。
func TestValidateResultSchemes_DropsHostlessThumbnailAndIcon(t *testing.T) {
	for _, raw := range []string{"https:///remote.example/i.png", "http:remote.example/i.png", "https:\\\\remote.example/i.png", "/i.png"} {
		thumb, icon := raw, raw
		r := &Result{URL: "https://example.com", Thumbnail: &thumb, Icon: &icon}
		require.NoError(t, validateResultSchemes(r))
		assert.Nil(t, r.Thumbnail, raw)
		assert.Nil(t, r.Icon, raw)
	}
	thumb, icon := "https://example.com/a (1).png", "HTTPS://example.com/i.png"
	r := &Result{URL: "https://example.com", Thumbnail: &thumb, Icon: &icon}
	require.NoError(t, validateResultSchemes(r))
	require.NotNil(t, r.Thumbnail)
	assert.Equal(t, "https://example.com/a%20%281%29.png", *r.Thumbnail)
	require.NotNil(t, r.Icon, "scheme の大文字小文字は問わない")
}

// host を持たない player の URL は、preview は残して player だけ捨てる。
func TestValidateResultSchemes_DropsHostlessPlayer(t *testing.T) {
	bad := "https:///remote.example/embed"
	w := 640
	r := &Result{URL: "https://example.com", Player: PlayerResult{URL: &bad, Width: &w, Allow: []string{"autoplay"}}}
	require.NoError(t, validateResultSchemes(r))
	assert.Nil(t, r.Player.URL)
	assert.Nil(t, r.Player.Width)
	assert.Equal(t, []string{}, r.Player.Allow)

	good := "https://remote.example/embed"
	r2 := &Result{URL: "https://example.com", Player: PlayerResult{URL: &good, Width: &w}}
	require.NoError(t, validateResultSchemes(r2))
	require.NotNil(t, r2.Player.URL)
	assert.Equal(t, good, *r2.Player.URL)
	assert.Equal(t, &w, r2.Player.Width)
}

// 相対参照で書かれた og:image も、解決した結果に host が無ければ返さない。
func TestFetcher_DropsHostlessResolvedThumbnail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><meta property="og:title" content="x">` +
			`<meta property="og:image" content="https:///remote.example/i.png">` +
			`<link rel="icon" href="/favicon.ico"></head></html>`))
	}))
	defer srv.Close()

	f := newTestFetcher(Config{Enabled: true, AllowRedirect: true, TimeoutMs: 5000, MaxContentLength: 1 << 20})
	result, err := f.Fetch(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Nil(t, result.Thumbnail, "host の無い thumbnail を返している")
	require.NotNil(t, result.Icon, "解決して host を持つ icon は残す")
	assert.Equal(t, srv.URL+"/favicon.ico", *result.Icon)
}

// 設定を毎回読むこと。管理画面で無効にしたら、再起動なしで取りに行かなくなる。
func TestFetcher_ConfigProviderIsLive(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>T</title></head></html>`))
	}))
	defer srv.Close()

	f := NewFetcher(Config{Enabled: true, TimeoutMs: 5000, MaxContentLength: 1 << 20}, nil)
	f.SetHTTPClient(&http.Client{Timeout: f.client.Timeout})
	cfg := Config{Enabled: false, TimeoutMs: 5000, MaxContentLength: 1 << 20}
	f.SetConfigProvider(func() (Config, error) { return cfg, nil })

	_, err := f.Fetch(context.Background(), srv.URL+"/a")
	require.ErrorIs(t, err, ErrDisabled, "無効にした設定を読んでいない")
	assert.Equal(t, 0, hits)

	cfg.Enabled = true
	_, err = f.Fetch(context.Background(), srv.URL+"/b")
	require.NoError(t, err)
	assert.Equal(t, 1, hits)

	// 設定を読めないときは取りに行かない。
	f.SetConfigProvider(func() (Config, error) { return Config{}, errors.New("db down") })
	_, err = f.Fetch(context.Background(), srv.URL+"/c")
	require.ErrorIs(t, err, ErrFetchFailed)
	assert.Equal(t, 1, hits)
}

// キャッシュは件数に上限があり、覚えた URL は取り直さないこと。
func TestFetcher_CacheIsBounded(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>T</title></head></html>`))
	}))
	defer srv.Close()

	f := NewFetcher(Config{Enabled: true, TimeoutMs: 5000, MaxContentLength: 1 << 20}, nil)
	f.SetHTTPClient(&http.Client{Timeout: f.client.Timeout})
	for i := 0; i < 1010; i++ {
		_, err := f.Fetch(context.Background(), fmt.Sprintf("%s/p?i=%d", srv.URL, i))
		require.NoError(t, err)
	}
	assert.LessOrEqual(t, f.cache.lru.Len(), 1000, "キャッシュが件数の上限を超えた")

	before := hits
	_, err := f.Fetch(context.Background(), fmt.Sprintf("%s/p?i=%d", srv.URL, 1009))
	require.NoError(t, err)
	assert.Equal(t, before, hits, "覚えた URL を取り直した")
}
