package admin_test

import (
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// orderCheckingInvalidator runs check when the auth cache is invalidated.
type orderCheckingInvalidator struct{ check func(userID string) }

func (o orderCheckingInvalidator) InvalidateTokensForUser(userID string) { o.check(userID) }

// TestSuspendUser_InvalidatesAfterTheWrite pins that suspend / unsuspend drop
// the cached authentications only after the new state is written (#3664).
// Dropping first lets a request that reads the user between the drop and the
// write cache the old state again, here and on the other processes.
func TestSuspendUser_InvalidatesAfterTheWrite(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "target"}
	var seen []bool
	h.SetUserTokenInvalidatorForTest(orderCheckingInvalidator{check: func(userID string) {
		require.Equal(t, "u1", userID)
		seen = append(seen, userRepo.Users["u1"].IsSuspended)
	}})

	require.Equal(t, http.StatusNoContent, doPost(h.SuspendUser, `{"userId":"u1"}`, nil).Code)
	require.Equal(t, http.StatusNoContent, doPost(h.UnsuspendUser, `{"userId":"u1"}`, nil).Code)
	assert.Equal(t, []bool{true, false}, seen, "the invalidation must see the written state")
}
