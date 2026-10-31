// Package jsdate reproduces the range of a JavaScript Date, where upstream
// Misskey turns a millisecond value into a Date.
//
// 本家は new Date(ms) で日時を作るので、絶対値が 8.64e15 ミリ秒を超える値は
// Invalid Date になる。Go の time.Time はその外も表せるため、判定を1か所に置く。
package jsdate

// MaxMillis is the largest absolute millisecond value a JavaScript Date holds
// (ECMAScript TimeClip: 8.64e15 ms from the epoch).
const MaxMillis = 8_640_000_000_000_000

// ValidMillis reports whether ms is a time a JavaScript Date can hold.
func ValidMillis(ms int64) bool {
	return ms >= -MaxMillis && ms <= MaxMillis
}
