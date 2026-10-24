package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// scalarVersion is the @scalar/api-reference version /api-doc loads.
//
// **版を固定し、SRIで中身を照合する。** 本家は版を付けずにjsdelivrから読む
// (api-doc.tsx)ので、CDNが配る中身が変われば、そのままこちらのoriginで動く。
// 版を上げるときは、jsdelivrの実ファイルからsha384を計算し直してscalarIntegrity
// も変える(手順はdocs/upstream-catch-up.md)。
const scalarVersion = "1.73.1"

// scalarScriptURL is the file jsdelivr serves for the package's default
// entry ("browser" of its package.json).
const scalarScriptURL = "https://cdn.jsdelivr.net/npm/@scalar/api-reference@" + scalarVersion + "/dist/browser/standalone.js"

// scalarIntegrity is the SRI hash of scalarScriptURL. It was computed from
// the file jsdelivr served, which matched dist/browser/standalone.js of the
// npm tarball.
const scalarIntegrity = "sha384-kYDGzV91Jnn3TbHINV3nt54riK2uMJDfN5Al8dAkz4FssELTBWbD8rgw32sTKfOi"

// apiDocPalette is the colors of the frontend's default themes
// (frontend/packages/frontend-shared/themes/l-elythia.json5 and
// d-elythia.json5) as --ely-* variables, light first and dark under
// prefers-color-scheme.
//
// **配色はここだけに置く。** 下のapiDocScalarVarsは、この--ely-*をScalarの変数へ
// 写すだけで色の値を持たない。fg-strong / fg-muted / navBgのダーク(= bg)は
// テーマのファイルに無い値で、fgから読みやすさのために足した。
//
// **CSSのコメント(`/* */`や`//`)を書かない。** apiDocTemplateの本文として
// html/templateが読むので、コメントは出力から黙って削られる。`{{`も書けない。
const apiDocPalette = `:root {
	--ely-bg: #f4f4f8;
	--ely-panel: #ffffff;
	--ely-nav: #f3f1fb;
	--ely-info: #ecebf8;
	--ely-fg: #4e5068;
	--ely-fg-strong: #2f3150;
	--ely-fg-muted: #7a7c94;
	--ely-accent: #4f46b8;
	--ely-on-accent: #ffffff;
	--ely-accent-2: #6a5cdc;
	--ely-link: #3f6fd1;
	--ely-divider: rgba(33, 36, 79, 0.1);
	--ely-font: "Hiragino Maru Gothic Pro", "BIZ UDGothic", "Hiragino Sans", "Noto Sans JP", system-ui, sans-serif;
	--ely-mono: ui-monospace, "SFMono-Regular", "Cascadia Code", Menlo, Consolas, monospace;
}
@media (prefers-color-scheme: dark) {
	:root {
		--ely-bg: #060a1f;
		--ely-panel: #0e1433;
		--ely-nav: #060a1f;
		--ely-info: #1a2150;
		--ely-fg: #e4e0f2;
		--ely-fg-strong: #f4f1fb;
		--ely-fg-muted: #9a97b8;
		--ely-accent: #fdddcf;
		--ely-on-accent: #1a1530;
		--ely-accent-2: #ccc3f7;
		--ely-link: #b7c3ff;
		--ely-divider: rgba(253, 241, 235, 0.08);
		color-scheme: dark;
	}
}
html, body { height: 100%; margin: 0; padding: 0; }
body { background: var(--ely-bg); color: var(--ely-fg); font-family: var(--ely-font); }
`

// apiDocScalarVars maps the --ely-* palette onto Scalar's theme variables.
//
// Scalarの変数は.light-mode / .dark-modeに付く。値は--ely-*から読むので、
// prefers-color-schemeが変わればScalarの側も同じ色のまま追従する。
const apiDocScalarVars = `.light-mode, .dark-mode, .light-mode .dark-mode {
	--scalar-background-1: var(--ely-panel);
	--scalar-background-2: var(--ely-bg);
	--scalar-background-3: var(--ely-info);
	--scalar-background-accent: color-mix(in srgb, var(--ely-accent) 12%, transparent);
	--scalar-color-1: var(--ely-fg-strong);
	--scalar-color-2: var(--ely-fg);
	--scalar-color-3: var(--ely-fg-muted);
	--scalar-color-accent: var(--ely-accent);
	--scalar-border-color: var(--ely-divider);
	--scalar-link-color: var(--ely-link);
	--scalar-button-1: var(--ely-accent);
	--scalar-button-1-color: var(--ely-on-accent);
	--scalar-button-1-hover: var(--ely-accent-2);
	--scalar-sidebar-background-1: var(--ely-nav);
	--scalar-sidebar-color-1: var(--ely-fg-strong);
	--scalar-sidebar-color-2: var(--ely-fg);
	--scalar-sidebar-color-active: var(--ely-accent);
	--scalar-sidebar-border-color: var(--ely-divider);
	--scalar-sidebar-item-hover-background: var(--ely-info);
	--scalar-sidebar-item-hover-color: var(--ely-fg-strong);
	--scalar-sidebar-item-active-background: color-mix(in srgb, var(--ely-accent) 14%, transparent);
	--scalar-sidebar-search-background: var(--ely-panel);
	--scalar-sidebar-search-border-color: var(--ely-divider);
	--scalar-sidebar-search-color: var(--ely-fg-muted);
	--scalar-font: var(--ely-font);
	--scalar-font-code: var(--ely-mono);
	--scalar-radius: 8px;
	--scalar-radius-lg: 12px;
	--scalar-radius-xl: 16px;
}`

