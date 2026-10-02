package mfm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParse_KeycapEmojiMatchesMfmJs fixes keycap emoji parsing to mfm-js
// 0.26.0 (#3320). Expected trees were produced by running mfm-js's parse on
// each input (serializeTree is in mention_mfmjs_test.go).
//
// キーキャップは `#` `*` `0`〜`9` + 任意の U+FE0F + U+20E3 の形だけが 1 つの絵文字に
// なる。以前の mk-go は先頭の ASCII を絵文字に含めず、`#️⃣` はハッシュタグになった。
func TestParse_KeycapEmojiMatchesMfmJs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"#\u20e3", "unicodeEmoji:#\u20e3"},
		{"#\ufe0f\u20e3", "unicodeEmoji:#\ufe0f\u20e3"},
		{"*\u20e3", "unicodeEmoji:*\u20e3"},
		{"*\ufe0f\u20e3", "unicodeEmoji:*\ufe0f\u20e3"},
		{"0\ufe0f\u20e3", "unicodeEmoji:0\ufe0f\u20e3"},
		{"1\ufe0f\u20e3", "unicodeEmoji:1\ufe0f\u20e3"},
		{"2\ufe0f\u20e3", "unicodeEmoji:2\ufe0f\u20e3"},
		{"3\ufe0f\u20e3", "unicodeEmoji:3\ufe0f\u20e3"},
		{"4\ufe0f\u20e3", "unicodeEmoji:4\ufe0f\u20e3"},
		{"5\ufe0f\u20e3", "unicodeEmoji:5\ufe0f\u20e3"},
		{"6\ufe0f\u20e3", "unicodeEmoji:6\ufe0f\u20e3"},
		{"7\ufe0f\u20e3", "unicodeEmoji:7\ufe0f\u20e3"},
		{"8\ufe0f\u20e3", "unicodeEmoji:8\ufe0f\u20e3"},
		{"9\ufe0f\u20e3", "unicodeEmoji:9\ufe0f\u20e3"},
		{"5\u20e3", "unicodeEmoji:5\u20e3"},
		{"a#\ufe0f\u20e3", "text:a|unicodeEmoji:#\ufe0f\u20e3"},
		{"#\ufe0f\u20e3x", "unicodeEmoji:#\ufe0f\u20e3|text:x"},
		{"#\ufe0f\u20e3 #tag", "unicodeEmoji:#\ufe0f\u20e3|text: |hashtag:tag"},
		{"12\u20e3", "text:1|unicodeEmoji:2\u20e3"},
		{"1\u20e3\ufe0f", "unicodeEmoji:1\u20e3|text:\ufe0f"},
		{"a\u20e3", "text:a\u20e3"},
		{"\u20e3", "text:\u20e3"},
		{"\u20e3x", "text:\u20e3x"},
		{"\ufe0f\u20e3", "text:\ufe0f\u20e3"},
		{"a\ufe0f\u20e3", "text:a\ufe0f\u20e3"},
		{"\ufe0f", "text:\ufe0f"},
		{"a\ufe0e", "text:a\ufe0e"},
		{"\u2764\ufe0f", "unicodeEmoji:\u2764\ufe0f"},
		{"\u263a\ufe0f", "unicodeEmoji:\u263a\ufe0f"},
		{"\u00a9\ufe0f", "unicodeEmoji:\u00a9\ufe0f"},
		{"#\ufe0f", "hashtag:\ufe0f"},
		{"1\ufe0f", "text:1\ufe0f"},
		{"#\ufe0e\u20e3", "hashtag:\ufe0e\u20e3"},
		{"#\ufe0f\ufe0f\u20e3", "hashtag:\ufe0f\ufe0f\u20e3"},
		{"x #\ufe0f\u20e3 y", "text:x |unicodeEmoji:#\ufe0f\u20e3|text: y"},
		{"<b>#\ufe0f\u20e3</b>", "bold[unicodeEmoji:#\ufe0f\u20e3]"},
		{"**1\ufe0f\u20e3**", "bold[unicodeEmoji:1\ufe0f\u20e3]"},
		{"[#\ufe0f\u20e3](https://e.x)", "link:https://e.x[unicodeEmoji:#\ufe0f\u20e3]"},
		{"#tag1\ufe0f\u20e3", "hashtag:tag1\ufe0f\u20e3"},
		{"@a#\ufe0f\u20e3", "mention:@a|unicodeEmoji:#\ufe0f\u20e3"},
		{":a:#\ufe0f\u20e3", "emojiCode:a|unicodeEmoji:#\ufe0f\u20e3"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, serializeTree(Parse(tc.in)))
		})
	}
}
