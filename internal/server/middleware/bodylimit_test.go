package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
)

// #2075: BodyLimitByPath は path 別に body size を cap する (/api 1MiB / inbox 64KiB /
// upload は maxFileSize ベース / その他 1MiB)。超過は 413。
func TestBodyLimitByPath(t *testing.T) {
	// テスト内で扱いやすいよう小さめの maxFileSize を渡す。実際の既定は 250MB。
	const testMaxFileSize int64 = 4 * 1024 * 1024
	e := echo.New()
	e.Use(BodyLimitByPath(testMaxFileSize, nil))
	// body を読む handler。chunked 超過時に limitedReader の 413 を伝播する。
	h := func(c *echo.Context) error {
		if _, err := io.ReadAll(c.Request().Body); err != nil {
			if he := apierr.HTTPStatusError(err); he != nil {
				return he
			}
			return c.NoContent(http.StatusBadRequest)
		}
		return c.NoContent(http.StatusOK)
	}
	e.POST("/api/meta", h)
	e.POST("/api/drive/files/create", h)
	e.POST("/api/drive/files/create-chunked/append", h)
	e.POST("/api/drive/files/create-chunked/start", h)
	e.POST("/inbox", h)
	e.POST("/users/:id/inbox", h)
	e.POST("/nolimit", h)

	post := func(path, ct string, n int, chunked bool) int {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(bytes.Repeat([]byte("a"), n)))
		if ct != "" {
			req.Header.Set(echo.HeaderContentType, ct)
		}
		if chunked {
			req.ContentLength = -1 // limitedReader 経路 (read 中の 413) を通す
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}

	const mb = 1024 * 1024
	const kb = 1024

	// /api → 1MiB (= 1048576)。境界: ちょうどは許容、+1 は 413。
	assert.Equal(t, http.StatusRequestEntityTooLarge, post("/api/meta", "application/json", mb+1, false), "/api > 1MiB は 413")
	assert.Equal(t, http.StatusOK, post("/api/meta", "application/json", mb, false), "/api ちょうど 1MiB は許容")
	assert.Equal(t, http.StatusOK, post("/api/meta", "application/json", 500*kb, false))
	// /api chunked 超過 → handler の read で 413 伝播。
	assert.Equal(t, http.StatusRequestEntityTooLarge, post("/api/meta", "application/json", mb+1, true), "/api chunked 超過も 413")

	// upload endpoint (drive/files/create) は 1MiB ではなく maxFileSize ベース。
	// **無制限ではない。** 無制限にすると、route の RequireAuth より前に走る
	// global auth が multipart を parse し、未認証のままディスクを食い潰せる。
	const uploadPath = "/api/drive/files/create"
	assert.Equal(t, http.StatusOK, post(uploadPath, "multipart/form-data; boundary=x", 2*mb, false), "upload は 1MiB 制限から外れる")
	assert.Equal(t, http.StatusOK, post(uploadPath, "multipart/form-data; boundary=x", int(testMaxFileSize)+mb, false), "upload は maxFileSize + 余白ちょうどまで許容")
	assert.Equal(t, http.StatusRequestEntityTooLarge, post(uploadPath, "multipart/form-data; boundary=x", int(testMaxFileSize)+mb+1, false), "upload は maxFileSize + 余白を超えたら 413")
	assert.Equal(t, http.StatusRequestEntityTooLarge, post(uploadPath, "multipart/form-data; boundary=x", int(testMaxFileSize)+mb+1, true), "upload chunked 超過も 413")
	// HIGH-1 regression: 非 upload endpoint への multipart は 1MiB が効く (DoS bypass 防止)。
	assert.Equal(t, http.StatusRequestEntityTooLarge, post("/api/meta", "multipart/form-data; boundary=x", 2*mb, false), "非 upload endpoint の multipart は 1MiB cap")
	assert.Equal(t, http.StatusOK, post("/api/meta", "multipart/form-data; boundary=x", 500*kb, false), "非 upload endpoint の小 multipart は通過")

	// inbox → 64KiB (= 65536)。
	assert.Equal(t, http.StatusRequestEntityTooLarge, post("/inbox", "application/activity+json", 64*kb+1, false), "inbox > 64KiB は 413")
	assert.Equal(t, http.StatusOK, post("/inbox", "application/activity+json", 64*kb, false), "inbox ちょうど 64KiB は許容")
	assert.Equal(t, http.StatusRequestEntityTooLarge, post("/users/u1/inbox", "application/activity+json", 64*kb+1, false), "/users/:id/inbox も 64KiB")

	// #2313: chunked upload の append は 1MiB 制限から外すが、create のように
	// 無制限にはしない。1 リクエスト = 1 チャンクなので固定上限 (33MiB) を掛ける。
	const appendPath = "/api/drive/files/create-chunked/append"
	assert.Equal(t, http.StatusOK, post(appendPath, "multipart/form-data; boundary=x", 2*mb, false), "append は 1MiB 制限から外れる")
	assert.Equal(t, http.StatusOK, post(appendPath, "multipart/form-data; boundary=x", 33*mb, false), "append ちょうど 33MiB は許容")
	assert.Equal(t, http.StatusRequestEntityTooLarge, post(appendPath, "multipart/form-data; boundary=x", 33*mb+1, false), "append > 33MiB は 413")
	assert.Equal(t, http.StatusRequestEntityTooLarge, post(appendPath, "multipart/form-data; boundary=x", 33*mb+1, true), "append chunked 超過も 413")
	// start / finish / abort は JSON なので通常の 1MiB のまま。除外を広げすぎて
	// いないことを固定する。
	assert.Equal(t, http.StatusRequestEntityTooLarge, post("/api/drive/files/create-chunked/start", "application/json", mb+1, false), "start は 1MiB のまま")

	// **その他のパスも 1MiB で切る。** 無制限にすると、global な
	// auth.Authenticate が token 抽出で body を丸ごと読むため、未登録の
	// パス (404/405) にさえ巨大な body を送るだけでメモリを食える。
	assert.Equal(t, http.StatusOK, post("/nolimit", "application/json", 1*mb-1024, false), "1MiB 未満は通る")
	assert.Equal(t, http.StatusRequestEntityTooLarge, post("/nolimit", "application/json", 2*mb, false), "対象外 path も 1MiB で 413")
}