// apiDocConfiguration is the Scalar configuration of /api-doc, passed in the
// data-configuration attribute so that the page needs no inline script.
//
// darkModeを渡さないと、Scalarはprefers-color-schemeに従う(既定の"system")。
// 色の--ely-*もprefers-color-schemeで切り替わるので、Scalarの切り替えボタンは
// 隠す。押すとScalarだけがダークになり、色は切り替わらないまま混ざるため。
// telemetryは1.73.1では設定のschemaにあるだけで読む側が無いが、既定は有効で、
// 将来の版で外部へ送り始めたときに備えて切る。agent / mcpはScalarのサービスへ
// つなぐ機能で、使わないので切る。既定のフォントはfonts.scalar.comから読むので
// 使わない(閲覧者のIPを第三者へ渡さない)。
//
// **proxyUrlは空にして、Test Requestを中継させない。** 未指定だと、Scalarは
// layoutによってproxy.scalar.comを既定の中継先にする(standaloneのbundleの
// `$x` / `XHe`)。中継されると、利用者が入れたアクセストークンが第三者へ渡る。
// /api-docはAPIと同じoriginから配るので、中継は要らない。空文字は「中継しない」
// として扱われる(未指定のときだけ既定値に置き換わる)。
//
// **「Open API Client」のボタンも隠す。** 押すとclient.scalar.comへ/api.jsonの
// 絶対URL付きで移り、その先のScalar Clientは既定でproxy.scalar.comを使う。
func apiDocConfiguration() string {
	cfg := map[string]any{
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
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		// 値はすべて定数なので、失敗しない。
		panic(fmt.Sprintf("apidoc: %v", err))
	}
	return string(b)
}

// apiDocHTML is the page served at /api-doc. It is fixed at build time.
//
// 本家のapi-doc.tsxと同じく、`<script id="api-reference">`をScalarが見つけて
// `/api.json`を読む。設定は属性で渡すので、inlineのscriptは無い。
//
// 設定のJSONはhtml/templateで属性へ埋める。文字列をつなげて`html.EscapeString`
// で逃がしても結果は同じだが、CodeQL(go/unsafe-quoting)はそれを逃がしとして
// 認めず、JSONが引用符を抜けうると判定する(#3810)。
var apiDocHTML = renderAPIDocHTML()

// apiDocTemplate is the markup of /api-doc. Only the configuration is filled
// in by the template; the rest are constants.
var apiDocTemplate = template.Must(template.New("api-doc").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<title>Elythia API</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="icon" href="/favicon.ico">
<link rel="apple-touch-icon" href="/apple-touch-icon.png">
<style>
` + apiDocPalette + `</style>
</head>
<body>
<script id="api-reference" data-url="/api.json" data-configuration="{{.}}"></script>
<script src="` + scalarScriptURL + `" integrity="` + scalarIntegrity + `" crossorigin="anonymous"></script>
</body>
</html>
`))

func renderAPIDocHTML() string {
	var sb strings.Builder
	if err := apiDocTemplate.Execute(&sb, apiDocConfiguration()); err != nil {
		// 入力は定数だけなので、失敗しない。
		panic(fmt.Sprintf("apidoc: %v", err))
	}
	return sb.String()
}

// APIDoc serves /api-doc, the API reference page that renders /api.json.
//
// CSPはfrontendと同じ設定(frontendContentSecurityPolicy)に従い、script-srcに
// Scalarのファイルだけを足す(版まで含めたURLで、jsdelivrの他のパッケージは
// 許さない)。他のページのCSPは変えない。styleはScalarが実行時に`<style>`を
// 差し込むので、frontendの既定のまま'unsafe-inline'で通す。hashを足すと
// 'unsafe-inline'が無視され、Scalarの差し込むstyleが全部止まる。
func (s *Server) APIDoc(c echo.Context) error {
	applyFrontendCSP(c, s.config.FrontendContentSecurityPolicy, cspExtras{Script: []string{scalarScriptURL}})
	// 本家と同じ。中身はビルド時に決まる。
	c.Response().Header().Set(echo.HeaderCacheControl, "public, max-age=86400")
	return c.HTML(http.StatusOK, apiDocHTML)
}
