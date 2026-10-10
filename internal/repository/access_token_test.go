package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccessTokenRepository_FindByHash(t *testing.T) {
	repo := NewAccessTokenRepository(testDB)
	user := insertTestUser(t, "u_atf_1", "tokenuser")
	defer cleanupUser(t, user.ID)

	token := &model.AccessToken{
		ID:     "at_atf_1",
		Token:  "rawtoken123",
		Hash:   "testhash123",
		UserID: user.ID,
	}
	require.NoError(t, testDB.Create(token).Error)
	defer testDB.Exec(`DELETE FROM "access_token" WHERE id = ?`, token.ID)

	found, err := repo.FindByHash("testhash123")
	require.NoError(t, err)
	assert.Equal(t, token.ID, found.ID)
	assert.NotNil(t, found.User)
	assert.Equal(t, user.ID, found.User.ID)
}

func TestAccessTokenRepository_FindByHash_NotFound(t *testing.T) {
	repo := NewAccessTokenRepository(testDB)

	_, err := repo.FindByHash("nonexistent_hash")
	assert.Error(t, err)
}

func TestAccessTokenRepository_FindByHashOrToken(t *testing.T) {
	repo := NewAccessTokenRepository(testDB)
	user := insertTestUser(t, "u_atfh_1", "tokenuser_or")
	defer cleanupUser(t, user.ID)

	// 2 種類の token を作って両 path を verify する:
	//   miauthToken = miauth/gen-token と同じ shape (hash 列で hit する)
	//   appToken    = auth/accept と同じ shape (hash = sha256(token+secret) なので
	//                 hash 列では miss、raw token 列で hit する)
	miauthToken := &model.AccessToken{
		ID:     "at_atfh_miauth",
		Token:  "raw_miauth_xyz",
		Hash:   "hash_miauth_xyz",
		UserID: user.ID,
	}
	require.NoError(t, testDB.Create(miauthToken).Error)
	defer testDB.Exec(`DELETE FROM "access_token" WHERE id = ?`, miauthToken.ID)

	appToken := &model.AccessToken{
		ID:     "at_atfh_app",
		Token:  "raw_app_xyz",
		Hash:   "hash_app_with_secret",
		UserID: user.ID,
	}
	require.NoError(t, testDB.Create(appToken).Error)
	defer testDB.Exec(`DELETE FROM "access_token" WHERE id = ?`, appToken.ID)

	t.Run("hits via hash column (miauth path)", func(t *testing.T) {
		found, err := repo.FindByHashOrToken("hash_miauth_xyz", "raw_miauth_xyz")
		require.NoError(t, err)
		assert.Equal(t, miauthToken.ID, found.ID)
		assert.NotNil(t, found.User)
	})

	t.Run("hits via token column (app/auth path)", func(t *testing.T) {
		// hash 値は middleware が知らない (= sha256(token+secret))。
		// raw token 列で OR 検索が hit するはず。
		found, err := repo.FindByHashOrToken("sha256_of_raw_app_xyz_alone", "raw_app_xyz")
		require.NoError(t, err)
		assert.Equal(t, appToken.ID, found.ID)
		assert.NotNil(t, found.User)
	})

	t.Run("not found when neither matches", func(t *testing.T) {
		_, err := repo.FindByHashOrToken("nonexistent_hash", "nonexistent_token")
		assert.Error(t, err)
	})

	// 本家の app 認証のクライアントは i = sha256(accessToken + appSecret) を送る。
	// 提示された値 (小文字にして) が hash 列と一致すれば引ける (#3490)。
	t.Run("hits when the presented value is the stored hash (upstream app form)", func(t *testing.T) {
		found, err := repo.FindByHashOrToken("sha256_of_presented_value", "HASH_APP_WITH_SECRET")
		require.NoError(t, err)
		assert.Equal(t, appToken.ID, found.ID)
	})
}
