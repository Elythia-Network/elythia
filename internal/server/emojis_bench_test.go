package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apiemojis "github.com/elythia-network/elythia/internal/api/emojis"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// newEmojisBenchEcho wires /api/emojis behind the production gzip middleware
// with n local emojis held by the same cached repository production uses.
func newEmojisBenchEcho(tb testing.TB, n int) *echo.Echo {
	tb.Helper()
	mock := testutil.NewMockEmojiRepository()
	for i := range n {
		cat := "cat"
		e := &model.Emoji{
			ID:          fmt.Sprintf("emoji%04d", i),
			Name:        fmt.Sprintf("emoji_%04d", i),
			Category:    &cat,
			Aliases:     []string{"alias", fmt.Sprintf("a%d", i)},
			PublicURL:   fmt.Sprintf("https://files.example/emoji/%04d.webp", i),
			OriginalURL: fmt.Sprintf("https://files.example/emoji/%04d.png", i),
		}
		if err := mock.Create(e); err != nil {
			tb.Fatal(err)
		}
	}
	h := apiemojis.NewHandler(repository.NewCachedEmojiRepository(mock))
	e := echo.New()
	e.Use(echomw.GzipWithConfig(gzipConfig()))
	e.GET("/api/emojis", h.Emojis)
	return e
}

// BenchmarkEmojisGzip measures one GET /api/emojis (200 emojis, gzip accepted)
// once the emoji list itself is cached.
func BenchmarkEmojisGzip(b *testing.B) {
	e := newEmojisBenchEcho(b, 200)
	b.ReportAllocs()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/api/emojis", nil)
		req.Header.Set(echo.HeaderAcceptEncoding, "gzip, deflate, br")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status %d", rec.Code)
		}
	}
}

// /api/emojis は handler が gzip 済みの本文を返すので、全体の gzip の
// middleware が二重に包まないこと。
func TestEmojisIsCompressedOnceBehindGlobalGzip(t *testing.T) {
	e := newEmojisBenchEcho(t, 200)
	req := httptest.NewRequest(http.MethodGet, "/api/emojis", nil)
	req.Header.Set(echo.HeaderAcceptEncoding, "gzip, deflate, br")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "gzip", rec.Header().Get(echo.HeaderContentEncoding))
	assert.Equal(t, []string{echo.HeaderAcceptEncoding}, rec.Header().Values(echo.HeaderVary))
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	require.NoError(t, err)
	plain, err := io.ReadAll(zr)
	require.NoError(t, err)
	var body struct {
		Emojis []map[string]any `json:"emojis"`
	}
	require.NoError(t, json.Unmarshal(plain, &body), "1 回 gunzip すれば JSON になる")
	assert.Len(t, body.Emojis, 200)
}
