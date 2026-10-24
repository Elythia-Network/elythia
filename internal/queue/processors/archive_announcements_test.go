package processors

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeAnnouncementArchiver struct {
	calls []time.Time
	n     int64
	err   error
}

func (f *fakeAnnouncementArchiver) ArchiveExpired(now time.Time) (int64, error) {
	f.calls = append(f.calls, now)
	return f.n, f.err
}

func TestArchiveAnnouncements_ArchivesAtTheCurrentTime(t *testing.T) {
	for _, n := range []int64{0, 3} {
		repo := &fakeAnnouncementArchiver{n: n}
		proc := NewArchiveAnnouncementsProcessor(repo)
		now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
		proc.clock = func() time.Time { return now }

		require.NoError(t, proc.Handle(context.Background(), driver.RawTask{TypeName: "test"}))
		require.Len(t, repo.calls, 1)
		assert.True(t, repo.calls[0].Equal(now), "期限は処理した時刻と比べる")
	}
}

func TestArchiveAnnouncements_DefaultClockIsNow(t *testing.T) {
	repo := &fakeAnnouncementArchiver{}
	before := time.Now()
	require.NoError(t, NewArchiveAnnouncementsProcessor(repo).Handle(context.Background(), driver.RawTask{TypeName: "test"}))
	require.Len(t, repo.calls, 1)
	assert.False(t, repo.calls[0].Before(before))
	assert.False(t, repo.calls[0].After(time.Now()))
}

// 失敗は次の回が拾うので、再試行させず success 扱いにする。
func TestArchiveAnnouncements_ErrorSwallowed(t *testing.T) {
	repo := &fakeAnnouncementArchiver{err: errors.New("boom")}
	require.NoError(t, NewArchiveAnnouncementsProcessor(repo).Handle(context.Background(), driver.RawTask{TypeName: "test"}))
	assert.Len(t, repo.calls, 1)
}

func TestArchiveAnnouncements_NilRepoNoOp(t *testing.T) {
	require.NoError(t, NewArchiveAnnouncementsProcessor(nil).Handle(context.Background(), driver.RawTask{TypeName: "test"}))
}
