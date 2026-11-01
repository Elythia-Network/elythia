package meta

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GET and POST clear the browser caches with 204 + Clear-Site-Data; any other
// method is 405 without the header, like upstream (#3926).
func TestClearBrowserCache(t *testing.T) {
	for _, tc := range []struct {
		method string
		status int
	}{
		{http.MethodGet, http.StatusNoContent},
		{http.MethodPost, http.StatusNoContent},
		{http.MethodHead, http.StatusMethodNotAllowed},
		{http.MethodPut, http.StatusMethodNotAllowed},
		{http.MethodDelete, http.StatusMethodNotAllowed},
		{http.MethodPatch, http.StatusMethodNotAllowed},
	} {
		t.Run(tc.method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(tc.method, "/api/clear-browser-cache", nil), rec)
			require.NoError(t, ClearBrowserCache(c))
			assert.Equal(t, tc.status, rec.Code)
			assert.Empty(t, rec.Body.String())
			if tc.status == http.StatusNoContent {
				assert.Equal(t, `"cache", "prefetchCache", "prerenderCache", "executionContexts"`, rec.Header().Get("Clear-Site-Data"))
			} else {
				assert.Empty(t, rec.Header().Get("Clear-Site-Data"))
			}
		})
	}
}
