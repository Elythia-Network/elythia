package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/frontendutil"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

func cspHeadersFor(t *testing.T, mode string) http.Header {
	t.Helper()
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	applyFrontendCSP(c, mode, cspExtras{})
	return rec.Header()
}

// mode ごとに付く header が変わる。未知の値では**何も付けない**のが要点で、
// 設定ミスで enforce に倒れてフロントが動かなくなるより無効の方が安全。
func TestApplyFrontendCSP_HeaderPerMode(t *testing.T) {
	cases := []struct {
		mode       string
		wantHeader string
	}{
		{CSPModeOff, ""},
		{CSPModeReportOnly, "Content-Security-Policy-Report-Only"},
		{CSPModeEnforce, "Content-Security-Policy"},
		{"", ""},
		{"typo", ""},
		{"Report-Only", ""}, // 大文字小文字は揃えない = 未知扱い
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			h := cspHeadersFor(t, tc.mode)
			if tc.wantHeader == "" {
				assert.Empty(t, h.Get("Content-Security-Policy"))
				assert.Empty(t, h.Get("Content-Security-Policy-Report-Only"))
				return
			}
			assert.NotEmpty(t, h.Get(tc.wantHeader))
			// 片方だけが付く (両方付けるとブラウザが両方評価してしまう)。
			other := "Content-Security-Policy"
			if tc.wantHeader == other {
				other = "Content-Security-Policy-Report-Only"
			}
			assert.Empty(t, h.Get(other))
		})
	}
}

// report-only / enforce のどちらでも report-uri を出す。enforce でブロックが
// 起きていることに気付けないと、「一部の機能だけ動かない」という報告しか
// 上がってこない。
func TestApplyFrontendCSP_AlwaysReports(t *testing.T) {
	for _, mode := range []string{CSPModeReportOnly, CSPModeEnforce} {
		h := cspHeadersFor(t, mode)
		v := h.Get("Content-Security-Policy") + h.Get("Content-Security-Policy-Report-Only")
		assert.Containsf(t, v, "report-uri "+CSPReportPath, "mode=%s", mode)
	}
}

// ポリシーの中身を固定する。**緩める変更は意図的にしか起こしてはいけない**ので、
// 主要 directive の存在と危険な緩和が無いことを assert する。
func TestFrontendCSP_Policy(t *testing.T) {
	policy := buildFrontendCSP(false, cspExtras{})

	t.Run("必要な directive が揃っている", func(t *testing.T) {
		for _, d := range []string{
			"default-src 'self'",
			"base-uri 'self'",
			"object-src 'none'",
			"form-action 'self'",
			"script-src", "style-src", "img-src", "media-src",
			"font-src", "connect-src", "worker-src", "frame-src",
		} {
			assert.Containsf(t, policy, d, "missing %q", d)
		}
	})

	t.Run("危険な緩和を含まない", func(t *testing.T) {
		// script-src に 'unsafe-eval' を入れると CSP の意味がほぼ無くなる。
		assert.NotContains(t, policy, "unsafe-eval")
		// ワイルドカード許可はしない ('self' で足りる)。
		assert.NotContains(t, policy, "default-src *")
		assert.NotContains(t, policy, "script-src *")
	})

	t.Run("frame-ancestors を含まない", func(t *testing.T) {
		// X-Frame-Options (frameguard.go) が /embed/ を除外する仕組みを持って
		// いるので、CSP に重ねると除外を二重管理することになる。embed の配線が
		// 入るときに一緒に設計する。
		assert.NotContains(t, policy, "frame-ancestors")
	})

	t.Run("report 無しなら report-uri を含まない", func(t *testing.T) {
		assert.NotContains(t, policy, "report-uri")
	})
}

