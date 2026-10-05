package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserRepository_FindProfileByVerifyCode(t *testing.T) {
	repo := NewUserRepository(testDB)
	u := insertTestUser(t, "verify_u1", "vu1")
	defer cleanupUser(t, u.ID)

	// profile 作成
	require.NoError(t, repo.CreateProfile(&model.UserProfile{UserID: u.ID}))

	code := "test-verify-code-123"
	require.NoError(t, repo.UpdateProfile(u.ID, map[string]any{"emailVerifyCode": code}))

	got, err := repo.FindProfileByVerifyCode(code)
	require.NoError(t, err)
	assert.Equal(t, u.ID, got.UserID)
}

func TestUserRepository_FindProfileByVerifyCode_NotFound(t *testing.T) {
	repo := NewUserRepository(testDB)
	_, err := repo.FindProfileByVerifyCode("nonexistent-code")
	assert.Error(t, err)
}
