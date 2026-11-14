package paramshadow

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/paramschema"
)

const testParamDef = `{"type":"object","properties":{
	"noteId":{"type":"string","format":"misskey:id"},
	"limit":{"type":"integer","minimum":1,"maximum":100,"default":10},
	"flag":{"type":"boolean"}},"required":["noteId"]}`

func testEndpoints(t *testing.T) map[string]paramschema.Endpoint {
	t.Helper()
	s := mustCompile(t, testParamDef)
	return map[string]paramschema.Endpoint{
		"t/post":   {Schema: s},
		"t/get":    {Schema: s, AllowGet: true},
		"t/file":   {Schema: s, RequireFile: true},
		"t/broken": {CompileErr: errors.New("broken")},
	}
}

func ok(c echo.Context) error { return c.JSON(http.StatusOK, map[string]any{"ok": true}) }

// TestMiddlewarePassesTheRoutesWithoutAParamDef checks that a request to a
// route outside /api or without a paramDef in eps reaches the handler with
// its body, however invalid the body would be.
func TestMiddlewarePassesTheRoutesWithoutAParamDef(t *testing.T) {
	e := echo.New()
	var read string
	h := func(c echo.Context) error {
		b, _ := io.ReadAll(c.Request().Body)
		read = string(b)
		return ok(c)
	}
	mw := Middleware(testEndpoints(t))
	for _, p := range []string{"/api/t/other", "/other/t/post"} {
		e.POST(p, h, mw)
	}
	for _, target := range []string{"/api/t/other", "/other/t/post"} {
		read = ""
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{"noteId":"a-b"}`))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || read != `{"noteId":"a-b"}` {
			t.Errorf("%s: status %d, the handler read %q", target, rec.Code, read)
		}
	}
}

func TestMiddlewareReadErrorReachesTheHandler(t *testing.T) {
	boom := errors.New("boom")
	e := echo.New()
	var handlerErr error
	e.POST("/api/t/post", func(c echo.Context) error {
		_, handlerErr = io.ReadAll(c.Request().Body)
		return ok(c)
	}, Middleware(testEndpoints(t)))
	req := httptest.NewRequest(http.MethodPost, "/api/t/post", nil)
	req.Body = io.NopCloser(io.MultiReader(strings.NewReader(`{"a"`), errReader{boom}))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if !errors.Is(handlerErr, boom) {
		t.Errorf("handler read err = %v", handlerErr)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status %d, want the handler's 200", rec.Code)
	}
}
