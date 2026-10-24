package mfm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestKeywordCheckURLs mirrors upstream test/unit/misc/extract-urls-for-keyword-check.ts.
func TestKeywordCheckURLs(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		want     []string // exact result when non-nil
		contains string   // a value the result must contain
		empty    bool
	}{
		{name: "empty text", in: "", empty: true},
		{name: "no URL", in: "hello world", empty: true},
		{name: "plain URL", in: "see https://example.com/path", want: []string{"https://example.com/path", "https://example.com/path"}},
		{name: "percent-encoded host", in: "https://%65xample.com/", contains: "https://example.com/"},
		{name: "percent-encoded host and dot", in: "https://exa%6Dple%2Ecom/", contains: "https://example.com/"},
		{name: "uppercase host", in: "https://EXAMPLE.com/", contains: "https://example.com/"},
		{name: "fullwidth host", in: "<https://ｅｘａｍｐｌｅ．com/>", contains: "https://example.com/"},
		{name: "ideographic full stop", in: "<https://example。com/>", contains: "https://example.com/"},
		{name: "link syntax", in: "[label](https://%65xample.com/)", contains: "https://example.com/"},
		{name: "decoded path query hash", in: "https://example.com/%E7%A6%81%E6%AD%A2?q=%E7%A6%81#%E7%A6%81", contains: "https://example.com/禁止?q=禁#禁"},
		{name: "punycode host", in: "https://xn--r8jz45g.jp/", want: []string{"https://xn--r8jz45g.jp/", "https://例え.jp/"}},
		{name: "port kept", in: "https://%65xample.com:8080/", contains: "https://example.com:8080/"},
		{name: "default port dropped", in: "https://example.com:443/", want: []string{"https://example.com/", "https://example.com/"}},
		{name: "invalid escape kept", in: "https://example.com/%E7%A6", want: []string{"https://example.com/%E7%A6", "https://example.com/%E7%A6"}},
		{name: "mixed valid and invalid escapes", in: "https://example.com/%E7%A6%81%E6%AD%A2/%E7%A6?q=%E7%A6%81%FF%E6%AD%A2#%E7%A6%E7%A6%81", contains: "https://example.com/禁止/%E7%A6?q=禁%FF止#%E7%A6禁"},
		{name: "non-hex escape", in: "https://example.com/%zz%E7%A6%81", contains: "https://example.com/%zz禁"},
		{name: "userinfo dropped in display form", in: "https://u:p@example.com/", want: []string{"https://u:p@example.com/", "https://example.com/"}},
		{name: "empty query and fragment", in: "https://example.com/?#", want: []string{"https://example.com/?#", "https://example.com/"}},
		{name: "beyond stored length", in: strings.Repeat("a", keywordCheckMaxUTF16) + " https://%65xample.com/", empty: true},
		{name: "IPv6 host", in: "https://[::1]/", want: []string{"https://[::1]/", "https://[::1]/"}},
		// 拡張部分が空の ACE ラベルは基本文字だけに decode される (Node の new URL + domainToUnicode と同じ)
		{name: "empty-extension punycode label", in: "https://xn--badword-.com/", want: []string{"https://xn--badword-.com/", "https://badword.com/"}},
		{name: "empty-extension punycode label uppercase", in: "https://XN--BadWord-.com/", want: []string{"https://xn--badword-.com/", "https://badword.com/"}},
		{name: "empty-extension punycode label with hyphen", in: "https://xn--a-b-.com/", want: []string{"https://xn--a-b-.com/", "https://a-b.com/"}},
		{name: "empty punycode label", in: "https://xn---.com/", empty: true},
		// 和文・全角・半角の句点で区切っても、Node と同じく `.` として読む。句点は
		// URL の文字ではないので、MFM では山括弧で囲んだ形 (リンクの中も同じ) で書く
		{name: "empty-extension label before ideographic full stop (angle brackets)", in: "<https://xn--badword-\u3002example/>", want: []string{"https://xn--badword-.example/", "https://badword.example/"}},
		{name: "empty-extension label before ideographic full stop", in: "[x](<https://xn--badword-\u3002example/>)", want: []string{"https://xn--badword-.example/", "https://badword.example/"}},
		{name: "empty-extension label before fullwidth full stop", in: "[x](<https://xn--badword-\uff0eexample/>)", want: []string{"https://xn--badword-.example/", "https://badword.example/"}},
		{name: "empty-extension label before halfwidth full stop", in: "[x](<https://xn--badword-\uff61example/>)", want: []string{"https://xn--badword-.example/", "https://badword.example/"}},
		{name: "bare-dash label before ideographic full stop", in: "<https://xn---r8jz45g\u3002jp/>", want: []string{"https://xn---r8jz45g.jp/", "https://\u4f8b\u3048.jp/"}},
		{name: "bare-dash punycode label", in: "https://xn---r8jz45g.jp/", want: []string{"https://xn---r8jz45g.jp/", "https://例え.jp/"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := KeywordCheckURLs(tc.in)
			switch {
			case tc.empty:
				assert.Empty(t, got)
			case tc.want != nil:
				assert.Equal(t, tc.want, got)
			default:
				assert.Contains(t, got, tc.contains)
			}
		})
	}
}

