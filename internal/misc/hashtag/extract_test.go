package hashtag

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtract(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "empty input",
			in:   []string{""},
			want: nil,
		},
		{
			name: "no hashtags",
			in:   []string{"hello world"},
			want: nil,
		},
		{
			name: "single tag",
			in:   []string{"hello #golang"},
			want: []string{"golang"},
		},
		{
			name: "tag at start",
			in:   []string{"#golang is fast"},
			want: []string{"golang"},
		},
		{
			name: "multiple tags",
			in:   []string{"learn #go and #rust today"},
			want: []string{"go", "rust"},
		},
		{
			name: "duplicate tags collapse",
			in:   []string{"#go again #go and #GO"},
			want: []string{"go"},
		},
		{
			name: "japanese tag",
			in:   []string{"おはよう #朝 みなさん"},
			want: []string{"朝"},
		},
		{
			name: "underscore and hyphen",
			in:   []string{"#hello_world #foo-bar"},
			want: []string{"hello_world", "foo-bar"},
		},
		{
			name: "no space before #",
			in:   []string{"foo#bar"},
			want: nil, // word boundary 必須
		},
		{
			name: "consecutive hashtags pick first only",
			in:   []string{"#one#two"},
			want: []string{"one"}, // upstream Misskey と同じく #two は拾わない
		},
		{
			name: "after quote",
			in:   []string{`"#quoted"`},
			want: []string{"quoted"},
		},
		{
			name: "hashtag-only line",
			in:   []string{"#alpha\n#beta"},
			want: []string{"alpha", "beta"},
		},
		{
			name: "case preserved on first occurrence",
			in:   []string{"#Golang and #golang"},
			want: []string{"Golang"},
		},
		{
			name: "text + cw",
			in:   []string{"hello #foo", "warning #bar"},
			want: []string{"foo", "bar"},
		},
		{
			// #2106 L65: >128 code point の tag は byte truncate でなく drop する
			// (NormalizeNoteTags と揃える)。同行の有効な tag は残る。
			name: "long tag dropped",
			in:   []string{"#" + strings.Repeat("a", MaxTagLength+50) + " #ok"},
			want: []string{"ok"},
		},
		{
			name: "fenced code block excluded",
			in:   []string{"normal #yes\n```\n#secret\n```\nback #also"},
			want: []string{"yes", "also"},
		},
		{
			name: "inline code excluded",
			in:   []string{"check `#var` here #real"},
			want: []string{"real"},
		},
		{
			name: "url fragment not picked up",
			in:   []string{"see https://example.com/path#anchor for #real"},
			want: []string{"real"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Extract(tc.in...)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestExtractUserTags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "normalizes to lowercase",
			in:   []string{"I love #Golang and #GoLang"},
			want: []string{"golang"}, // case-insensitive dedup after normalize
		},
		{
			name: "NFKC folds full-width",
			in:   []string{"#Ｔｏｋｙｏ"},
			want: []string{"tokyo"},
		},
		{
			name: "AP person.tag style #-prefixed names",
			in:   []string{"#art", "#Music"},
			want: []string{"art", "music"},
		},
		{
			name: "no hashtags returns nil",
			in:   []string{"plain text without tags"},
			want: nil,
		},
		{
			name: "empty input returns nil",
			in:   []string{""},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ExtractUserTags(tc.in...))
		})
	}
}

// #1948-18: ExtractNoteTags は note.tags を NFKC+lowercase 正規化し、>128 char を
// drop、32 件 cap、case-insensitive dedup する (upstream NoteCreateService)。
func TestExtractNoteTags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"normalizes to lowercase", []string{"#Misskey and #misskey"}, []string{"misskey"}},
		{"NFKC folds full-width", []string{"#Ｍｉｓｓｋｅｙ"}, []string{"misskey"}},
		{"no hashtags returns nil", []string{"plain text"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ExtractNoteTags(tc.in...))
		})
	}
}