// upload の上限は「設定が無い / 壊れている」場合でも必ず掛かる。ここが
// 無制限へ倒れると、修正したはずの未認証 DoS がそのまま戻る。
func TestUploadBodyLimit(t *testing.T) {
	const margin = int64(1) << 20
	tests := []struct {
		name        string
		maxFileSize int64
		want        int64
	}{
		{"configured size gets the framing margin", 10 << 20, (10 << 20) + margin},
		{"zero falls back to the default instead of unlimited", 0, defaultUploadBodyLimit + margin},
		{"negative falls back to the default instead of unlimited", -1, defaultUploadBodyLimit + margin},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := uploadBodyLimit(tt.maxFileSize); got != tt.want {
				t.Errorf("uploadBodyLimit(%d) = %d, want %d", tt.maxFileSize, got, tt.want)
			}
		})
	}
}

// 未設定 (0) を渡しても upload route が無制限にならないことを、middleware を
// 通した実挙動で固定する。uploadBodyLimit の単体テストだけだと、呼び出し側で
// 使い忘れても気付けない。
func TestBodyLimitByPathUploadIsNeverUnlimited(t *testing.T) {
	e := echo.New()
	e.Use(BodyLimitByPath(0, nil))
	e.POST("/api/drive/files/create", func(c *echo.Context) error {
		if _, err := io.ReadAll(c.Request().Body); err != nil {
			if he := apierr.HTTPStatusError(err); he != nil {
				return he
			}
			return c.NoContent(http.StatusBadRequest)
		}
		return c.NoContent(http.StatusOK)
	})

	over := defaultUploadBodyLimit + (1 << 20) + 1
	req := httptest.NewRequest(http.MethodPost, "/api/drive/files/create", bytes.NewReader(nil))
	req.Header.Set(echo.HeaderContentType, "multipart/form-data; boundary=x")
	// 実際に数百 MB を積むとテストが重いので ContentLength だけを詐称する。
	// echo BodyLimit は ContentLength を見て即座に 413 を返す経路を持つ。
	req.ContentLength = over
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code,
		"maxFileSize 未設定でも upload route は無制限にならない")
}

