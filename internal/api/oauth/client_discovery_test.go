package oauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateClientID(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		allowHTTP bool
		ok        bool
	}{
		{"valid https with path", "https://app.example/", false, true},
		{"valid https subpath", "https://app.example/oauth/client", false, true},
		{"http rejected in prod", "http://app.example/", false, false},
		{"http allowed in test", "http://app.example/", true, true},
		{"loopback localhost", "http://localhost/", true, true},
		{"loopback 127.0.0.1", "https://127.0.0.1/", false, true},
		{"fragment rejected", "https://app.example/#x", false, false},
		{"userinfo rejected", "https://user:pass@app.example/", false, false},
		{"dot segment rejected", "https://app.example/./x", false, false},
		{"dotdot segment rejected", "https://app.example/../x", false, false},
		// upstream の `\.\w+$` regex は IPv4 を素通しする (host が dot+digits で
		// 終わるため)。SSRF guard は fetch 時に private IP を弾くので、ここでは
		// upstream 挙動に合わせて public raw IPv4 は許容する。
		{"raw ipv4 accepted (upstream parity)", "https://93.184.216.34/", false, true},
		{"bare host (no tld) rejected", "https://appexample/", false, false},
		{"not a url", "::::not a url", false, false},
		{"empty", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateClientID(tc.raw, tc.allowHTTP)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// testClient builds an http.Client that talks to the given test server without
// SSRF restrictions (the SSRF guard is exercised at the integration layer).
func testClient() *http.Client { return &http.Client{} }

func TestDiscoverClientInformation_HTMLMicroformats(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body>
			<div class="h-app">
				<a class="u-url p-name" href="` + srvURL + `/">My Cool App</a>
				<img class="u-logo" src="/logo.png">
			</div>
			<link rel="redirect_uri" href="/callback">
		</body></html>`))
	}))
	defer srv.Close()
	srvURL = srv.URL

	info, err := discoverClientInformation(testClient(), srv.URL+"/")
	require.NoError(t, err)
	assert.Equal(t, "My Cool App", info.Name)
	assert.Equal(t, srv.URL+"/logo.png", info.Logo)
	assert.Contains(t, info.RedirectURIs, srv.URL+"/callback")
}

func TestDiscoverClientInformation_JSONMetadata(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"client_id": "` + srvURL + `/",
			"client_uri": "` + srvURL + `/",
			"client_name": "JSON App",
			"logo_uri": "/icon.png",
			"redirect_uris": ["` + srvURL + `/cb", "/relcb"]
		}`))
	}))
	defer srv.Close()
	srvURL = srv.URL

	info, err := discoverClientInformation(testClient(), srv.URL+"/")
	require.NoError(t, err)
	assert.Equal(t, "JSON App", info.Name)
	assert.Equal(t, srv.URL+"/icon.png", info.Logo)
	assert.Contains(t, info.RedirectURIs, srv.URL+"/cb")
	assert.Contains(t, info.RedirectURIs, srv.URL+"/relcb")
}

// The name and logo kept in the authorize transaction are bounded, whatever
// the size of the client document.
func TestDiscoverClientInformation_BoundsNameAndLogo(t *testing.T) {
	cases := map[string]struct {
		name, logoPath string
		wantName       string
		wantLogo       bool
	}{
		"within the limits": {name: strings.Repeat("あ", maxClientNameRunes), logoPath: "/icon.png",
			wantName: strings.Repeat("あ", maxClientNameRunes), wantLogo: true},
		"over the limits": {name: strings.Repeat("あ", maxClientNameRunes+1000), logoPath: "/" + strings.Repeat("a", maxClientLogoBytes),
			wantName: strings.Repeat("あ", maxClientNameRunes), wantLogo: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var srvURL string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"client_id": "` + srvURL + `/", "client_uri": "` + srvURL + `/",
					"client_name": "` + tc.name + `", "logo_uri": "` + tc.logoPath + `",
					"redirect_uris": ["` + srvURL + `/cb"]}`))
			}))
			defer srv.Close()
			srvURL = srv.URL

			info, err := discoverClientInformation(testClient(), srv.URL+"/")
			require.NoError(t, err)
			assert.Equal(t, tc.wantName, info.Name)
			if tc.wantLogo {
				assert.Equal(t, srv.URL+tc.logoPath, info.Logo)
			} else {
				assert.Empty(t, info.Logo)
			}
		})
	}
}

func TestDiscoverClientInformation_JSONClientIDMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"https://evil.example/","client_uri":"https://evil.example/"}`))
	}))
	defer srv.Close()

	_, err := discoverClientInformation(testClient(), srv.URL+"/")
	require.ErrorIs(t, err, errInvalidClient)
}

func TestDiscoverClientInformation_JSONClientURINotPrefix(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"` + srvURL + `/","client_uri":"https://other.example/"}`))
	}))
	defer srv.Close()
	srvURL = srv.URL

	_, err := discoverClientInformation(testClient(), srv.URL+"/")
	require.ErrorIs(t, err, errInvalidClient)
}

func TestDiscoverClientInformation_LinkHeader(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<`+srvURL+`/hdr-cb>; rel="redirect_uri"`)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>no microformats</body></html>`))
	}))
	defer srv.Close()
	srvURL = srv.URL

	info, err := discoverClientInformation(testClient(), srv.URL+"/")
	require.NoError(t, err)
	assert.Contains(t, info.RedirectURIs, srv.URL+"/hdr-cb")
}

func TestDiscoverClientInformation_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := discoverClientInformation(testClient(), srv.URL+"/")
	require.ErrorIs(t, err, errInvalidClient)
}

func TestParseLinkRedirectURIs(t *testing.T) {
	got := parseLinkRedirectURIs(`<https://a.example/cb>; rel="redirect_uri", <https://a.example/other>; rel="canonical"`)
	assert.Equal(t, []string{"https://a.example/cb"}, got)
}
