package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPasswordResetRequestRepository_CRUD(t *testing.T) {
	repo := NewPasswordResetRequestRepository(testDB)
	seedUser(t, "prr_u1")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "password_reset_request" WHERE "userId" = ?`, "prr_u1") })

	req := &model.PasswordResetRequest{ID: "prr_1", Token: "token_abc", UserID: "prr_u1"}
	require.NoError(t, repo.Create(req))

	found, err := repo.FindByToken("token_abc")
	require.NoError(t, err)
	assert.Equal(t, "prr_u1", found.UserID)

	require.NoError(t, repo.Delete("prr_1"))
	_, err = repo.FindByToken("token_abc")
	assert.Error(t, err)
}

func TestPasswordResetRequestRepository_FindByToken_NotFound(t *testing.T) {
	repo := NewPasswordResetRequestRepository(testDB)
	_, err := repo.FindByToken("nonexistent_token")
	assert.Error(t, err)
}
