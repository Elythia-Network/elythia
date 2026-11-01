// Package headreq lets handlers tell a HEAD request apart after the server
// has routed it to a GET route and presents it to them as GET (#3925).
package headreq

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// contextKey marks, on the echo context, a HEAD request routed to a GET route.
const contextKey = "mk.headAsGET"

// Mark records on c that the request is a HEAD request routed to a GET route.
func Mark(c echo.Context) {
	c.Set(contextKey, true)
}

// Rerouted reports whether c carries a HEAD request routed to a GET route.
func Rerouted(c echo.Context) bool {
	v, _ := c.Get(contextKey).(bool)
	return v
}

// Is reports whether the client sent HEAD, including a HEAD request presented
// to the handler as GET.
func Is(c echo.Context) bool {
	return Rerouted(c) || c.Request().Method == http.MethodHead
}

// Request returns the request to hand to http.ServeContent and the like: for
// a HEAD request presented as GET, a copy whose Method is HEAD again, so that
// they skip copying the body.
func Request(c echo.Context) *http.Request {
	r := c.Request()
	if !Rerouted(c) || r.Method == http.MethodHead {
		return r
	}
	asHEAD := r.WithContext(r.Context())
	asHEAD.Method = http.MethodHead
	return asHEAD
}