// #1948-18: >128 code point の tag は drop する (truncate ではない)。
func TestExtractNoteTags_DropsTooLong(t *testing.T) {
	long := strings.Repeat("a", 129)
	got := ExtractNoteTags("#short #" + long)
	assert.Equal(t, []string{"short"}, got, ">128 char tag は drop され short のみ残る (#1948-18)")
}

// #1948-18: NormalizeNoteTags は AP 由来の事前抽出 tag 配列を同じく正規化する。
func TestNormalizeNoteTags(t *testing.T) {
	assert.Equal(t, []string{"misskey", "art"}, NormalizeNoteTags([]string{"Misskey", "misskey", "Art"}))
	assert.Nil(t, NormalizeNoteTags(nil))
	assert.Nil(t, NormalizeNoteTags([]string{strings.Repeat("x", 200)}), ">128 char は drop")
}

// #1948-18: cap(32) は normalize より前に効く (upstream splice→map 順)。case 衝突で
// 空いた slot に upstream が落とす tag が残らないことを確認する。
func TestNormalizeNoteTags_CapBeforeNormalize(t *testing.T) {
	// ['Misskey','misskey','a1'..'a31'] (33 raw-distinct)。upstream は raw cap32 で
	// ['Misskey','misskey','a1'..'a30'] を残し a31 を drop → normalize → ['misskey','a1'..'a30']。
	in := []string{"Misskey", "misskey"}
	for i := 1; i <= 31; i++ {
		in = append(in, "a"+string(rune('0'+i/10))+string(rune('0'+i%10)))
	}
	got := NormalizeNoteTags(in)
	assert.Contains(t, got, "misskey")
	assert.Contains(t, got, "a01")
	assert.Contains(t, got, "a30")
	assert.NotContains(t, got, "a31", "cap が normalize より前に効くので a31 は drop (#1948-18)")
}

// caps at MaxNoteTags (32) entries.
func TestExtractNoteTags_Cap(t *testing.T) {
	parts := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		parts = append(parts, "#tag"+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	got := ExtractNoteTags(strings.Join(parts, " "))
	assert.Len(t, got, MaxNoteTags)
}

// caps at MaxUserTags (32) entries.
func TestExtractUserTags_Cap(t *testing.T) {
	parts := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		parts = append(parts, "#tag"+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	got := ExtractUserTags(strings.Join(parts, " "))
	assert.Len(t, got, MaxUserTags)
}

// NFKC 正規化は合字を展開するので、Extract の 128 rune フィルタを通った tag が
// ExtractUserTags の出力では膨らむ。`user.tags` は varchar(128)[] なので、
// 超過値を渡すと INSERT ごと落ちて **その actor が 1 行も作られない** (#2662)。
// note.tags も varchar(128)[]。user 側だけ直すと「actor は取り込めるのに
// 同じハッシュタグを含む Note は丸ごと落ちる」非対称が残る (#2662)。
func TestNormalizeNoteTags_DropsTagsThatGrowPastLimitAfterNormalization(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want int
	}{
		{"expands past limit", []string{strings.Repeat("\u337f", 100)}, 0},
		{"expands within limit", []string{strings.Repeat("\u337f", 30)}, 1},
		{"plain within limit", []string{strings.Repeat("a", 128)}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeNoteTags(tc.in)
			require.Len(t, got, tc.want)
			for _, g := range got {
				assert.LessOrEqual(t, utf8.RuneCountInString(g), MaxTagLength,
					"varchar(128)[] に収まること")
			}
		})
	}

	// MFM 抽出経路も同じ (リモート Note / ローカル投稿の両方が通る)。
	assert.Empty(t, ExtractNoteTags("#"+strings.Repeat("\u337f", 100)))
}

