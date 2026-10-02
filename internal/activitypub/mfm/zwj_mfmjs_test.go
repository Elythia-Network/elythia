package mfm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParse_ZWJMatchesMfmJs fixes how a zero width joiner (U+200D) outside an
// emoji sequence is parsed, matching mfm-js 0.26.0 (#3322). Expected trees
// were produced by running mfm-js's parse on each input (serializeTree is in
// mention_mfmjs_test.go).
//
// ZWJ は絵文字どうしをつなぐ文字で、並びの先頭にはならない。以前の mk-go は単独の
// ZWJ を絵文字として読んでいた。
func TestParse_ZWJMatchesMfmJs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\u200d", "text:\u200d"},
		{"a\u200d", "text:a\u200d"},
		{"\u200db", "text:\u200db"},
		{"#\ufe0f\u20e3\u200d", "unicodeEmoji:#\ufe0f\u20e3|text:\u200d"},
		{"1\u20e3\u200d1\u20e3", "unicodeEmoji:1\u20e3|text:\u200d|unicodeEmoji:1\u20e3"},
		{"x\u200d😀", "text:x\u200d|unicodeEmoji:😀"},
		{"😀\u200d", "unicodeEmoji:😀|text:\u200d"},
		{"\u200d\u200d", "text:\u200d\u200d"},
		{"あ\u200dい", "text:あ\u200dい"},
		{"\u200d#tag", "text:\u200d|hashtag:tag"},
		{"#a\u200db", "hashtag:a\u200db"},
		{"👨\u200d💻", "unicodeEmoji:👨\u200d💻"},
		{"🏳\ufe0f\u200d🌈", "unicodeEmoji:🏳\ufe0f\u200d🌈"},
		{"👩\u200d❤\ufe0f\u200d👨", "unicodeEmoji:👩\u200d❤\ufe0f\u200d👨"},
		{"**\u200d**", "bold[text:\u200d]"},
		{"<b>\u200d</b>", "bold[text:\u200d]"},
		{":a:\u200d", "emojiCode:a|text:\u200d"},
		{"@a\u200d", "mention:@a|text:\u200d"},
		{"https://e.x/\u200d", "url:https://e.x/|text:\u200d"},
		{"😀\u200d\u200d😀", "unicodeEmoji:😀|text:\u200d\u200d|unicodeEmoji:😀"},
		{"😀\u200dx", "unicodeEmoji:😀|text:\u200dx"},
		{"😀\u200d#tag", "unicodeEmoji:😀|text:\u200d|hashtag:tag"},
		{"👨\u200d💻\u200d", "unicodeEmoji:👨\u200d💻|text:\u200d"},
		{"\u200d😀", "text:\u200d|unicodeEmoji:😀"},
		{"😀\u200d#\ufe0f\u20e3", "unicodeEmoji:😀|text:\u200d|unicodeEmoji:#\ufe0f\u20e3"},
		{"😀\u200d1\u20e3", "unicodeEmoji:😀|text:\u200d|unicodeEmoji:1\u20e3"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, serializeTree(Parse(tc.in)))
		})
	}
}
