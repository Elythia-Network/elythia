package note

import (
	"errors"
	"time"
)

// MaxPollExpiresAtUnixMilli is the latest poll deadline accepted, in Unix
// milliseconds: 9999-12-30T23:59:59.999Z. A deadline up to this instant has a
// four-digit year in every time zone (UTC-12 to UTC+14).
//
// upstream は `new Date(Date.now() + expiredAfter)` で期限を作り、JS の Date の
// 上限 (+275760-09-13) までは受け付ける。ただし 5 桁の年は Go では扱えない所が
// 多い。time.Time の MarshalJSON は error を返し (notes のエクスポートが落ちる)、
// API と AP の "2006-01-02T15:04:05.000Z" 形式は JS の Date が読めない文字列になり、
// simple protocol の接続では読み戻しの parse にも失敗する。どれも UTC ではなく
// プロセスや DB セッションの時刻帯で年を見るので、最も東の UTC+14 でも 9999 年に
// 収まるよう、1 日手前を上限にする。これより手前の値は upstream と同じ期限になる (#3443)。
const MaxPollExpiresAtUnixMilli int64 = 253_402_214_399_999

// ErrPollExpiryOutOfRange is returned when a poll deadline would be later than
// MaxPollExpiresAtUnixMilli.
var ErrPollExpiryOutOfRange = errors.New("poll expiry is out of range")

// PollExpiresAtAfter returns the poll deadline that is expiredAfterMs
// milliseconds after now (the poll.expiredAfter request parameter).
// It returns ErrPollExpiryOutOfRange when the deadline would be later than
// MaxPollExpiresAtUnixMilli.
func PollExpiresAtAfter(now time.Time, expiredAfterMs int64) (time.Time, error) {
	// time.Duration(ms) * time.Millisecond は約 9.22e12 ms (約292年) で int64 を
	// 溢れて過去の期限になる。ミリ秒のまま足し、和が上限を超えないかを先に見る (#3443)。
	nowMs := now.UnixMilli()
	if expiredAfterMs > MaxPollExpiresAtUnixMilli-nowMs {
		return time.Time{}, ErrPollExpiryOutOfRange
	}
	// ミリ秒未満の端数を戻し、範囲内の値では従来の now.Add と同じ時刻にする。
	fraction := now.Sub(time.UnixMilli(nowMs))
	return time.UnixMilli(nowMs + expiredAfterMs).Add(fraction), nil
}

// PollExpiresAtFromUnixMilli converts the poll.expiresAt request parameter
// (absolute Unix milliseconds) to a deadline. It returns
// ErrPollExpiryOutOfRange when the value is later than
// MaxPollExpiresAtUnixMilli. Past values are left to the already-expired check
// of the create service.
func PollExpiresAtFromUnixMilli(expiresAtMs int64) (time.Time, error) {
	if expiresAtMs > MaxPollExpiresAtUnixMilli {
		return time.Time{}, ErrPollExpiryOutOfRange
	}
	return time.UnixMilli(expiresAtMs), nil
}

// ValidatePollExpiresAt returns ErrPollExpiryOutOfRange when t is later than
// MaxPollExpiresAtUnixMilli (compared at millisecond precision).
func ValidatePollExpiresAt(t time.Time) error {
	if t.UnixMilli() > MaxPollExpiresAtUnixMilli {
		return ErrPollExpiryOutOfRange
	}
	return nil
}
