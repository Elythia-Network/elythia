package paramschema

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
)

// This file gives Go values decoded by encoding/json the meaning ajv gives the
// same JSON after JSON.parse.

// kind is the JavaScript view of a decoded value.
type kind int

const (
	kindUnknown kind = iota // not a value encoding/json produces
	kindNull
	kindBool
	kindNumber    // a finite number
	kindNonFinite // a number JSON.parse turns into ±Infinity (1e400)
	kindString
	kindArray
	kindObject
)

// number returns v as a float64 when v is a JSON number (float64 or
// json.Number). A json.Number out of float64 range becomes ±Inf like
// JSON.parse does.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		// 範囲を超える数は ±InfとErrRangeで返る。JSON.parseもInfinityにする
		// ので、範囲の誤りだけは値を使う。綴りの誤りはdecoderが作るjson.Number
		// には無いので、数ではない扱いにする。
		f, err := strconv.ParseFloat(string(n), 64)
		var ne *strconv.NumError
		if err != nil && (!errors.As(err, &ne) || ne.Err != strconv.ErrRange) {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func kindOf(v any) kind {
	switch v.(type) {
	case nil:
		return kindNull
	case bool:
		return kindBool
	case string:
		return kindString
	case []any:
		return kindArray
	case map[string]any:
		return kindObject
	}
	f, ok := number(v)
	if !ok {
		return kindUnknown
	}
	// ajvのstrictNumbers (既定でtrue)は、有限でない数をnumberとみなさない。
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return kindNonFinite
	}
	return kindNumber
}

// isType reports whether v is of the JSON Schema type t, as ajv's
// checkDataType does with strictNumbers.
func isType(t string, v any) bool {
	k := kindOf(v)
	switch t {
	case "null":
		return k == kindNull
	case "boolean":
		return k == kindBool
	case "string":
		return k == kindString
	case "array":
		return k == kindArray
	case "object":
		return k == kindObject
	case "number":
		return k == kindNumber
	case "integer":
		if k != kindNumber {
			return false
		}
		f, _ := number(v)
		// JavaScriptには整数の型が無く、`!(x % 1)`で見る。10.0も1e1も整数。
		return f == math.Trunc(f)
	}
	return false
}

func isAnyType(types []string, v any) bool {
	for _, t := range types {
		if isType(t, v) {
			return true
		}
	}
	return false
}

// jsEqual is JavaScript's strict equality (===) between decoded values: equal
// numbers (1 === 1.0, 0 === -0), strings and booleans, and null. Arrays and
// objects are never equal to anything here.
//
// **ajvはenumのobject・配列の値と、型の無い(またはobject・配列の)itemsの
// uniqueItemsを、fast-deep-equalで比べる(#3673)。** fast-deep-equalは、比べる
// objectが`constructor`キーにobjectを持つと参照で比べ、`toString` / `valueOf`
// キーを持つとそれを呼んでTypeErrorを投げる(本家ではINTERNAL_ERRORになる)。
// これは写さず、その形のschemaをCompileで拒むので、ここに来るのはprimitiveとの
// 比較だけになる。
func jsEqual(a, b any) bool {
	ka, kb := kindOf(a), kindOf(b)
	if (ka == kindNumber || ka == kindNonFinite) && (kb == kindNumber || kb == kindNonFinite) {
		fa, _ := number(a)
		fb, _ := number(b)
		return fa == fb
	}
	if ka != kb {
		return false
	}
	switch ka {
	case kindNull:
		return true
	case kindBool:
		return a.(bool) == b.(bool)
	case kindString:
		return a.(string) == b.(string)
	}
	return false
}

// ucs2length counts code points like ajv's ucs2length (with unicode: true).
// Strings from encoding/json are valid UTF-8, so this is the rune count.
func ucs2length(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// jsNumberString formats f like JavaScript's String(f), for the numbers in
// ajv's messages ("must be <= 100", "must be >= -0.5").
func jsNumberString(f float64) string {
	if f == 0 {
		return "0"
	}
	a := math.Abs(f)
	if a >= 1e-6 && a < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	// 指数表記は、JSでは仮数の小数点以下を最短にし、指数に0を詰めない(1e+21、1e-7)。
	s := strconv.FormatFloat(f, 'e', -1, 64)
	mant, exp, _ := strings.Cut(s, "e")
	sign := exp[0]
	exp = strings.TrimLeft(exp[1:], "0")
	return mant + "e" + string(sign) + exp
}

// escapeFragment is ajv's escapeFragment for a property name in schemaPath:
// a JSON pointer token, then encodeURIComponent.
func escapeFragment(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		const hex = "0123456789ABCDEF"
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

// deepCopy copies a default value so that requests never share (and mutate)
// the schema's own value. ajv embeds the default as a literal, which builds a
// new value on every call.
func deepCopy(v any) any {
	switch v := v.(type) {
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = deepCopy(x)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = deepCopy(x)
		}
		return out
	}
	return v
}