// script と style で扱いが違う (#2786)。
//
//   - **script は `'unsafe-inline'` を外した。** SPA shell の inline script は
//     内容が起動時に固定なので hash で通す。ここに戻すと XSS で注入された
//     `<script>` がそのまま実行されるようになる
//   - **style は残す。** Vue の `:style` が DOM の inline `style` 属性になり、
//     属性は `style-src-attr` の管轄で hash では救えない。外すと UI が広範に壊れる
func TestFrontendCSP_InlineScriptIsHashedNotUnsafe(t *testing.T) {
	policy := buildFrontendCSP(false, cspExtras{})
	for _, d := range strings.Split(policy, "; ") {
		switch {
		case strings.HasPrefix(d, "script-src"):
			assert.NotContainsf(t, d, "'unsafe-inline'",
				"%s: inline script は hash で通す。ここを戻すと注入された script が実行される", d)
		case strings.HasPrefix(d, "style-src"):
			assert.Containsf(t, d, "'unsafe-inline'",
				"%s: Vue の :style が inline style 属性になるので外せない", d)
		}
	}
}

// hash は呼び出し側が渡した文字列そのものから導く。
func TestCSPScriptHashes(t *testing.T) {
	// 空は飛ばす (loader が外部参照のとき inline script は存在しない)。
	assert.Empty(t, cspScriptHashes("", ""))

	// RFC 4648 の base64 で `'sha256-` に包む。期待値は独立に計算した
	// (`printf 'x' | openssl dgst -sha256 -binary | openssl base64`)。
	got := cspScriptHashes("x")
	assert.Equal(t, []string{"'sha256-LXEWQrcmsEQBYnyp+6wy9chTD7GQPMTbAiWHF5IaSIE='"}, got)

	// **内容が 1 文字変われば hash も変わる。** ここが固定だと、HTML を変えたのに
	// CSP が古いままという状態を検出できない。
	assert.NotEqual(t, cspScriptHashes("const A = 1;"), cspScriptHashes("const A = 2;"))

	// 複数渡すと順に並ぶ。
	assert.Len(t, cspScriptHashes("a", "b"), 2)
}

func postCSPReport(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, CSPReportPath, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, "application/csp-report")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, cspReportHandler()(c))
	return rec
}

