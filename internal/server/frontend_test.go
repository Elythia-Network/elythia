package server

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/api/oauth"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/frontendutil"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// frontendHTML が upstream `_splash.tsx` 互換の splash markup を返すこと。
// upstream 互換要素 (#xxx):
//   - id="splash" wrapper
//   - id="splashIcon" な img 要素
//   - スピナーの代わりに Elythia 独自の夜空 (星雲・#splashSky の canvas・#splashMark)
//   - default img src は /static-assets/splash.png (meta.iconUrl 未設定時)
//   - meta.iconUrl が設定されていればそちらを使う
//   - ai.png mascot は splash には使わない
func TestFrontendHTML_SplashStructure(t *testing.T) {
	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}

	t.Run("default fallback to /static-assets/splash.png", func(t *testing.T) {
		repo := testutil.NewMockMetaRepository()
		handler := frontendHTML(cfg, repo, nil, nil)

		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, handler(c))

		body := rec.Body.String()
		assert.Contains(t, body, `<div id="splash">`)
		assert.Contains(t, body, `<img id="splashIcon"`)
		assert.Contains(t, body, `src="/static-assets/splash.png"`,
			"default splash icon should be /static-assets/splash.png")
		// 星は boot.js がこの canvas に描く。id が変わると星空が出ない
		assert.Contains(t, body, `<canvas id="splashSky"></canvas>`)
		assert.Contains(t, body, `<div id="splashMark"><img id="splashIcon"`)
		assert.Contains(t, body, `class="splashNebula n1"`)
		assert.Contains(t, body, `class="splashNebula n3"`, "星雲は 3 つ")
		// 以前のスピナー (#2549) は置かない
		assert.NotContains(t, body, `id="splashSpinner"`)
		// テーマカラーが未設定なら、ブラウザの UI は夜空の色にする
		assert.Contains(t, body, `<meta name="theme-color" content="#0a112e">`)
		// ai.png は mascot 用、splash には出ない
		assert.NotContains(t, body, `src="/assets/ai.png"`,
			"splash should not use ai.png (mascot)")
		// 旧 placeholder の "Loading..." text も無いこと
		assert.NotContains(t, body, "<p>Loading...</p>")
	})

	t.Run("uses configured meta.iconUrl when set", func(t *testing.T) {
		repo := testutil.NewMockMetaRepository()
		customIcon := "https://example.test/files/server-icon.png"
		repo.Meta = &model.Meta{ID: "x", IconURL: &customIcon}

		handler := frontendHTML(cfg, repo, nil, nil)
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, handler(c))

		body := rec.Body.String()
		// splash img src には server icon が反映される
		assert.True(t, strings.Contains(body, `<img id="splashIcon" src="`+customIcon+`"`),
			"splash icon should use configured meta.iconUrl")
	})

	t.Run("mascot field is not used for splash", func(t *testing.T) {
		// 旧実装は mascotImageUrl を splash に使っていたので regression guard。
		repo := testutil.NewMockMetaRepository()
		mascot := "https://example.test/custom-mascot.png"
		repo.Meta = &model.Meta{ID: "x", MascotImageURL: &mascot}

		handler := frontendHTML(cfg, repo, nil, nil)
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, handler(c))

		body := rec.Body.String()
		// splash img には反映されないこと (mascotImageUrl は /api/meta で
		// frontend が別用途で使う field、splash のアイコンではない)。
		// strings.Index は見つからない場合 -1 を返すので、有効な offset は
		// >= 0 で check する (relative offset なので 0 もあり得る)。
		splashStart := strings.Index(body, `<img id="splashIcon"`)
		require.GreaterOrEqual(t, splashStart, 0, "splashIcon img not found in body")
		splashEnd := strings.Index(body[splashStart:], `</div>`)
		require.GreaterOrEqual(t, splashEnd, 0, "closing </div> not found after splashIcon")
		splashSection := body[splashStart : splashStart+splashEnd]
		assert.NotContains(t, splashSection, mascot,
			"mascot URL should not appear in splash img")
		// default splash.png が使われる
		assert.Contains(t, splashSection, `src="/static-assets/splash.png"`)
	})
}

// `<head>` の icon link 群が upstream `views/base.tsx` と同じ値を返すこと。
//
// 特に `<link rel="apple-touch-icon">` が無いと、iOS Safari は manifest の
// icons にフォールバックする。192/512 は `purpose: "maskable"` なので候補から
// 外れ、splash.png がホーム画面アイコンに選ばれてしまう (#2527)。当時の
// splash.png は透明背景で、iOS では黒く塗られていた。
func TestFrontendHTML_IconLinks(t *testing.T) {
	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}

	render := func(t *testing.T, m *model.Meta) string {
		t.Helper()
		repo := testutil.NewMockMetaRepository()
		if m != nil {
			repo.Meta = m
		}
		handler := frontendHTML(cfg, repo, nil, nil)
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, handler(c))
		return rec.Body.String()
	}

	t.Run("defaults match upstream base.tsx", func(t *testing.T) {
		body := render(t, nil)

		assert.Contains(t, body, `<link rel="icon" href="/favicon.ico">`,
			"upstream の fallback は /favicon.ico")
		assert.Contains(t, body, `<link rel="apple-touch-icon" href="/apple-touch-icon.png">`,
			"apple-touch-icon が無いと iOS が manifest 側の splash.png を拾う")
	})

	t.Run("uses meta.iconUrl for favicon", func(t *testing.T) {
		icon := "https://example.test/files/server-icon.png"
		body := render(t, &model.Meta{ID: "x", IconURL: &icon})

		assert.Contains(t, body, `<link rel="icon" href="`+icon+`">`)
		// app512IconUrl 未設定なら apple-touch-icon は default のまま
		// (upstream も iconUrl は apple-touch-icon に流用しない)。
		assert.Contains(t, body, `<link rel="apple-touch-icon" href="/apple-touch-icon.png">`)
	})

	t.Run("uses meta.app512IconUrl for apple-touch-icon", func(t *testing.T) {
		appIcon := "https://example.test/files/app-512.png"
		body := render(t, &model.Meta{ID: "x", App512IconURL: &appIcon})

		assert.Contains(t, body, `<link rel="apple-touch-icon" href="`+appIcon+`">`)
		// favicon 側は iconUrl 由来なので巻き込まれない
		assert.Contains(t, body, `<link rel="icon" href="/favicon.ico">`)
	})
}

