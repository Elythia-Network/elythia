package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// identiconMetaStub は enableIdenticonGeneration だけを制御する最小の stub。
type identiconMetaStub struct {
	repository.MetaRepository
	enabled bool
}

func (s identiconMetaStub) Fetch() (*model.Meta, error) {
	return &model.Meta{EnableIdenticonGeneration: s.enabled}, nil
}

func serveIdenticon(t *testing.T, metaRepo repository.MetaRepository) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	e.GET("/identicon/:x", identiconHandler(metaRepo))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/identicon/u1", nil))
	return rec
}

// identicon は mk-go が実際に PNG バイトを返す route。生成経路と、無効時の
// 透明ピクセル経路の**両方**に CSP を付ける (#2404)。片方だけだと、meta フラグを
// 切り替えただけで header が変わることになる。
//
// upstream の identicon には CSP が無い。他のアセット route と揃えた、upstream より
// 厳しい側の意図的な divergence。
func TestIdenticonHandler_SetsAssetCSP(t *testing.T) {
	cases := []struct {
		name     string
		metaRepo repository.MetaRepository
	}{
		{"生成経路", identiconMetaStub{enabled: true}},
		{"無効時の透明ピクセル経路", identiconMetaStub{enabled: false}},
		{"metaRepo なし (常に生成)", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveIdenticon(t, tc.metaRepo)

			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
			assert.Equal(t, "default-src 'none'; style-src 'unsafe-inline'",
				rec.Header().Get("Content-Security-Policy"))
			assert.NotEmpty(t, rec.Body.Bytes(), "PNG バイトが返る")
		})
	}
}
