package repository

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// publickeyColumns は resolver が actor の鍵から書く列と、その上限。
// migration/000031_user_publickey.up.sql / 000050_user_publickey_extra.up.sql と
// 一致させる。upstream の MiUserPublickey も同じ 256 / 4096。
var publickeyColumns = []struct {
	table  string
	column string
	max    int
}{
	{"user_publickey", "keyId", 256},
	{"user_publickey", "keyPem", 4096},
	{"user_publickey_extra", "keyId", 256},
	{"user_publickey_extra", "keyPem", 4096},
	{"user_publickey_extra", "alg", 32},
}

// 列の上限そのものを schema から固定する (#2726)。
func TestUserPublickey_ColumnLimits(t *testing.T) {
	for _, tc := range publickeyColumns {
		var n int
		require.NoError(t, testDB.Raw(`SELECT character_maximum_length FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
			tc.table, tc.column).Scan(&n).Error)
		assert.Equal(t, tc.max, n,
			"%s.%s の列長が変わっている (internal/core/federation/resolver.go の定数も直すこと)",
			tc.table, tc.column)
	}
}

// resolver が「収まる」と判断した長さで実際に書けること (#2726)。
// **全角で埋める** — byte で数える実装だと 3 倍になって入らない。
func TestUserPublickey_ColumnLimitsAcceptMaxLengthValues(t *testing.T) {
	user := insertTestUser(t, "u_pk_1", "pkuser1")
	defer cleanupUser(t, user.ID)

	keyID := "https://remote.example/#" + strings.Repeat("あ", 232)
	require.Equal(t, 256, len([]rune(keyID)))
	keyPEM := strings.Repeat("い", 4096)

	require.NoError(t, NewUserPublickeyRepository(testDB).Upsert(&model.UserPublickey{
		UserID: user.ID, KeyID: keyID, KeyPEM: keyPEM,
	}))
	defer testDB.Exec(`DELETE FROM "user_publickey" WHERE "userId" = ?`, user.ID)

	require.NoError(t, NewUserPublickeyExtraRepository(testDB).Upsert(&model.UserPublickeyExtra{
		UserID: user.ID, KeyID: keyID, KeyPEM: keyPEM, Alg: model.AlgEd25519,
	}))
	defer testDB.Exec(`DELETE FROM "user_publickey_extra" WHERE "userId" = ?`, user.ID)

	var stored string
	require.NoError(t, testDB.Raw(
		`SELECT "keyId" FROM "user_publickey" WHERE "userId" = ?`, user.ID).Scan(&stored).Error)
	assert.Equal(t, 256, len([]rune(stored)))
	require.NoError(t, testDB.Raw(
		`SELECT "keyPem" FROM "user_publickey_extra" WHERE "userId" = ?`, user.ID).Scan(&stored).Error)
	assert.Equal(t, 4096, len([]rune(stored)))
}