// SSR 埋め込み meta (`#misskey_meta`) の policies が
// `{...DEFAULT_POLICIES, ...meta.policies}` になること。
//
// upstream HtmlTemplateService は metaEntityService.packDetailed(meta) を
// そのまま埋め込むため instance.policies の上書きが反映される。mk-go は
// 以前 role.DefaultPolicies() 固定を埋めていたため、
// admin/roles/update-default-policies や update-meta で policy を変えても
// client 側の instance.policies に一切反映されなかった。frontend の
// instance.ts は data-generated-at が localStorage の instanceCachedAt より
// 新しいと SSR 値で cache を上書きし以後 1 時間 /api/meta を叩かないので、
// 誤った policies が恒久的に居座る。
func TestFrontendHTML_EmbeddedMetaPolicies(t *testing.T) {
	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}

	extractMeta := func(t *testing.T, body string) map[string]any {
		t.Helper()
		const openTag = `id="misskey_meta"`
		idx := strings.Index(body, openTag)
		require.NotEqual(t, -1, idx, "misskey_meta script should be embedded")
		start := strings.Index(body[idx:], ">")
		require.NotEqual(t, -1, start)
		start += idx + 1
		end := strings.Index(body[start:], "</script>")
		require.NotEqual(t, -1, end)

		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(body[start:start+end]), &parsed))
		return parsed
	}

	render := func(t *testing.T, m *model.Meta) map[string]any {
		t.Helper()
		repo := testutil.NewMockMetaRepository()
		repo.Meta = m
		handler := frontendHTML(cfg, repo, nil, nil)

		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, handler(c))
		return extractMeta(t, rec.Body.String())
	}

	t.Run("no override falls back to default policies", func(t *testing.T) {
		parsed := render(t, &model.Meta{ID: "x"})

		policies, ok := parsed["policies"].(map[string]any)
		require.True(t, ok, "policies should be an object")
		assert.Equal(t, false, policies["canSearchNotes"])
		assert.Equal(t, true, policies["ltlAvailable"])

		features, ok := parsed["features"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, true, features["localTimeline"])
		assert.Equal(t, true, features["globalTimeline"])
	})

	// SSR 埋め込み meta も /api/meta と同じく version = 互換 Misskey 版、
	// mkGoVersion = mk-go の実装版で出すこと (#2274)。about 系ページが
	// fetchInstance を待たずに実装版を表示できるようにするため。
	t.Run("exposes mkGoVersion alongside the compatible Misskey version", func(t *testing.T) {
		parsed := render(t, &model.Meta{ID: "x"})

		assert.Equal(t, cfg.Version, parsed["version"], "version は互換 Misskey 版")
		assert.Equal(t, config.MkGoVersion, parsed["mkGoVersion"], "mkGoVersion は mk-go の実装版")
	})

	t.Run("meta.policies override is reflected", func(t *testing.T) {
		parsed := render(t, &model.Meta{
			ID:       "x",
			Policies: []byte(`{"canSearchNotes":true,"ltlAvailable":false,"gtlAvailable":false}`),
		})

		policies, ok := parsed["policies"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, true, policies["canSearchNotes"], "admin override should win")
		assert.Equal(t, false, policies["ltlAvailable"])
		// 上書きしていない key は default のまま残る
		assert.Equal(t, true, policies["canPublicNote"])

		features, ok := parsed["features"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, false, features["localTimeline"])
		assert.Equal(t, false, features["globalTimeline"])
	})
}

// #2313 / #2314: SSR 埋め込み meta にも分割アップロードの能力告知を載せる。
// frontend の instance.ts は SSR 埋め込みを localStorage cache より優先し、以後
// 1 時間 /api/meta を再取得しない。ここに載せ忘れると、admin が有効にしても
// client は最大 1 時間 従来の単発アップロードに倒れ続け、100MB 超が
// リバースプロキシで弾かれる。
func TestFrontendHTML_EmbedsChunkedUploadCapability(t *testing.T) {
	cfg := &config.Config{URL: "https://example.com", Version: "2026.7.0"}
	repo := testutil.NewMockMetaRepository()
	repo.Meta = &model.Meta{ID: "x"}

	extract := func(t *testing.T, handler echo.HandlerFunc) map[string]any {
		t.Helper()
		e := echo.New()
		rec := httptest.NewRecorder()
		require.NoError(t, handler(e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)))
		m := regexp.MustCompile(`(?s)id="misskey_meta"[^>]*>(.*?)</script>`).FindStringSubmatch(rec.Body.String())
		require.Len(t, m, 2, "misskey_meta element must be present")
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(html.UnescapeString(m[1])), &out))
		return out
	}

	// 未対応構成では field ごと出さない (純正 Misskey と同じ undefined)。
	got := extract(t, frontendHTML(cfg, repo, nil, func() (int64, bool) { return 0, false }))
	_, present := got["chunkedUpload"]
	assert.False(t, present)

	// 未配線でも落ちない。
	got = extract(t, frontendHTML(cfg, repo, nil, nil))
	_, present = got["chunkedUpload"]
	assert.False(t, present)

	// 利用可能なら /api/meta と同じ shape で出す。
	got = extract(t, frontendHTML(cfg, repo, nil, func() (int64, bool) { return 10 * 1024 * 1024, true }))
	require.NotNil(t, got["chunkedUpload"])
	assert.Equal(t, float64(10*1024*1024), got["chunkedUpload"].(map[string]any)["chunkSize"])
}

