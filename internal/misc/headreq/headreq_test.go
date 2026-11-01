package headreq

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
)

func newContext(method string) echo.Context {
	return echo.New().NewContext(httptest.NewRequest(method, "/", nil), httptest.NewRecorder())
}

func TestUnmarkedRequests(t *testing.T) {
	get := newContext(http.MethodGet)
	assert.False(t, Rerouted(get))
	assert.False(t, Is(get))
	assert.Same(t, get.Request(), Request(get), "a GET request is handed over as is")

	head := newContext(http.MethodHead)
	assert.False(t, Rerouted(head))
	assert.True(t, Is(head), "a HEAD request routed to a HEAD route is HEAD")
	assert.Same(t, head.Request(), Request(head))
}

func TestMarkedRequestPresentedAsGET(t *testing.T) {
	c := newContext(http.MethodGet)
	Mark(c)
	assert.True(t, Rerouted(c))
	assert.True(t, Is(c))
	got := Request(c)
	assert.Equal(t, http.MethodHead, got.Method)
	assert.NotSame(t, c.Request(), got)
	assert.Equal(t, http.MethodGet, c.Request().Method, "the request the handler sees is left alone")
}

// A marked request whose Method is already HEAD (seen outside presentAsGET)
// is handed over as is.
func TestMarkedRequestStillHEAD(t *testing.T) {
	c := newContext(http.MethodHead)
	Mark(c)
	assert.Same(t, c.Request(), Request(c))
}