// プラグイン peer の受け口はプラグインごとの上限で cap される。**/api の 1MiB に
// 吸われない**ことがこのテストの主眼で、吸われると宣言が無意味になる。
func TestBodyLimitByPath_PluginPeer(t *testing.T) {
	const small int64 = 4 * 1024
	e := echo.New()
	e.Use(BodyLimitByPath(0, map[string]int64{
		"/api/plugin/small/_peer": small,
	}))
	h := func(c *echo.Context) error {
		if _, err := io.ReadAll(c.Request().Body); err != nil {
			if he := apierr.HTTPStatusError(err); he != nil {
				return he
			}
			return c.NoContent(http.StatusBadRequest)
		}
		return c.NoContent(http.StatusOK)
	}
	e.POST("/api/plugin/small/_peer", h)
	e.POST("/api/plugin/other/_peer", h)

	post := func(path string, n int, chunked bool) int {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(bytes.Repeat([]byte("a"), n)))
		req.Header.Set(echo.HeaderContentType, "application/json")
		if chunked {
			req.ContentLength = -1
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}

	assert.Equal(t, http.StatusOK, post("/api/plugin/small/_peer", int(small), false), "上限ちょうどは許容")
	assert.Equal(t, http.StatusRequestEntityTooLarge, post("/api/plugin/small/_peer", int(small)+1, false), "+1 は 413")
	// ContentLength を伏せても limitedReader 側で落ちる (偽装で抜けられない)。
	assert.Equal(t, http.StatusRequestEntityTooLarge, post("/api/plugin/small/_peer", int(small)+1, true), "chunked でも 413")
	// 表に無いプラグインは /api の 1MiB のまま。
	assert.Equal(t, http.StatusOK, post("/api/plugin/other/_peer", int(small)+1, false), "表に無いパスは /api の上限")
}

// **未登録のパスでも上限が効くこと。**
//
// これが H-1 の本体。echo は未マッチのパスでも global middleware を通すので、
// ルートが存在しなくても body を読む middleware (auth.Authenticate) が動く。
// 404/405 を返す前にヒープへ載るため、「存在しないパスだから安全」は成立しない。
func TestBodyLimitByPath_AppliesToUnregisteredPaths(t *testing.T) {
	t.Parallel()

	const maxFileSize int64 = 4 * 1024 * 1024

	e := echo.New()
	e.Use(BodyLimitByPath(maxFileSize, nil))
	// **global middleware が body を読む**状況を再現する。本番では
	// auth.Authenticate の extractToken がこれを行う。
	var read int64
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			n, _ := io.Copy(io.Discard, c.Request().Body)
			atomic.AddInt64(&read, n)
			return next(c)
		}
	})
	e.POST("/api/meta", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })

	post := func(path string, n int) int {
		req := httptest.NewRequest(http.MethodPost, path,
			bytes.NewReader(bytes.Repeat([]byte("a"), n)))
		req.Header.Set(echo.HeaderContentType, "application/json")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}

	const mb = 1024 * 1024

	for _, path := range []string{
		"/",                       // ルート (405 になるパス)
		"/definitely-not-a-route", // 未登録 (404)
		"/oauth/token",            // api グループの外の実在ルート相当
		"/proxy/x.png",
	} {
		atomic.StoreInt64(&read, 0)
		code := post(path, 4*mb)
		assert.Equal(t, http.StatusRequestEntityTooLarge, code,
			"%s: 4MiB は 413 で止まるべき", path)
		assert.LessOrEqual(t, atomic.LoadInt64(&read), int64(mb+1),
			"%s: 上限を超える量を読んではいけない (読んだ %d バイト)", path, atomic.LoadInt64(&read))
	}

	// 1MiB 未満は従来どおり通る (404/405 は返るが、body 上限では止まらない)。
	assert.NotEqual(t, http.StatusRequestEntityTooLarge, post("/definitely-not-a-route", 512*1024))
}
