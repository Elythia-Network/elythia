package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
)

var jst = time.FixedZone("JST", 9*3600)

func TestParseSchedule(t *testing.T) {
	s, err := ParseSchedule(config.BackupScheduleOptions{}, nil)
	require.NoError(t, err)
	assert.Nil(t, s, "an empty interval turns the schedule off")

	s, err = ParseSchedule(config.BackupScheduleOptions{Interval: "24h", At: "04:30", Keep: 7, Verify: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, s.Interval)
	assert.Equal(t, "04:30", s.AtString())
	assert.Equal(t, 7, s.Keep)
	assert.True(t, s.Verify)
	assert.Equal(t, time.Local, s.Location, "a nil location means the process TZ")
	assert.Equal(t, 36*time.Hour, s.DelayAfter, "the delay defaults to 1.5 intervals")

	s, err = ParseSchedule(config.BackupScheduleOptions{Interval: "6h", DelayAfter: "6h"}, jst)
	require.NoError(t, err)
	assert.Equal(t, "", s.AtString())
	assert.Equal(t, 6*time.Hour, s.DelayAfter, "delayAfter equal to the interval is accepted")

	for _, tc := range []struct {
		name string
		o    config.BackupScheduleOptions
		want string
	}{
		{"unit missing", config.BackupScheduleOptions{Interval: "24"}, "backup.schedule.interval"},
		{"too short", config.BackupScheduleOptions{Interval: "59s"}, "shorter than 1m0s"},
		{"negative keep", config.BackupScheduleOptions{Interval: "1h", Keep: -1}, "backup.schedule.keep"},
		{"at hour 24", config.BackupScheduleOptions{Interval: "1h", At: "24:00"}, "backup.schedule.at"},
		{"at minute 60", config.BackupScheduleOptions{Interval: "1h", At: "04:60"}, "backup.schedule.at"},
		{"at without zero padding", config.BackupScheduleOptions{Interval: "1h", At: "4:00"}, "backup.schedule.at"},
		{"bad delayAfter", config.BackupScheduleOptions{Interval: "1h", DelayAfter: "x"}, "backup.schedule.delayAfter"},
		{"delayAfter shorter than interval", config.BackupScheduleOptions{Interval: "2h", DelayAfter: "119m"}, "shorter than the interval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSchedule(tc.o, jst)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestScheduleAtDaily(t *testing.T) {
	s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", At: "04:00"}, jst)
	latest := time.Date(2026, 10, 10, 4, 0, 0, 0, jst)
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, jst)

	first := s.Start(now, &latest)
	assert.Equal(t, time.Date(2026, 10, 11, 4, 0, 0, 0, jst), first, "a recent generation waits for the next 04:00")
	assert.Equal(t, time.Date(2026, 10, 12, 4, 0, 0, 0, jst), s.Next(first))
	assert.Equal(t, time.Date(2026, 10, 11, 4, 0, 0, 0, jst), s.Next(time.Date(2026, 10, 11, 3, 59, 59, 0, jst)))
	assert.Equal(t, time.Date(2026, 10, 10, 4, 0, 0, 0, jst), s.Next(time.Date(2026, 10, 9, 23, 0, 0, 0, jst)), "slots before the anchor")

	// 03:00 に起動したなら、その日の 04:00 が次。
	s = mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", At: "04:00"}, jst)
	early := time.Date(2026, 10, 10, 3, 0, 0, 0, jst)
	latest = early.Add(-time.Hour)
	assert.Equal(t, time.Date(2026, 10, 10, 4, 0, 0, 0, jst), s.Start(early, &latest))
}

func TestScheduleCatchUp(t *testing.T) {
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, jst)
	for _, at := range []string{"", "04:00"} {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", At: at}, jst)
		assert.Equal(t, now, s.Start(now, nil), "no generation: run now (at=%q)", at)

		s = mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", At: at}, jst)
		old := now.Add(-24 * time.Hour)
		assert.Equal(t, now, s.Start(now, &old), "a generation one interval old: run now (at=%q)", at)
	}
	// At があれば、取り戻した後は 04:00 にそろう。
	s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", At: "04:00"}, jst)
	s.Start(now, nil)
	assert.Equal(t, time.Date(2026, 10, 11, 4, 0, 0, 0, jst), s.Next(now))
}

func TestScheduleWithoutAt(t *testing.T) {
	s := mustSchedule(t, config.BackupScheduleOptions{Interval: "6h"}, jst)
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, jst)
	latest := now.Add(-2 * time.Hour)
	first := s.Start(now, &latest)
	assert.Equal(t, latest.Add(6*time.Hour), first, "the next run is one interval after the newest generation")
	assert.Equal(t, first.Add(6*time.Hour), s.Next(first))
	// 取るのに時間がかかって次の枠を過ぎても、枠はずれない。
	assert.Equal(t, first.Add(12*time.Hour), s.Next(first.Add(7*time.Hour)))
}

