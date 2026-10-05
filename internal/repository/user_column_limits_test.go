package repository

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
)

// userIdentityColumns は resolver が actor から書く URI / host 列と、その上限。
// migration/000001_initial.up.sql と一致させる。
var userIdentityColumns = []struct {
	column string
	max    int
}{
	{"uri", 512},
	{"host", 128},
	{"inbox", 512},
	{"sharedInbox", 512},
	{"featured", 512},
	{"movedToUri", 512},
}

// 列の上限そのものを schema から固定する (#2723)。
//
// resolver 側の定数 (userURIMaxRunes / userHostMaxRunes) と独立に同じ数値が書かれて
// いるだけだと、揃って動かせば全部緑になる。列が変わったらここが落ちる。
func TestUser_IdentityColumnLimits(t *testing.T) {
	for _, tc := range userIdentityColumns {
		var n int
		require.NoError(t, testDB.Raw(`SELECT character_maximum_length FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'user' AND column_name = ?`,
			tc.column).Scan(&n).Error)
		assert.Equal(t, tc.max, n,
			"user.%s の列長が変わっている (internal/core/federation/resolver.go の定数も直すこと)", tc.column)
	}
}

// resolver が「収まる」と判断した長さで実際に書けること (#2723)。
//
// mock repository は列制約を持たないので、resolver 側のテストだけでは「本当に入る
// 長さか」を確かめられない。**全角で埋める** — byte で数える実装だと 3 倍になって
// 入らない (列はコードポイント単位)。
func TestUser_IdentityColumnLimitsAcceptMaxLengthValues(t *testing.T) {
	id := "u_uricollimit_00000000000000000"
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user" WHERE id = ?`, id) })

	uri := strings.Repeat("あ", 512)
	host := strings.Repeat("あ", 128)
	require.NoError(t, testDB.Create(&model.User{
		ID:                id,
		Username:          "uricollimit",
		UsernameLower:     "uricollimit",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
		URI:               &uri,
		Host:              &host,
		Inbox:             &uri,
		SharedInbox:       &uri,
		Featured:          &uri,
		MovedToURI:        &uri,
	}).Error)

	for _, tc := range userIdentityColumns {
		var stored string
		require.NoError(t, testDB.Raw(
			`SELECT `+quoteColumn(tc.column)+` FROM "user" WHERE id = ?`, id).
			Scan(&stored).Error)
		assert.Equal(t, tc.max, len([]rune(stored)), "user.%s", tc.column)
	}
}

// quoteColumn は識別子を quote する。列名は上のリテラル表だけから来るが、素で
// 埋めると camelCase が folding されて存在しない列になる。
func quoteColumn(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
