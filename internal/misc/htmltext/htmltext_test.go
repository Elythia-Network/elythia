package htmltext

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The expected values are the output of upstream's htmlToPlainText
// (sanitize-html 2.18.1) for the same inputs.
func TestPlainText(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain", "hello world", "hello world"},
		{"separators keep word boundaries", "<p>one</p><p>two</p>three<br>four<div>five</div>", "one two three four five"},
		{"inline tags are removed, text kept", "<b>bold</b> and <a href=\"https://example.com\">link</a>", "bold and link"},
		{"non-text tags are dropped with contents", "a<script>alert(1)</script>b<style>p{}</style>c<template>t</template>d", "abcd"},
		{"entities are decoded", "Tom &amp; Jerry &lt;3 &gt; &quot;x&quot;", "Tom & Jerry <3 > \"x\""},
		{"escaped br is a separator", "a&lt;br&gt;b&lt;BR /&gt;c", "a b c"},
		{"white space is collapsed and trimmed", "  a \n\t b\u00a0\u00a0c\uFEFF ", "a b c"},
		{"only tags", "<p></p><br>", ""},
		{"nested non-text", "x<textarea><b>y</b></textarea>z", "xz"},
		{"option", "a<option>o</option>b", "ab"},
		{"list items", "<ul><li>1</li><li>2</li></ul>", "1 2"},
		{"nbsp and numeric references", "a&nbsp;b &#39;q&#39;", "a b 'q'"},
		{"comments", "<!-- c -->x", "x"},
		{"unclosed paragraphs", "<p>a<p>b", "a b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, PlainText(tc.in))
		})
	}
}