// 外部 media proxy 構成では、その origin を CSP に足す (#2501)。リモート画像も
// カスタム絵文字も proxy の origin から配信されるため、無いと enforce で全滅する。
// internal proxy ('self') 構成では何も足さない。
func TestFrontendHTML_ExternalMediaProxyOriginInCSP(t *testing.T) {
	serve := func(cfg *config.Config) string {
		t.Helper()
		handler := frontendHTML(cfg, testutil.NewMockMetaRepository(), nil, nil)
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, handler(c))
		return rec.Header().Get("Content-Security-Policy")
	}

	t.Run("external proxy origin appears in media directives", func(t *testing.T) {
		csp := serve(&config.Config{
			URL: "https://example.test", Version: "0.0.1-test",
			FrontendContentSecurityPolicy: CSPModeEnforce,
			ExternalMediaProxyEnabled:     true,
			MediaProxy:                    "https://proxy.example/proxy",
		})
		require.NotEmpty(t, csp)
		for _, d := range strings.Split(csp, "; ") {
			name, _, _ := strings.Cut(d, " ")
			switch name {
			case "img-src", "media-src", "connect-src":
				assert.Containsf(t, d, "https://proxy.example", "%s に proxy origin が要る", name)
			case "script-src":
				// 画像を配るだけの host にスクリプト実行の権限を与えない。
				assert.NotContains(t, d, "https://proxy.example")
			}
		}
	})

	t.Run("internal proxy adds nothing", func(t *testing.T) {
		csp := serve(&config.Config{
			URL: "https://example.test", Version: "0.0.1-test",
			FrontendContentSecurityPolicy: CSPModeEnforce,
			MediaProxy:                    "https://example.test/proxy",
		})
		require.NotEmpty(t, csp)
		assert.NotContains(t, csp, "proxy.example")
	})
}

// 有効な captcha 業者の origin が CSP に載る (#2502)。無いと enforce で
// captcha の script が読めずサインアップが壊れる。無効の業者は載せない。
func TestFrontendHTML_CaptchaOriginsInCSP(t *testing.T) {
	cfg := &config.Config{
		URL: "https://example.test", Version: "0.0.1-test",
		FrontendContentSecurityPolicy: CSPModeEnforce,
	}
	serve := func(meta *model.Meta) string {
		t.Helper()
		repo := testutil.NewMockMetaRepository()
		if meta != nil {
			repo.Meta = meta
		}
		handler := frontendHTML(cfg, repo, nil, nil)
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, handler(c))
		return rec.Header().Get("Content-Security-Policy")
	}

	t.Run("turnstile 有効で origin が載る", func(t *testing.T) {
		csp := serve(&model.Meta{ID: "x", EnableTurnstile: true})
		require.NotEmpty(t, csp)
		for _, d := range strings.Split(csp, "; ") {
			name, _, _ := strings.Cut(d, " ")
			switch name {
			case "script-src":
				assert.Contains(t, d, "https://challenges.cloudflare.com")
			case "connect-src":
				// 公式の要求は script / frame のみ。
				assert.NotContains(t, d, "cloudflare")
			}
		}
		assert.NotContains(t, csp, "hcaptcha", "無効の業者は載せない")
	})

	// hcaptcha 単独のケースも見る。captchaCSPExtras の引数は positional な
	// bool 3 連なので、取り違えると「有効にした業者と違う origin が載る」。
	// turnstile だけの検証では第 1・第 2 引数の入れ替えを検出できない。
	t.Run("hcaptcha 有効でその origin だけが載る", func(t *testing.T) {
		csp := serve(&model.Meta{ID: "x", EnableHcaptcha: true})
		require.NotEmpty(t, csp)
		assert.Contains(t, csp, "https://*.hcaptcha.com")
		assert.NotContains(t, csp, "recaptcha")
		assert.NotContains(t, csp, "cloudflare")
	})

	t.Run("全て無効なら載らない", func(t *testing.T) {
		csp := serve(&model.Meta{ID: "x"})
		require.NotEmpty(t, csp)
		assert.NotContains(t, csp, "cloudflare")
		assert.NotContains(t, csp, "hcaptcha")
		assert.NotContains(t, csp, "recaptcha")
	})
}

