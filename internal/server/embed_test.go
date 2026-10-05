package server

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/frontendutil"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strptr(s string) *string { return &s }

// 埋め込みは認証を伴わない経路なので、ここが緩むとそのまま IDOR になる。
// upstream ClientServerService の `/embed/notes/:note` と同じ判定を固定する。
func TestEmbedNoteIsPublic(t *testing.T) {
	tests := []struct {
		name string
		note *model.Note
		want bool
	}{
		{
			name: "public local note is embeddable",
			note: &model.Note{Visibility: "public"},
			want: true,
		},
		{
			name: "home visibility is embeddable",
			note: &model.Note{Visibility: "home"},
			want: true,
		},
		{
			// 宛先を指定した投稿。埋め込むと宛先以外の誰でも読める。
			name: "specified visibility is never embeddable",
			note: &model.Note{Visibility: "specified"},
			want: false,
		},
		{
			// フォロワー限定。同上。
			name: "followers visibility is never embeddable",
			note: &model.Note{Visibility: "followers"},
			want: false,
		},
		{
			// リモートノートは自分が権威でないので配らない (upstream と同じ)。
			name: "remote note is not embeddable",
			note: &model.Note{Visibility: "public", UserHost: strptr("remote.example")},
			want: false,
		},
		{
			// host が空文字列はローカル扱い。DB 上 NULL でなく "" が入る経路への保険。
			name: "empty host counts as local",
			note: &model.Note{Visibility: "public", UserHost: strptr("")},
			want: true,
		},
		{
			name: "remote followers note is not embeddable",
			note: &model.Note{Visibility: "followers", UserHost: strptr("remote.example")},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := embedNoteIsPublic(tt.note); got != tt.want {
				t.Errorf("embedNoteIsPublic(%+v) = %v, want %v", tt.note, got, tt.want)
			}
		})
	}
}

// JSON を <script> に埋める以上、閉じタグの早期終了は必ず潰す。ノート本文も
// ユーザー名も攻撃者が自由に書けるので、ここが抜けると XSS になる。
func TestEscapeJSONForScript(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "closing script tag is neutralised",
			input: `{"text":"</script><img src=x onerror=alert(1)>"}`,
			want:  `{"text":"\u003c/script\u003e\u003cimg src=x onerror=alert(1)\u003e"}`,
		},
		{
			name:  "ampersand is escaped",
			input: `{"text":"a&b"}`,
			want:  `{"text":"a\u0026b"}`,
		},
		{
			name:  "plain json is unchanged",
			input: `{"a":1}`,
			want:  `{"a":1}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := escapeJSONForScript(tt.input); got != tt.want {
				t.Errorf("escapeJSONForScript(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// 攻撃者が制御できる文字列がどう来ても、script 要素を閉じる並びは残らない。
func TestEscapeJSONForScriptNeverLeavesClosingTag(t *testing.T) {
	payloads := []string{
		`{"text":"</script>"}`,
		`{"text":"</SCRIPT>"}`,
		`{"text":"</script >"}`,
		`{"name":"<!--</script>"}`,
	}
	for _, p := range payloads {
		got := escapeJSONForScript(p)
		if strings.Contains(got, "<") || strings.Contains(got, ">") {
			t.Errorf("escapeJSONForScript(%q) left raw angle brackets: %q", p, got)
		}
	}
}

func TestJSStringLiteral(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "plain version", input: "2026.9.0", want: `"2026.9.0"`},
		{name: "double quote cannot close the literal", input: `a"b`, want: `"a\"b"`},
		{name: "backslash is escaped", input: `a\`, want: `"a\\"`},
		{name: "closing script tag is neutralised", input: "</script>", want: `"\u003c/script\u003e"`},
		{name: "html entity is not used", input: "a&b", want: `"a\u0026b"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, jsStringLiteral(tt.input))
		})
	}
}

