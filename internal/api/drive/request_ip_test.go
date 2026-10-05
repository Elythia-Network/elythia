package drive

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **プロセス内 API 呼び出しの IP を記録しないこと。**
//
// `pluginCaller.Call` は `RemoteAddr` に `127.0.0.1:0` を置く (IP を見る
// middleware が解釈に困らないようにするため) が、**その IP は実在しない**。
// 記録すると、モデレーターが `admin/drive/show-file` で見る値が実際の取得元と
// 無関係な `127.0.0.1` になる。`user_ip` とレート制限は既に除外しているので、
// drive だけ取り残されていた。
func TestRequestIPFromContext_SkipsInternalCalls(t *testing.T) {
	e := echo.New()

	newCtx := func(internal bool) echo.Context {
		req := httptest.NewRequest(http.MethodPost, "/api/drive/files/create", nil)
		req.RemoteAddr = "203.0.113.9:1234"
		if internal {
			req = middleware.MarkInternalCall(req)
			req.RemoteAddr = "127.0.0.1:0"
		}
		return e.NewContext(req, httptest.NewRecorder())
	}

	got := requestIPFromContext(newCtx(false))
	require.NotNil(t, got, "外から来たリクエストの IP は記録すること")
	assert.Equal(t, "203.0.113.9", *got)
	assert.Equal(t, "203.0.113.9", requestIPValue(newCtx(false)))

	assert.Nil(t, requestIPFromContext(newCtx(true)),
		"プロセス内呼び出しの IP を記録している (実在しないアドレスが moderation 画面に出る)")
	assert.Empty(t, requestIPValue(newCtx(true)))
}

// **`upload-from-url` の経路も同じであること。**
//
// あちらは `RequestIP` を `string` で持つので `requestIPFromContext` を通らず、
// 取り残されやすい (実際に `c.RealIP()` を直に呼んでいた)。
func TestFilesUploadFromURL_SkipsInternalCallIP(t *testing.T) {
	for _, internal := range []bool{false, true} {
		name := "external"
		if internal {
			name = "internal"
		}
		t.Run(name, func(t *testing.T) {
			h, _, _ := newHandler(t)
			rp := &recordingProcessor{calls: make(chan URLUploadInput, 1)}
			h.SetURLUploader(rp)

			c, rec := newJSONReq(t, `{"url":"https://example.com/x"}`)
			req := c.Request()
			req.RemoteAddr = "203.0.113.9:1234"
			if internal {
				req = middleware.MarkInternalCall(req)
				req.RemoteAddr = "127.0.0.1:0"
			}
			c.SetRequest(req)
			setUser(c, "u1")
			require.NoError(t, h.FilesUploadFromURL(c))
			require.Equal(t, http.StatusNoContent, rec.Code)

			select {
			case in := <-rp.calls:
				if internal {
					assert.Empty(t, in.RequestIP,
						"プロセス内呼び出しの IP を記録している")
				} else {
					assert.Equal(t, "203.0.113.9", in.RequestIP)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Process was not invoked")
			}
		})
	}
}
