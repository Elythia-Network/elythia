package server

import (
	"go/parser"
	"go/scanner"
	"go/token"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// stripComments removes comments from a Go, TypeScript or Vue source so that
// commented-out code is not mistaken for live wiring.
//
// **正規表現で剥がさない (#3369 I3)。** 以前は `(?s)/\*.*?\*/` などで剥がしていたので、
// 文字列やコメントの中の `i/*` が後ろのどこかの `*/` と組になり、間のコードごと
// 消えて検査が黙って外れた (#3676 の作業中に、コメントを動かしただけで 968 行が
// 読まれなくなった)。言語ごとに文字列とコメントを区別する走査で剥がす。
//
// ブロックコメントは中の改行だけを残して消す (行番号を保つ)。行コメントは行末まで
// 消す。どの言語かは中身で決める: package 節として読めれば Go、行頭に
// `<template` / `<script` / `<style` があれば Vue の SFC、それ以外は TypeScript。
func stripComments(src string) string {
	return stripCommentsWith(src, false)
}

// stripCommentsKeepLine removes block and HTML comments but keeps `//` line
// comments, for gates that prefer counting a commented-out line to missing a
// live one.
func stripCommentsKeepLine(src string) string {
	return stripCommentsWith(src, true)
}

func stripCommentsWith(src string, keepLine bool) string {
	if isGoSource(src) {
		return stripGoComments(src, keepLine)
	}
	if vueSFCRe.MatchString(src) {
		return stripVueComments(src, keepLine)
	}
	return stripScriptComments(src, keepLine)
}

// vueSFCRe matches a top-level block of a Vue single-file component.
var vueSFCRe = regexp.MustCompile(`(?m)^<(?:template|script|style)\b`)

// isGoSource reports whether src starts with a Go package clause.
func isGoSource(src string) bool {
	_, err := parser.ParseFile(token.NewFileSet(), "", src, parser.PackageClauseOnly)
	return err == nil
}

// stripGoComments removes Go comments using go/scanner, so string, rune and
// raw string literals are never mistaken for comments.
func stripGoComments(src string, keepLine bool) string {
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	// 構文の誤りは無視する。読めた範囲のコメントだけを落とす。
	s.Init(file, []byte(src), func(token.Position, string) {}, scanner.ScanComments)
	var b strings.Builder
	last := 0
	for {
		pos, tok, _ := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok != token.COMMENT {
			continue
		}
		start := file.Offset(pos)
		var end int
		if strings.HasPrefix(src[start:], "//") {
			if keepLine {
				continue
			}
			end = strings.IndexByte(src[start:], '\n')
			if end < 0 {
				end = len(src)
			} else {
				end += start
			}
		} else {
			end = strings.Index(src[start+2:], "*/")
			if end < 0 {
				end = len(src)
			} else {
				end += start + 4
			}
		}
		b.WriteString(src[last:start])
		b.WriteString(newlinesOf(src[start:end]))
		last = end
	}
	b.WriteString(src[last:])
	return b.String()
}

// stripVueComments removes `<!-- -->` comments from the template part of a
// Vue single-file component and script comments from its `<script>` and
// `<style>` blocks.
//
// **template では `//` も `/* */` もコメントにならない。** template の本文に
// `Don't` のような引用符が単独で現れても、文字列として読まない。タグの中で `=` の
// 直後に来る引用符は属性値として閉じの引用符まで読み飛ばすので、`title="<!--"` を
// コメントの開始と読まない。`=` の直後に限るのは、`{{ n<max }} isn't` のように
// タグでない `<` + 英字の後に来る単独の引用符を、属性値と読まないため。属性値は
// `>` (`:a="x > 1"`) や改行を含みうるので、範囲を次の `>` や行で切らない。
// 閉じの引用符が無ければ、引用符をただの文字として読む。
//
// 読み違える形は残る: タグでない `<` + 英字の後に `='` のような並びが来ると、そこから
// 閉じの引用符までを属性値と読み、間の `<!--` や `<script` を見落とす (今のソースには
// 無い。#3369)。
func stripVueComments(src string, keepLine bool) string {
	var b strings.Builder
	inTag := false
	// prevSig is the last byte written that is not a space.
	var prevSig byte
	i := 0
	for i < len(src) {
		rest := src[i:]
		c := src[i]
		if inTag {
			switch {
			case (c == '"' || c == '\'') && prevSig == '=':
				if end := strings.IndexByte(rest[1:], c); end >= 0 {
					b.WriteString(rest[:end+2])
					i += end + 2
					prevSig = c
					continue
				}
			case c == '>':
				inTag = false
			}
		}
		switch {
		case strings.HasPrefix(rest, "<!--"):
			end := strings.Index(rest[4:], "-->")
			if end < 0 {
				b.WriteString(newlinesOf(rest))
				return b.String()
			}
			b.WriteString(newlinesOf(rest[:end+7]))
			i += end + 7
		case strings.HasPrefix(rest, "<script") || strings.HasPrefix(rest, "<style"):
			tag := "</script"
			if strings.HasPrefix(rest, "<style") {
				tag = "</style"
			}
			open := strings.IndexByte(rest, '>')
			if open < 0 {
				b.WriteString(rest)
				return b.String()
			}
			body := rest[open+1:]
			end := strings.Index(body, tag)
			if end < 0 {
				end = len(body)
			}
			b.WriteString(rest[:open+1])
			b.WriteString(stripScriptComments(body[:end], keepLine))
			i += open + 1 + end
			// 閉じタグを `<script` / `<style` の開始と取り違えないよう、そのまま写す。
			if strings.HasPrefix(src[i:], tag) {
				b.WriteString(tag)
				i += len(tag)
			}
		default:
			if c == '<' && len(rest) > 1 && (isASCIILetter(rest[1]) || rest[1] == '/') {
				inTag = true
			}
			b.WriteByte(c)
			i++
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				prevSig = c
			}
		}
	}
	return b.String()
}

