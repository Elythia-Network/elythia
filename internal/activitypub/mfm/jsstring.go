package mfm

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/misc/jsstr"
)

// jsUnixtimeISO returns `new Date(parseInt(text, 10) * 1000).toISOString()`
// as upstream's unixtime fn computes it. ok is false where JavaScript throws
// (parseInt gives NaN, or the time is outside the Date range).
func jsUnixtimeISO(text string) (string, bool) {
	// parseInt は先頭の空白 (trim と同じ集合) を飛ばし、符号の後ろの数字だけを
	// 読む。"12abc" は 12、"1e3" は 1、"0x10" は 0 になる
	s := strings.TrimLeftFunc(text, jsstr.IsSpace)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	n := 0
	for n < len(s) && isASCIIDigit(s[n]) {
		n++
	}
	if n == 0 {
		return "", false
	}
	// 数字だけなので構文の誤りは起きない。桁あふれは ±Inf になり、下の範囲の
	// 検査で落ちる
	sec, _ := strconv.ParseFloat(s[:n], 64)
	if neg {
		sec = -sec
	}
	// Date の範囲は ±8.64e15 ミリ秒。範囲内の値は 2^53 より小さく、1000 倍しても
	// 浮動小数点の誤差は出ない
	ms := sec * 1000
	if math.Abs(ms) > 8.64e15 {
		return "", false
	}
	t := time.UnixMilli(int64(ms)).UTC()
	year := t.Year()
	var y string
	switch {
	case year >= 0 && year <= 9999:
		y = fmt.Sprintf("%04d", year)
	case year < 0:
		y = fmt.Sprintf("-%06d", -year)
	default:
		y = fmt.Sprintf("+%06d", year)
	}
	return y + t.Format("-01-02T15:04:05.000Z"), true
}
