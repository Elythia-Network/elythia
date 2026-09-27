package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/config"
)

// **本番でビルド成果物が欠けていても dev server へは流さない。**
//
// 以前は「ビルド成果物が無ければ proxy」だったので、bind-mount の付け忘れや
// ビルド途中の窓で `/vite/*` が認証なしに `localhost:5173` へ reverse proxy
// されていた。dev server の代わりに httptest のサーバーを立て、到達したかを
// 数えて確かめる。
func TestRegisterFrontendAssets(t *testing.T) {
	const spaShellBody = "<!doctype html>spa-shell"

	var hits atomic.Int32
	devServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("from-dev-server"))
	}))
	defer devServer.Close()

	built := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(built, "entry.js"), []byte("from-build"), 0o644))
	missing := filepath.Join(t.TempDir(), "absent")

	tests := []struct {
		name     string
		cfg      *config.Config
		prefix   string
		dir      string
		wantCode int
		wantBody string
		wantHit  bool
	}{
		{name: "production without build answers 404", cfg: &config.Config{}, prefix: "/vite", dir: missing, wantCode: http.StatusNotFound},
		{name: "nil config without build answers 404", cfg: nil, prefix: "/vite", dir: missing, wantCode: http.StatusNotFound},
		{name: "embed without build answers 404", cfg: &config.Config{}, prefix: "/embed_vite", dir: missing, wantCode: http.StatusNotFound},
		{name: "production serves built assets", cfg: &config.Config{}, prefix: "/vite", dir: built, wantCode: http.StatusOK, wantBody: "from-build"},
		{name: "dev proxies without build", cfg: &config.Config{Dev: true}, prefix: "/vite", dir: missing, wantCode: http.StatusOK, wantBody: "from-dev-server", wantHit: true},
		{name: "dev proxies even with build", cfg: &config.Config{Dev: true}, prefix: "/embed_vite", dir: built, wantCode: http.StatusOK, wantBody: "from-dev-server", wantHit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits.Store(0)
			e := echo.New()
			registerFrontendAssets(e, tt.cfg, tt.prefix, tt.dir, devServer.URL)
			// 本番と同じく SPA の catchall を後から登録する (router.go の
			// `s.echo.GET("/*", frontend)`)。これが無いと echo の既定の 404 が
			// 返るので、ビルド成果物が無いときの 404 の明示登録を消しても
			// 「404 が返る」ままテストが通ってしまう。
			e.GET("/*", func(c echo.Context) error {
				return c.HTML(http.StatusOK, spaShellBody)
			})

			for _, method := range []string{http.MethodGet, http.MethodPost} {
				req := httptest.NewRequest(method, tt.prefix+"/entry.js", nil)
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)

				if method == http.MethodPost && !tt.wantHit {
					// Static は GET / HEAD しか持たないので、POST の応答は
					// 405 / 404 のどちらでもよい。見るのは dev server へ
					// 届かないことだけ。
					continue
				}
				assert.Equal(t, tt.wantCode, rec.Code, method)
				assert.NotEqual(t, spaShellBody, rec.Body.String(), "%s: SPA の catchall に落とさない", method)
				if tt.wantBody != "" {
					assert.Equal(t, tt.wantBody, rec.Body.String(), method)
				}
			}
			if tt.wantHit {
				assert.NotZero(t, hits.Load(), "dev モードでは dev server へ流す")
			} else {
				assert.Zero(t, hits.Load(), "dev モードでなければ dev server へ流さない")
			}
		})
	}
}

// TestViteProxyIsOnlyBuiltByRegisterFrontendAssets keeps the dev-server proxy
// behind the dev-mode check.
//
// **router へ直接 `newViteProxy` を書き戻す形を落とす。** 振る舞いのテストは
// `registerFrontendAssets` しか見ないので、router が「無ければ proxy」を
// 自前で書き直しても緑のまま通る。`internal/server` は CI のカバレッジ対象外で
// router を組み立てるテストも無い (#2762 の wiring-check と同じ理由)。
func TestViteProxyIsOnlyBuiltByRegisterFrontendAssets(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	fset := token.NewFileSet()
	var callers []string
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				// 呼び出しに限らず、値として持ち出す形 (`h := newViteProxy`) も数える。
				if id, ok := n.(*ast.Ident); ok && id.Name == "newViteProxy" && id.Pos() != fn.Name.Pos() {
					callers = append(callers, name+"#"+fn.Name.Name)
				}
				return true
			})
		}
	}
	assert.Equal(t, []string{"devmode.go#registerFrontendAssets"}, callers,
		"newViteProxy は registerFrontendAssets (dev モードの判定の内側) からだけ使うこと")
}