// isASCIILetter reports whether c is an ASCII letter.
func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// isIdentByte reports whether c can be part of a JavaScript identifier (ASCII
// only).
func isIdentByte(c byte) bool {
	return isASCIILetter(c) || c >= '0' && c <= '9' || c == '_' || c == '$'
}

// regexKeywords are the keywords after which a `/` begins a regular
// expression literal.
var regexKeywords = map[string]bool{
	"return": true, "typeof": true, "case": true, "in": true, "of": true, "void": true, "delete": true,
	"throw": true, "new": true, "yield": true, "await": true, "else": true, "do": true, "instanceof": true,
}

// stripScriptComments removes `//` and `/* */` comments from JavaScript,
// TypeScript or SCSS, skipping string, template and regular expression
// literals.
//
// **正規表現のリテラルは直前の字句で推し量る。** `(` `,` `=` などの記号と、
// `return` `typeof` `case` などのキーワード (regexKeywords) の後の `/` は正規表現、
// それ以外は割り算として読む。読み違える形は残る: `)` や `]` の後に置いた
// 正規表現 (`if (x) /a/.test(s)`) や、プロパティ名がキーワードと同じ形
// (`o.return / 2`)。正規表現を割り算と読み違えたうえで、その正規表現が `/*` を
// 含むと、次の `*/` まで (複数行を) 消す。SCSS の引用符の無い `url(//cdn…)` は
// 行コメントとして行末まで消す (どちらも今のソースには無い。#3369)。
func stripScriptComments(src string, keepLine bool) string {
	var b strings.Builder
	// テンプレートリテラルの `${` を開いたときの括弧の深さ。`}` で深さが戻ったら
	// テンプレートの本文に戻る。
	var templates []int
	depth := 0
	inTemplate := false
	var prev byte
	// word is the identifier that ends at prev (empty when prev is not part of
	// one), to tell `return /re/` from a division.
	word := ""
	n := len(src)
	i := 0
	for i < n {
		c := src[i]
		var next byte
		if i+1 < n {
			next = src[i+1]
		}
		if inTemplate {
			switch {
			case c == '\\' && i+1 < n:
				b.WriteString(src[i : i+2])
				i += 2
			case c == '`':
				b.WriteByte(c)
				i++
				inTemplate = false
				prev = c
			case c == '$' && next == '{':
				b.WriteString("${")
				i += 2
				templates = append(templates, depth)
				depth++
				inTemplate = false
				prev = '{'
			default:
				b.WriteByte(c)
				i++
			}
			continue
		}
		switch {
		case c == '/' && next == '/':
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				end = n - i
			}
			if keepLine {
				b.WriteString(src[i : i+end])
			}
			i += end
		case c == '/' && next == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				end = n
			} else {
				end += i + 4
			}
			b.WriteString(newlinesOf(src[i:end]))
			i = end
		case c == '\'' || c == '"':
			end := quotedEnd(src, i, c)
			b.WriteString(src[i:end])
			i = end
			prev, word = c, ""
		case c == '`':
			b.WriteByte(c)
			i++
			inTemplate = true
			word = ""
		case c == '/' && (regexMayStart(prev) || regexKeywords[word]):
			end := regexLiteralEnd(src, i)
			b.WriteString(src[i:end])
			i = end
			prev, word = '/', ""
		default:
			switch c {
			case '{':
				depth++
			case '}':
				if len(templates) > 0 && templates[len(templates)-1] == depth-1 {
					templates = templates[:len(templates)-1]
					depth--
					b.WriteByte(c)
					i++
					inTemplate = true
					continue
				}
				depth--
			}
			b.WriteByte(c)
			i++
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				switch {
				case !isIdentByte(c):
					word = ""
				case i >= 2 && isIdentByte(src[i-2]):
					word += string(c)
				default:
					word = string(c)
				}
				prev = c
			}
		}
	}
	return b.String()
}

