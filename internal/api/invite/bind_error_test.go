package invite

import (
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// TestList_BindErrorIsInvalidParam pins that a body invite/list cannot bind
// (a JSON array, not an object) is answered with 400 INVALID_PARAM, as
// upstream's ajv `type: 'object'` check does. The bind error used to be
// ignored (#3330).
func TestList_BindErrorIsInvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	testutil.AssertInvalidParam(t, post(h.List, `[]`, &model.User{ID: "u1", Username: "u1"}))
}
