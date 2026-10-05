package notes

import (
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// TestHandlers_BindErrorIsInvalidParam pins that a body these endpoints
// cannot bind (a JSON array, not an object) is answered with 400
// INVALID_PARAM, as upstream's ajv `type: 'object'` check does. The bind
// error used to be ignored (#3330).
func TestHandlers_BindErrorIsInvalidParam(t *testing.T) {
	h, _ := newDraftHandlerWithRepo()
	me := &model.User{ID: "u1", Username: "u1"}
	cases := map[string]func(echo.Context) error{
		"DraftsList":          h.DraftsList,
		"PollsRecommendation": h.PollsRecommendation,
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			testutil.AssertInvalidParam(t, postDraft(fn, `[]`, me))
		})
	}
}
