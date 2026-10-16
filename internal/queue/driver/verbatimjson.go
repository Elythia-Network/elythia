package driver

import (
	"bytes"
	"encoding/json"
	"errors"
	"unicode/utf8"
)

// MaxVerbatimJSONDepth bounds the nesting of a value accepted by
// ClassifyJSONEmbedding.
//
// 埋め込んだ値は、ジョブの data (と管理画面の一覧) の中で外側の入れ子と合わせて
// 深さを数えられる。encoding/json の上限 (10000) に近い本文を埋め込むと、積む
// ときか読むときに失敗するので、深いものは埋め込まずに文字列で運ぶ。ActivityPub の
// activity は普通 10 段にも届かない。
const MaxVerbatimJSONDepth = 64

// MaxFramedJSONDepth bounds the nesting of a task payload embedded in the job
// data (mkqdriver's framing).
//
// **本文の上限より数段多くする。** 配送・受信の payload は本文を 1 段内側に持つ
// ので、本文が上限ちょうど (64 段) だと payload は 65 段になる。同じ上限で
// 判定すると、本文は新しい形 (`bodyEscaped` など) なのに外側だけ以前の base64 の
// 形になり、#3614 より前の版がそれを読むと本文を知らない key として捨てたまま
// エラーにならずに処理を進める (#3614 のレビュー L-1)。
const MaxFramedJSONDepth = MaxVerbatimJSONDepth + 8

// JSONEmbedding says how a JSON value survives being embedded as a
// json.RawMessage and written by encoding/json.
type JSONEmbedding int

const (
	// NotEmbeddable values change when written (whitespace between tokens,
	// invalid UTF-8, nesting deeper than MaxVerbatimJSONDepth, or HTML
	// escapes that UnescapeJSONHTML could not tell apart).
	NotEmbeddable JSONEmbedding = iota
	// EmbedVerbatim values are written byte for byte.
	EmbedVerbatim
	// EmbedHTMLEscaped values are written with <, >, & and U+2028 / U+2029
	// turned into \u003c and so on; UnescapeJSONHTML restores them exactly.
	EmbedHTMLEscaped
)

// ClassifyJSONEmbedding reports how b, provided it is valid JSON, comes back
// after being embedded as a json.RawMessage in a job payload.
//
// It does not validate the JSON grammar: encoding/json validates a
// json.RawMessage while writing it, so the caller embeds b and falls back
// to another shape when json.Marshal fails (IsInvalidRawJSON).
//
// 文法の検査 (json.Valid) を省くのは、書き出すときの compact が同じ走査を
// もう一度するため。配送の job は 1 つの投稿で inbox の数だけ作るので、走査の
// 回数がそのまま CPU になる (#3614)。
//
// encoding/json は json.RawMessage を書き出すときに、空白を詰め (compact)、
// 文字列の中の <, >, & と U+2028 / U+2029 を \uXXXX に書き換える。読む側は
// 書かれたバイト列をそのまま受け取るので、書き換えが起きる値を埋め込むと元の
// バイト列に戻らない。受信の本文は受付で Digest と照合したバイト列で、worker は
// それをそのまま処理する (LD 署名の検証も同じ本文で行う)。意味が同じでも、別の
// バイト列を渡さない。
//
//   - 空白は戻せないので、詰まっていない値は埋め込まない
//   - HTML の書き換えは、元の値に \u003c などの並び (書き換えの結果と同じ 6 バイト)
//     が 1 つも無ければ、読んだ後に全部を元の文字へ戻せば元のバイト列になる
//     (EmbedHTMLEscaped)。Misskey は JSON.stringify で本文を書くので、受信の
//     本文には生の < が入る。元から並びを含むなら区別できないので埋め込まない
//
// 不正な UTF-8 も対象にしない (encoding/json は文字列の中の不正なバイトを
// そのまま通すが、読む側のクライアントによっては一覧全体が読めなくなる)。
func ClassifyJSONEmbedding(b []byte) JSONEmbedding {
	return ClassifyJSONEmbeddingWithin(b, MaxVerbatimJSONDepth)
}

// ClassifyJSONEmbeddingWithin is ClassifyJSONEmbedding with a nesting limit
// of maxDepth instead of MaxVerbatimJSONDepth.
func ClassifyJSONEmbeddingWithin(b []byte, maxDepth int) JSONEmbedding {
	if len(b) == 0 {
		return NotEmbeddable
	}
	depth := 0
	inString := false
	html := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch c {
		case '<', '>', '&':
			html = true
		case 0xE2:
			// U+2028 / U+2029 は UTF-8 で E2 80 A8 / E2 80 A9。
			if i+2 < len(b) && b[i+1] == 0x80 && (b[i+2] == 0xA8 || b[i+2] == 0xA9) {
				html = true
			}
		}
		if inString {
			switch c {
			case '\\':
				i++
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case ' ', '\t', '\n', '\r':
			return NotEmbeddable
		case '{', '[':
			depth++
			if depth > maxDepth {
				return NotEmbeddable
			}
		case '}', ']':
			depth--
		}
	}
	if inString || depth != 0 || !utf8.Valid(b) {
		return NotEmbeddable
	}
	if !html {
		return EmbedVerbatim
	}
	if containsHTMLEscape(b) {
		return NotEmbeddable
	}
	return EmbedHTMLEscaped
}