// inline script の VERSION は JS の文字列リテラルとして埋め込む。
// html.EscapeString は <script> の中では効かないので、`"` で抜けられた。
func TestShells_VersionIsAJSStringLiteral(t *testing.T) {
	cfg := &config.Config{URL: "https://example.test", Version: `1"</script>`}
	const want = `const VERSION = "1\"\u003c/script\u003e";`

	t.Run("embed", func(t *testing.T) {
		h := &embedHandlers{cfg: cfg, metaRepo: testutil.NewMockMetaRepository()}
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/embed/notes/x", nil), rec)
		require.NoError(t, h.render(c, nil))
		assert.Contains(t, rec.Body.String(), want)
	})

	t.Run("spa", func(t *testing.T) {
		handler := frontendHTML(cfg, testutil.NewMockMetaRepository(), nil, nil)
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, handler(c))
		assert.Contains(t, rec.Body.String(), want)
	})
}

// embed shell の `<head>` が upstream `views/base-embed.tsx` と同じ icon link を
// 出すこと。SPA shell と同じ fallback (#2527)。
func TestEmbedShell_IconLinks(t *testing.T) {
	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}

	render := func(t *testing.T, m *model.Meta) string {
		t.Helper()
		repo := testutil.NewMockMetaRepository()
		if m != nil {
			repo.Meta = m
		}
		h := &embedHandlers{cfg: cfg, metaRepo: repo}

		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/embed/notes/x", nil), rec)
		require.NoError(t, h.render(c, nil))
		return rec.Body.String()
	}

	t.Run("defaults match upstream base-embed.tsx", func(t *testing.T) {
		body := render(t, nil)

		assert.Contains(t, body, `<link rel="icon" href="/favicon.ico">`)
		assert.Contains(t, body, `<link rel="apple-touch-icon" href="/apple-touch-icon.png">`)
	})

	t.Run("reflects meta icon urls", func(t *testing.T) {
		icon := "https://example.test/files/server-icon.png"
		appIcon := "https://example.test/files/app-512.png"
		body := render(t, &model.Meta{ID: "x", IconURL: &icon, App512IconURL: &appIcon})

		assert.Contains(t, body, `<link rel="icon" href="`+icon+`">`)
		assert.Contains(t, body, `<link rel="apple-touch-icon" href="`+appIcon+`">`)
	})
}

// embed も SPA シェルと同じく loader を埋め込む (#2551)。同じファイル名 (ハッシュ
// 無し) を参照していたので、同じ形で古い版が居座る。
func TestEmbedShell_InlinesLoaderAssets(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "loader"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "loader", "style.css"),
		[]byte("#embed{--marker:inlined-embed-css}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "loader", "boot.js"),
		[]byte("window.__markerEmbedJs = 1;"), 0o644))
	t.Setenv("MISSKEY_FRONTEND_EMBED_DIR", dir)
	frontendutil.ResetLoaderCacheForTest()
	t.Cleanup(frontendutil.ResetLoaderCacheForTest)

	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}
	h := &embedHandlers{cfg: cfg, metaRepo: testutil.NewMockMetaRepository()}

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/embed/notes/x", nil), rec)
	require.NoError(t, h.render(c, nil))

	body := rec.Body.String()
	assert.Contains(t, body, "#embed{--marker:inlined-embed-css}")
	assert.Contains(t, body, "window.__markerEmbedJs = 1;")
	assert.NotContains(t, body, `href="/embed_vite/loader/style.css"`)
	assert.NotContains(t, body, `src="/embed_vite/loader/boot.js"`)
}

func TestEmbedShell_FallsBackToLoaderReferences(t *testing.T) {
	t.Setenv("MISSKEY_FRONTEND_EMBED_DIR", t.TempDir())
	frontendutil.ResetLoaderCacheForTest()
	t.Cleanup(frontendutil.ResetLoaderCacheForTest)

	cfg := &config.Config{URL: "https://example.test", Version: "0.0.1-test"}
	h := &embedHandlers{cfg: cfg, metaRepo: testutil.NewMockMetaRepository()}

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/embed/notes/x", nil), rec)
	require.NoError(t, h.render(c, nil))

	body := rec.Body.String()
	assert.Contains(t, body, `<link rel="stylesheet" href="/embed_vite/loader/style.css">`)
	assert.Contains(t, body, `<script src="/embed_vite/loader/boot.js"></script>`)
}