// PWA manifest が upstream `ClientServerService.manifestHandler` と同じ形を
// 返すこと。icons は PWA のホーム画面アイコンそのものなので、192/512 の
// maskable と splash の any の組み合わせが崩れると純正と見た目が変わる。
func TestManifestJSON(t *testing.T) {
	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}

	serve := func(t *testing.T, m *model.Meta) map[string]any {
		t.Helper()
		repo := testutil.NewMockMetaRepository()
		if m != nil {
			repo.Meta = m
		}
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/manifest.json", nil), rec)
		require.NoError(t, manifestJSON(cfg, repo)(c))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "max-age=300", rec.Header().Get("Cache-Control"))

		var out map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}

	iconsOf := func(t *testing.T, manifest map[string]any) []map[string]any {
		t.Helper()
		raw, ok := manifest["icons"].([]any)
		require.True(t, ok, "icons should be an array")
		out := make([]map[string]any, 0, len(raw))
		for _, i := range raw {
			icon, ok := i.(map[string]any)
			require.True(t, ok)
			out = append(out, icon)
		}
		return out
	}

	t.Run("defaults match upstream", func(t *testing.T) {
		manifest := serve(t, nil)

		assert.Equal(t, "/", manifest["start_url"])
		assert.Equal(t, "standalone", manifest["display"])
		// background_color だけは upstream (#313a42) と違え、起動画面の夜空の色にしている
		assert.Equal(t, "#0a112e", manifest["background_color"])
		// theme_color の既定も upstream (#86b300) と違え、夜空の色にしている
		assert.Equal(t, "#0a112e", manifest["theme_color"])

		icons := iconsOf(t, manifest)
		require.Len(t, icons, 3)
		assert.Equal(t, "/static-assets/icons/192.png", icons[0]["src"])
		assert.Equal(t, "192x192", icons[0]["sizes"])
		assert.Equal(t, "maskable", icons[0]["purpose"])
		assert.Equal(t, "/static-assets/icons/512.png", icons[1]["src"])
		assert.Equal(t, "512x512", icons[1]["sizes"])
		assert.Equal(t, "maskable", icons[1]["purpose"])
		assert.Equal(t, "/static-assets/splash.png", icons[2]["src"])
		assert.Equal(t, "300x300", icons[2]["sizes"])
		// maskable しか無いと iOS が候補を見つけられないので any が要る。
		assert.Equal(t, "any", icons[2]["purpose"])

		shortcuts, ok := manifest["shortcuts"].([]any)
		require.True(t, ok, "upstream は safemode ショートカットを持つ")
		require.Len(t, shortcuts, 1)
		assert.Equal(t, map[string]any{"name": "Safemode", "url": "/?safemode=true"}, shortcuts[0])

		shareTarget, ok := manifest["share_target"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "/share/", shareTarget["action"])
		assert.Equal(t, "GET", shareTarget["method"])
	})

	t.Run("reflects meta app icon urls", func(t *testing.T) {
		icon192 := "https://example.test/files/app-192.png"
		icon512 := "https://example.test/files/app-512.png"
		manifest := serve(t, &model.Meta{ID: "x", App192IconURL: &icon192, App512IconURL: &icon512})

		icons := iconsOf(t, manifest)
		require.Len(t, icons, 3)
		assert.Equal(t, icon192, icons[0]["src"])
		assert.Equal(t, icon512, icons[1]["src"])
		// splash は meta で差し替えられない (upstream も固定)。
		assert.Equal(t, "/static-assets/splash.png", icons[2]["src"])
	})

	t.Run("reflects name / shortName / themeColor", func(t *testing.T) {
		name := "テストサーバー"
		short := "テスト"
		color := "#00eb91"
		manifest := serve(t, &model.Meta{ID: "x", Name: &name, ShortName: &short, ThemeColor: &color})

		assert.Equal(t, name, manifest["name"])
		assert.Equal(t, short, manifest["short_name"])
		assert.Equal(t, color, manifest["theme_color"])
	})

	t.Run("shortName 未設定なら name にフォールバック", func(t *testing.T) {
		name := "テストサーバー"
		manifest := serve(t, &model.Meta{ID: "x", Name: &name})

		assert.Equal(t, name, manifest["short_name"])
	})

	t.Run("manifestJsonOverride が最後に重なる", func(t *testing.T) {
		manifest := serve(t, &model.Meta{
			ID:                   "x",
			ManifestJSONOverride: `{"name":"上書き","icons":[{"src":"/custom.png","sizes":"192x192"}]}`,
		})

		assert.Equal(t, "上書き", manifest["name"])
		icons := iconsOf(t, manifest)
		require.Len(t, icons, 1, "配列は merge ではなく置換")
		assert.Equal(t, "/custom.png", icons[0]["src"])
	})

	t.Run("不正な override は無視する", func(t *testing.T) {
		manifest := serve(t, &model.Meta{ID: "x", ManifestJSONOverride: `{ not json`})

		// default が保たれる (override を落としても manifest 自体は壊さない)
		assert.Len(t, iconsOf(t, manifest), 3)
	})
}

// SPA shell の OGP が upstream `views/base.tsx` の ogSlot と一致すること。
//
// og:image は upstream (ClientServerService.ts:441) では `meta.bannerUrl` で、
// 未設定ならタグ自体を出さない。mk-go は以前 icon を入れていたが、既定値の
// `/static-assets/icons/192.png` は相対 URL で OGP としては解決できなかった
// (#2527)。
func TestFrontendHTML_OpenGraph(t *testing.T) {
	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}

	render := func(t *testing.T, m *model.Meta) string {
		t.Helper()
		repo := testutil.NewMockMetaRepository()
		if m != nil {
			repo.Meta = m
		}
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, frontendHTML(cfg, repo, nil, nil)(c))
		return rec.Body.String()
	}

	t.Run("og:image comes from bannerUrl", func(t *testing.T) {
		banner := "https://example.test/files/banner.png"
		body := render(t, &model.Meta{ID: "x", BannerURL: &banner})

		assert.Contains(t, body, `<meta property="og:image" content="`+banner+`">`)
	})

	t.Run("og:image is omitted without a banner", func(t *testing.T) {
		body := render(t, &model.Meta{ID: "x"})

		assert.NotContains(t, body, `property="og:image"`,
			"upstream は img が null ならタグごと出さない")
		// icon が og:image に紛れ込む旧挙動の regression guard。
		assert.NotContains(t, body, `content="/static-assets/icons/192.png"`)
	})

	t.Run("description falls back to upstream default", func(t *testing.T) {
		body := render(t, nil)

		assert.Contains(t, body, `<meta property="og:description" content="`+defaultInstanceDescription+`">`)
		// meta.description が null なら name="description" は出さない。
		assert.NotContains(t, body, `name="description"`)
	})

	t.Run("description tag is emitted when meta has one", func(t *testing.T) {
		desc := "テストサーバーです"
		body := render(t, &model.Meta{ID: "x", Description: &desc})

		assert.Contains(t, body, `<meta name="description" content="`+desc+`">`)
		assert.Contains(t, body, `<meta property="og:description" content="`+desc+`">`)
	})

	t.Run("empty description keeps the tag but uses the default text", func(t *testing.T) {
		empty := ""
		body := render(t, &model.Meta{ID: "x", Description: &empty})

		// upstream は `props.desc != null` でタグの有無、
		// `props.desc || defaultDescription` で中身を決める。
		assert.Contains(t, body, `<meta name="description" content="`+defaultInstanceDescription+`">`)
	})

	t.Run("name と description は HTML エスケープされる", func(t *testing.T) {
		// 管理者が `"` を入れると属性が閉じて head が壊れる。upstream は
		// kitajs/html が属性値を自動エスケープするので同じ挙動に揃える。
		name := `Evil" onload="alert(1)`
		desc := "<script>alert(1)</script>"
		body := render(t, &model.Meta{ID: "x", Name: &name, Description: &desc})

		assert.NotContains(t, body, `onload="alert(1)`)
		assert.NotContains(t, body, "<script>alert(1)</script>")
		assert.Contains(t, body, `&lt;script&gt;alert(1)&lt;/script&gt;`)
	})
}