// IsVerbatimJSON reports whether b, provided it is valid JSON, is written
// byte for byte when embedded as a json.RawMessage
// (ClassifyJSONEmbedding(b) == EmbedVerbatim).
func IsVerbatimJSON(b []byte) bool {
	return ClassifyJSONEmbedding(b) == EmbedVerbatim
}

// IsVerbatimJSONWithin is IsVerbatimJSON with a nesting limit of maxDepth.
func IsVerbatimJSONWithin(b []byte, maxDepth int) bool {
	return ClassifyJSONEmbeddingWithin(b, maxDepth) == EmbedVerbatim
}

// htmlEscapes are the sequences encoding/json writes for <, >, &, U+2028 and
// U+2029 (always in lowercase hex), with the bytes they stand for.
var htmlEscapes = [...]struct {
	hex string
	raw string
}{
	{"003c", "<"},
	{"003e", ">"},
	{"0026", "&"},
	{"2028", "\u2028"},
	{"2029", "\u2029"},
}

// htmlEscapeAt returns the bytes the escape sequence starting at b[i] (a
// backslash) stands for, if it is one of htmlEscapes.
func htmlEscapeAt(b []byte, i int) (string, bool) {
	if i+6 > len(b) || b[i+1] != 'u' {
		return "", false
	}
	hex := b[i+2 : i+6]
	for _, e := range htmlEscapes {
		if string(hex) == e.hex {
			return e.raw, true
		}
	}
	return "", false
}

// containsHTMLEscape reports whether b contains any of htmlEscapes as a
// substring (escaped or not).
func containsHTMLEscape(b []byte) bool {
	for i := 0; ; {
		j := bytes.IndexByte(b[i:], '\\')
		if j < 0 {
			return false
		}
		i += j
		if _, ok := htmlEscapeAt(b, i); ok {
			return true
		}
		i++
	}
}

// UnescapeJSONHTML reverses the HTML escaping encoding/json applied to a
// value classified as EmbedHTMLEscaped, returning the original bytes.
//
// 元の値に並びが無いことを ClassifyJSONEmbedding が確かめているので、見つかった
// 並びはすべて書き換えの結果で、全部を戻してよい。並びは \ で始まり \ を 1 つしか
// 含まないので、重なって見つかることも無い。
func UnescapeJSONHTML(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; ; {
		j := bytes.IndexByte(b[i:], '\\')
		if j < 0 {
			return append(out, b[i:]...)
		}
		out = append(out, b[i:i+j]...)
		i += j
		if raw, ok := htmlEscapeAt(b, i); ok {
			out = append(out, raw...)
			i += 6
			continue
		}
		out = append(out, b[i])
		i++
	}
}

// EscapeJSONHTML escapes <, >, &, U+2028 and U+2029 in b the way
// encoding/json does (lowercase \u003c and so on). b must be valid JSON,
// where these only occur inside strings. UnescapeJSONHTML reverses it for a
// value classified as EmbedHTMLEscaped.
func EscapeJSONHTML(b []byte) []byte {
	const hex = "0123456789abcdef"
	// 容量に足し算を使うと CodeQL が overflow を指摘する。伸長は append に任せる。
	out := make([]byte, 0, len(b))
	start := 0
	for i := 0; i < len(b); i++ {
		c := b[i]
		var r rune
		switch {
		case c == '<' || c == '>' || c == '&':
			r = rune(c)
		case c == 0xE2 && i+2 < len(b) && b[i+1] == 0x80 && (b[i+2] == 0xA8 || b[i+2] == 0xA9):
			r = 0x2028 | rune(b[i+2]&1)
		default:
			continue
		}
		out = append(out, b[start:i]...)
		out = append(out, '\\', 'u', hex[r>>12&0xF], hex[r>>8&0xF], hex[r>>4&0xF], hex[r&0xF])
		if r >= 0x2028 {
			i += 2
		}
		start = i + 1
	}
	return append(out, b[start:]...)
}

// IsInvalidRawJSON reports whether err is json.Marshal rejecting an embedded
// json.RawMessage that is not valid JSON (see ClassifyJSONEmbedding).
func IsInvalidRawJSON(err error) bool {
	var me *json.MarshalerError
	return errors.As(err, &me)
}
