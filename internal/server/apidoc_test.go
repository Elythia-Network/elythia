package server

import (
	"encoding/json"
	stdhtml "html"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
)

func getAPIDoc(t *testing.T, cspMode string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	s := &Server{echo: e, config: &config.Config{FrontendContentSecurityPolicy: cspMode}}
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/api-doc", nil), rec)
	require.NoError(t, s.APIDoc(c))
	return rec
}

var scriptTagPattern = regexp.MustCompile(`(?s)<script([^>]*)>(.*?)</script>`)

func TestAPIDoc(t *testing.T) {
	rec := getAPIDoc(t, CSPModeOff)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "public, max-age=86400", rec.Header().Get(echo.HeaderCacheControl))
	assert.Equal(t, echo.MIMETextHTMLCharsetUTF8, rec.Header().Get(echo.HeaderContentType))
	// CSPを切っている(既定)なら、これまでどおり何も付けない。
	assert.Empty(t, rec.Header().Get("Content-Security-Policy"))
	assert.Empty(t, rec.Header().Get("Content-Security-Policy-Report-Only"))

	body := rec.Body.String()
	assert.Contains(t, body, "<title>Elythia API</title>")
	assert.Contains(t, body, `<link rel="icon" href="/favicon.ico">`)
	// 本家の名前やロゴは出さない。
	assert.NotContains(t, strings.ToLower(body), "misskey")

	scripts := scriptTagPattern.FindAllStringSubmatch(body, -1)
	require.Len(t, scripts, 2)
	// inlineのscriptは無い(CSPにhashを足さずに済む)。
	for _, s := range scripts {
		assert.Empty(t, strings.TrimSpace(s[2]), s[0])
	}
	assert.Equal(t, ` src="`+scalarScriptURL+`" integrity="`+scalarIntegrity+`" crossorigin="anonymous"`, scripts[1][1])

	ref := regexp.MustCompile(`^ id="api-reference" data-url="/api.json" data-configuration="([^"]*)"$`).FindStringSubmatch(scripts[0][1])
	require.NotNil(t, ref, scripts[0][1])
	var cfg map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdhtml.UnescapeString(ref[1])), &cfg))
	assert.Equal(t, map[string]any{
		"url":                  "/api.json",
		"theme":                "none",
		"customCss":            apiDocScalarVars,
		"withDefaultFonts":     false,
		"telemetry":            false,
		"agent":                map[string]any{"disabled": true},
		"mcp":                  map[string]any{"disabled": true},
		"showDeveloperTools":   "never",
		"documentDownloadType": "json",
		"hideDarkModeToggle":   true,
		"proxyUrl":             "",
		"hideClientButton":     true,
	}, cfg)
	// Test Requestを第三者のproxyへ中継させない。キーが無い(未指定)と既定の
	// proxyになりうるので、空文字が入っていることを見る。
	proxy, ok := cfg["proxyUrl"]
	assert.True(t, ok)
	assert.Equal(t, "", proxy)
	// 「Open API Client」はclient.scalar.comへ移り、その先は既定でproxyを使うので隠す。
	assert.Equal(t, true, cfg["hideClientButton"])
	// darkModeを渡さない(prefers-color-schemeに従わせる)。
	assert.NotContains(t, cfg, "darkMode")
	assert.Contains(t, body, apiDocPalette)
}

func TestAPIDoc_CSP(t *testing.T) {
	enforce := getAPIDoc(t, CSPModeEnforce).Header().Get("Content-Security-Policy")
	report := getAPIDoc(t, CSPModeReportOnly).Header().Get("Content-Security-Policy-Report-Only")
	assert.Equal(t, enforce, report)

	// frontendと同じpolicyに、Scalarのファイルだけをscript-srcに足す。
	want := buildFrontendCSP(true, cspExtras{})
	want = strings.Replace(want, "script-src 'self' "+esmShSources, "script-src 'self' "+esmShSources+" "+scalarScriptURL, 1)
	assert.Equal(t, want, enforce)

	// 他のページのCSPには足さない。
	assert.NotContains(t, buildFrontendCSP(true, cspExtras{}), "jsdelivr")
}

