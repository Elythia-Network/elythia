package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserPublickeyRepository_UpsertAndFind(t *testing.T) {
	repo := NewUserPublickeyRepository(testDB)
	seedUser(t, "upk_u1")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user_publickey" WHERE "userId" = ?`, "upk_u1") })

	pk := &model.UserPublickey{UserID: "upk_u1", KeyID: "https://example.com/users/x#main-key", KeyPEM: "-----BEGIN PUBLIC KEY-----\nAAA\n-----END PUBLIC KEY-----"}
	require.NoError(t, repo.Upsert(pk))

	// FindByUserID
	found, err := repo.FindByUserID("upk_u1")
	require.NoError(t, err)
	assert.Equal(t, pk.KeyID, found.KeyID)

	// FindByKeyID
	found, err = repo.FindByKeyID(pk.KeyID)
	require.NoError(t, err)
	assert.Equal(t, "upk_u1", found.UserID)

	// Upsert更新 (同一userIdで別keyを保存)
	pk2 := &model.UserPublickey{UserID: "upk_u1", KeyID: "updated_key_id", KeyPEM: "-----BEGIN PUBLIC KEY-----\nBBB\n-----END PUBLIC KEY-----"}
	require.NoError(t, repo.Upsert(pk2))

	found, err = repo.FindByUserID("upk_u1")
	require.NoError(t, err)
	assert.Equal(t, "updated_key_id", found.KeyID)

	// Delete
	require.NoError(t, repo.Delete("upk_u1"))
	_, err = repo.FindByUserID("upk_u1")
	assert.Error(t, err)
}

func TestUserPublickeyRepository_FindByUserID_NotFound(t *testing.T) {
	repo := NewUserPublickeyRepository(testDB)
	_, err := repo.FindByUserID("upk_nonexistent")
	assert.Error(t, err)
}

func TestUserPublickeyRepository_FindByKeyID_NotFound(t *testing.T) {
	repo := NewUserPublickeyRepository(testDB)
	_, err := repo.FindByKeyID("nonexistent_key")
	assert.Error(t, err)
}