// upstream `views/base.tsx` にあって mk-go に無かった head タグ群 (#2527)。
func TestFrontendHTML_UpstreamHeadTags(t *testing.T) {
	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}

	render := func(t *testing.T, m *model.Meta) string {
		t.Helper()
		repo := testutil.NewMockMetaRepository()
		if m != nil {
			repo.Meta = m
		}
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, frontendHTML(cfg, repo, nil, nil)(c))
		return rec.Body.String()
	}

	t.Run("theme-color-orig / format-detection / opensearch", func(t *testing.T) {
		color := "#00eb91"
		body := render(t, &model.Meta{ID: "x", ThemeColor: &color})

		// frontend はテーマ色を戻すときに theme-color-orig を読む。
		assert.Contains(t, body, `<meta name="theme-color" content="`+color+`">`)
		assert.Contains(t, body, `<meta name="theme-color-orig" content="`+color+`">`)
		assert.Contains(t, body, `<meta name="format-detection" content="telephone=no,date=no,address=no,email=no,url=no">`)
		assert.Contains(t, body,
			`<link rel="search" type="application/opensearchdescription+xml" title="Elythia" href="https://example.test/opensearch.xml">`)
	})

	t.Run("opensearch href は URL の末尾スラッシュを重複させない", func(t *testing.T) {
		trailing := &config.Config{URL: "https://example.test/", Version: "0.0.1-test"}
		repo := testutil.NewMockMetaRepository()
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, frontendHTML(trailing, repo, nil, nil)(c))

		assert.Contains(t, rec.Body.String(), `href="https://example.test/opensearch.xml"`)
		assert.NotContains(t, rec.Body.String(), "//opensearch.xml")
	})

	t.Run("branding 画像の prefetch は自 origin だけ", func(t *testing.T) {
		local := "https://example.test/files/error.png"
		relative := "/files/info.png"
		external := "https://cdn.example.com/not-found.png"
		body := render(t, &model.Meta{
			ID:                  "x",
			ServerErrorImageURL: &local,
			InfoImageURL:        &relative,
			NotFoundImageURL:    &external,
		})

		assert.Contains(t, body, `<link rel="prefetch" as="image" href="`+local+`">`)
		assert.Contains(t, body, `<link rel="prefetch" as="image" href="`+relative+`">`)
		// CSP (default-src 'self') で弾かれるだけなので外部 origin は prefetch
		// しない (埋め込み meta JSON には値として載るので link だけを見る)。
		assert.NotContains(t, body, `<link rel="prefetch" as="image" href="`+external+`">`)
	})

	t.Run("未設定なら prefetch を出さない", func(t *testing.T) {
		body := render(t, &model.Meta{ID: "x"})

		assert.NotContains(t, body, `rel="prefetch"`)
	})
}

func TestSameOriginURL(t *testing.T) {
	const base = "https://example.test"

	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "absolute path", url: "/files/x.png", want: true},
		{name: "same origin absolute url", url: "https://example.test/files/x.png", want: true},
		{name: "origin itself", url: "https://example.test", want: true},
		{name: "other origin", url: "https://cdn.example.com/x.png", want: false},
		// `//host/x` は scheme-relative で別 origin。`/` 始まりの判定より先に
		// 弾かないと外部 URL を自 origin と誤判定する。
		{name: "scheme relative url", url: "//cdn.example.com/x.png", want: false},
		// prefix 一致だけで判定すると別ホストを取り込む。
		{name: "prefix lookalike host", url: "https://example.test.evil.com/x.png", want: false},
		{name: "empty", url: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sameOriginURL(base, tt.url))
		})
	}
}