// 何を投げても 204。内容が妥当だったかを攻撃者に返さない。
func TestCSPReportHandler_AlwaysNoContent(t *testing.T) {
	cases := map[string]string{
		"正規の report":    `{"csp-report":{"document-uri":"https://x.example/","violated-directive":"script-src","blocked-uri":"https://evil.example/x.js"}}`,
		"空 body":        ``,
		"壊れた JSON":      `{not json`,
		"csp-report 無し": `{"foo":"bar"}`,
		"配列":            `[1,2,3]`,
		"巨大 body":       `{"csp-report":{"document-uri":"` + strings.Repeat("A", cspReportMaxBody) + `"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := postCSPReport(t, body)
			assert.Equal(t, http.StatusNoContent, rec.Code)
			assert.Empty(t, rec.Body.String())
		})
	}
}

// effective-directive しか無い形式 (CSP Level 3 寄りの実装) でも読める。
func TestCSPReportHandler_EffectiveDirectiveFallback(t *testing.T) {
	rec := postCSPReport(t, `{"csp-report":{"effective-directive":"img-src","blocked-uri":"data:"}}`)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// object storage / 外部 media proxy の origin は img-src / media-src /
// connect-src にだけ足す (#2425 / #2501)。
//
// drive のファイルは object storage から直接配信されるので、`'self'` だけだと
// enforce 時に画像・動画・音声が丸ごと表示できなくなる。本番の report-only で
// 実際に img-src / media-src の違反が出て判明した。connect-src は通知音の
// fetch (sound.ts の decodeAudioData 経路) が使う。
func TestBuildFrontendCSP_ExtraMediaOrigins(t *testing.T) {
	const origin = "https://objects.example"
	policy := buildFrontendCSP(false, cspExtras{Media: []string{origin}})

	byName := map[string]string{}
	for _, d := range strings.Split(policy, "; ") {
		name, _, _ := strings.Cut(d, " ")
		byName[name] = d
	}

	t.Run("img-src と media-src と connect-src に足す", func(t *testing.T) {
		assert.Contains(t, byName["img-src"], origin)
		assert.Contains(t, byName["media-src"], origin)
		assert.Contains(t, byName["connect-src"], origin)
	})

	t.Run("他の directive には足さない", func(t *testing.T) {
		// **script-src に足すと外部 origin からのスクリプト実行を許すことになる。**
		// 画像を配るだけの host にその権限を与える理由は無い。
		for _, name := range []string{
			"default-src", "script-src", "style-src",
			"font-src", "worker-src", "frame-src", "object-src",
			"base-uri", "form-action",
		} {
			assert.NotContainsf(t, byName[name], origin, "%s に足してはいけない", name)
		}
	})

	t.Run("origin が無ければ元のまま", func(t *testing.T) {
		assert.Equal(t, buildFrontendCSP(false, cspExtras{}),
			buildFrontendCSP(false, cspExtras{Media: []string{}}))
	})
}

// captcha 業者の origin は**有効な業者の分だけ**足す (#2502)。無いと enforce で
// captcha の script が読めずサインアップが壊れ、常時許可すると使っていない
// 業者の origin にスクリプト実行の権限を与えることになる。
func TestBuildFrontendCSP_CaptchaOrigins(t *testing.T) {
	directives := func(policy string) map[string]string {
		byName := map[string]string{}
		for _, d := range strings.Split(policy, "; ") {
			name, _, _ := strings.Cut(d, " ")
			byName[name] = d
		}
		return byName
	}

	t.Run("turnstile", func(t *testing.T) {
		byName := directives(buildFrontendCSP(false, captchaCSPExtras(false, false, true)))
		assert.Contains(t, byName["script-src"], "https://challenges.cloudflare.com")
		// 公式の要求は script / frame のみ。connect-src には足さない。
		assert.NotContains(t, byName["connect-src"], "cloudflare")
		// 他の業者の origin は載らない。
		assert.NotContains(t, byName["script-src"], "hcaptcha")
		assert.NotContains(t, byName["script-src"], "recaptcha")
	})

	t.Run("hcaptcha", func(t *testing.T) {
		byName := directives(buildFrontendCSP(false, captchaCSPExtras(true, false, false)))
		// 公式ガイダンスは script / style / connect (+frame) に wildcard 込みの
		// 両 origin を要求する。
		assert.Contains(t, byName["script-src"], "https://hcaptcha.com https://*.hcaptcha.com")
		assert.Contains(t, byName["connect-src"], "https://*.hcaptcha.com")
		assert.Contains(t, byName["style-src"], "https://*.hcaptcha.com")
	})

	t.Run("recaptcha", func(t *testing.T) {
		byName := directives(buildFrontendCSP(false, captchaCSPExtras(false, true, false)))
		// api.js が本体を www.gstatic.com/recaptcha/ から読む。
		assert.Contains(t, byName["script-src"], "https://www.recaptcha.net https://www.gstatic.com")
		assert.NotContains(t, byName["connect-src"], "recaptcha")
		assert.NotContains(t, byName["style-src"], "recaptcha")
	})

	t.Run("全て無効なら何も足さない", func(t *testing.T) {
		assert.Equal(t, cspExtras{}, captchaCSPExtras(false, false, false))
	})

	t.Run("captcha origin は script/connect/style 以外に足さない", func(t *testing.T) {
		byName := directives(buildFrontendCSP(false, captchaCSPExtras(true, true, true)))
		for name, d := range byName {
			if name == "script-src" || name == "connect-src" || name == "style-src" {
				continue
			}
			assert.NotContainsf(t, d, "cloudflare", "%s に captcha origin を足さない", name)
			assert.NotContainsf(t, d, "hcaptcha", "%s に captcha origin を足さない", name)
		}
	})
}

// baseUrl から origin だけを取り出す。path を残すと prefix 一致になり、
// 実際の配信 URL がわずかに違う形で外れる。
func TestObjectStorageOrigin(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://objects.example/bucket", "https://objects.example"},
		{"https://objects.example/bucket/prefix/", "https://objects.example"},
		{"https://objects.example", "https://objects.example"},
		{"http://localhost:9000/bucket", "http://localhost:9000"},
		{"  https://objects.example/b  ", "https://objects.example"},
		// 解釈できない値は捨てる。**不正な source expression が 1 つでもあると
		// その directive ごと落とす実装があるので、ゴミを混ぜない。**
		{"", ""},
		{"not a url", ""},
		{"/relative/path", ""},
		{"ftp://objects.example/b", ""},
		{"javascript:alert(1)", ""},
		{"data:text/html,x", ""},
	}
	for _, tc := range cases {
		assert.Equalf(t, tc.want, objectStorageOrigin(tc.in), "objectStorageOrigin(%q)", tc.in)
	}
}

// applyFrontendCSP まで origin が届く。
func TestApplyFrontendCSP_PassesMediaOrigins(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)

	applyFrontendCSP(c, CSPModeReportOnly, cspExtras{Media: []string{"https://objects.example"}})

	v := rec.Header().Get("Content-Security-Policy-Report-Only")
	require.NotEmpty(t, v)
	assert.Contains(t, v, "img-src 'self' data: blob: https://objects.example")
}

// esm.sh の許可は**意図的な妥協**なので、増えていないことを固定する (#2425)。
//
// Misskey frontend は shiki (コードブロックの syntax highlight) を CDN から
// 動的 import する。バンドルに切り替えると 30 ロケール分が複製されてビルド
// 成果物が 242MB → 508MB に倍増するため、CDN を 1 つ許す方を選んだ。
//
// **外部 origin がこれ以上増えるのは別の判断**なので、ここが落ちたら意図した
// 追加かどうかを確認すること。
func TestFrontendCSP_ExternalOriginsAreLimited(t *testing.T) {
	policy := buildFrontendCSP(false, cspExtras{})

	t.Run("script-src で許す外部は esm.sh だけ", func(t *testing.T) {
		var scriptSrc string
		for _, d := range strings.Split(policy, "; ") {
			if strings.HasPrefix(d, "script-src") {
				scriptSrc = d
			}
		}
		require.NotEmpty(t, scriptSrc)

		var external []string
		for _, tok := range strings.Fields(scriptSrc) {
			if strings.HasPrefix(tok, "http://") || strings.HasPrefix(tok, "https://") {
				external = append(external, tok)
			}
		}
		assert.Equal(t, []string{"https://esm.sh"}, external,
			"script-src の外部 origin を増やすときは理由を確認すること")
	})

	t.Run("connect-src で許す外部は esm.sh だけ", func(t *testing.T) {
		// script-src で既に信頼している CDN の source map 取得が report を
		// 汚すため (#2501)。新しい信頼先を増やすものではない。
		var connectSrc string
		for _, d := range strings.Split(policy, "; ") {
			if strings.HasPrefix(d, "connect-src") {
				connectSrc = d
			}
		}
		require.NotEmpty(t, connectSrc)

		var external []string
		for _, tok := range strings.Fields(connectSrc) {
			if strings.HasPrefix(tok, "http://") || strings.HasPrefix(tok, "https://") {
				external = append(external, tok)
			}
		}
		assert.Equal(t, []string{"https://esm.sh"}, external)
	})

	// リモート添付動画 (MkMediaVideo が file.url を直接読む) と埋め込み
	// プレイヤー (MkUrlPreview の player.url iframe) は任意の origin を指す
	// ので、この 2 つだけは https: スキームを許す (#2501)。'self' に絞ると
	// 該当機能が全滅する (実際に本番で全滅していた)。
	t.Run("https: スキームを許すのは media-src と frame-src だけ", func(t *testing.T) {
		for _, d := range strings.Split(policy, "; ") {
			name, _, _ := strings.Cut(d, " ")
			hasScheme := slices.Contains(strings.Fields(d), "https:")
			if name == "media-src" || name == "frame-src" {
				assert.Truef(t, hasScheme, "%s は https: を許す必要がある", name)
			} else {
				assert.Falsef(t, hasScheme, "%s に https: スキームが入っている", d)
			}
		}
	})

	// http: は mixed content でどのみちブロックされるうえ、http 配信の
	// ページでは平文取得を広く許すことになる。どの directive にも入れない。
	t.Run("http: スキームはどこにも入れない", func(t *testing.T) {
		for _, d := range strings.Split(policy, "; ") {
			assert.Falsef(t, slices.Contains(strings.Fields(d), "http:"),
				"%s に http: スキームが入っている", d)
		}
	})

	t.Run("他の directive に外部 origin を入れない", func(t *testing.T) {
		for _, d := range strings.Split(policy, "; ") {
			if strings.HasPrefix(d, "script-src") || strings.HasPrefix(d, "connect-src") {
				continue
			}
			for _, tok := range strings.Fields(d) {
				assert.Falsef(t, strings.HasPrefix(tok, "https://") || strings.HasPrefix(tok, "http://"),
					"%s に外部 origin %s が入っている", d, tok)
			}
		}
	})
}

// **HTML に実際に出た inline script の hash が CSP に載っていること** (#2786)。
//
// これが今回の要。`'unsafe-inline'` を外した以上、HTML 側を変えたのに hash が
// 追従しないと **CSP を有効にしている運用者の画面が真っ白になる**。ここは
// レンダリング結果から script を取り出して突き合わせるので、片方だけ変えれば
// 落ちる。
func TestFrontendCSP_HashesCoverRenderedInlineScripts(t *testing.T) {
	// **loader を inline させる。** built assets が無いと `loader.JS` が空になり、
	// HTML に出る inline script は bootGlobals だけになる。実運用でいちばん大きい
	// inline script (loader) の hash 経路がテストの外に出てしまうので、fixture を
	// 置いて 2 つとも通す (`TestFrontendHTML_InlinesLoaderAssets` と同じ形)。
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "loader"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "loader", "boot.js"),
		[]byte("window.__markerBootJs = 1;"), 0o644))
	t.Setenv("MISSKEY_FRONTEND_DIR", dir)
	frontendutil.ResetLoaderCacheForTest()
	// cache は sync.Once なので、戻さないと fixture の loader がプロセス全体に
	// 残る。同 package に shell を描画するテストが他にもある。
	t.Cleanup(frontendutil.ResetLoaderCacheForTest)

	cfg := &config.Config{
		URL:                           "https://example.test",
		Version:                       "0.0.1-test",
		FrontendContentSecurityPolicy: CSPModeEnforce,
	}
	handler := frontendHTML(cfg, testutil.NewMockMetaRepository(), nil, nil)

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	require.NoError(t, handler(c))

	policy := rec.Header().Get("Content-Security-Policy")
	require.NotEmpty(t, policy, "enforce なので header が出るはず")
	// **directive を切り出して見る。** 部分文字列だと `script-src 'self'
	// https://esm.sh 'unsafe-inline'` のように順を変えられると通る。
	for _, d := range strings.Split(policy, "; ") {
		if strings.HasPrefix(d, "script-src") {
			require.NotContainsf(t, d, "'unsafe-inline'", "script-src に unsafe-inline が戻っている: %s", d)
		}
	}

	// レンダリング結果から属性なしの <script> の中身を取り出す。
	// `type="application/json"` の meta ブロックは属性付きなので拾わない
	// (実行されないので script-src の対象外)。
	bodies := inlineScriptRe.FindAllStringSubmatch(rec.Body.String(), -1)
	// bootGlobals と loader の 2 つ。**件数を固定する** — NotEmpty だと片方が
	// 消えても通り、loader の hash 経路が無検証に戻る。
	require.Len(t, bodies, 2, "inline script は bootGlobals と loader の 2 つ")

	for _, m := range bodies {
		hash := cspScriptHashes(m[1])
		require.Len(t, hash, 1)
		assert.Containsf(t, policy, hash[0],
			"HTML に出た inline script の hash が CSP に無い。script: %.60s...", m[1])
	}
}

// `<script>` に属性が付かないものだけを拾う (`type="application/json"` を除外)。
var inlineScriptRe = regexp.MustCompile(`(?s)<script>(.*?)</script>`)

// cspMediaExtras は SPA shell と embed で共有する (#2789)。両方の CSP header の
// 値がここで決まるので、順序と条件を固定する。
func TestCSPMediaExtras(t *testing.T) {
	const osBase = "https://s3.example.test/bucket"
	osBaseCopy := osBase

	tests := []struct {
		name string
		cfg  *config.Config
		meta *model.Meta
		want []string
	}{
		{
			name: "no object storage and no external proxy adds nothing",
			cfg:  &config.Config{},
			meta: &model.Meta{},
			want: nil,
		},
		{
			// useObjectStorage を切っても baseUrl は meta に残ることがある。
			// 使っていない host を CSP に載せる理由が無い。
			name: "base url without useObjectStorage is ignored",
			cfg:  &config.Config{},
			meta: &model.Meta{UseObjectStorage: false, ObjectStorageBaseURL: &osBaseCopy},
			want: nil,
		},
		{
			name: "object storage origin is added when enabled",
			cfg:  &config.Config{},
			meta: &model.Meta{UseObjectStorage: true, ObjectStorageBaseURL: &osBaseCopy},
			want: []string{"https://s3.example.test"},
		},
		{
			// 有効なのに baseUrl が未設定。nil 参照で落ちてはならない。
			name: "enabled without a base url adds nothing",
			cfg:  &config.Config{},
			meta: &model.Meta{UseObjectStorage: true, ObjectStorageBaseURL: nil},
			want: nil,
		},
		{
			// cfg が無い経路。呼び出し元は必ず渡すが、ガードを外すと panic する。
			name: "nil cfg is tolerated",
			cfg:  nil,
			meta: &model.Meta{UseObjectStorage: true, ObjectStorageBaseURL: &osBaseCopy},
			want: []string{"https://s3.example.test"},
		},
		{
			name: "external media proxy origin is added",
			cfg:  &config.Config{ExternalMediaProxyEnabled: true, MediaProxy: "https://proxy.example.test/proxy"},
			meta: &model.Meta{},
			want: []string{"https://proxy.example.test"},
		},
		{
			// internal proxy ('self') は何も足さない (#2501)。
			name: "internal media proxy adds nothing",
			cfg:  &config.Config{ExternalMediaProxyEnabled: false, MediaProxy: "https://proxy.example.test/proxy"},
			meta: &model.Meta{},
			want: nil,
		},
		{
			// **順序は object storage → media proxy で固定する。** header の
			// 文字列がこの順序で決まるので、入れ替えると値だけ変わって差分が
			// ノイズになる。
			name: "object storage comes before the media proxy",
			cfg:  &config.Config{ExternalMediaProxyEnabled: true, MediaProxy: "https://proxy.example.test/proxy"},
			meta: &model.Meta{UseObjectStorage: true, ObjectStorageBaseURL: &osBaseCopy},
			want: []string{"https://s3.example.test", "https://proxy.example.test"},
		},
		{
			// meta の取得に失敗した経路。cfg だけで決まる分は足す必要がある。
			name: "nil meta still adds the external proxy",
			cfg:  &config.Config{ExternalMediaProxyEnabled: true, MediaProxy: "https://proxy.example.test/proxy"},
			meta: nil,
			want: []string{"https://proxy.example.test"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, cspMediaExtras(tt.cfg, tt.meta))
		})
	}
}
