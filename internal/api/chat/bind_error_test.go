package chat

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
	h, _ := newTestHandler()
	cases := map[string]func(echo.Context) error{
		"RoomsOwned":       h.RoomsOwned,
		"RoomsJoined":      h.RoomsJoined,
		"InvitationsInbox": h.InvitationsInbox,
		"RoomsJoining":     h.RoomsJoining,
		"History":          h.History,
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			testutil.AssertInvalidParam(t, post(fn, `[]`, u1))
		})
	}
}
