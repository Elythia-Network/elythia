package federation

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// 添付の name は URL の basename から作る (#2723)。
//
// upstream の `uploadFromUrl` が `urlObj.pathname.split('/').pop()` で決めるのと
// 同じ置き場にする。AP の `name` は代替テキストなので `comment` 側に入る。
func TestAttachmentFileName(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"basename", "https://media.example/files/cat.png", "cat.png"},
		{"query is not part of the name", "https://m.example/a/b.jpg?x=1&y=2#z", "b.jpg"},
		{"no extension", "https://m.example/media/0a1b2c", "0a1b2c"},
		// percent-encoding は解かない。解くと %2F が / になり、upstream が弾く
		// 名前を作ってしまう。
		{"percent encoding is kept", "https://m.example/a/%E7%8C%AB.png", "%E7%8C%AB.png"},
		{"encoded slash does not split", "https://m.example/a%2Fb.png", "a%2Fb.png"},
		// ValidateFileName に落ちるものは upstream と同じく untitled。
		{"root path", "https://m.example/", "untitled"},
		{"empty path", "https://m.example", "untitled"},
		{"dot dot", "https://m.example/a/..", "untitled"},
		// 末尾スラッシュは upstream だと空 segment になる。path.Base は手前の
		// segment を返してしまうので、そこを固定する。
		{"trailing slash", "https://m.example/a/b/", "untitled"},
		{"unparseable", "https://m.example/\x7f", "untitled"},
		{"too long", "https://m.example/" + strings.Repeat("a", 201), "untitled"},
		{"exactly 200", "https://m.example/" + strings.Repeat("a", 200), strings.Repeat("a", 200)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, attachmentFileName(tc.url))
		})
	}
}

// 列 (varchar(256)) に必ず収まること。ValidateFileName が UTF-16 で 200 単位
// (rune でも 200 以下) で止めるので
// 実際には超えないが、上限の出どころを 1 つにしてある。
func TestAttachmentFileName_FitsColumn(t *testing.T) {
	for _, raw := range []string{
		// ValidateFileName を通る最長 (ASCII で 200 文字)。**percent-encoding 後の長さで
		// 数える**ので、非 ASCII は 1 文字 9 rune (%E3%81%82) に膨らむ。
		"https://m.example/" + strings.Repeat("a", 200),
		// 通らない側 (fallback に落ちる)。
		"https://m.example/" + strings.Repeat("a", 201),
		"https://m.example/" + strings.Repeat("あ", 199) + ".png",
		"https://m.example/",
	} {
		got := attachmentFileName(raw)
		assert.NotEmpty(t, got, "NOT NULL 列に空を書こうとしている")
		assert.LessOrEqual(t, len([]rune(got)), driveFileNameMaxRunes, raw)
	}
}
