// Package redisglob escapes literals for Redis glob patterns (SCAN MATCH,
// PSUBSCRIBE).
package redisglob

import "strings"

// Escape escapes the glob characters of s so that a pattern built as
// Escape(prefix) + "rest*" matches the prefix literally.
//
// 逃がさないと、prefix に `*` や `[` があるとき、別のサーバーや別のアプリの
// キー・チャネルまで当たる。
func Escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '*', '?', '[', ']':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
