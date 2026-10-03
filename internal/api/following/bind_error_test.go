package following

import (
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/shiroha-a/mk/internal/testutil"
)

// TestHandlers_BindErrorIsInvalidParam pins that a body these endpoints
// cannot bind (a JSON array, not an object) is answered with 400
// INVALID_PARAM, as upstream's ajv `type: 'object'` check does. The bind
// error used to be ignored (#3330).
func TestHandlers_BindErrorIsInvalidParam(t *testing.T) {
	h, users := newTestHandler(t)
	me := addUser(users, "me", false)
	cases := map[string]echo.HandlerFunc{
		"ListRequests": h.ListRequests,
		"RequestsSent": h.RequestsSent,
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			testutil.AssertInvalidParam(t, postJSON(fn, `[]`, me))
		})
	}
}
