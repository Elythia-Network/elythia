package repository

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupUserPublickeyExtra(t *testing.T, userID string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "user_publickey_extra" WHERE "userId" = ?`, userID)
}

func TestUserPublickeyExtraRepository_UpsertAndFind(t *testing.T) {
	repo := NewUserPublickeyExtraRepository(testDB)
	user := insertTestUser(t, "u_upkx_1", "upkx1")
	defer cleanupUser(t, user.ID)
	defer cleanupUserPublickeyExtra(t, user.ID)

	pk1 := &model.UserPublickeyExtra{
		UserID: user.ID,
		KeyID:  "https://example.com/users/x#ed25519-key",
		KeyPEM: "-----BEGIN PUBLIC KEY-----\nAAA\n-----END PUBLIC KEY-----",
		Alg:    model.AlgEd25519,
	}
	require.NoError(t, repo.Upsert(pk1))

	got, err := repo.FindByUserAndKeyID(user.ID, pk1.KeyID)
	require.NoError(t, err)
	assert.Equal(t, pk1.KeyPEM, got.KeyPEM)
	assert.Equal(t, model.AlgEd25519, got.Alg)
	assert.Equal(t, user.ID, got.UserID)

	// 同一 (userId, keyId) で再 Upsert すると更新される
	pk1.KeyPEM = "-----BEGIN PUBLIC KEY-----\nBBB\n-----END PUBLIC KEY-----"
	require.NoError(t, repo.Upsert(pk1))
	got, err = repo.FindByUserAndKeyID(user.ID, pk1.KeyID)
	require.NoError(t, err)
	assert.Contains(t, got.KeyPEM, "BBB")
}

func TestUserPublickeyExtraRepository_MultipleKeysPerUser(t *testing.T) {
	repo := NewUserPublickeyExtraRepository(testDB)
	user := insertTestUser(t, "u_upkx_2", "upkx2")
	defer cleanupUser(t, user.ID)
	defer cleanupUserPublickeyExtra(t, user.ID)

	require.NoError(t, repo.Upsert(&model.UserPublickeyExtra{
		UserID: user.ID, KeyID: "k1", KeyPEM: "PEM1", Alg: model.AlgEd25519,
	}))
	require.NoError(t, repo.Upsert(&model.UserPublickeyExtra{
		UserID: user.ID, KeyID: "k2", KeyPEM: "PEM2", Alg: model.AlgEd25519,
	}))

	rows, err := repo.ListByUserID(user.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestUserPublickeyExtraRepository_DeleteByKeyID(t *testing.T) {
	repo := NewUserPublickeyExtraRepository(testDB)
	user := insertTestUser(t, "u_upkx_dkid", "upkx_dkid")
	defer cleanupUser(t, user.ID)
	defer cleanupUserPublickeyExtra(t, user.ID)

	require.NoError(t, repo.Upsert(&model.UserPublickeyExtra{
		UserID: user.ID, KeyID: "k1", KeyPEM: "PEM1", Alg: model.AlgEd25519,
	}))
	require.NoError(t, repo.Upsert(&model.UserPublickeyExtra{
		UserID: user.ID, KeyID: "k2", KeyPEM: "PEM2", Alg: model.AlgEd25519,
	}))

	// k1 のみ削除 → k2 は残る (key rotation シナリオの primitive)
	require.NoError(t, repo.DeleteByKeyID(user.ID, "k1"))

	rows, err := repo.ListByUserID(user.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "k2", rows[0].KeyID)

	// 存在しない (userId, keyId) の delete は no-op (GORM Delete は 0 row でも nil)
	assert.NoError(t, repo.DeleteByKeyID(user.ID, "non_existent"))
}

func TestUserPublickeyExtraRepository_DeleteByUserID(t *testing.T) {
	repo := NewUserPublickeyExtraRepository(testDB)
	user := insertTestUser(t, "u_upkx_3", "upkx3")
	defer cleanupUser(t, user.ID)
	defer cleanupUserPublickeyExtra(t, user.ID)

	require.NoError(t, repo.Upsert(&model.UserPublickeyExtra{
		UserID: user.ID, KeyID: "k1", KeyPEM: "PEM1", Alg: model.AlgEd25519,
	}))
	require.NoError(t, repo.DeleteByUserID(user.ID))

	rows, err := repo.ListByUserID(user.ID)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestUserPublickeyExtraRepository_NotFound(t *testing.T) {
	repo := NewUserPublickeyExtraRepository(testDB)
	_, err := repo.FindByUserAndKeyID("nope_user", "nope_key")
	assert.Error(t, err)
}

func TestUserPublickeyExtraRepository_QueryErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewUserPublickeyExtraRepository(testDB.WithContext(ctx))

	err := repo.Upsert(&model.UserPublickeyExtra{UserID: "x", KeyID: "k", KeyPEM: "p", Alg: model.AlgEd25519})
	assert.Error(t, err)
	_, err = repo.FindByUserAndKeyID("x", "k")
	assert.Error(t, err)
	_, err = repo.ListByUserID("x")
	assert.Error(t, err)
	err = repo.DeleteByUserID("x")
	assert.Error(t, err)
	err = repo.DeleteByKeyID("x", "k")
	assert.Error(t, err)
}
