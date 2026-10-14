package urlpreview

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOEmbedPlayer_Rich(t *testing.T) {
	doc := oembedResponse{
		Type:   "rich",
		HTML:   `<iframe src="https://player.example.com/embed/abc" width="640" height="360" allow="autoplay; encrypted-media; web-share; geolocation" allowfullscreen></iframe>`,
		Width:  float64(640),
		Height: float64(360),
	}
	pr := parseOEmbedPlayer(doc)
	require.NotNil(t, pr.URL)
	assert.Equal(t, "https://player.example.com/embed/abc", *pr.URL)
	assert.Equal(t, 640, *pr.Width)
	assert.Equal(t, 360, *pr.Height)
	// allow からは allowlist directive のみ通り、`geolocation` (非許可) は落ちる。
	// allowfullscreen 単体属性は "fullscreen" 等価として補完される。
	assert.ElementsMatch(t, []string{"autoplay", "encrypted-media", "web-share", "fullscreen"}, pr.Allow)
}

func TestParseOEmbedPlayer_Photo_DropsHTML(t *testing.T) {
	doc := oembedResponse{Type: "photo", HTML: `<iframe src="https://x"></iframe>`}
	pr := parseOEmbedPlayer(doc)
	assert.Nil(t, pr.URL)
	assert.Empty(t, pr.Allow)
}

func TestParseOEmbedPlayer_HTTPSrcRejected(t *testing.T) {
	doc := oembedResponse{
		Type: "video",
		HTML: `<iframe src="http://insecure.example/embed"></iframe>`,
	}
	pr := parseOEmbedPlayer(doc)
	// mixed-content embed は drop
	assert.Nil(t, pr.URL)
}

func TestParseOEmbedPlayer_NoIframe(t *testing.T) {
	doc := oembedResponse{Type: "rich", HTML: `<div>no embed here</div>`}
	pr := parseOEmbedPlayer(doc)
	assert.Nil(t, pr.URL)
}

func TestParseOEmbedPlayer_StringDimensions(t *testing.T) {
	doc := oembedResponse{
		Type:   "video",
		HTML:   `<iframe src="https://player.example.com/v"></iframe>`,
		Width:  "640",
		Height: "360px",
	}
	pr := parseOEmbedPlayer(doc)
	require.NotNil(t, pr.URL)
	assert.Equal(t, 640, *pr.Width)
	assert.Equal(t, 360, *pr.Height)
}

// Fetcher が HTML 中の oEmbed alternate link を見つけて 2nd GET を打ち、
// PlayerResult を Result.Player に設定するエンド to エンド経路。
func TestFetcher_OEmbedDiscoveryAndFetch(t *testing.T) {
	oembed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(oembedResponse{
			Type:   "video",
			HTML:   `<iframe src="https://player.example.com/v" allow="autoplay; fullscreen"></iframe>`,
			Width:  float64(1280),
			Height: float64(720),
		})
	}))
	defer oembed.Close()

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head>
			<meta property="og:title" content="Video Page">
			<link rel="alternate" type="application/json+oembed" href="` + oembed.URL + `">
		</head><body></body></html>`))
	}))
	defer page.Close()

	f := NewFetcher(Config{
		Enabled:          true,
		AllowRedirect:    true,
		TimeoutMs:        5000,
		MaxContentLength: 1 << 20,
	}, nil)
	// httptest server は plain TCP なので safehttp の SSRF guard を避ける
	f.SetHTTPClient(&http.Client{Timeout: f.client.Timeout})

	res, err := f.Fetch(context.Background(), page.URL)
	require.NoError(t, err)
	require.NotNil(t, res.Player.URL)
	assert.Equal(t, "https://player.example.com/v", *res.Player.URL)
	assert.Equal(t, 1280, *res.Player.Width)
	assert.ElementsMatch(t, []string{"autoplay", "fullscreen"}, res.Player.Allow)
}

// oEmbed エンドポイントが落ちていても preview 全体は失敗しない (best-effort)。
func TestFetcher_OEmbedFailureNonFatal(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head>
			<meta property="og:title" content="Page">
			<link rel="alternate" type="application/json+oembed" href="http://127.0.0.1:1/oembed">
		</head><body></body></html>`))
	}))
	defer page.Close()

	f := NewFetcher(Config{Enabled: true, AllowRedirect: true, TimeoutMs: 1000, MaxContentLength: 1 << 20}, nil)
	f.SetHTTPClient(&http.Client{Timeout: f.client.Timeout})

	res, err := f.Fetch(context.Background(), page.URL)
	require.NoError(t, err)
	require.NotNil(t, res.Title)
	assert.Equal(t, "Page", *res.Title)
	// player は埋まらないが Allow は zero value
	assert.Nil(t, res.Player.URL)
	assert.Empty(t, res.Player.Allow)
}