// quotedEnd returns the offset just past the string literal starting at i.
// An unterminated literal ends at the end of the line.
func quotedEnd(src string, i int, quote byte) int {
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case quote:
			return j + 1
		case '\n':
			return j
		}
	}
	return len(src)
}

// regexMayStart reports whether a `/` after prev begins a regular expression
// literal rather than a division.
func regexMayStart(prev byte) bool {
	return prev == 0 || strings.IndexByte("(,=:[!&|?{};+-*%<>~^", prev) >= 0
}

// regexLiteralEnd returns the offset just past the regular expression literal
// starting at i, including its flags. It stops at the end of the line.
func regexLiteralEnd(src string, i int) int {
	inClass := false
	for j := i + 1; j < len(src); j++ {
		switch c := src[j]; {
		case c == '\\':
			j++
		case c == '\n':
			return j
		case c == '[':
			inClass = true
		case c == ']':
			inClass = false
		case c == '/' && !inClass:
			j++
			for j < len(src) && (src[j] >= 'a' && src[j] <= 'z') {
				j++
			}
			return j
		}
	}
	return len(src)
}

// newlinesOf returns the newlines in s, so removing s keeps line numbers.
func newlinesOf(s string) string {
	return strings.Repeat("\n", strings.Count(s, "\n"))
}

