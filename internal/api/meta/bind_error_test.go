package meta

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/testutil"
)

// TestMeta_BindErrorIsInvalidParam pins that a body meta cannot bind (a JSON
// array, not an object) is answered with 400 INVALID_PARAM, as upstream's
// ajv `type: 'object'` check does. The bind error used to be ignored and the
// full meta returned (#3330).
func TestMeta_BindErrorIsInvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", strings.NewReader(`[]`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Meta(e.NewContext(req, rec)))
	testutil.AssertInvalidParam(t, rec)
}
