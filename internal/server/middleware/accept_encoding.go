package middleware

import (
	"strconv"
	"strings"
)

// AcceptsEncoding reports whether an Accept-Encoding header value admits the
// content-coding (e.g. "br", "gzip"): it is listed, or "*" is, with a
// non-zero q-value. An explicit "coding;q=0" wins over "*".
//
// echo の gzip middleware は部分一致 (strings.Contains) で見ているが、自前で
// 符号化した本文を返す経路では `br;q=0` のような拒否も尊重する。
func AcceptsEncoding(header, coding string) bool {
	return EncodingQ(header, coding) > 0
}

// EncodingQ returns the q-value an Accept-Encoding header value gives the
// content-coding: its own element's, else the "*" element's, else 0.
// A missing q counts as 1; an unparsable q counts as 0 (RFC 9110 qvalue).
func EncodingQ(header, coding string) float64 {
	wildcard := 0.0
	for part := range strings.SplitSeq(header, ",") {
		name, params, _ := strings.Cut(part, ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name != coding && name != "*" {
			continue
		}
		q := qValue(params)
		if name == coding {
			return q
		}
		wildcard = q
	}
	return wildcard
}

// qValue returns the q parameter of one Accept-Encoding element, 1 when
// absent and 0 when it is not a qvalue in [0, 1].
func qValue(params string) float64 {
	for p := range strings.SplitSeq(params, ";") {
		k, v, found := strings.Cut(p, "=")
		if !found || strings.ToLower(strings.TrimSpace(k)) != "q" {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || q < 0 || q > 1 {
			return 0
		}
		return q
	}
	return 1
}