// favicon / og:image / og:url / ap:id の相対 URL が page URL を base に解決される。
func TestParseHTML_RelativeURLsResolved(t *testing.T) {
	htmlBody := `<html><head>
		<meta property="og:title" content="X">
		<meta property="og:image" content="/img/cover.jpg">
		<meta property="og:url" content="/canonical">
		<link rel="icon" href="favicon.ico">
		<link rel="alternate" type="application/activity+json" href="/users/alice">
	</head></html>`
	r := ParseHTML(strings.NewReader(htmlBody), "https://example.com/posts/1")
	require.NotNil(t, r)
	assert.Equal(t, "https://example.com/img/cover.jpg", *r.Thumbnail)
	assert.Equal(t, "https://example.com/posts/favicon.ico", *r.Icon)
	assert.Equal(t, "https://example.com/canonical", r.URL)
	require.NotNil(t, r.ActivityPub)
	assert.Equal(t, "https://example.com/users/alice", *r.ActivityPub)
}

func TestFilterAllow(t *testing.T) {
	cases := map[string][]string{
		"":                                       {},
		"autoplay; geolocation; encrypted-media": {"autoplay", "encrypted-media"},
		"  fullscreen  ;  picture-in-picture  ":  {"fullscreen", "picture-in-picture"},
		"camera; microphone":                     {}, // none allowlisted
		"autoplay; autoplay; web-share; web-share": {"autoplay", "web-share"},       // dedup
		"autoplay 'self'; encrypted-media 'src'":   {"autoplay", "encrypted-media"}, // value-list は捨てる
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			assert.ElementsMatch(t, want, filterAllow(in))
		})
	}
}

func TestNumAttr(t *testing.T) {
	assert.Equal(t, 640, numAttr(float64(640)))
	assert.Equal(t, 360, numAttr(360))
	assert.Equal(t, 720, numAttr("720"))
	assert.Equal(t, 1080, numAttr("1080px"))
	assert.Equal(t, 0, numAttr("not a number"))
	assert.Equal(t, 0, numAttr(nil))
}

// TestParseOEmbedPlayer_NoDimensions: width/height 省略時は Width/Height
// pointer を nil のまま残し、frontend 側で iframe size を default 値に
// fallback できるようにする (#639 review #5)。
func TestParseOEmbedPlayer_NoDimensions(t *testing.T) {
	doc := oembedResponse{
		Type: "video",
		HTML: `<iframe src="https://player.example.com/v"></iframe>`,
	}
	pr := parseOEmbedPlayer(doc)
	require.NotNil(t, pr.URL)
	assert.Nil(t, pr.Width)
	assert.Nil(t, pr.Height)
	assert.Empty(t, pr.Allow)
}

// TestParseHTML_OEmbedDiscoveryRelative: discovery 用の <link rel="alternate"
// type="application/json+oembed"> が相対 path で書かれていても base URL
// で絶対化される (#639 review #7)。
func TestParseHTML_OEmbedDiscoveryRelative(t *testing.T) {
	htmlBody := `<html><head>
		<meta property="og:title" content="Video Page">
		<link rel="alternate" type="application/json+oembed" href="/oembed?id=abc">
	</head></html>`
	r := ParseHTML(strings.NewReader(htmlBody), "https://example.com/posts/1")
	require.NotNil(t, r)
	// oEmbedURL は unexported field — 同 package テストで直接確認できる。
	assert.Equal(t, "https://example.com/oembed?id=abc", r.oEmbedURL)
}

// TestParseHTML_OEmbedDiscoveryXMLIgnored: text/xml+oembed link は dead
// code として削除済 (#639 review #1)。XML link しか提供しない provider
// に対して oEmbedURL は空のまま (= 2nd fetch 発火しない)。
func TestParseHTML_OEmbedDiscoveryXMLIgnored(t *testing.T) {
	htmlBody := `<html><head>
		<link rel="alternate" type="text/xml+oembed" href="/oembed.xml">
	</head></html>`
	r := ParseHTML(strings.NewReader(htmlBody), "https://example.com/")
	require.NotNil(t, r)
	assert.Equal(t, "", r.oEmbedURL, "XML oembed alternate must NOT trigger 2nd fetch")
}

// TestResolveURL_ParseFailureReturnsEmpty: parse 不能な rel は frontend
// に malformed URL を流さず空文字を返す (#639 review #3)。
func TestResolveURL_ParseFailureReturnsEmpty(t *testing.T) {
	htmlBody := `<html><head>
		<link rel="icon" href="://broken">
	</head></html>`
	r := ParseHTML(strings.NewReader(htmlBody), "https://example.com/")
	require.NotNil(t, r)
	// Parse failure では Icon を埋めない (空文字 → frontend で nil 判定)。
	assert.Nil(t, r.Icon)
}
