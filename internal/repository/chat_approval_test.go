package repository

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatApprovalRepository_CRUD(t *testing.T) {
	repo := NewChatApprovalRepository(testDB)
	u1 := insertTestUser(t, "u_ca_1", "causer1")
	u2 := insertTestUser(t, "u_ca_2", "causer2")
	u3 := insertTestUser(t, "u_ca_3", "causer3")
	defer cleanupUser(t, u1.ID)
	defer cleanupUser(t, u2.ID)
	defer cleanupUser(t, u3.ID)

	// Exists: initially false
	exists, err := repo.Exists(u1.ID, u2.ID)
	require.NoError(t, err)
	assert.False(t, exists)

	// Create
	a := &model.ChatApproval{ID: "ca_1", UserID: u1.ID, OtherID: u2.ID}
	require.NoError(t, repo.Create(a))
	defer testDB.Exec(`DELETE FROM "chat_approval" WHERE id = ?`, a.ID)

	a2 := &model.ChatApproval{ID: "ca_2", UserID: u1.ID, OtherID: u3.ID}
	require.NoError(t, repo.Create(a2))
	defer testDB.Exec(`DELETE FROM "chat_approval" WHERE id = ?`, a2.ID)

	// Exists: now true
	exists, err = repo.Exists(u1.ID, u2.ID)
	require.NoError(t, err)
	assert.True(t, exists)

	// Exists: reverse pair is false
	exists, err = repo.Exists(u2.ID, u1.ID)
	require.NoError(t, err)
	assert.False(t, exists)

	// ListByUser
	rows, err := repo.ListByUser(u1.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	// Delete
	require.NoError(t, repo.Delete(u1.ID, u2.ID))
	exists, err = repo.Exists(u1.ID, u2.ID)
	require.NoError(t, err)
	assert.False(t, exists)

	// ListByUser after delete
	rows, err = repo.ListByUser(u1.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

func TestChatApprovalRepository_Errors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewChatApprovalRepository(testDB.WithContext(ctx))

	_, err := repo.Exists("a", "b")
	assert.Error(t, err)

	_, err = repo.ListByUser("a")
	assert.Error(t, err)
}
