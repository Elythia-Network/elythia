package mfm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFromHTML(t *testing.T) {
	tests := []struct {
		name string
		html string
		want string
	}{
		// --- TS本家テストケースと同等 ---
		{
			name: "p",
			html: "<p>a</p><p>b</p>",
			want: "a\n\nb",
		},
		{
			name: "block element",
			html: "<div>a</div><div>b</div>",
			want: "a\nb",
		},
		{
			name: "inline element (li)",
			html: "<ul><li>a</li><li>b</li></ul>",
			want: "a\nb",
		},
		{
			name: "block code",
			html: "<pre><code>a\nb</code></pre>",
			want: "```\na\nb\n```",
		},
		{
			name: "inline code",
			html: "<code>a</code>",
			want: "`a`",
		},
		{
			name: "quote",
			html: "<blockquote>a\nb</blockquote>",
			want: "> a\n> b",
		},
		{
			name: "br",
			html: "<p>abc<br><br/>d</p>",
			want: "abc\n\nd",
		},
		{
			name: "link with different text",
			html: `<p>a <a href="https://example.com/b">c</a> d</p>`,
			want: "a [c](https://example.com/b) d",
		},
		{
			name: "link with same text",
			html: `<p>a <a href="https://example.com/b">https://example.com/b</a> d</p>`,
			want: "a https://example.com/b d",
		},
		{
			name: "link with no url",
			html: `<p>a <a href="b">c</a> d</p>`,
			want: "a [c](b) d",
		},
		{
			name: "link without href",
			html: `<p>a <a>c</a> d</p>`,
			want: "a c d",
		},
		{
			name: "link without text",
			html: `<p>a <a href="https://example.com/b"></a> d</p>`,
			want: "a https://example.com/b d",
		},
		{
			name: "link without both",
			html: `<p>a <a></a> d</p>`,
			want: "a  d",
		},
		{
			name: "link with non-ascii url, different text",
			html: `<p>a <a href="https://example.com/ä">c</a> d</p>`,
			want: "a [c](<https://example.com/ä>) d",
		},
		{
			name: "link with non-ascii url, same text",
			html: `<p>a <a href="https://example.com/ä">https://example.com/ä</a> d</p>`,
			want: "a <https://example.com/ä> d",
		},
		{
			// 本家の urlRegexFull は `!` を含むので、囲まずにそのまま書く (#3314)。
			name: "link with ! in url, same text",
			html: `<p>a <a href="https://example.com/a!b">https://example.com/a!b</a> d</p>`,
			want: "a https://example.com/a!b d",
		},
		{
			name: "link with ! in url, different text",
			html: `<p>a <a href="https://example.com/a!b">c</a> d</p>`,
			want: "a [c](https://example.com/a!b) d",
		},
		{
			name: "link with ! in url, without text",
			html: `<p>a <a href="https://example.com/a!b"></a> d</p>`,
			want: "a https://example.com/a!b d",
		},
		{
			name: "mention",
			html: `<p>a <a href="https://example.com/@user" class="u-url mention">@user</a> d</p>`,
			want: "a @user@example.com d",
		},
		{
			name: "hashtag",
			html: `<p>a <a href="https://example.com/tags/a">#a</a> d</p>`,
			want: "a #a d",
		},
		{
			name: "bold",
			html: "<b>bold</b>",
			want: "**bold**",
		},
		{
			name: "strong",
			html: "<strong>strong</strong>",
			want: "**strong**",
		},
		{
			name: "italic",
			html: "<i>italic</i>",
			want: "<i>italic</i>",
		},
		{
			name: "em",
			html: "<em>emphasis</em>",
			want: "<i>emphasis</i>",
		},
		{
			name: "strikethrough s",
			html: "<s>deleted</s>",
			want: "~~deleted~~",
		},
		{
			name: "strikethrough del",
			html: "<del>deleted</del>",
			want: "~~deleted~~",
		},
		{
			name: "small",
			html: "<small>small text</small>",
			want: "<small>small text</small>",
		},
		{
			name: "h1",
			html: "<h1>heading</h1>",
			want: "【heading】",
		},
		// --- Mastodon固有マークアップ ---
		{
			name: "Mastodon link with invisible/ellipsis spans",
			html: `<a href="https://example.com/very/long/path"><span class="invisible">https://</span><span class="ellipsis">example.com/very/lon</span><span class="">g/path</span></a>`,
			want: "https://example.com/very/long/path",
		},
		{
			name: "Mastodon paragraph with HTML",
			html: `<p>テキスト本文<br />2行目</p>`,
			want: "テキスト本文\n2行目",
		},
		{
			name: "Mastodon mention with h-card span",
			html: `<span class="h-card"><a href="https://remote.example/@user" class="u-url mention">@<span>user</span></a></span>`,
			want: "@user@remote.example",
		},
		{
			name: "Pixelfed br+newline normalization",
			html: "<p>line1<br>\nline2</p>",
			want: "line1\nline2",
		},
		// --- エッジケース ---
		{
			name: "empty string",
			html: "",
			want: "",
		},
		{
			name: "plain text only",
			html: "hello world",
			want: "hello world",
		},
		{
			name: "nested formatting",
			html: "<p><b>bold <i>and italic</i></b></p>",
			want: "**bold <i>and italic</i>**",
		},
		{
			name: "multiple paragraphs with links",
			html: `<p>Check <a href="https://example.com">example.com</a></p><p>Done.</p>`,
			want: "Check [example.com](https://example.com)\n\nDone.",
		},
		// --- #362 回帰テスト: drop-in 互換報告で DB に残っていた実データ ---
		{
			name: "#362 p class=quote-inline with RE: prefix + mastodon ellipsis link",
			html: `<p class="quote-inline">RE: <a href="https://example.com/notes/x" target="_blank" rel="nofollow noopener" translate="no"><span class="invisible">https://</span><span class="ellipsis">example.com/notes</span><span class="invisible">/x</span></a></p><p>replied body</p>`,
			want: "RE: https://example.com/notes/x\n\nreplied body",
		},
		{
			name: "#362 h-card mention followed by body text",
			html: `<p><span class="h-card"><a class="u-url mention" href="https://remote.example/@alice">@<span>alice</span></a></span> hello body</p>`,
			want: "@alice@remote.example hello body",
		},
		{
			name: "#362 consecutive <br /> in paragraph",
			html: `<p>line1<br />line2<br />line3</p>`,
			want: "line1\nline2\nline3",
		},
		{
			name: "#362 <span> only wrapper inside <p>",
			html: `<p><span>just text in span</span></p>`,
			want: "just text in span",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromHTML(tt.html)
			assert.Equal(t, tt.want, got)
		})
	}
}