// shellOverrides が upstream base.tsx の props (title / desc / ogSlot) と同じ
// 差し替えをすること (#2528)。permalink はこれでインスタンス既定を上書きする。
func TestRenderFrontendShell_Overrides(t *testing.T) {
	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}
	name := "実験室"
	instanceDesc := "インスタンスの説明"

	render := func(t *testing.T, ov shellOverrides) string {
		t.Helper()
		repo := testutil.NewMockMetaRepository()
		repo.Meta = &model.Meta{ID: "x", Name: &name, Description: &instanceDesc}
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, renderFrontendShell(c, cfg, repo, nil, nil, frontendutil.ClientEntryInfo{}, ov))
		return rec.Body.String()
	}

	t.Run("Title は <title> と opensearch にだけ効く", func(t *testing.T) {
		body := render(t, shellOverrides{Title: "@alice | 実験室"})

		assert.Contains(t, body, `<title>@alice | 実験室</title>`)
		assert.Contains(t, body, `<link rel="search" type="application/opensearchdescription+xml" title="@alice | 実験室"`)
		// og:site_name / instance_url は upstream でもインスタンスの値のまま
		assert.Contains(t, body, `<meta property="og:site_name" content="実験室">`)
	})

	t.Run("Description は description と og:description を差し替える", func(t *testing.T) {
		desc := "ノートの本文"
		body := render(t, shellOverrides{Description: &desc})

		assert.Contains(t, body, `<meta name="description" content="ノートの本文">`)
		assert.Contains(t, body, `<meta property="og:description" content="ノートの本文">`)
		// 埋め込み meta JSON には値として載るので、タグ単位で見る。
		assert.NotContains(t, body, `<meta name="description" content="`+instanceDesc+`">`)
		assert.NotContains(t, body, `<meta property="og:description" content="`+instanceDesc+`">`)
	})

	t.Run("空の Description は upstream 既定文言に落ちる", func(t *testing.T) {
		// upstream base.tsx は `props.desc != null` でタグの有無、
		// `props.desc || defaultDescription` で中身を決める。
		empty := ""
		body := render(t, shellOverrides{Description: &empty})

		assert.Contains(t, body, `<meta name="description" content="`+defaultInstanceDescription+`">`)
	})

	t.Run("OG があれば shell の既定 OGP を出さない", func(t *testing.T) {
		body := render(t, shellOverrides{OG: `<meta property="og:type" content="article">` + "\n"})

		assert.Equal(t, 1, strings.Count(body, `property="og:type"`))
		assert.Contains(t, body, `<meta property="og:type" content="article">`)
		assert.NotContains(t, body, `content="website"`)
		// ページ固有 description が無い permalink ではインスタンス説明も出さない
		assert.NotContains(t, body, `<meta name="description"`)
	})

	t.Run("Head は verbatim で入る", func(t *testing.T) {
		body := render(t, shellOverrides{Head: `<meta name="misskey:note-id" content="n1">` + "\n"})

		assert.Contains(t, body, `<meta name="misskey:note-id" content="n1">`)
		// OG が空なので既定 OGP は残る
		assert.Contains(t, body, `<meta property="og:type" content="website">`)
	})
}

// upstream ClientServerService は shell とページ種別ごとに Cache-Control を
// 出している (#2534)。付いていないと共有キャッシュや CDN のヒューリスティック
// 判断に委ねられる。
func TestRenderFrontendShell_CacheControl(t *testing.T) {
	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}

	render := func(t *testing.T, ov shellOverrides) *httptest.ResponseRecorder {
		t.Helper()
		repo := testutil.NewMockMetaRepository()
		repo.Meta = &model.Meta{ID: "x"}
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, renderFrontendShell(c, cfg, repo, nil, nil, frontendutil.ClientEntryInfo{}, ov))
		return rec
	}

	t.Run("既定は upstream renderBase と同じ", func(t *testing.T) {
		rec := render(t, shellOverrides{})
		assert.Equal(t, "public, max-age=30", rec.Header().Get("Cache-Control"))
	})

	t.Run("ページ側の指定が優先される", func(t *testing.T) {
		rec := render(t, shellOverrides{CacheControl: "public, max-age=15"})
		assert.Equal(t, "public, max-age=15", rec.Header().Get("Cache-Control"))
	})

	t.Run("X-Robots-Tag は値ごとに 1 行ずつ", func(t *testing.T) {
		rec := render(t, shellOverrides{RobotsTag: []string{"noimageai", "noai"}})
		assert.Equal(t, []string{"noimageai", "noai"}, rec.Header().Values("X-Robots-Tag"))
	})

	t.Run("指定が無ければ X-Robots-Tag は出ない", func(t *testing.T) {
		rec := render(t, shellOverrides{})
		assert.Empty(t, rec.Header().Values("X-Robots-Tag"))
	})
}

// OAuth 同意画面は共有キャッシュに載せない。transaction id を含む HTML が
// CDN に載ると別の利用者に配られる (upstream も OAuth 側で no-store)。
func TestFrontendConsentHTML_IsNotCacheable(t *testing.T) {
	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}
	repo := testutil.NewMockMetaRepository()
	repo.Meta = &model.Meta{ID: "x"}

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/oauth/authorize", nil), rec)
	require.NoError(t, frontendConsentHTML(cfg, repo, nil, nil)(c, oauth.ConsentMeta{
		TransactionID: "tx-secret",
		ClientName:    "Test App",
		Scope:         "read:account",
	}))

	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Contains(t, rec.Body.String(), `content="tx-secret"`)
}

// boot.js は shell が出す canvas を id で引いて星を描く。
// **片方だけ id を変えると、エラーも出ずに星だけが消える。** Go 側の markup と
// 実物の boot.js が同じ id を使っていることをここで突き合わせる。
func TestFrontendHTML_SplashSkyIDMatchesBootLoader(t *testing.T) {
	js, err := os.ReadFile(filepath.Join("..", "..", "frontend", "packages", "frontend", "public", "loader", "boot.js"))
	require.NoError(t, err)
	for _, id := range []string{"splash", "splashSky"} {
		assert.Contains(t, splashMarkup, `id="`+id+`"`)
		assert.Contains(t, string(js), `getElementById('`+id+`')`)
	}
}

