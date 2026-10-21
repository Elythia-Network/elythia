// Package jsnum decodes JSON numbers the way upstream Misskey's ajv judges
// them.
package jsnum

import (
	"bytes"
	"errors"
	"math"
	"strconv"
)

// errNotInteger is returned for a JSON value Int does not hold.
var errNotInteger = errors.New("jsnum: not an integer")

// Int is an integer request param that accepts every JSON number whose value
// is an integer, as ajv's "type": "integer" does: 10, 10.0 and 1e1 are all 10.
// A fraction (10.5), a string, a boolean and a value outside int64 are
// errors. A JSON null leaves the value unchanged; a *Int field stays nil.
//
// encoding/jsonのintは`10.0`や`1e1`を型の誤りにするが、本家(JavaScript)では
// どれも同じ数の10で、ajvの`integer`を通る(#3742)。
type Int int64

// UnmarshalJSON decodes an integer-valued JSON number.
func (n *Int) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if string(data) == "null" {
		return nil
	}
	if v, err := strconv.ParseInt(string(data), 10, 64); err == nil {
		*n = Int(v)
		return nil
	}
	// 文字列などはParseFloatが拒否する。"NaN"や"Inf"はJSONの数として
	// 届かない(decoderが先に拒否する)が、念のためここでも落とす。
	f, err := strconv.ParseFloat(string(data), 64)
	if err != nil || f != math.Trunc(f) || math.IsInf(f, 0) {
		return errNotInteger
	}
	// float64で表せるint64の範囲は[-2^63, 2^63)。2^63ちょうどはint64に入らない。
	if f < math.MinInt64 || f >= math.MaxInt64 {
		return errNotInteger
	}
	*n = Int(f)
	return nil
}

// IntPtr returns the value as *int, or nil for a nil n.
func (n *Int) IntPtr() *int {
	if n == nil {
		return nil
	}
	v := int(*n)
	return &v
}

// Int64Ptr returns the value as *int64, or nil for a nil n.
func (n *Int) Int64Ptr() *int64 {
	if n == nil {
		return nil
	}
	v := int64(*n)
	return &v
}