func TestExtractUserTags_DropsTagsThatGrowPastLimitAfterNormalization(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		// U+337F SQUARE CORPORATION は NFKC で 4 rune (株式会社) に展開する。
		{"expands past limit", "#" + strings.Repeat("\u337f", 100), 0},
		// 展開しても収まるものは残る。
		{"expands within limit", "#" + strings.Repeat("\u337f", 30), 1},
		{"plain within limit", "#" + strings.Repeat("a", 128), 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractUserTags(tc.in)
			require.Len(t, got, tc.want)
			for _, g := range got {
				assert.LessOrEqual(t, utf8.RuneCountInString(g), MaxTagLength,
					"varchar(128)[] に収まること")
			}
		})
	}
}

// NUL 入りの tag は落とす。`note.tags` / `user.tags` は varchar(128)[] で NUL を
// 受け付けず、INSERT ごと落ちて **その actor / Note が 1 行も作られない** (#2662)。
// 正規化 (NFKC) は NUL を除去しない。
func TestTags_DropNUL(t *testing.T) {
	withNUL := "ab" + "\u0000" + "cd"
	// リモート actor の `tag` は `"#" + name` の形で渡される。
	require.NotEmpty(t, ExtractUserTags("#ok"), "対照: NUL 無しなら拾う")
	assert.Empty(t, ExtractUserTags("#"+withNUL), "user.tags")
	assert.Empty(t, ExtractNoteTags("#"+withNUL), "MFM 抽出経路")

	// NormalizeNoteTags は抽出済みの名前を受け取る (# は付かない)。
	require.NotEmpty(t, NormalizeNoteTags([]string{"ok"}), "対照")
	assert.Empty(t, NormalizeNoteTags([]string{withNUL}), "note.tags")
}

// TestExtract_ParsesOnlyTheFirstMaxParseBytes checks that each fragment is
// cut to MaxParseBytes before parsing, at a UTF-8 boundary (#3514).
func TestExtract_ParsesOnlyTheFirstMaxParseBytes(t *testing.T) {
	head := "#head "
	pad := strings.Repeat("a", MaxParseBytes-len(head)-len(" #edge")-1)
	// #edge は上限の内側で終わる。その直後の 3 バイトの文字が境界をまたぐ
	text := head + pad + " #edge" + "あ" + " #tail"
	require.Greater(t, len(text), MaxParseBytes)
	assert.Equal(t, []string{"head", "edge"}, Extract(text))
	assert.Equal(t, []string{"head", "edge"}, ExtractNoteTags(text))
	// 断片ごとに切るので、2 つ目の断片は 1 つ目の長さに影響されない
	assert.Equal(t, []string{"head", "edge", "cw"}, Extract(text, "#cw"))

	cut := cutForParse([]string{text, "short"})
	assert.True(t, utf8.ValidString(cut[0]))
	assert.LessOrEqual(t, len(cut[0]), MaxParseBytes)
	assert.True(t, strings.HasSuffix(cut[0], " #edge"), "cut at the start of the multibyte rune")
	assert.Equal(t, "short", cut[1])

	// ちょうど上限の長さの断片は切らない (p[MaxParseBytes] を読まない)
	exact := strings.Repeat("a", MaxParseBytes-len(" #last")) + " #last"
	require.Len(t, exact, MaxParseBytes)
	assert.Equal(t, []string{exact}, cutForParse([]string{exact}))
	assert.Equal(t, []string{"last"}, Extract(exact))
	// 1 バイト超える断片は、ちょうど上限の位置で切る
	over := exact[:MaxParseBytes-len("last")] + "ab" + "cd" + "e"
	require.Len(t, over, MaxParseBytes+1)
	assert.Equal(t, []string{"abcd"}, Extract(over))

	// 上限は解析器のメモ表の上限 (16MiB と 64 × 入力長の大きい方) を 16MiB に留める長さにする
	assert.LessOrEqual(t, 64*MaxParseBytes, 16<<20)
	assert.Greater(t, 64*(MaxParseBytes+1), 16<<20)
}