func TestScalarPin(t *testing.T) {
	assert.Equal(t, "https://cdn.jsdelivr.net/npm/@scalar/api-reference@"+scalarVersion+"/dist/browser/standalone.js", scalarScriptURL)
	assert.Regexp(t, `^\d+\.\d+\.\d+$`, scalarVersion)
	assert.Regexp(t, `^sha384-[A-Za-z0-9+/]{64}$`, scalarIntegrity)
}

// apiDocPaletteの色がfrontendの既定テーマ(l-elythia / d-elythia)の値と
// 一致することを確かめる。テーマの色を変えたら、こちらも揃える。
func TestAPIDocPaletteMatchesThemes(t *testing.T) {
	light := readThemeProps(t, "l-elythia.json5")
	dark := readThemeProps(t, "d-elythia.json5")
	lightVars, darkVars := paletteVars(t)

	for prop, v := range map[string]string{
		"bg": "--ely-bg", "panel": "--ely-panel", "navBg": "--ely-nav", "infoBg": "--ely-info",
		"fg": "--ely-fg", "accent": "--ely-accent", "buttonGradateB": "--ely-accent-2",
		"link": "--ely-link", "divider": "--ely-divider",
	} {
		assert.Equal(t, normalizeColor(light[prop]), normalizeColor(lightVars[v]), "light %s", prop)
	}
	for prop, v := range map[string]string{
		"bg": "--ely-bg", "panel": "--ely-panel", "infoBg": "--ely-info", "fg": "--ely-fg",
		"accent": "--ely-accent", "fgOnAccent": "--ely-on-accent", "buttonGradateB": "--ely-accent-2",
		"link": "--ely-link", "divider": "--ely-divider",
	} {
		assert.Equal(t, normalizeColor(dark[prop]), normalizeColor(darkVars[v]), "dark %s", prop)
	}
	// ダークのnavBgは'@bg'。
	assert.Equal(t, "@bg", dark["navBg"])
	assert.Equal(t, darkVars["--ely-bg"], darkVars["--ely-nav"])
	// ライトとダークで同じ変数を全部持つ。
	for k := range lightVars {
		if k == "--ely-font" || k == "--ely-mono" {
			continue
		}
		assert.Contains(t, darkVars, k)
	}
}

var themePropPattern = regexp.MustCompile(`(?m)^\s*(\w+):\s*'([^']*)',`)

func readThemeProps(t *testing.T, name string) map[string]string {
	t.Helper()
	b, err := os.ReadFile("../../frontend/packages/frontend-shared/themes/" + name)
	require.NoError(t, err)
	out := map[string]string{}
	for _, m := range themePropPattern.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = m[2]
	}
	require.NotEmpty(t, out["accent"], name)
	return out
}

var cssVarPattern = regexp.MustCompile(`(--ely-[\w-]+):\s*([^;]+);`)

// paletteVars splits apiDocPalette into the light (:root) and dark
// (prefers-color-scheme: dark) variables.
func paletteVars(t *testing.T) (light, dark map[string]string) {
	t.Helper()
	l, d, ok := strings.Cut(apiDocPalette, "@media (prefers-color-scheme: dark)")
	require.True(t, ok)
	parse := func(s string) map[string]string {
		out := map[string]string{}
		for _, m := range cssVarPattern.FindAllStringSubmatch(s, -1) {
			out[m[1]] = strings.TrimSpace(m[2])
		}
		return out
	}
	light, dark = parse(l), parse(d)
	require.NotEmpty(t, light)
	require.NotEmpty(t, dark)
	return light, dark
}

// normalizeColor lowercases, expands #rgb and drops spaces.
func normalizeColor(c string) string {
	c = strings.ToLower(strings.ReplaceAll(c, " ", ""))
	if len(c) == 4 && c[0] == '#' {
		c = "#" + strings.Repeat(c[1:2], 2) + strings.Repeat(c[2:3], 2) + strings.Repeat(c[3:4], 2)
	}
	return c
}
