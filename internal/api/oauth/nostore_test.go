package oauth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// **資格情報を載せる応答に no-store を付けること (RFC 6749 §5.1 は MUST)。**
//
// `/oauth/*` は api グループの外なので、mk-go 既定の Cache-Control も掛からない。
func TestApplyOAuthNoStore(t *testing.T) {
	t.Parallel()

	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, "/oauth/token", nil), httptest.NewRecorder())
	applyOAuthNoStore(c)

	require.Equal(t, "no-store", c.Response().Header().Get("Cache-Control"))
	require.Equal(t, "no-cache", c.Response().Header().Get("Pragma"))
}

// token endpoint のエラー応答にも付くこと。
func TestTokenError_SetsNoStore(t *testing.T) {
	t.Parallel()

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, "/oauth/token", nil), rec)
	require.NoError(t, tokenError(c, http.StatusBadRequest, "invalid_request", "bad"))
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}
