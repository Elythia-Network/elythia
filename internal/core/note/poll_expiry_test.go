package note_test

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// maxDurationMs is the largest millisecond count that time.Duration can hold
// (about 292 years). Beyond it, Duration(ms) * time.Millisecond overflows.
const maxDurationMs = int64(math.MaxInt64 / int64(time.Millisecond))

func TestPollExpiresAtAfter(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 34, 56, 789_123_456, time.UTC)
	nowMs := now.UnixMilli()
	fraction := 123_456 * time.Nanosecond // sub-millisecond part of now

	tests := []struct {
		name    string
		after   int64
		want    time.Time
		wantErr bool
	}{
		{name: "one millisecond", after: 1, want: now.Add(time.Millisecond)},
		{name: "one hour", after: 3_600_000, want: now.Add(time.Hour)},
		{name: "largest duration", after: maxDurationMs, want: now.Add(time.Duration(maxDurationMs) * time.Millisecond)},
		{name: "just above duration range", after: maxDurationMs + 1, want: time.UnixMilli(nowMs + maxDurationMs + 1).Add(fraction)},
		{name: "exactly the upper limit", after: note.MaxPollExpiresAtUnixMilli - nowMs, want: time.UnixMilli(note.MaxPollExpiresAtUnixMilli).Add(fraction)},
		{name: "one past the upper limit", after: note.MaxPollExpiresAtUnixMilli - nowMs + 1, wantErr: true},
		{name: "int64 max", after: math.MaxInt64, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := note.PollExpiresAtAfter(now, tt.after)
			if tt.wantErr {
				require.ErrorIs(t, err, note.ErrPollExpiryOutOfRange)
				return
			}
			require.NoError(t, err)
			assert.True(t, got.Equal(tt.want), "got %v, want %v", got, tt.want)
			assert.True(t, got.After(now), "deadline must be in the future")
		})
	}
}

func TestPollExpiresAtAfter_KeepsSubMillisecond(t *testing.T) {
	now := time.Now()
	got, err := note.PollExpiresAtAfter(now, 3_600_000)
	require.NoError(t, err)
	assert.True(t, got.Equal(now.Add(time.Hour)), "got %v, want %v", got, now.Add(time.Hour))
}

func TestPollExpiresAtFromUnixMilli(t *testing.T) {
	got, err := note.PollExpiresAtFromUnixMilli(1_700_000_000_000)
	require.NoError(t, err)
	assert.True(t, got.Equal(time.UnixMilli(1_700_000_000_000)))

	got, err = note.PollExpiresAtFromUnixMilli(note.MaxPollExpiresAtUnixMilli)
	require.NoError(t, err)
	assert.Equal(t, "9999-12-30T23:59:59.999Z", got.UTC().Format("2006-01-02T15:04:05.000Z"))

	_, err = note.PollExpiresAtFromUnixMilli(note.MaxPollExpiresAtUnixMilli + 1)
	require.ErrorIs(t, err, note.ErrPollExpiryOutOfRange)
	_, err = note.PollExpiresAtFromUnixMilli(math.MaxInt64)
	require.ErrorIs(t, err, note.ErrPollExpiryOutOfRange)
}

func TestValidatePollExpiresAt(t *testing.T) {
	last := time.UnixMilli(note.MaxPollExpiresAtUnixMilli).Add(999 * time.Microsecond)
	require.NoError(t, note.ValidatePollExpiresAt(last))
	require.NoError(t, note.ValidatePollExpiresAt(time.Now()))
	require.ErrorIs(t, note.ValidatePollExpiresAt(time.UnixMilli(note.MaxPollExpiresAtUnixMilli+1)), note.ErrPollExpiryOutOfRange)
	require.ErrorIs(t, note.ValidatePollExpiresAt(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)), note.ErrPollExpiryOutOfRange)
	require.ErrorIs(t, note.ValidatePollExpiresAt(time.UnixMilli(math.MaxInt64)), note.ErrPollExpiryOutOfRange)
}

// The limit must keep a four-digit year in every time zone: encoding/json
// rejects years beyond 9999 and formats in the time's own location (notes
// export marshals the poll deadline as a time.Time read back from the DB).
func TestMaxPollExpiresAt_IsJSONSerializableInEveryZone(t *testing.T) {
	limit := time.UnixMilli(note.MaxPollExpiresAtUnixMilli).Add(999 * time.Microsecond)
	assert.Equal(t, "9999-12-30T23:59:59.999Z", limit.UTC().Format("2006-01-02T15:04:05.000Z"))
	for _, offsetHours := range []int{-12, 0, 9, 14} {
		zone := time.FixedZone("test", offsetHours*3600)
		_, err := json.Marshal(limit.In(zone))
		require.NoError(t, err, "UTC%+d", offsetHours)
	}
	_, err := json.Marshal(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))
	require.Error(t, err, "encoding/json rejects a five-digit year")
}

// internal/repository の poll_test.go は import の向きの都合で上限を数値で書いている。
// 定数を変えたらあちらも変えるよう、ここで値を固定する。
func TestMaxPollExpiresAtUnixMilli_MatchesRepositoryTest(t *testing.T) {
	assert.Equal(t, int64(253_402_214_399_999), note.MaxPollExpiresAtUnixMilli)
}
