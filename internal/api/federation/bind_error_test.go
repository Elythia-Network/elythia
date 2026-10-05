package federation

import (
	"testing"

	"github.com/elythia-network/elythia/internal/testutil"
)

// TestStats_BindErrorIsInvalidParam pins that a body federation/stats cannot
// bind (a JSON array, not an object) is answered with 400 INVALID_PARAM, as
// upstream's ajv `type: 'object'` check does. The bind error used to be
// ignored (#3330).
func TestStats_BindErrorIsInvalidParam(t *testing.T) {
	h, _ := newHandler(t)
	testutil.AssertInvalidParam(t, postBody(h.Stats, `[]`))
}
