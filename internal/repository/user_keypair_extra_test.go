package repository

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupUserKeypairExtra(t *testing.T, userID string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "user_keypair_extra" WHERE "userId" = ?`, userID)
}

func TestUserKeypairExtraRepository_UpsertAndFind(t *testing.T) {
	repo := NewUserKeypairExtraRepository(testDB)
	user := insertTestUser(t, "u_kpx_1", "kpx1")
	defer cleanupUser(t, user.ID)
	defer cleanupUserKeypairExtra(t, user.ID)

	k := &model.UserKeypairExtra{
		UserID:            user.ID,
		Ed25519PublicKey:  "PUBKEY_v1",
		Ed25519PrivateKey: "PRIVKEY_v1",
	}
	require.NoError(t, repo.Upsert(k))

	got, err := repo.FindByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, "PUBKEY_v1", got.Ed25519PublicKey)
	assert.Equal(t, "PRIVKEY_v1", got.Ed25519PrivateKey)

	// Upsert は同一 userId で更新できる
	k.Ed25519PublicKey = "PUBKEY_v2"
	k.Ed25519PrivateKey = "PRIVKEY_v2"
	require.NoError(t, repo.Upsert(k))

	got, err = repo.FindByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, "PUBKEY_v2", got.Ed25519PublicKey)
	assert.Equal(t, "PRIVKEY_v2", got.Ed25519PrivateKey)
}

// InsertIfAbsent は ON CONFLICT DO NOTHING 動作:
//   - 既存行が無いとき → 新規挿入
//   - 既存行があるとき → no-op (= 旧 row 保持、新 row 廃棄) → race-safe な
//     lazy backfill primitive (#1072)。
func TestUserKeypairExtraRepository_InsertIfAbsent(t *testing.T) {
	repo := NewUserKeypairExtraRepository(testDB)
	user := insertTestUser(t, "u_kpx_iia", "kpx_iia")
	defer cleanupUser(t, user.ID)
	defer cleanupUserKeypairExtra(t, user.ID)

	// 1 回目: 新規挿入 → inserted=true
	inserted, err := repo.InsertIfAbsent(&model.UserKeypairExtra{
		UserID: user.ID, Ed25519PublicKey: "PUB1", Ed25519PrivateKey: "PRIV1",
	})
	require.NoError(t, err)
	assert.True(t, inserted, "初回 insert は inserted=true")
	got, err := repo.FindByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, "PUB1", got.Ed25519PublicKey)

	// 2 回目: 既存行があるので no-op (旧 row が保持される) → inserted=false
	inserted, err = repo.InsertIfAbsent(&model.UserKeypairExtra{
		UserID: user.ID, Ed25519PublicKey: "PUB2", Ed25519PrivateKey: "PRIV2",
	})
	require.NoError(t, err)
	assert.False(t, inserted, "既存行ありの場合 inserted=false")
	got, err = repo.FindByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, "PUB1", got.Ed25519PublicKey, "既存行は保持される (race-safe)")
	assert.Equal(t, "PRIV1", got.Ed25519PrivateKey)
}

func TestUserKeypairExtraRepository_Delete(t *testing.T) {
	repo := NewUserKeypairExtraRepository(testDB)
	user := insertTestUser(t, "u_kpx_2", "kpx2")
	defer cleanupUser(t, user.ID)
	defer cleanupUserKeypairExtra(t, user.ID)

	require.NoError(t, repo.Upsert(&model.UserKeypairExtra{
		UserID:            user.ID,
		Ed25519PublicKey:  "PUB",
		Ed25519PrivateKey: "PRIV",
	}))
	require.NoError(t, repo.Delete(user.ID))

	_, err := repo.FindByUserID(user.ID)
	assert.Error(t, err)
}

func TestUserKeypairExtraRepository_FindByUserID_NotFound(t *testing.T) {
	repo := NewUserKeypairExtraRepository(testDB)
	_, err := repo.FindByUserID("u_kpx_nonexistent")
	assert.Error(t, err)
}

func TestUserKeypairExtraRepository_QueryErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewUserKeypairExtraRepository(testDB.WithContext(ctx))

	err := repo.Upsert(&model.UserKeypairExtra{UserID: "x", Ed25519PublicKey: "p", Ed25519PrivateKey: "k"})
	assert.Error(t, err)
	_, err = repo.FindByUserID("x")
	assert.Error(t, err)
	err = repo.Delete("x")
	assert.Error(t, err)
}