// 文字列やコメントの中の `/*` で、後ろのコードごと消さないこと (#3369 I3)。
func TestStripCommentsKeepsCodeAfterCommentLikeStrings(t *testing.T) {
	tests := []struct {
		name string
		src  string
		live []string
		dead []string
	}{
		{
			name: "go string with an opening block comment",
			src:  "package server\n\nvar s = \"i/*\"\n\nfunc live() {} // gone()\n/* dead() */\nfunc after() {}\n",
			live: []string{`"i/*"`, "func live()", "func after()"},
			dead: []string{"gone()", "dead()"},
		},
		{
			name: "go raw string ending in a backslash",
			src:  "package server\n\nvar re = `a\\`\n\nfunc live() {} /* dead() */\nvar u = \"https://x\"\n",
			live: []string{"func live()", `"https://x"`},
			dead: []string{"dead()"},
		},
		{
			name: "go line comment holding an opening block comment",
			src:  "package server\n\n// see i/*\nfunc live() {}\n/* dead() */\n",
			live: []string{"func live()"},
			dead: []string{"see i", "dead()"},
		},
		{
			name: "ts string with an opening block comment",
			src:  "const s = 'i/*';\nlive();\n/* dead() */\nconst u = 'https://x'; after(); // gone()\n",
			live: []string{"'i/*'", "live();", "'https://x'", "after();"},
			dead: []string{"dead()", "gone()"},
		},
		{
			name: "ts template literal and regular expression",
			src:  "const t = `/* ${a /* dead() */} //`; live();\nconst re = /\\/*x/; after();\n",
			live: []string{"`/* ${a ", "} //`", "live();", "/\\/*x/", "after();"},
			dead: []string{"dead()"},
		},
		{
			name: "ts regular expression after a keyword",
			src:  "function f(s) {\n\treturn /\\/*/.test(s);\n}\nlive1();\nlive2();\n/* dead() */\nafter();\nconst t = typeof x === 'y' ? 1 : 2; case1: switch (k) { case /x*/.source: live3(); }\n",
			live: []string{"/\\/*/", "live1();", "live2();", "after();", "case /x*/.source", "live3();"},
			dead: []string{"dead()"},
		},
		{
			name: "ts division after an identifier",
			src:  "const r = a / b; live1(); /* dead() */ const q = returned / 2; live2();\n",
			live: []string{"a / b", "live1();", "returned / 2", "live2();"},
			dead: []string{"dead()"},
		},
		{
			name: "vue attribute value holding a comment opener",
			src:  "<template>\n<div title=\"<!--\" :a='x > 1'>live1</div>\n<!-- dead1 -->\n</template>\n<script setup lang=\"ts\">\nlive2(); // dead2\n</script>\n",
			live: []string{`title="<!--"`, "live1", "live2();"},
			dead: []string{"dead1", "dead2"},
		},
		{
			name: "vue text holding a lone quote after a less-than",
			src:  "<template>\n<p>{{ n<max }} isn't</p>\n</template>\n<script setup>\nlive2(); // gone2\nconst s = 'x';\n</script>\n",
			live: []string{"n<max", "isn't", "live2();", "const s = 'x';"},
			dead: []string{"gone2"},
		},
		{
			name: "vue multi-line attribute value",
			src:  "<template>\n<div :class=\"{\n a: x > 1,\n b: '<!--'\n}\">live1</div>\n<!-- dead1 -->\n</template>\n",
			live: []string{"a: x > 1", "'<!--'", "live1"},
			dead: []string{"dead1"},
		},
		{
			name: "vue template text with a lone quote",
			src:  "<template>\n<p>Don't</p>\n<!-- <X @contextmenu=\"dead\"/> -->\n<a href=\"//x\" @click=\"live\">i/*</a>\n</template>\n<script setup lang=\"ts\">\nconst s = '<!--';\nlive2(); // gone()\n/* dead2() */\nconst e = '-->';\n</script>\n<style>\n/* .dead3 */\n.live3 {}\n</style>\n",
			live: []string{"Don't", `href="//x"`, `@click="live"`, "i/*", "'<!--'", "live2();", "'-->'", ".live3"},
			dead: []string{"@contextmenu", "gone()", "dead2()", ".dead3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripComments(tt.src)
			for _, s := range tt.live {
				require.Containsf(t, got, s, "live code was stripped:\n%s", got)
			}
			for _, s := range tt.dead {
				require.NotContainsf(t, got, s, "a comment survived:\n%s", got)
			}
			require.Equal(t, strings.Count(tt.src, "\n"), strings.Count(got, "\n"), "line numbers moved")
		})
	}
}

// 行コメントを残す版が、`//` を残してブロックと HTML のコメントだけを落とすこと。
func TestStripCommentsKeepLine(t *testing.T) {
	got := stripCommentsKeepLine("<template>\n<!-- dead -->\n</template>\n<script>\nconst u = 'https://x'; // kept\n/* dead2 */\n</script>\n")
	require.Contains(t, got, "// kept")
	require.Contains(t, got, "'https://x'")
	require.NotContains(t, got, "dead")

	got = stripCommentsKeepLine("package server\n\n// kept\n/* dead */\n")
	require.Contains(t, got, "// kept")
	require.NotContains(t, got, "dead")
}
