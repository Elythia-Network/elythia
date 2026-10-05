package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// emojiApplicationColumns は申請経路が書く列と、その上限 (#3022)。
// `internal/core/emojiapplication` の `application*MaxRunes` と同じ数値が独立に
// 書かれているだけだと、揃って動かせば全部緑になる。
var emojiApplicationColumns = []struct {
	table  string
	column string
	max    int
}{
	{"emoji_application", "name", 128},
	{"emoji_application", "category", 128},
	{"emoji_application", "license", 1024},
	{"emoji_application", "comment", 2048},
	{"emoji_application", "remoteHost", 128},
	{"emoji_application", "remoteName", 128},
	{"emoji_application", "rejectReason", 2048},
	{"emoji_application", "fileId", 32},
	{"emoji_application", "id", 32},
	{"emoji_application_quota_reset", "reason", 1024},
}

// emojiApplicationCoveredColumns は上の一覧が**必ず含む**列。
//
// **行の存在だけでは守れない。** 1 行消しても他は緑のままなので、突き合わせが
// 静かに減る (#3018 の `mustDetectSecretFields` と同じ形)。
// `internal/core/emojiapplication` が上限定数を持つ列はここに全部並べる。
var emojiApplicationCoveredColumns = []string{
	"id", "name", "category", "license", "comment",
	"remoteHost", "remoteName", "rejectReason", "fileId",
}

// 申請経路が見ている列の上限を schema から固定する (#3022)。
func TestEmojiApplication_ColumnLimits(t *testing.T) {
	// **突き合わせる列の集合そのものを固定する。** 一覧から行が消えても他の
	// アサーションは緑のままなので、ここで気付けるようにする。
	covered := make(map[string]bool, len(emojiApplicationColumns))
	for _, tc := range emojiApplicationColumns {
		if tc.table == "emoji_application" {
			covered[tc.column] = true
		}
	}
	for _, want := range emojiApplicationCoveredColumns {
		assert.True(t, covered[want],
			"emoji_application.%s が突き合わせの一覧から外れている", want)
	}

	for _, tc := range emojiApplicationColumns {
		var n int
		require.NoError(t, testDB.Raw(`SELECT character_maximum_length FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
			tc.table, tc.column).Scan(&n).Error)
		assert.Equal(t, tc.max, n,
			"%s.%s の列長が変わっている (internal/core/emojiapplication/service.go の定数も直すこと)",
			tc.table, tc.column)
	}
	// 配列は `character_maximum_length` が NULL なので format_type で見る (#2726)。
	assert.Equal(t, "character varying(128)[]",
		arrayElementMaxLength(t, "emoji_application", "aliases"),
		"emoji_application.aliases の要素長が変わっている (internal/core/emojiapplication/service.go の定数も直すこと)")
}

// 申請側が「収まる」と判断した長さで実際に書けること (#3022)。
// **全角で埋める** — byte で数える実装だと 3 倍になって入らない。
// 上限の値そのものは `internal/core/emojiapplication` が定数で持ち、
// 上の `TestEmojiApplication_ColumnLimits` が DDL と突き合わせる。
func TestEmojiApplication_AcceptsMaxLengthValues(t *testing.T) {
	user := insertTestUser(t, "u_eacl_1", "eacluser")
	defer cleanupUser(t, user.ID)

	category := strings.Repeat("あ", 128)
	now := time.Now()
	app := &model.EmojiApplication{
		ID:       "ea_collimit_1",
		UserID:   user.ID,
		Kind:     model.EmojiApplicationKindOwn,
		Status:   model.EmojiApplicationPending,
		Name:     strings.Repeat("a", 128),
		Category: &category,
		Aliases:  model.StringArray{strings.Repeat("い", 128)},
		License:  strings.Repeat("う", 1024),
		Comment:  ptrString(strings.Repeat("え", 2048)),
		// 却下理由も同じ行に載るので一緒に確かめる。
		RejectReason: ptrString(strings.Repeat("お", 2048)),
		// `fileId` は FK を張っていないので、実在しない id でも書ける。
		FileID:    ptrString(strings.Repeat("f", 32)),
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, testDB.Create(app).Error)
	defer testDB.Exec(`DELETE FROM emoji_application WHERE id = ?`, app.ID)

	var got model.EmojiApplication
	require.NoError(t, testDB.First(&got, "id = ?", app.ID).Error)
	assert.Equal(t, 1024, len([]rune(got.License)), "上限ちょうどの license が切られている")
	require.NotNil(t, got.Category)
	assert.Equal(t, 128, len([]rune(*got.Category)))
	require.NotNil(t, got.Comment)
	assert.Equal(t, 2048, len([]rune(*got.Comment)))
	require.NotNil(t, got.RejectReason)
	assert.Equal(t, 2048, len([]rune(*got.RejectReason)))
	require.Len(t, got.Aliases, 1)
	assert.Equal(t, 128, len([]rune(got.Aliases[0])))
	require.NotNil(t, got.FileID)
	assert.Equal(t, 32, len([]rune(*got.FileID)))
}

func ptrString(s string) *string { return &s }
