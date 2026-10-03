package mfm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestToHTMLWithMentions_MatchesUpstream fixes mention hrefs to upstream
// MfmService.toHtml(nodes, mentionedRemoteUsers) (#3329). Expected HTML was
// produced by running mfm-js 0.26.0's parse and a copy of MfmService.toHtml on
// Node with config.url = https://example.com and the same mentionedRemoteUsers.
//
// 本家は username と host を toLowerCase して一致する最初の要素の url (空なら uri)
// をリンク先にし、見つからなければ `${config.url}/${acct}` にする。
func TestToHTMLWithMentions_MatchesUpstream(t *testing.T) {
	str := func(s string) *string { return &s }
	bob := func(url *string) []MentionedRemoteUser {
		return []MentionedRemoteUser{{URI: "https://remote.example/users/1", URL: url, Username: "bob", Host: str("remote.example")}}
	}
	cases := []struct {
		name      string
		in        string
		mentioned []MentionedRemoteUser
		want      string
	}{
		{"local", "@alice", nil, `<a href="https://example.com/@alice" class="u-url mention">@alice</a>`},
		{"local keeps case", "@Alice", nil, `<a href="https://example.com/@Alice" class="u-url mention">@Alice</a>`},
		{"unknown remote links to local acct page", "@bob@REMOTE.Example", nil, `<a href="https://example.com/@bob@REMOTE.Example" class="u-url mention">@bob@REMOTE.Example</a>`},
		{"url", "@bob@remote.example", bob(str("https://remote.example/@bob")), `<a href="https://remote.example/@bob" class="u-url mention">@bob@remote.example</a>`},
		{"case-insensitive match", "@BOB@Remote.EXAMPLE", bob(str("https://remote.example/@bob")), `<a href="https://remote.example/@bob" class="u-url mention">@BOB@Remote.EXAMPLE</a>`},
		{"no url falls back to uri", "@bob@remote.example", bob(nil), `<a href="https://remote.example/users/1" class="u-url mention">@bob@remote.example</a>`},
		{"empty url falls back to uri", "@bob@remote.example", bob(str("")), `<a href="https://remote.example/users/1" class="u-url mention">@bob@remote.example</a>`},
		{"other host", "@bob@other.example", bob(str("https://remote.example/@bob")), `<a href="https://example.com/@bob@other.example" class="u-url mention">@bob@other.example</a>`},
		{"local mention does not match remote user", "@bob", bob(str("https://remote.example/@bob")), `<a href="https://example.com/@bob" class="u-url mention">@bob</a>`},
		{"null host matches local mention", "@bob", []MentionedRemoteUser{{URI: "https://remote.example/users/1", URL: str("https://remote.example/@bob"), Username: "bob"}}, `<a href="https://remote.example/@bob" class="u-url mention">@bob</a>`},
		{"null host does not match remote mention", "@bob@remote.example", []MentionedRemoteUser{{URI: "https://remote.example/users/1", URL: str("https://remote.example/@bob"), Username: "bob"}}, `<a href="https://example.com/@bob@remote.example" class="u-url mention">@bob@remote.example</a>`},
		{"empty host does not match local mention", "@bob", []MentionedRemoteUser{{URI: "https://remote.example/users/1", URL: str("https://remote.example/@bob"), Username: "bob", Host: str("")}}, `<a href="https://example.com/@bob" class="u-url mention">@bob</a>`},
		{"long s in username does not match s", "@sam@remote.example", []MentionedRemoteUser{{URI: "https://x/1", URL: str("https://remote.example/@sam"), Username: "ſam", Host: str("remote.example")}}, `<a href="https://example.com/@sam@remote.example" class="u-url mention">@sam@remote.example</a>`},
		{"long s in host does not match s", "@sam@s.example", []MentionedRemoteUser{{URI: "https://x/1", URL: str("https://s.example/@sam"), Username: "sam", Host: str("ſ.example")}}, `<a href="https://example.com/@sam@s.example" class="u-url mention">@sam@s.example</a>`},
		{"first match wins", "@bob@remote.example", []MentionedRemoteUser{
			{URI: "https://x/1", URL: str("https://first.example/"), Username: "bob", Host: str("remote.example")},
			{URI: "https://x/2", URL: str("https://second.example/"), Username: "BOB", Host: str("remote.example")},
		}, `<a href="https://first.example/" class="u-url mention">@bob@remote.example</a>`},
		{"url is normalized and escaped", "@bob@remote.example", bob(str(`HTTPS://Remote.Example:443/a/../@b"o'b<>`)), `<a href="https://remote.example/@b%22o&#039;b%3C%3E" class="u-url mention">@bob@remote.example</a>`},
		{"unparsable url is text", "@bob@remote.example", bob(str("not a url")), `@bob@remote.example`},
		{"unparsable uri is text", "@bob@remote.example", []MentionedRemoteUser{{URI: "not a url", Username: "bob", Host: str("remote.example")}}, `@bob@remote.example`},
		{"nested", "**@bob@remote.example** and @carol", bob(str("https://remote.example/@bob")), `<b><a href="https://remote.example/@bob" class="u-url mention">@bob@remote.example</a></b> and <a href="https://example.com/@carol" class="u-url mention">@carol</a>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ToHTMLWithMentions(Parse(tc.in), testHost, tc.mentioned))
		})
	}
}

// TestToHTMLWithMentions_NonHTTPURLIsText checks that a mentioned user's url
// with a scheme other than http / https is not linked.
//
// 本家は new URL() が読めれば scheme を見ずにリンクにする。url はリモートの
// actor が送ってきた値なので、mk-go は javascript: などを href に書かない
// (docs/divergence.md)。本家も取り込むときに url を http(s) に限っているので、
// 通常は差が出ない。
func TestToHTMLWithMentions_NonHTTPURLIsText(t *testing.T) {
	js := "javascript:alert(1)"
	host := "remote.example"
	mentioned := []MentionedRemoteUser{{URI: "https://remote.example/users/1", URL: &js, Username: "bob", Host: &host}}
	assert.Equal(t, "@bob@remote.example", ToHTMLWithMentions(Parse("@bob@remote.example"), testHost, mentioned))
}

// TestToHTML_IgnoresMentionedUsers checks that ToHTML links every mention to
// this server, like upstream calls without mentionedRemoteUsers.
func TestToHTML_IgnoresMentionedUsers(t *testing.T) {
	assert.Equal(t, `<a href="https://example.com/@bob@remote.example" class="u-url mention">@bob@remote.example</a>`,
		ToHTML(Parse("@bob@remote.example"), testHost))
	assert.Empty(t, ToHTMLWithMentions(nil, testHost, nil))
}

// TestParseMentionedRemoteUsers decodes the column as upstream writes it.
func TestParseMentionedRemoteUsers(t *testing.T) {
	got := ParseMentionedRemoteUsers(`[{"uri":"https://remote.example/users/1","url":"https://remote.example/@bob","username":"bob","host":"remote.example"},{"uri":"https://r.example/u/2","username":"c","host":"r.example"}]`)
	require.Len(t, got, 2)
	assert.Equal(t, "https://remote.example/users/1", got[0].URI)
	require.NotNil(t, got[0].URL)
	assert.Equal(t, "https://remote.example/@bob", *got[0].URL)
	assert.Equal(t, "bob", got[0].Username)
	require.NotNil(t, got[0].Host)
	assert.Equal(t, "remote.example", *got[0].Host)
	assert.Nil(t, got[1].URL)

	assert.Nil(t, ParseMentionedRemoteUsers(""))
	assert.Nil(t, ParseMentionedRemoteUsers("["))
	assert.Empty(t, ParseMentionedRemoteUsers("[]"))
}