// TestKeywordCheckURLs_StoredLengthBoundary pins the cut at the stored length:
// a URL that ends exactly at the boundary is read, one past it is not.
func TestKeywordCheckURLs_StoredLengthBoundary(t *testing.T) {
	url := "https://%65xample.com/"
	inside := strings.Repeat("a", keywordCheckMaxUTF16-len(url)-1) + " " + url
	assert.Contains(t, KeywordCheckURLs(inside), "https://example.com/")

	// BMP 外の文字は UTF-16 で 2 単位に数える
	astral := strings.Repeat("\U0001F600", keywordCheckMaxUTF16/2) + " " + url
	assert.Empty(t, KeywordCheckURLs(astral))
}

func TestKeywordCheckTarget(t *testing.T) {
	assert.Equal(t, "no urls here", KeywordCheckTarget("no urls here"))
	// href の時点でホストのパーセントエンコードは解かれる (WHATWG URL の host parser)
	assert.Equal(t, "x https://%65xample.com/\nhttps://example.com/\nhttps://example.com/",
		KeywordCheckTarget("x https://%65xample.com/"))
}

// TestSafeDecodeURIComponent mirrors upstream test/unit/misc/safe-decode-uri-component.ts.
func TestSafeDecodeURIComponent(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/%E7%A6%81%E6%AD%A2", "/禁止"},
		{"%F0%9F%98%80", "\U0001F600"},
		{"%E7%A6", "%E7%A6"},
		{"%zz", "%zz"},
		{"%E7%A6%81%FF%E6%AD%A2", "禁%FF止"},
		{"%E7%A6%E7%A6%81", "%E7%A6禁"},
		{"%zz%E7%A6%81", "%zz禁"},
		{"plain", "plain"},
		{"%e7%a6%81", "禁"},
		{"%EF%BF%BD", "�"},
		// 冗長表現とサロゲートは decodeURIComponent と同じく不正として残す
		{"%C0%AF", "%C0%AF"},
		{"%ED%A0%80", "%ED%A0%80"},
		{"%2", "%2"},
		{"a%", "a%"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, safeDecodeURIComponent(tc.in), tc.in)
	}
}

func TestEmptyExtensionPunycodeBasic(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"xn--abc-", "abc", true},
		{"XN--AbC-", "abc", true},
		{"xn--a-b-", "a-b", true},
		{"xn---", "", true},
		{"xn--abc", "", false},
		{"xn--ab_c-", "", false},
		{"xn--\u00e9-", "", false},
		{"abc-", "", false},
		{"xn--", "", false},
	} {
		got, ok := emptyExtensionPunycodeBasic(tc.in)
		assert.Equal(t, tc.ok, ok, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
}

func TestClipUTF16(t *testing.T) {
	assert.Equal(t, "ab", clipUTF16("abc", 2))
	assert.Equal(t, "abc", clipUTF16("abc", 3))
	// サロゲートペアの途中では切らない
	assert.Equal(t, "a", clipUTF16("a\U0001F600", 2))
	assert.Equal(t, "a\U0001F600", clipUTF16("a\U0001F600", 3))
}