// embed にも SPA shell と同じ CSP を付ける (#2789)。embed は
// `X-Frame-Options` の除外対象 = 第三者ページに iframe で埋め込まれる唯一の
// 経路なので、script が注入されると埋め込み先ではなく**こちらの origin** で動く。
func TestEmbedShell_CSP(t *testing.T) {
	// **loader を inline させる。** built assets が無いと `loader.JS` が空になり、
	// HTML に出る inline script は bootGlobals だけになる。実運用でいちばん大きい
	// inline script (loader) の hash 経路がテストの外に出るので、fixture を置いて
	// 2 つとも通す (SPA 側の `TestFrontendCSP_HashesCoverRenderedInlineScripts`
	// と同じ形)。
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "loader"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "loader", "boot.js"),
		[]byte("window.__markerEmbedBootJs = 1;"), 0o644))
	t.Setenv("MISSKEY_FRONTEND_EMBED_DIR", dir)
	frontendutil.ResetLoaderCacheForTest()
	// cache は sync.Once なので、戻さないと fixture の loader がプロセス全体に
	// 残る。同 package に embed shell を描画するテストが他にもある。
	t.Cleanup(frontendutil.ResetLoaderCacheForTest)

	render := func(t *testing.T, cfg *config.Config, m *model.Meta) *httptest.ResponseRecorder {
		t.Helper()
		repo := testutil.NewMockMetaRepository()
		if m != nil {
			repo.Meta = m
		}
		h := &embedHandlers{cfg: cfg, metaRepo: repo}

		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/embed/notes/x", nil), rec)
		require.NoError(t, h.render(c, nil))
		return rec
	}

	base := func() *config.Config {
		return &config.Config{URL: "https://example.test", Version: "0.0.1-test"}
	}

	t.Run("off sends no header", func(t *testing.T) {
		cfg := base()
		cfg.FrontendContentSecurityPolicy = CSPModeOff
		rec := render(t, cfg, nil)

		assert.Empty(t, rec.Header().Get("Content-Security-Policy"))
		assert.Empty(t, rec.Header().Get("Content-Security-Policy-Report-Only"))
	})

	// **クレジット画像の origin は embed に足さない** (#2892)。`/about-misskey` は
	// SPA shell にしか無いので、embed で許す理由が無い。captcha の origin を
	// embed に足さないのと同じ判断で、これが崩れると「使っていない host を
	// CSP に載せない」という方針が形骸化する。
	t.Run("credit image origins are not added to the embed shell", func(t *testing.T) {
		cfg := base()
		cfg.FrontendContentSecurityPolicy = CSPModeEnforce
		rec := render(t, cfg, nil)

		// SPA 側と同じ理由で host はリテラルで書く (定数をループの反復元にすると
		// 空にしたときに素通りする)。
		csp := rec.Header().Get("Content-Security-Policy")
		require.NotEmpty(t, csp)
		assert.NotContains(t, csp, "https://avatars.githubusercontent.com",
			"embed の CSP にクレジット画像の origin が漏れている")
		assert.NotContains(t, csp, "https://assets.misskey-hub.net",
			"embed の CSP にクレジット画像の origin が漏れている")
	})

	t.Run("report-only sends the report-only header", func(t *testing.T) {
		cfg := base()
		cfg.FrontendContentSecurityPolicy = CSPModeReportOnly
		rec := render(t, cfg, nil)

		assert.Empty(t, rec.Header().Get("Content-Security-Policy"))
		assert.NotEmpty(t, rec.Header().Get("Content-Security-Policy-Report-Only"))
	})

	t.Run("enforce covers the shell's own inline script", func(t *testing.T) {
		cfg := base()
		cfg.FrontendContentSecurityPolicy = CSPModeEnforce
		rec := render(t, cfg, nil)

		csp := rec.Header().Get("Content-Security-Policy")
		require.NotEmpty(t, csp)

		// **HTML に実際に入っている script の hash が載っていること。** 別々に
		// 組み立てると、片方だけ変えたときに埋め込みが白紙になる (#2786)。
		body := rec.Body.String()
		for _, script := range inlineScriptBodies(t, body) {
			sum := sha256.Sum256([]byte(script))
			want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			assert.Contains(t, csp, want,
				"inline script の hash が CSP に無い: %q", script)
		}

		// #2786 で外したものが embed 側から復活していないこと。
		scriptSrc := cspDirective(t, csp, "script-src")
		assert.NotContains(t, scriptSrc, "'unsafe-inline'")
		assert.Contains(t, scriptSrc, "'sha256-")

		// `frame-ancestors` は入れない。X-Frame-Options 側が /embed/ の除外を
		// 持っており、CSP に重ねると除外が二重管理になる。
		assert.NotContains(t, csp, "frame-ancestors")
	})

	t.Run("object storage origin is allowed for media", func(t *testing.T) {
		cfg := base()
		cfg.FrontendContentSecurityPolicy = CSPModeEnforce
		baseURL := "https://s3.example.test/bucket"
		rec := render(t, cfg, &model.Meta{
			ID: "x", UseObjectStorage: true, ObjectStorageBaseURL: &baseURL,
		})

		// drive のファイルが object storage から直接配信される構成では、
		// これが無いと埋め込んだ投稿の画像・動画が丸ごと出ない。
		csp := rec.Header().Get("Content-Security-Policy")
		assert.Contains(t, cspDirective(t, csp, "img-src"), "https://s3.example.test")
		assert.Contains(t, cspDirective(t, csp, "media-src"), "https://s3.example.test")
		// connect-src も mediaDirectives に含まれる。ここが欠けると「通知音
		// だけ鳴らない」類の気付きにくい壊れ方をする (frontend_csp.go)。
		assert.Contains(t, cspDirective(t, csp, "connect-src"), "https://s3.example.test")
	})

	t.Run("captcha origins are not added", func(t *testing.T) {
		cfg := base()
		cfg.FrontendContentSecurityPolicy = CSPModeEnforce
		rec := render(t, cfg, &model.Meta{ID: "x", EnableHcaptcha: true, EnableRecaptcha: true})

		// embed はサインアップ経路を持たない。使っていない host を CSP に
		// 載せる理由が無い。
		csp := rec.Header().Get("Content-Security-Policy")
		// **captchaCSPExtras が実際に出す origin を見る。** `google.com` は
		// どの分岐でも生成されないので、書いても常に通る。
		assert.NotContains(t, csp, "hcaptcha.com")
		assert.NotContains(t, csp, "recaptcha.net")
		assert.NotContains(t, csp, "gstatic.com")
		assert.NotContains(t, csp, "challenges.cloudflare.com")
	})
}

// inlineScriptBodies extracts the bodies of `<script>` elements that carry no
// `src` and no `type` attribute (= the ones a CSP hash must cover).
func inlineScriptBodies(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	rest := body
	for {
		i := strings.Index(rest, "<script>")
		if i < 0 {
			break
		}
		rest = rest[i+len("<script>"):]
		j := strings.Index(rest, "</script>")
		require.GreaterOrEqual(t, j, 0, "閉じていない <script> がある")
		out = append(out, rest[:j])
		rest = rest[j:]
	}
	// **件数を固定する。** NotEmpty だと片方が消えても通り、loader の hash 経路が
	// 無検証に戻る (実際、この形にする前は `inlineScripts` から loader を落とす
	// 変異が生き残った)。
	require.Len(t, out, 2, "inline script は bootGlobals と loader の 2 つ")
	return out
}

// cspDirective returns one directive from a CSP header value.
func cspDirective(t *testing.T, csp, name string) string {
	t.Helper()
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if d == name || strings.HasPrefix(d, name+" ") {
			return d
		}
	}
	t.Fatalf("directive %q が CSP に無い: %s", name, csp)
	return ""
}
