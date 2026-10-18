package paramschema

import (
	"strings"
	"testing"
)

// The meaning of the translated patterns is checked against JavaScript by the
// conformance corpus (synthetic:javascript-regexp-semantics and the upstream
// patterns). These tests pin what the translator refuses: syntax JavaScript's
// u flag rejects, and syntax Go's regexp cannot express the same way.
func TestTranslateJSRegexpRejects(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{name: "invalid UTF-8", src: "\xff", want: "UTF-8"},
		{name: "lookahead", src: "a(?=b)", want: "unsupported group"},
		{name: "named group", src: "(?<x>a)", want: "unsupported group"},
		{name: "unbalanced close", src: "a)", want: "unbalanced )"},
		{name: "unbalanced open", src: "(a", want: "unbalanced ("},
		{name: "lone close bracket", src: "a]", want: "lone"},
		{name: "lone close brace", src: "a}", want: "lone"},
		{name: "quantifier at start", src: "*a", want: "nothing to repeat"},
		{name: "quantifier after quantifier", src: "a**", want: "nothing to repeat"},
		{name: "quantifier after assertion", src: "^*", want: "nothing to repeat"},
		{name: "quantifier after word boundary", src: `\b+`, want: "nothing to repeat"},
		{name: "non-word-boundary", src: `a\B`, want: `\B at 1`},
		{name: "non-word-boundary in a class", src: `[\B]`, want: "unsupported escape"},
		{name: "quantifier after open group", src: "(+)", want: "nothing to repeat"},
		{name: "brace quantifier at start", src: "{2}", want: "nothing to repeat"},
		{name: "unterminated brace", src: "a{2", want: "lone {"},
		{name: "non-numeric brace", src: "a{x}", want: "bad quantifier"},
		{name: "leading zero brace", src: "a{02}", want: "bad quantifier"},
		{name: "reversed brace", src: "a{3,2}", want: "bad quantifier"},
		{name: "non-numeric upper brace", src: "a{1,x}", want: "bad quantifier"},
		{name: "trailing backslash", src: `a\`, want: "trailing backslash"},
		{name: "escaped dash outside class", src: `\-`, want: `\-`},
		{name: "octal escape", src: `\01`, want: "octal"},
		{name: "short hex escape", src: `\x4`, want: `\x`},
		{name: "bad hex escape", src: `\xzz`, want: `\x`},
		{name: "short unicode escape", src: `\u00`, want: `\u`},
		{name: "unterminated code point escape", src: `\u{41`, want: `\u{}`},
		{name: "empty code point escape", src: `\u{}`, want: `\u{}`},
		{name: "code point out of range", src: `\u{110000}`, want: `\u{}`},
		{name: "lone high surrogate", src: `\uD83D`, want: "lone surrogate"},
		{name: "lone low surrogate", src: `\uDE00`, want: "lone surrogate"},
		{name: "high surrogate then non-surrogate", src: `\uD83DA`, want: "lone surrogate"},
		{name: "property escape", src: `\p{L}`, want: "unsupported escape"},
		{name: "back reference", src: `(a)\1`, want: "unsupported escape"},
		{name: "control escape", src: `\cA`, want: "unsupported escape"},
		{name: "unterminated class", src: "[ab", want: "unterminated class"},
		{name: "trailing backslash in class", src: `[a\`, want: "trailing backslash"},
		{name: "negated space in class", src: `[\S]`, want: `\S inside a class`},
		{name: "class escape as range start", src: `[\d-z]`, want: "class escape in a range"},
		{name: "class escape as range end", src: `[a-\d]`, want: "class escape in a range"},
		{name: "reversed range", src: "[z-a]", want: "out of order"},
		{name: "bad escape as range end", src: `[a-\q]`, want: "unsupported escape"},
		{name: "empty class", src: "[]", want: "empty class"},
		{name: "empty negated class", src: "[^]", want: "empty class"},
		{name: "bad escape in class", src: `[\q]`, want: "unsupported escape"},
		{name: "repeat count Go rejects", src: "a{1001}", want: "invalid repeat count"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := compileJSRegexp(tt.src)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("compileJSRegexp(%q) = %v, want an error containing %q", tt.src, err, tt.want)
			}
		})
	}
}

func TestTranslateJSRegexpMatches(t *testing.T) {
	tests := []struct {
		src     string
		match   []string
		nomatch []string
	}{
		{src: `^[a-c-]+$`, match: []string{"a-c", "-"}, nomatch: []string{"d"}},
		{src: `^\u{41}\x42$`, match: []string{"AB"}, nomatch: []string{"ab"}},
		{src: `^[\-\]]+$`, match: []string{"-]"}, nomatch: []string{"a"}},
		{src: `^[^\s]+$`, match: []string{"a\u0085"}, nomatch: []string{"a\u3000", "\ufeff"}},
		{src: `^a+?$`, match: []string{"aaa"}},
		{src: `^\d\D\w\W$`, match: []string{"1a_ "}, nomatch: []string{"\u0661a_ "}},
	}
	for _, tt := range tests {
		re, err := compileJSRegexp(tt.src)
		if err != nil {
			t.Fatalf("compileJSRegexp(%q): %v", tt.src, err)
		}
		for _, s := range tt.match {
			if !re.MatchString(s) {
				t.Errorf("%q should match %q", tt.src, s)
			}
		}
		for _, s := range tt.nomatch {
			if re.MatchString(s) {
				t.Errorf("%q should not match %q", tt.src, s)
			}
		}
	}
}
