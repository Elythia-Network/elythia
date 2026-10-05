package mkqdriver

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// **件数 0 は mkq に渡さない。** driver の意味では `WithKeepCompleted(0)` は
// 「件数で制限しない」だが、mkq の `WithScheduleKeepCompleted(0)` は「完了したら
// 即削除」で逆になる。Add 側 (option.go) と同じ 0-skip を scheduler にも掛ける。
func TestScheduleRetentionOptions(t *testing.T) {
	tests := []struct {
		name string
		opts []driver.EnqueueOption
		want int
	}{
		{name: "none", want: 0},
		{name: "explicit zero counts are dropped", opts: []driver.EnqueueOption{driver.WithKeepCompleted(0), driver.WithKeepFailed(0)}, want: 0},
		{name: "positive counts are passed", opts: []driver.EnqueueOption{driver.WithKeepCompleted(30), driver.WithKeepFailed(100)}, want: 2},
		{name: "ages are passed", opts: []driver.EnqueueOption{driver.WithKeepCompletedAge(time.Hour), driver.WithKeepFailedAge(time.Hour)}, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scheduleRetentionOptions(driver.ApplyEnqueueOptions(tt.opts))
			assert.Len(t, got, tt.want)
		})
	}
}
