package emojiapplications_test

import (
	"testing"

	"github.com/shiroha-a/mk/internal/api/emojiapplications"
	"github.com/shiroha-a/mk/internal/testutil"
)

// TestListMine_BindErrorIsInvalidParam pins that a body list-mine cannot bind
// (a JSON array, not an object) is answered with 400 INVALID_PARAM, as
// upstream's ajv `type: 'object'` check does. The bind error used to be
// ignored (#3330).
func TestListMine_BindErrorIsInvalidParam(t *testing.T) {
	h := emojiapplications.NewHandler(nil, &stubApps{}, nil)
	testutil.AssertInvalidParam(t, doPost(h.ListMine, `[]`, alice))
}
