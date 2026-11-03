package queue

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// 期限の時刻のジョブは、期限より前には走らせない (#3994)。遅延はミリ秒へ切り上げ、
// 計算を誤ったときの保険として 1 秒の余裕を足す (ホスト間の時計のずれは効かない。
// dueJobMargin のコメント)。
func TestDueJobDelay(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		at   time.Time
		want time.Duration
	}{
		{"past", now.Add(-time.Minute), 0},
		{"exactly now", now, 0},
		{"an hour later", now.Add(time.Hour), time.Hour + dueJobMargin},
		{"rounds sub-millisecond up", now.Add(1500 * time.Microsecond), 2*time.Millisecond + dueJobMargin},
		{"one nanosecond", now.Add(time.Nanosecond), time.Millisecond + dueJobMargin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, dueJobDelay(tc.at, now))
		})
	}
	assert.Equal(t, time.Second, dueJobMargin)
}

// 遠い期限 (約 292 年で Sub が飽和する) でも、遅延は負にならず上限で抑えられる
// (#3994 の敵対的レビュー。抑えないと切り上げで符号があふれ、すぐ走る)。
func TestDueJobDelay_FarFutureIsCapped(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{
		time.UnixMilli(8_640_000_000_000_000), // JS の Date の上限
		now.AddDate(300, 0, 0),
		now.Add(maxDueJobDelay + time.Hour),
	} {
		assert.Equal(t, maxDueJobDelay+dueJobMargin, dueJobDelay(at, now), at)
	}
}

// 枠のジョブの最後の試行は、予定から 2 分半ほど後 (3 分の許容の内側)。
func TestResendSlotLastTry(t *testing.T) {
	assert.Equal(t, 151*time.Second, ResendSlotLastTry())
}
