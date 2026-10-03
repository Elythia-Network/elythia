package signup_test

import (
	"testing"

	"github.com/shiroha-a/mk/internal/testutil"
)

// TestApplicationApply_BindErrorIsInvalidParam pins that a body
// signup-application/apply cannot bind (a JSON array, not an object) is
// answered with 400 INVALID_PARAM like the other /api endpoints. The bind
// error used to be ignored (#3330).
func TestApplicationApply_BindErrorIsInvalidParam(t *testing.T) {
	env := newApprovalEnv(t, true)
	testutil.AssertInvalidParam(t, doPost(env.handler.ApplicationApply, `[]`))
}
