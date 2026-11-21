// Package vary adds a field to the Vary response header without dropping the
// fields set before it.
package vary

import (
	"net/http"
	"strings"
)

// Add appends field to h's Vary unless it is already named there.
//
// **Set で上書きしない。** handler より前の middleware (gzip の Accept-Encoding、
// CORS の Origin) が足した欄が消え、共有キャッシュが符号化や CORS の違う応答を
// 取り違える。同じ欄を 2 度足さないのは、包む側と包まれる側の handler がどちらも
// Accept を足すため。
func Add(h http.Header, field string) {
	if !Has(h, field) {
		h.Add("Vary", field)
	}
}

// Has reports whether h's Vary names field (case-insensitively), or is "*".
func Has(h http.Header, field string) bool {
	for _, v := range h.Values("Vary") {
		for f := range strings.SplitSeq(v, ",") {
			f = strings.TrimSpace(f)
			if f == "*" || strings.EqualFold(f, field) {
				return true
			}
		}
	}
	return false
}
