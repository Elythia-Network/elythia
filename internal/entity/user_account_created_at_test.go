package entity

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
)

// accountCreatedAt は Elythia 独自の additive field で、リモートの人の作成日時を
// 出す。分からないときとローカルの人には key ごと出さない (本家の e2e がキーの
// 過不足を見るため)。createdAt (ID の日時) は変えない (#3465)。
func TestPackUserDetailed_AccountCreatedAt(t *testing.T) {
	idGen := newTestIDGen(t)
	firstSeen := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	created := time.Date(2018, 3, 4, 5, 6, 7, 0, time.FixedZone("JST", 9*60*60))
	host := "remote.example"
	const wantCreatedAt = "2025-06-01T12:00:00.000Z"

	cases := []struct {
		name string
		user *model.User
		want any // nil は key が無いこと
	}{
		{"remote with value", &model.User{ID: idGen.Generate(firstSeen), Host: &host, AccountCreatedAt: &created}, "2018-03-03T20:06:07.000Z"},
		{"remote without value", &model.User{ID: idGen.Generate(firstSeen), Host: &host}, nil},
		{"local never has it", &model.User{ID: idGen.Generate(firstSeen), AccountCreatedAt: &created}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.user.Username = "alice"
			tc.user.AvatarDecorations = datatypes.JSON([]byte("[]"))
			b, err := json.Marshal(PackUserDetailed(tc.user, nil, idGen))
			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, json.Unmarshal(b, &got))

			value, present := got["accountCreatedAt"]
			if tc.want == nil {
				assert.False(t, present, "分からないとき・ローカルの人には key を出さない (null も出さない)")
			} else {
				assert.Equal(t, tc.want, value)
			}
			assert.Equal(t, wantCreatedAt, got["createdAt"], "createdAt は ID の日時のまま変えない")
		})
	}
}
