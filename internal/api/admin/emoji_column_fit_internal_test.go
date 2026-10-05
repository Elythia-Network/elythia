package admin

import (
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 列に入るかの判定そのもの (#3018)。handler 経由のテストは経路ごとの順序を見るが、
// 数え方はここで固定する。

// 上限の値そのものを DDL 側と同じ数で固定する (#3018)。**列ごとに別々に持つ** —
// `emoji.category` / `aliases` / `roleIdsThatCanBeUsedThisEmojiAsReaction` は
// たまたま同じ 128 だが独立に変わりうるので、1 つを小さくする変異がここで落ちる。
// DDL 側の実値は `internal/repository/emoji_column_limits_test.go` が
// `information_schema` と突き合わせている。
func TestEmojiColumnLimitConstants(t *testing.T) {
	assert.Equal(t, 128, emojiNameMaxRunes, "emoji.name は varchar(128)")
	assert.Equal(t, 128, emojiCategoryMaxRunes, "emoji.category は varchar(128)")
	assert.Equal(t, 128, emojiAliasMaxRunes, "emoji.aliases は varchar(128)[]")
	assert.Equal(t, 1024, emojiLicenseMaxRunes, "emoji.license は varchar(1024)")
	assert.Equal(t, 128, emojiRoleIDMaxRunes,
		"emoji.roleIdsThatCanBeUsedThisEmojiAsReaction は varchar(128)[]")
	assert.Equal(t, 512, emojiURLMaxRunes, "emoji.originalUrl / publicUrl は varchar(512)")
}

// role id も上限ちょうどは通す (alias と独立に上限を取り違える変異をここで落とす)。
func TestNormalizeEmojiRoleIDs(t *testing.T) {
	atLimit := strings.Repeat("あ", emojiRoleIDMaxRunes)
	over := strings.Repeat("あ", emojiRoleIDMaxRunes+1)
	assert.Equal(t, []string{"role1", atLimit}, normalizeEmojiRoleIDs([]string{"role1", atLimit, over, ""}))
}

func TestEmojiBodyFits(t *testing.T) {
	s := func(v string) *string { return &v }
	for _, tc := range []struct {
		name string
		v    *string
		max  int
		want bool
	}{
		{"省略 (nil) は常に通る", nil, 4, true},
		{"上限ちょうどは通る", s("abcd"), 4, true},
		{"1 文字超過は通らない", s("abcde"), 4, false},
		// **rune で数える。** varchar はコードポイントで数えるので、byte で見ると
		// 全角は列の約 1/3 しか使えず、正当な入力が 400 になる。
		{"全角も rune で数える", s(strings.Repeat("あ", 4)), 4, true},
		{"全角の 1 文字超過", s(strings.Repeat("あ", 5)), 4, false},
		// NUL は長さに関わらず SQLSTATE 22021 で弾かれるので通さない。
		{"NUL は通らない", s("a\x00b"), 8, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, emojiBodyFits(tc.v, tc.max))
		})
	}
}

func TestNormalizeEmojiAliases(t *testing.T) {
	long := strings.Repeat("あ", emojiAliasMaxRunes+1)
	got := normalizeEmojiAliases([]string{"ok", long, "", "a\x00b", strings.Repeat("い", emojiAliasMaxRunes)})
	assert.Equal(t, []string{"ok", "ab", strings.Repeat("い", emojiAliasMaxRunes)}, got,
		"入らない要素だけを落とし、NUL は落としてから判定すること")
}

// **空になっても nil を返さない。** `model.StringArray(nil)` の `Value()` は SQL の
// NULL になり (`internal/pgarray`)、`emoji.aliases` は NOT NULL なので書き込みが
// 制約違反で落ちる (#729 で踏んだのと同じ形)。呼び出し側の
// `req.Aliases != nil` (= 明示送信したか) の判定も壊れる。
func TestNormalizeEmojiAliasesNeverReturnsNil(t *testing.T) {
	for _, in := range [][]string{nil, {}, {strings.Repeat("あ", emojiAliasMaxRunes+1)}, {""}} {
		got := normalizeEmojiAliases(in)
		require.NotNil(t, got)
		assert.Empty(t, got)
		v, err := model.StringArray(got).Value()
		require.NoError(t, err)
		assert.Equal(t, "{}", v, "空配列が SQL NULL になると NOT NULL 制約で落ちる")
	}
}
