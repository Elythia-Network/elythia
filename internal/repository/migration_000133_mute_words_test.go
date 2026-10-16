package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #3369: 000133 は、ワードミュートの要素のうち `/.../` の形でない文字列を同じ語の
// 組に書き換え、空白だけの文字列を消す。配列の要素と正規表現の文字列は変えない。
//
// **migration をテストの中で流す。** 適用済みの schema を見るだけでは、
// migration の中身を変えても緑のまま通る。
func TestMigration000133_RewritesPlainStringMuteWords(t *testing.T) {
	up := migrationSQL(t, "000133_mute_words_plain_string_to_keyword.up.sql")
	type row struct{ id, muted, hard string }
	rows := []row{
		// 文字列は語の組に、配列と正規表現はそのまま、順序も保つ。空白だけの文字列は消す。
		{"u_m133_a", `["foo", ["a", "b"], "/re/i", "", "  ", "Foo Bar"]`, `["spam", ["x"], "/sp+am/i", " ", "Mixed Case"]`},
		// 書き換えるものが無い行は触らない (配列・正規表現・空の一覧)。
		{"u_m133_b", `[["keep"], "/keep/"]`, `[]`},
		// 正規表現として残す形。hardMutedWords は #3369 より前のサーバー (Go、flags は
		// gimsuy) か frontend (JavaScript、flags は任意、`.` は \r や U+2028 に当たらない)
		// のどちらかの形なら残す。mutedWords は frontend の形だけ。TS 版が書く
		// `/foo/v` `/foo/d` (本家 i/update は flags を問わず受け付ける) はどちらも残る。
		{"u_m133_c", `["/foo/v", "/foo/d", "/foo/x", "/a\nb/", "/a\rb/", "/a\u2028b/", "/a\u2029b/"]`, `["/foo/v", "/foo/d", "/foo/x", "/foo/gimsuy", "/a\nb/", "/a\rb/", "/a\u2028b/", "/a\u2029b/x"]`},
		// 空白だけの判定は JavaScript の trim() と同じ文字の集合 (locale に依らない)。
		{"u_m133_e", `["\u3000", "\u00a0", "\ufeff", "\t\u2028 ", "\u205f", "\u200a", " x "]`, `["\u3000\u3000", "\u1680", "\ufeff", "\u00a0\u202f", "\u205f", "\u200a", " x "]`},
		// 一覧が配列でない行は触らない。
		{"u_m133_d", `{"not": "array"}`, `"plain"`},
	}
	for _, r := range rows {
		u := insertTestUser(t, r.id, r.id)
		defer cleanupUser(t, u.ID)
		require.NoError(t, testDB.Exec(`INSERT INTO "user_profile" ("userId", "mutedWords", "hardMutedWords") VALUES (?, ?::jsonb, ?::jsonb)`, u.ID, r.muted, r.hard).Error)
		defer testDB.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, u.ID)
	}

	require.NoError(t, testDB.Exec(up).Error)

	get := func(id string) (string, string) {
		var got struct {
			Muted string `gorm:"column:mutedWords"`
			Hard  string `gorm:"column:hardMutedWords"`
		}
		require.NoError(t, testDB.Raw(`SELECT "mutedWords"::text AS "mutedWords", "hardMutedWords"::text AS "hardMutedWords" FROM "user_profile" WHERE "userId" = ?`, id).Scan(&got).Error)
		return got.Muted, got.Hard
	}
	want := map[string][2]string{
		"u_m133_a": {`[["foo"], ["a", "b"], "/re/i", ["Foo Bar"]]`, `[["spam"], ["x"], "/sp+am/i", ["Mixed Case"]]`},
		"u_m133_b": {`[["keep"], "/keep/"]`, `[]`},
		"u_m133_c": {`["/foo/v", "/foo/d", "/foo/x", ["/a\nb/"], ["/a\rb/"], ["/a\u2028b/"], ["/a\u2029b/"]]`, `["/foo/v", "/foo/d", "/foo/x", "/foo/gimsuy", ["/a\nb/"], "/a\rb/", "/a\u2028b/", ["/a\u2029b/x"]]`},
		"u_m133_e": {`[[" x "]]`, `[[" x "]]`},
		"u_m133_d": {`{"not": "array"}`, `"plain"`},
	}
	check := func() {
		for id, w := range want {
			m, h := get(id)
			assert.JSONEq(t, w[0], m, "%s mutedWords", id)
			assert.JSONEq(t, w[1], h, "%s hardMutedWords", id)
		}
	}
	check()
	// 2 回流しても変わらない。
	require.NoError(t, testDB.Exec(up).Error)
	check()
}