func TestScheduleSubDailyWithAt(t *testing.T) {
	s := mustSchedule(t, config.BackupScheduleOptions{Interval: "6h", At: "04:00"}, jst)
	now := time.Date(2026, 10, 10, 11, 0, 0, 0, jst)
	latest := now.Add(-time.Hour)
	assert.Equal(t, time.Date(2026, 10, 10, 16, 0, 0, 0, jst), s.Start(now, &latest))
	assert.Equal(t, time.Date(2026, 10, 10, 22, 0, 0, 0, jst), s.Next(time.Date(2026, 10, 10, 16, 0, 0, 0, jst)))
	assert.Equal(t, time.Date(2026, 10, 10, 4, 0, 0, 0, jst), s.Next(time.Date(2026, 10, 10, 1, 0, 0, 0, jst)), "slots before At on the start day")
}

func TestScheduleMultiDay(t *testing.T) {
	s := mustSchedule(t, config.BackupScheduleOptions{Interval: "72h", At: "04:00"}, jst)
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, jst)
	latest := now.Add(-time.Hour)
	first := s.Start(now, &latest)
	assert.Equal(t, time.Date(2026, 10, 13, 4, 0, 0, 0, jst), first)
	assert.Equal(t, time.Date(2026, 10, 16, 4, 0, 0, 0, jst), s.Next(first))
	assert.Equal(t, time.Date(2026, 10, 10, 4, 0, 0, 0, jst), s.Next(time.Date(2026, 10, 7, 5, 0, 0, 0, jst)))
}

// 再起動しても、枠は最新の世代を取った日から数える (再起動した日へずれない)。
func TestScheduleMultiDayKeepsSlotsAcrossRestart(t *testing.T) {
	s := mustSchedule(t, config.BackupScheduleOptions{Interval: "168h", At: "04:00"}, jst)
	latest := time.Date(2026, 10, 4, 4, 0, 30, 0, jst)
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, jst)
	first := s.Start(now, &latest)
	assert.Equal(t, time.Date(2026, 10, 11, 4, 0, 0, 0, jst), first, "one interval after the newest generation's slot")
	assert.Equal(t, time.Date(2026, 10, 18, 4, 0, 0, 0, jst), s.Next(first))
}

// 保留された枠を日付をまたいでから取った世代でも、起点はその枠の at にする
// (取った日の at にすると 1 日後ろへずれる)。
func TestScheduleMultiDayAnchorsToSlotBeforeLatest(t *testing.T) {
	s := mustSchedule(t, config.BackupScheduleOptions{Interval: "48h", At: "23:30"}, time.UTC)
	latest := time.Date(2026, 10, 10, 0, 30, 0, 0, time.UTC)
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	first := s.Start(now, &latest)
	assert.Equal(t, time.Date(2026, 10, 11, 23, 30, 0, 0, time.UTC), first, "one interval after the 10/09 23:30 slot")
	assert.Equal(t, time.Date(2026, 10, 13, 23, 30, 0, 0, time.UTC), s.Next(first))

	// at ちょうどに取り始めた世代は、その日の at が起点。
	s = mustSchedule(t, config.BackupScheduleOptions{Interval: "48h", At: "23:30"}, time.UTC)
	latest = time.Date(2026, 10, 9, 23, 30, 0, 0, time.UTC)
	assert.Equal(t, time.Date(2026, 10, 11, 23, 30, 0, 0, time.UTC), s.Start(now, &latest))
}

func TestScheduleAtDriftsAcrossRestarts(t *testing.T) {
	for _, tc := range []struct {
		interval, at string
		want         bool
	}{
		{"7h", "04:00", true},
		{"36h", "04:00", true},
		{"5m", "04:00", false},
		{"8h", "04:00", false},
		{"24h", "04:00", false},
		{"72h", "04:00", false},
		{"7h", "", false},
	} {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: tc.interval, At: tc.at}, time.UTC)
		assert.Equal(t, tc.want, s.AtDriftsAcrossRestarts(), "%s at %q", tc.interval, tc.at)
	}
}

func TestScheduleKeepsWallClockAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", At: "04:00"}, ny)
	// 2026-11-01 02:00 に夏時間が終わり、その日は 25 時間ある。
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, ny)
	latest := now.Add(-time.Hour)
	first := s.Start(now, &latest)
	assert.Equal(t, time.Date(2026, 10, 31, 4, 0, 0, 0, ny), first)
	second := s.Next(first)
	assert.Equal(t, time.Date(2026, 11, 1, 4, 0, 0, 0, ny), second)
	third := s.Next(second)
	assert.Equal(t, time.Date(2026, 11, 2, 4, 0, 0, 0, ny), third)
	assert.Equal(t, 25*time.Hour, second.Sub(first), "the slot follows the wall clock, not 24h of elapsed time")
	assert.Equal(t, time.Date(2026, 11, 2, 4, 0, 0, 0, ny), s.Next(time.Date(2026, 11, 1, 4, 30, 0, 0, ny)))
}

func TestFloorDiv(t *testing.T) {
	assert.Equal(t, int64(2), floorDiv(5, 2))
	assert.Equal(t, int64(-3), floorDiv(-5, 2))
	assert.Equal(t, int64(-2), floorDiv(-4, 2))
	assert.Equal(t, int64(0), floorDiv(0, 2))
}
