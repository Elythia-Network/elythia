package wordmute

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Expected outcomes of one hard-mute regex on one text.
const (
	jsHit  = "hit"  // evaluated, and the frontend's RegExp matches too
	jsMiss = "miss" // evaluated, and the frontend's RegExp does not match either
	noEval = "skip" // the server does not evaluate the rule on this text
)

// evaluatedOn reports whether the server evaluates the single rule of rules on text.
func evaluatedOn(rules []byte, text string) bool {
	rs := parse(rules)
	if rs == nil || len(rs.rules) != 1 {
		return false
	}
	r, ok := rs.rules[0].(regexRule)
	return ok && r.guard&textFeatures(text) == 0
}

// #3369: サーバーが正規表現を判定するのは、JavaScript (frontend の
// `new RegExp(pattern, flags)`) と Go で test() の結果が同じになるときだけ。
// 判定したら frontend と同じ結果になり、そうでなければ判定しない (隠さない)。
// 期待値の hit / miss は node 22 の `new RegExp(p, f).test(text.trim())` の結果。
// 普通の規則 (ASCII の語、`^` `$`、`|`、グループ、`[a-z]`、`\d`、`\b` など) は
// 判定し続けることも、この表で固定する。accepted は i/update の受け付けで、
// この修正の前後で変わらない。
func TestMatch_EvaluatesOnlyWhatJavaScriptReadsTheSame(t *testing.T) {
	cases := []struct {
		name, pattern, flags, text string
		want                       string
		accepted                   bool
	}{
		{"\\p{Han} without u is the letters p{Han}", `\p{Han}`, "", "漢字", noEval, true},
		{"\\p{Han} without u matches p{Han}", `\p{Han}`, "", "p{Han}", noEval, true},
		{"\\p{Han} with u is a syntax error", `\p{Han}`, "u", "漢字", noEval, true},
		{"\\p{Script=Han} with u", `\p{Script=Han}`, "u", "漢字", noEval, false},
		{"\\p{sc=Han} with u", `\p{sc=Han}`, "u", "x", noEval, false},
		{"\\pL without u is pL", `\pL`, "", "abc", noEval, true},
		{"\\pL without u matches pL", `\pL`, "", "a pL", noEval, true},
		{"\\pL with u is a syntax error", `\pL`, "u", "abc", noEval, true},
		{"\\p{L} with u", `\p{L}`, "u", "abc", noEval, true},
		{"\\P{L} with u", `^\P{L}+$`, "u", "abc", noEval, true},
		{"\\p{Lu} with iu matches lower case", `\p{Lu}`, "iu", "a", noEval, true},
		{"\\p{C} differs (unassigned)", `\p{C}`, "u", "x", noEval, true},
		{"(?i) is a syntax error", `(?i)foo`, "", "FOO", noEval, true},
		{"(?i:) is not read by every browser", `(?i:foo)`, "", "FOO", noEval, true},
		{"(?P<n>) is a syntax error", `(?P<n>x)`, "", "x", noEval, true},
		{"named group", `(?<n>x)`, "", "x", jsHit, true},
		{"duplicate group name", `(?<n>x)|(?<n>y)`, "", "x", noEval, true},
		{"[[:alpha:]] is a class of [:alph then ]", `[[:alpha:]]`, "", "a", noEval, true},
		{"[[:alpha:]] matches a]", `[[:alpha:]]`, "", "a]", noEval, true},
		{"\\Q...\\E are the letters Q and E", `\Qa.b\E`, "", "a.b", noEval, true},
		{"\\Q...\\E matches Qa-bE", `\Qa.b\E`, "", "Qa-bE", noEval, true},
		{"\\A is A", `\Ax`, "", "x", noEval, true},
		{"\\A matches Ax", `\Ax`, "", "Ax", noEval, true},
		{"\\z is z", `x\z`, "", "x", noEval, true},
		{"\\z matches xz", `x\z`, "", "xz", noEval, true},
		{"\\z with u is a syntax error", `x\z`, "u", "x", noEval, true},
		{"\\C is C", `\C`, "", "C", noEval, false},
		{"\\k without named groups is k", `\k`, "", "k", noEval, false},
		{"\\k<n> is a back reference", `(?<n>a)\k<n>`, "", "aa", noEval, false},
		{"back reference", `(a)\1`, "", "aa", noEval, false},
		{"lookahead", `(?=a)a`, "", "a", noEval, false},
		{"lookbehind", `(?<=a)b`, "", "ab", noEval, false},
		{"dot without u is one code unit", `^.$`, "", "😀", noEval, true},
		{"dot with u is one code point", `^.$`, "u", "😀", jsHit, true},
		{"dot without u does not cover an emoji", `a.b`, "", "a😀b", noEval, true},
		{"two dots without u cover an emoji", `a..b`, "", "a😀b", noEval, true},
		{"quantifier without u applies to the low surrogate", `😀{2}`, "", "😀😀", noEval, true},
		{"quantifier with u applies to the emoji", `😀{2}`, "u", "😀😀", jsHit, true},
		{"class without u holds both surrogates", `[😀]`, "", "😁", noEval, true},
		{"class with u holds the emoji", `[😀]`, "u", "😁", jsMiss, true},
		{"negated class without u matches a half", `[^😀]`, "", "😁", noEval, true},
		{"negated class without u at the start", `^[^😀]`, "", "😁", noEval, true},
		{"lone surrogate escape without u", `\uD83D`, "", "😀", noEval, false},
		{"lone surrogate escape with u", `\uD83D`, "u", "😀", noEval, false},
		{"surrogate pair escape with u", `^\uD83D\uDE00$`, "u", "😀", noEval, false},
		{"\\u{} with u", `\u{1F600}`, "u", "a😀", noEval, false},
		{"\\W without u matches a half", `^\W$`, "", "😀", noEval, true},
		{"\\W\\W without u covers an emoji", `^\W\W$`, "", "😀", noEval, true},
		{"\\s matches U+3000", `a\sb`, "", "a\u3000b", noEval, true},
		{"\\s matches \\v", `a\sb`, "", "a\x0bb", noEval, true},
		{"\\s matches U+FEFF", `a\sb`, "", "a\ufeffb", noEval, true},
		{"\\S does not match U+00A0", `a\Sb`, "", "a\u00a0b", noEval, true},
		{"dot does not match \\r", `a.b`, "", "a\x0db", noEval, true},
		{"dot does not match U+2028", `a.b`, "", "a\u2028b", noEval, true},
		{"dot matches U+0085", `a.b`, "", "a\u0085b", jsHit, true},
		{"dot with s matches \\r", `a.b`, "s", "a\x0db", jsHit, true},
		{"m ^ after \\r", `^b`, "m", "a\x0db", noEval, true},
		{"m ^ after U+2029", `^b`, "m", "a\u2029b", noEval, true},
		{"m ^ not after other", `^b`, "m", "ab", jsMiss, true},
		{"m $ before U+2028", `a$`, "m", "a\u2028b", noEval, true},
		{"m $ at the end", `a$`, "m", "ba", jsHit, true},
		{"m $ in the middle", `a$\n`, "m", "a\x0ab", jsHit, true},
		{"m ^ in the middle", `a\n^b`, "m", "a\x0ab", jsHit, true},
		{"m ^ in a group", `(^b)`, "m", "a\x0ab", jsHit, true},
		{"m ^ in the second alternative", `x|^b`, "m", "a\x0db", noEval, true},
		{"^ without m is the start", `^b`, "", "a\x0ab", jsMiss, true},
		{"$ without m is the end", `a$`, "", "a\x0ab", jsMiss, true},
		{"my ^ stays at the start", `^b`, "my", "a\x0ab", jsMiss, true},
		{"i without u: k does not match the Kelvin sign", `k`, "i", "\u212a", noEval, true},
		{"i with u: k matches the Kelvin sign", `k`, "iu", "\u212a", jsHit, true},
		{"i without u: s does not match long s", `s`, "i", "\u017f", noEval, true},
		{"i without u: sharp s does not match capital sharp s", `ß`, "i", "ẞ", noEval, true},
		{"i with u: sharp s matches capital sharp s", `ß`, "iu", "ẞ", noEval, true},
		{"i without u: final sigma", `σ`, "i", "ς", noEval, true},
		{"i without u: e acute", `é`, "i", "É", noEval, true},
		{"i without u: Angstrom sign stays alone", `å`, "i", "\u212b", noEval, true},
		{"i without u: class range", `^[a-z]+$`, "i", "ABC", jsHit, true},
		{"i without u: negated class", `[^a]`, "i", "A", jsMiss, true},
		{"i with u: negated class excludes the Kelvin sign", `[^k]`, "iu", "\u212a", jsMiss, true},
		{"i with u: \\w matches long s", `\w`, "iu", "\u017f", jsHit, true},
		{"i without u: \\w does not match long s", `\w`, "i", "\u017f", noEval, true},
		{"i with u: \\W does not match long s", `\W`, "iu", "\u017f", jsMiss, true},
		{"i with u: \\b differs", `\bs`, "iu", "\u017fs", noEval, true},
		{"i without u: \\b", `\bs`, "i", "\u017fs", noEval, true},
		{"[^] matches a line break", `a[^]b`, "", "a\x0ab", noEval, false},
		{"[] matches nothing", `[]a]`, "", "xa]", noEval, true},
		{"[\\b] is a backspace", `[\b]`, "", "a\x08b", noEval, false},
		{"class escape in a range", `[\d-z]`, "", "-", noEval, true},
		{"\\u3042", `\u3042`, "", "あ", noEval, false},
		{"\\x41", `\x41`, "", "A", jsHit, true},
		{"\\cA", `\cA`, "", "\x01", noEval, false},
		{"\\0", `a\0`, "", "a\x00", noEval, true},
		{"octal escape", `\01`, "", "\x01", noEval, true},
		{"\\d is ASCII", `\d`, "", "\u0661", jsMiss, true},
		{"\\- outside a class with u", `\-`, "u", "-", noEval, true},
		{"\\- inside a class with u", `[\-]`, "u", "-", jsHit, true},
		{"\\/ ", `a\/b`, "", "a/b", jsHit, true},
		{"a{,2} is literal", `a{,2}`, "", "a{,2}", noEval, true},
		{"{ alone is literal without u", `a{`, "", "a{", noEval, true},
		{"{ alone is a syntax error with u", `a{`, "u", "a{", noEval, true},
		{"] alone is literal without u", `a]`, "", "a]", jsHit, true},
		{"] alone is a syntax error with u", `a]`, "u", "a]", noEval, true},
		{"} alone is literal without u", `a}`, "", "a}", jsHit, true},
		{"{2} without an atom", `{2}`, "", "{2}", noEval, false},
		{"reversed range", `a{2,1}`, "", "aa", noEval, false},
		{"nested quantifier", `a**`, "", "a", noEval, false},
		{"quantifier on an assertion", `^*a`, "", "a", noEval, true},
		{"lazy quantifier", `a+?b`, "", "aab", jsHit, true},
		{"braced range", `^a{2,3}$`, "", "aaaa", jsMiss, true},
		{"open braced range", `^a{2,}$`, "", "aaaa", jsHit, true},
		{"unmatched )", `a)`, "", "a)", noEval, false},
		{"y sticks to the start", `b`, "y", "ab", jsMiss, true},
		{"\\D and \\S", `\D\S`, "", "ab", jsHit, true},
		{"control escapes", `a\f\n\r\t\vb`, "", "a\x0c\x0a\x0d\x09\x0bb", jsHit, true},
		{"\\x with one digit", `\x4`, "", "x4", noEval, false},
		{"\\u with three digits", `\u123`, "", "u123", noEval, false},
		{"\\u{} without u", `\u{41}`, "", "u{41}", noEval, false},
		{"empty \\u{}", `\u{}`, "u", "a", noEval, false},
		{"\\u{} over the maximum", `\u{110000}`, "u", "a", noEval, false},
		{"\\u{} unclosed", `\u{41`, "u", "a", noEval, false},
		{"surrogate escape followed by a letter with u", `\uD83Dx`, "u", "x", noEval, false},
		{"high surrogate followed by a high surrogate with u", `\uD83D\uD83D`, "u", "x", noEval, false},
		{"\\c at the end", `a\c`, "", "a\\c", noEval, false},
		{"trailing backslash", `a\`, "", "a", noEval, false},
		{"\\p{gc=Lu}", `\p{gc=Lu}`, "u", "A", noEval, false},
		{"\\p{General_Category=Nd}", `\p{General_Category=Nd}`, "u", "a", noEval, false},
		{"unknown script", `\p{Script=Nope}`, "u", "a", noEval, false},
		{"unclosed \\p{", `\p{Lu`, "u", "a", noEval, false},
		{"unclosed class", `[a`, "", "[a", noEval, false},
		{"unclosed group", `(a`, "", "(a", noEval, false},
		{"group name starting with a digit", `(?<1a>x)`, "", "x", noEval, true},
		{"empty group name", `(?<>x)`, "", "x", noEval, false},
		{"unclosed group name", `(?<a`, "", "x", noEval, false},
		{"quantifier over four digits", `a{10000}`, "", "a", noEval, false},
		{"quantifier range over four digits", `a{1,10000}`, "", "a", noEval, false},
		{"{n, without }", `a{1,`, "", "a{1,", noEval, true},
		{"{n,x} is literal", `a{1,x}`, "", "a{1,x}", noEval, true},
		{"astral identity escape without u", `\😀`, "", "😀", noEval, false},
		{"astral range in a class without u", `[😀-😁]`, "", "😀", noEval, true},
		{"\\k inside a class without u", `[\k]`, "", "k", noEval, false},
		{"\\cA inside a class", `[\cA]`, "", "\x01", noEval, false},
		{"\\uD83D inside a class without u", `[\uD83D]`, "", "😀", noEval, false},
		{"i with an emoji without u", `😀`, "i", "😀", noEval, true},
		{"y with alternation", `b|a`, "y", "ab", jsHit, true},
		{"plain word", `foo`, "", "a foo b", jsHit, true},
		{"plain word miss", `foo`, "", "a bar b", jsMiss, true},
		{"i flag", `foo`, "i", "a FOO b", jsHit, true},
		{"anchored start", `^bar`, "", "bar baz", jsHit, true},
		{"anchored start miss", `^bar`, "", "foo bar", jsMiss, true},
		{"anchored end", `bar$`, "", "foo bar", jsHit, true},
		{"alternation", `spam|scam`, "", "this is scam", jsHit, true},
		{"non-capturing group", `(?:spam|scam)+`, "i", "SCAMSPAM", jsHit, true},
		{"capturing group", `(ab)+c`, "", "ababc", jsHit, true},
		{"class range", `[a-z]+`, "", "ABC def", jsHit, true},
		{"class range miss", `^[a-z]+$`, "", "ABC", jsMiss, true},
		{"negated class ascii text", `x[^a]y`, "", "xby", jsHit, true},
		{"digits", `\d{3}`, "", "tel 123", jsHit, true},
		{"word boundary", `\bcat\b`, "", "a cat sat", jsHit, true},
		{"word boundary miss", `\bcat\b`, "", "concatenate", jsMiss, true},
		{"escaped dot", `a\.b`, "", "a.b", jsHit, true},
		{"escaped dot miss", `a\.b`, "", "axb", jsMiss, true},
		{"dot on plain text", `foo.*bar`, "", "foo and bar", jsHit, true},
		{"dot with s across lines", `foo.bar`, "s", "foo\x0abar", jsHit, true},
		{"dot without s on newline", `foo.bar`, "", "foo\x0abar", jsMiss, true},
		{"m flag after newline", `^bad`, "m", "ok\x0abad", jsHit, true},
		{"y flag", `foo`, "y", "foo bar", jsHit, true},
		{"japanese literal", `漢字`, "", "漢字です", jsHit, true},
		{"japanese class", `[ぁ-ん]+ですわ`, "", "そうですわ", jsHit, true},
		{"emoji literal", `😀`, "", "hi 😀", jsHit, true},
		{"emoji literal with u", `😀+`, "u", "😀😀", jsHit, true},
		{"i with ASCII and japanese text", `spam`, "i", "これはSPAMです", jsHit, true},
		{"quantifier range", `x{2,3}`, "", "axxb", jsHit, true},
		{"lazy", `a+?b`, "", "aab", jsHit, true},
		{"named group", `(?<w>foo)`, "", "foo", jsHit, true},
		{"hex escape", `\x41`, "", "A", jsHit, true},
		{"slash escape", `a\/b`, "", "a/b", jsHit, true},
		{"dot skipped with emoji in the text", `a.b`, "", "a😀b", noEval, true},
		{"dot evaluated without emoji", `a.b`, "", "axb", jsHit, true},
		{"dot skipped with CR in the text", `a.b`, "", "a\x0db", noEval, true},
		{"dot with u and s on emoji", `a.b`, "us", "a😀b", jsHit, true},
		{"negated class skipped with emoji", `x[^a]y`, "", "x😀y", noEval, true},
		{"\\s skipped with U+3000", `a\sb`, "", "a\u3000b", noEval, true},
		{"\\s evaluated on ASCII space", `a\sb`, "", "a b", jsHit, true},
		{"m skipped with CR", `^b`, "m", "a\x0db", noEval, true},
		{"i without u skipped with Kelvin sign", `k`, "i", "\u212a", noEval, true},
		{"i without u evaluated on k", `k`, "i", "K", jsHit, true},
		{"i with u on Kelvin sign", `k`, "iu", "\u212a", jsHit, true},
		{"\\W without u skipped with emoji", `\W`, "", "😀", noEval, true},
		{"unmatched ) balanced by the y wrapper", `a)(?:b`, "y", "ab", noEval, true},
		{"\\x over 0x7F with i", `\xe5`, "i", "\u212b", noEval, true},
		{"\\x{} is x repeated without u", `\x{41}`, "", "A", noEval, true},
		{"quantifier with a leading zero", `^a{01}$`, "", "a{01}", noEval, true},
		{"quantifier range with a leading zero", `a{1,02}b`, "", "a{1,02}b", noEval, true},
		{"quantifier with a leading zero on a", `a{01}`, "", "a", noEval, true},
		{"quantifier {0}", `^a{0}b$`, "", "b", jsHit, true},
		{"quantifier {10}", `^a{10}$`, "", "aaaaaaaaaa", jsHit, true},
		{"quantifier {1,10}", `^a{1,10}$`, "", "aaa", jsHit, true},
		{"[^] followed by more", `[^]a]`, "", "b", noEval, true},
		{"quantifier after \\b", `\b+foo`, "", "foo", noEval, true},
		{"dot without u on the first supplementary character", `^.$`, "", "\U00010000", noEval, true},
		{"dot without u on the last BMP character", `^.$`, "", "\uffff", jsHit, true},
		{"^ is not after U+0085 (JavaScript trim keeps it)", `^foo`, "", "\u0085foo", jsMiss, true},
		{"$ is not before U+0085", `foo$`, "", "foo\u0085", jsMiss, true},
		{"^ after U+FEFF (JavaScript trim removes it)", `^foo`, "", "\ufefffoo", jsHit, true},
		{"y before U+FEFF at the end", `foo$`, "y", "foo\ufeff", jsHit, true},
		{"class escape followed by a trailing dash", `[\d-]`, "", "-", jsHit, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lit := "/" + tc.pattern + "/" + tc.flags
			rules, err := json.Marshal([]string{lit})
			require.NoError(t, err)
			assert.Equal(t, tc.want != noEval, evaluatedOn(rules, tc.text), "whether %s is evaluated on %q", lit, tc.text)
			assert.Equal(t, tc.want == jsHit, Match(rules, tc.text), "%s on %q", lit, tc.text)
			assert.Equal(t, tc.accepted, ValidateHardMutedWords(rules) == nil, "i/update acceptance of %s", lit)
		})
	}
}

// 本文の文字で判定を外す印 (textGuard) は、その文字があるときだけ効く。
func TestTextFeatures(t *testing.T) {
	cases := []struct {
		text string
		want textGuard
	}{
		{"plain ascii\nand 日本語", 0},
		{"emoji \U0001F600", guardSupplementary},
		{"cr\r", guardLineTerminator},
		{"ls\u2028", guardLineTerminator | guardUnicodeSpace},
		{"ps\u2029", guardLineTerminator | guardUnicodeSpace},
		{"vt\v", guardUnicodeSpace},
		{"nbsp\u00a0", guardUnicodeSpace},
		{"ogham\u1680", guardUnicodeSpace},
		{"en quad\u2000", guardUnicodeSpace},
		{"hair\u200a", guardUnicodeSpace},
		{"zwsp\u200b", 0},
		{"nnbsp\u202f", guardUnicodeSpace},
		{"mmsp\u205f", guardUnicodeSpace},
		{"ideographic\u3000", guardUnicodeSpace},
		{"bom\ufeff", guardUnicodeSpace},
		{"nel\u0085", 0},
		{"long s\u017f", guardFoldSpecial},
		{"kelvin\u212a", guardFoldSpecial},
		{"angstrom\u212b", 0},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, textFeatures(tc.text), "%q", tc.text)
	}
}

// 実際の経路 (MatchAppearNote → MatchNote) でも、本文と CW の前後を JavaScript の
// trim() と同じ文字で除くこと。U+0085 は除かず、U+FEFF は除く。
func TestMatchAppearNote_TrimsLikeJavaScript(t *testing.T) {
	startsWithNEL := []byte(`["/^\\x85/"]`)
	startsWithFoo := []byte(`["/^foo/"]`)
	assert.True(t, MatchAppearNote(startsWithNEL, "viewer", &Note{UserID: "a", Text: "\u0085foo"}))
	assert.True(t, MatchAppearNote(startsWithNEL, "viewer", &Note{UserID: "a", CW: "\u0085spoiler", Text: "body"}))
	assert.True(t, MatchAppearNote(startsWithFoo, "viewer", &Note{UserID: "a", Text: "\ufefffoo"}))
	assert.False(t, MatchAppearNote(startsWithFoo, "viewer", &Note{UserID: "a", Text: "\u0085foo"}))
}
