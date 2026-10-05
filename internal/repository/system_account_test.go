package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemAccountRepository_CreateAndFind(t *testing.T) {
	repo := NewSystemAccountRepository(testDB)
	seedUser(t, "sa_u1")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "system_account" WHERE id = ?`, "sa_1") })

	sa := &model.SystemAccount{ID: "sa_1", UserID: "sa_u1", Type: "sa_test_type"}
	require.NoError(t, repo.Create(sa))

	found, err := repo.FindByType("sa_test_type")
	require.NoError(t, err)
	assert.Equal(t, "sa_u1", found.UserID)
}

func TestSystemAccountRepository_FindByType_NotFound(t *testing.T) {
	repo := NewSystemAccountRepository(testDB)
	_, err := repo.FindByType("sa_nonexistent_type")
	assert.Error(t, err)
}