// 夜空の色はテーマカラーから取らない。
//
// 以前はスピナーの色として `--splash-color` にテーマカラーを入れていた (#2549)。
// 夜空は既定アイコンから決めた固定の配色で、テーマカラーを入れる先が無い。
// **管理者が自由に入れられる値を `<style>` に流さない**ことも、この形で保たれる。
func TestFrontendHTML_SplashIgnoresThemeColor(t *testing.T) {
	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}
	repo := testutil.NewMockMetaRepository()
	evil := "red}body{display:none"
	repo.Meta = &model.Meta{ID: "x", ThemeColor: &evil}

	handler := frontendHTML(cfg, repo, nil, nil)
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	require.NoError(t, handler(c))

	body := rec.Body.String()
	assert.NotContains(t, body, "--splash-color")
	// `<meta name="theme-color">` には属性 escape された値が出る (属性値なので正しい)。
	// style 要素の中に生のまま入っていないことを見る
	for _, style := range regexp.MustCompile(`(?s)<style>.*?</style>`).FindAllString(body, -1) {
		assert.NotContains(t, style, evil)
	}
}

// **loader は URL 参照ではなく埋め込む (#2551)。** ファイル名にハッシュが付かない
// ので、参照のままだと内容を変えても URL が変わらず、CDN の手前で古い版が居座る。
// HTML だけが新しくなると「新しいマークアップに古い CSS が当たる」状態になり、
// スプラッシュが静止したまま表示されなくなる (#2549 で実際に踏んだ)。
func TestFrontendHTML_InlinesLoaderAssets(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "loader"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "loader", "style.css"),
		[]byte("#splash{--marker:inlined-css}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "loader", "boot.js"),
		[]byte("window.__markerInlinedJs = 1;"), 0o644))
	t.Setenv("MISSKEY_FRONTEND_DIR", dir)
	frontendutil.ResetLoaderCacheForTest()
	t.Cleanup(frontendutil.ResetLoaderCacheForTest)

	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}
	handler := frontendHTML(cfg, testutil.NewMockMetaRepository(), nil, nil)

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	require.NoError(t, handler(c))

	body := rec.Body.String()
	assert.Contains(t, body, "#splash{--marker:inlined-css}")
	assert.Contains(t, body, "window.__markerInlinedJs = 1;")
	// 参照が残っていると、埋め込みと二重に読むうえ古い版が当たる余地が残る。
	assert.NotContains(t, body, `href="/vite/loader/style.css"`)
	assert.NotContains(t, body, `src="/vite/loader/boot.js"`)
}

// 読めなければ従来どおり参照に落とす。**ここが落ちると loader を一切読まない
// シェルになり、スプラッシュどころか起動しない。**
func TestFrontendHTML_FallsBackToLoaderReferences(t *testing.T) {
	t.Setenv("MISSKEY_FRONTEND_DIR", t.TempDir())
	frontendutil.ResetLoaderCacheForTest()
	t.Cleanup(frontendutil.ResetLoaderCacheForTest)

	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}
	handler := frontendHTML(cfg, testutil.NewMockMetaRepository(), nil, nil)

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	require.NoError(t, handler(c))

	body := rec.Body.String()
	assert.Contains(t, body, `<link rel="stylesheet" href="/vite/loader/style.css">`)
	assert.Contains(t, body, `<script src="/vite/loader/boot.js"></script>`)
}

// **dev では埋め込まない (#2477 と同じ理由)。** ビルド成果物が残っているだけの
// 状態で埋め込むと、dev server が返す新しい loader ではなく古い built を読む。
func TestFrontendHTML_DevModeKeepsReferences(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "loader"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "loader", "style.css"),
		[]byte("#splash{--marker:stale}"), 0o644))
	t.Setenv("MISSKEY_FRONTEND_DIR", dir)
	frontendutil.ResetLoaderCacheForTest()
	t.Cleanup(frontendutil.ResetLoaderCacheForTest)

	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test", Dev: true}
	handler := frontendHTML(cfg, testutil.NewMockMetaRepository(), nil, nil)

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	require.NoError(t, handler(c))

	body := rec.Body.String()
	assert.NotContains(t, body, "--marker:stale", "古い built を埋め込まない")
	assert.Contains(t, body, `href="/vite/loader/style.css"`)
}

// **SSR 埋め込み meta にも同じ変換が要る (#2583)。** frontend は埋め込みを
// localStorage cache より優先し、以後 1 時間 /api/meta を再取得しないので、
// ここが JSON5 のままだと片方だけ直しても直らない。
func TestFrontendHTML_PacksThemeAsJSON(t *testing.T) {
	repo := testutil.NewMockMetaRepository()
	theme := `{id:'6c445bcb',base:'dark',name:'夜の世界',props:{bg:'rgb(32, 34, 37)'}}`
	repo.Meta = &model.Meta{ID: "x", DefaultDarkTheme: &theme}

	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}
	handler := frontendHTML(cfg, repo, nil, nil)

	e := echo.New()
	rec := httptest.NewRecorder()
	require.NoError(t, handler(e.NewContext(
		httptest.NewRequest(http.MethodGet, "/", nil), rec)))

	embedded := extractEmbeddedMeta(t, rec.Body.String())
	dark, ok := embedded["defaultDarkTheme"].(string)
	require.True(t, ok, "文字列で埋め込まれること")

	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(dark), &parsed),
		"JSON として読めること (client は JSON.parse する)")
	assert.Equal(t, "夜の世界", parsed["name"])
}

func TestFrontendHTML_BrokenThemeBecomesNull(t *testing.T) {
	repo := testutil.NewMockMetaRepository()
	broken := "not a theme"
	repo.Meta = &model.Meta{ID: "x", DefaultDarkTheme: &broken}

	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}
	handler := frontendHTML(cfg, repo, nil, nil)

	e := echo.New()
	rec := httptest.NewRecorder()
	require.NoError(t, handler(e.NewContext(
		httptest.NewRequest(http.MethodGet, "/", nil), rec)))

	embedded := extractEmbeddedMeta(t, rec.Body.String())
	assert.Nil(t, embedded["defaultDarkTheme"])
}

