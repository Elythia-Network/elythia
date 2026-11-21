package middleware

import (
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
)

func TestFrameGuard(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"frontend shell is not framable", "/", "DENY"},
		{"user page is not framable", "/@alice", "DENY"},
		{
			// 認可プロンプトを透明な iframe で重ねる攻撃を塞ぐ。ここが抜けると
			// クリックジャッキングの本命が空く。
			name: "oauth consent screen is not framable",
			path: "/oauth/authorize",
			want: "DENY",
		},
		{"api responses carry the header too", "/api/meta", "DENY"},
		{
			// iframe に埋め込まれること自体が目的の経路。upstream も embed
			// route では header を外している。
			name: "embed pages stay framable",
			path: "/embed/notes/abc",
			want: "",
		},
		{"drive files stay framable", "/files/abc.pdf", ""},
		{"media proxy stays framable", "/proxy/image.webp", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			e.Use(FrameGuard())
			// **本番と同じルートを張る。** 判定はマッチしたパターンで行うので、
			// catchall しか無いと除外対象まで SPA 扱いになる (実際の構成では
			// `/embed/*` / `/files/:accessKey` / `/proxy/*` が個別に張られる)。
			ok := func(c *echo.Context) error { return c.NoContent(http.StatusOK) }
			e.Any("/embed/*", ok)
			e.Any("/files/:accessKey", ok)
			e.Any("/proxy/*", ok)
			e.Any("/*", ok)
			e.Any("/", ok)

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			assert.Equal(t, tt.want, rec.Header().Get("X-Frame-Options"),
				"path=%s", tt.path)
		})
	}
}

// 除外は prefix 一致なので、似た名前の別 path を巻き込まないことを固定する。
// `/embedded-thing` が除外されると、そこだけ守られない状態が静かに生まれる。
func TestFrameGuardSkipIsPrefixScoped(t *testing.T) {
	for _, p := range []string{"/embedded-thing", "/filestore", "/proxying"} {
		if frameGuardSkipped(p) {
			t.Errorf("frameGuardSkipped(%q) = true, want false", p)
		}
	}
}

// **ルートパターンで判定すること。**
//
// 生のリクエストパスで前方一致を取ると、`/files/` (キー無し) のように除外の
// 接頭辞に当たるが実際には SPA へ落ちるパスでヘッダが外れる。
// `/files/:accessKey` は空セグメントにマッチせず catchall に落ちるので、
// SPA シェルが `X-Frame-Options` 無しで返っていた。
func TestFrameGuard_UsesRoutePatternNotRawPath(t *testing.T) {
	t.Parallel()

	e := echo.New()
	e.Use(FrameGuard())
	h := func(c *echo.Context) error { return c.String(http.StatusOK, "ok") }
	e.GET("/files/:accessKey", h)
	e.GET("/embed/*", h)
	e.GET("/*", h)

	get := func(path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Header().Get("X-Frame-Options")
	}

	require.Equal(t, "", get("/files/abc"), "ファイル配信は従来どおり除外")
	require.Equal(t, "", get("/embed/notes/1"), "埋め込みは従来どおり除外")
	require.Equal(t, "DENY", get("/"), "SPA には付けること")
	require.Equal(t, "DENY", get("/files/"),
		"**キー無しの /files/ は SPA へ落ちるので付けること** (生パスの前方一致だと外れる)")
}
