package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **Vite dev server へのプロキシが落としやすい 3 点を固定する。**
//
// `httputil.ReverseProxy` を `Director` から `Rewrite` へ移したときに、どれも
// 黙って壊れる — (a) `SetURL` を忘れると宛先が解決されない、(b) `SetURL` は
// `Out.Host` を**空にして** `Out.URL.Host` を Host ヘッダにするので、そこを
// `r.In.Host` で上書きすると dev server が別 origin として扱う、(c)
// **`Rewrite` は X-Forwarded-* を落としてから呼ばれる**ので、`Director` 版で
// `ServeHTTP` が自動付与していた `X-Forwarded-For` が消える。
//
// いずれもコンパイルは通るうえ、開発時にしか通らない経路なので気付きにくい。
//
// 変異検証は 5 形 — 4 形が検出、1 形は意図どおり非検出:
//   - `r.SetURL(remote)` を `_ = remote` へ置換 → 502 (素直に消すと build が
//     落ちるだけなので「検出」にならない)
//   - `r.SetXForwarded()` を削除 → X-Forwarded-For が空
//   - `SetURL` の後に `r.Out.Host = r.In.Host` → Host が別 origin
//   - `SetURL` の後に `r.Out.URL.Path = "/"` → path が届かない
//   - `SetURL` の後に `r.Out.Host = remote.Host` → **非検出**。`SetURL` が既に
//     同じ結果にしているので冗長、という判断の裏取りにあたる
func TestNewViteProxy_ForwardsWithRemoteHost(t *testing.T) {
	var gotHost, gotPath, gotXFF string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotPath = r.URL.Path
		gotXFF = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	remote, err := url.Parse(backend.URL)
	require.NoError(t, err)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/vite/main.ts", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	rec := httptest.NewRecorder()

	require.NoError(t, newViteProxy(backend.URL)(e.NewContext(req, rec)))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Body.String(), "dev server の応答をそのまま返すこと")
	assert.Equal(t, remote.Host, gotHost, "dev server には remote の Host を送ること")
	assert.Equal(t, "/vite/main.ts", gotPath, "パスはそのまま転送すること")
	assert.Equal(t, "203.0.113.9", gotXFF, "X-Forwarded-For を落とさないこと")
}

// **inbound の `X-Forwarded-For` は引き継がない。**
//
// `Director` 版 (`NewSingleHostReverseProxy`) は `ServeHTTP` が client 由来の値へ
// **追記**していたが、`SetXForwarded()` は自分が観測した `RemoteAddr` で置き換える
// (stdlib の doc が「追記したければ呼ぶ前に inbound からコピーしろ」と明示している)。
// **等価な移行ではない**が、詐称された chain を dev server へ流さない方向なので
// こちらを採る。将来 inbound をコピーする実装に戻したらこのテストが落ちる。
func TestNewViteProxy_DropsInboundForwardedChain(t *testing.T) {
	var gotXFF string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/vite/main.ts", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	rec := httptest.NewRecorder()

	require.NoError(t, newViteProxy(backend.URL)(e.NewContext(req, rec)))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "203.0.113.9", gotXFF,
		"client が送ってきた 198.51.100.7 を引き継がず、観測した RemoteAddr だけを送ること")
}