// extractEmbeddedMeta pulls the `#misskey_meta` payload out of the shell.
func extractEmbeddedMeta(t *testing.T, body string) map[string]any {
	t.Helper()
	const openTag = `id="misskey_meta"`
	idx := strings.Index(body, openTag)
	require.NotEqual(t, -1, idx, "misskey_meta script should be embedded")
	start := strings.Index(body[idx:], ">")
	require.NotEqual(t, -1, start)
	start += idx + 1
	end := strings.Index(body[start:], "</script>")
	require.NotEqual(t, -1, end)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(body[start:start+end]), &parsed))
	return parsed
}

// SSR 埋め込み meta にもビルドの revision を載せること (#2700)。/about-elythia は
// fetchInstance を待たずに描けるので、こちらに無いと初回描画でだけ版が欠ける。
func TestFrontendHTML_EmbedsBuildRevision(t *testing.T) {
	// ldflags で埋める package 変数。プロセス共有なので必ず戻す (#2795)。
	prevCommit := config.MkGoCommit
	t.Cleanup(func() { config.MkGoCommit = prevCommit })
	config.MkGoCommit = "abc1234"

	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}
	repo := testutil.NewMockMetaRepository()
	repo.Meta = &model.Meta{ID: "x"}
	handler := frontendHTML(cfg, repo, nil, nil)

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	require.NoError(t, handler(c))

	parsed := extractEmbeddedMeta(t, rec.Body.String())
	assert.Equal(t, "abc1234", parsed["mkGoCommit"])
	assert.NotContains(t, parsed, "mkGoFrontendVersion", "廃止した field (#3379)")
}

// SSR 埋め込み meta の providesTarball も /api/meta と同じく設定を無視して
// false を返すこと (#2700)。
//
// **片側だけ戻る形を止めるためのテスト。** この埋め込み meta は frontend の
// instance cache を上書きする経路なので、`/api/meta` だけ直っていても
// ここが設定値を返すと、初回描画では tarball リンクが出るという食い違いになる。
// 既存の `TestSSRMetaCoversAPIMeta` はキーの有無しか見ないので捕まらない。
func TestFrontendHTML_ProvidesTarballIgnoresConfigFlag(t *testing.T) {
	cfg := &config.Config{
		URL:     "https://example.test",
		Version: "0.0.1-test",
		PublishTarballInsteadOfProvideRepositoryURL: true,
	}

	repo := testutil.NewMockMetaRepository()
	repo.Meta = &model.Meta{ID: "x"}
	handler := frontendHTML(cfg, repo, nil, nil)

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	require.NoError(t, handler(c))

	parsed := extractEmbeddedMeta(t, rec.Body.String())
	assert.Equal(t, false, parsed["providesTarball"],
		"設定が true でも mk-go に /tarball/ は無いので false を出すこと")
}

// `/about-misskey` のクレジット画像 62 枚が読める origin を img-src に足していること
// (#2892)。#2700 で upstream の謝辞を残す判断をしたので、ここを許さないとその
// ページには恒久的に壊れた画像が並ぶ。
//
// **media proxy 経由には落とせない** — mk-go の proxy は open proxy ではなく、
// allowlist は DB に実在する URL だけを通すので静的な URL は 403 になる。
func TestFrontendHTML_CreditImageOriginsInCSP(t *testing.T) {
	cfg := &config.Config{
		URL: "https://example.test", Version: "0.0.1-test",
		FrontendContentSecurityPolicy: CSPModeEnforce,
	}
	handler := frontendHTML(cfg, testutil.NewMockMetaRepository(), nil, nil)
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	require.NoError(t, handler(c))

	csp := rec.Header().Get("Content-Security-Policy")
	require.NotEmpty(t, csp)

	directives := map[string]string{}
	for _, d := range strings.Split(csp, "; ") {
		name, rest, _ := strings.Cut(d, " ")
		directives[name] = rest
	}

	// **host はリテラルで書く。** `creditImageOrigins` をループの反復元にすると
	// 検証対象そのものを truth にすることになり、定数を空にしても片方を消しても
	// 通ってしまう (= #2892 が直した状態に戻っても検出しない)。
	imgSrc, ok := directives["img-src"]
	require.True(t, ok, "img-src directive が無い")
	assert.Contains(t, imgSrc, "https://avatars.githubusercontent.com",
		"プロジェクトメンバーのアイコン 6 枚が読めない")
	assert.Contains(t, imgSrc, "https://assets.misskey-hub.net",
		"スポンサー 6 枚とパトロン 50 枚が読めない")

	// **増やす方向も固定する。** ワイルドカード (`https://*.githubusercontent.com`)
	// に広げると open image proxy の `camo.githubusercontent.com` まで開く。
	assert.Len(t, creditImageOrigins, 2, "許可する origin を増減させるときはこのテストも見直すこと")
	for _, origin := range creditImageOrigins {
		assert.NotContains(t, origin, "*", "ワイルドカードは使わない")
	}

	// **img-src だけに足す。** これらの host からは画像しか読まないので、
	// media-src / connect-src には要らない (`cspExtras.Media` と分けた理由)。
	for _, name := range []string{"media-src", "connect-src", "script-src", "style-src"} {
		for _, origin := range []string{"https://avatars.githubusercontent.com", "https://assets.misskey-hub.net"} {
			assert.NotContains(t, directives[name], origin,
				"%s に画像用の origin が漏れている", name)
		}
	}
}
