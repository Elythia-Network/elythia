package repository

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"github.com/elythia-network/elythia/internal/activitypub"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupUserKeypair(t *testing.T, userID string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "user_keypair" WHERE "userId" = ?`, userID)
}

func TestUserKeypairRepository_CreateAndFind(t *testing.T) {
	repo := NewUserKeypairRepository(testDB)
	user := insertTestUser(t, "u_kp_1", "kp1")
	defer cleanupUser(t, user.ID)

	k := &model.UserKeypair{
		UserID:     user.ID,
		PublicKey:  "PUBKEY",
		PrivateKey: "PRIVKEY",
	}
	require.NoError(t, repo.Create(k))
	defer cleanupUserKeypair(t, user.ID)

	got, err := repo.FindByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, "PUBKEY", got.PublicKey)
}

func TestUserKeypairRepository_QueryErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewUserKeypairRepository(testDB.WithContext(ctx))
	err := repo.Create(&model.UserKeypair{UserID: "x", PublicKey: "p", PrivateKey: "k"})
	assert.Error(t, err)
	_, err = repo.FindByUserID("x")
	assert.Error(t, err)
}

// PKCS#1 で保存された既存行を PKCS#8 に正規化する (#2378)。mk-go は以前
// PKCS#1 で発行しており、Misskey TS はそれを署名に使えない。
func TestUserKeypairRepository_NormalizePrivateKeysToPKCS8(t *testing.T) {
	repo := NewUserKeypairRepository(testDB)

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv),
	}))

	legacy := insertTestUser(t, "u_kp_pkcs1", "kppkcs1")
	defer cleanupUser(t, legacy.ID)
	require.NoError(t, repo.Create(&model.UserKeypair{
		UserID: legacy.ID, PublicKey: "PUB", PrivateKey: pkcs1,
	}))
	defer cleanupUserKeypair(t, legacy.ID)

	// 既に PKCS#8 の行は対象外 (= 変換件数に数えない)。
	modernPEM, _, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	modern := insertTestUser(t, "u_kp_pkcs8", "kppkcs8")
	defer cleanupUser(t, modern.ID)
	require.NoError(t, repo.Create(&model.UserKeypair{
		UserID: modern.ID, PublicKey: "PUB", PrivateKey: modernPEM,
	}))
	defer cleanupUserKeypair(t, modern.ID)

	n, err := repo.NormalizePrivateKeysToPKCS8()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 1, "PKCS#1 の行が変換されるべき")

	got, err := repo.FindByUserID(legacy.ID)
	require.NoError(t, err)
	assert.Contains(t, got.PrivateKey, "BEGIN PRIVATE KEY")
	assert.NotContains(t, got.PrivateKey, "RSA PRIVATE KEY")

	// 鍵そのものは変わらない。
	reparsed, err := activitypub.ParseRSAPrivateKey(got.PrivateKey)
	require.NoError(t, err)
	assert.Equal(t, priv.PublicKey, reparsed.PublicKey)

	// PKCS#8 の行は素通し。
	untouched, err := repo.FindByUserID(modern.ID)
	require.NoError(t, err)
	assert.Equal(t, modernPEM, untouched.PrivateKey)

	// 冪等: 2 回目は対象ゼロ。
	again, err := repo.NormalizePrivateKeysToPKCS8()
	require.NoError(t, err)
	assert.Equal(t, 0, again, "冪等でなければ起動のたびに書き込みが発生する")
}

// 破損した PEM の行があっても全体を止めず、他の行は変換される。
func TestUserKeypairRepository_NormalizeSkipsBrokenRow(t *testing.T) {
	repo := NewUserKeypairRepository(testDB)
	broken := insertTestUser(t, "u_kp_broken", "kpbroken")
	defer cleanupUser(t, broken.ID)
	require.NoError(t, repo.Create(&model.UserKeypair{
		UserID: broken.ID, PublicKey: "PUB",
		PrivateKey: "-----BEGIN RSA PRIVATE KEY-----\nbroken\n-----END RSA PRIVATE KEY-----\n",
	}))
	defer cleanupUserKeypair(t, broken.ID)

	n, err := repo.NormalizePrivateKeysToPKCS8()
	require.NoError(t, err, "1 行の破損で全体が止まってはいけない")
	assert.Equal(t, 0, n)
}
