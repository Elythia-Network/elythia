package processors

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeAnnouncementArchiver struct {
	calls     []time.Time
	n         int64
	err       error
	scheduled []*model.Announcement
	listErr   error
	listedAt  []time.Time
}

func (f *fakeAnnouncementArchiver) ArchiveExpired(now time.Time) (int64, error) {
	f.calls = append(f.calls, now)
	return f.n, f.err
}

func (f *fakeAnnouncementArchiver) ListScheduledArchives(now time.Time) ([]*model.Announcement, error) {
	f.listedAt = append(f.listedAt, now)
	return f.scheduled, f.listErr
}

type fakeArchiveScheduler struct {
	got []string // "<id> <RFC 3339 time>"
	err error
}

func (s *fakeArchiveScheduler) ScheduleAnnouncementArchive(id string, at time.Time) error {
	s.got = append(s.got, id+" "+at.UTC().Format(time.RFC3339Nano))
	return s.err
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
		assert.Empty(t, repo.listedAt, "Handle does not reschedule")
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

// 期限の時刻に 1 回しか走らないので、失敗は queue に試し直させる (#3994)。
func TestArchiveAnnouncements_ErrorIsReturnedForRetry(t *testing.T) {
	repo := &fakeAnnouncementArchiver{err: errors.New("boom")}
	assert.Error(t, NewArchiveAnnouncementsProcessor(repo).Handle(context.Background(), driver.RawTask{TypeName: "test"}))
	assert.Len(t, repo.calls, 1)
}

func TestArchiveAnnouncements_NilRepoNoOp(t *testing.T) {
	p := NewArchiveAnnouncementsProcessor(nil)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{TypeName: "test"}))
	require.NoError(t, p.RunAtStartup(context.Background()))
}

// 起動時は、期限を過ぎた行を落としてから、まだ来ていない行のジョブを積み直す (#3994)。
func TestArchiveAnnouncements_RunAtStartup(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	at1, at2 := now.Add(time.Hour), now.Add(48*time.Hour)
	repo := &fakeAnnouncementArchiver{scheduled: []*model.Announcement{
		{ID: "a1", AutoArchiveAt: &at1},
		{ID: "skip"},
		{ID: "a2", AutoArchiveAt: &at2},
	}}
	sched := &fakeArchiveScheduler{}
	p := NewArchiveAnnouncementsProcessor(repo)
	p.SetScheduler(sched)
	p.clock = func() time.Time { return now }

	require.NoError(t, p.RunAtStartup(context.Background()))
	require.Len(t, repo.calls, 1, "archives the overdue rows")
	assert.True(t, repo.calls[0].Equal(now))
	require.Len(t, repo.listedAt, 1)
	assert.True(t, repo.listedAt[0].Equal(now))
	assert.Equal(t, []string{
		"a1 " + at1.Format(time.RFC3339Nano),
		"a2 " + at2.Format(time.RFC3339Nano),
	}, sched.got)
}

func TestArchiveAnnouncements_RunAtStartupFailures(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	at := now.Add(time.Hour)

	// 落とすのに失敗したら、そのまま返す。
	repo := &fakeAnnouncementArchiver{err: errors.New("boom")}
	p := NewArchiveAnnouncementsProcessor(repo)
	p.SetScheduler(&fakeArchiveScheduler{})
	assert.Error(t, p.RunAtStartup(context.Background()))
	assert.Empty(t, repo.listedAt)

	// 一覧を読めなければ返す。
	repo = &fakeAnnouncementArchiver{listErr: errors.New("boom")}
	p = NewArchiveAnnouncementsProcessor(repo)
	p.SetScheduler(&fakeArchiveScheduler{})
	assert.Error(t, p.RunAtStartup(context.Background()))

	// 1 件積めなくても残りを積む。
	repo = &fakeAnnouncementArchiver{scheduled: []*model.Announcement{{ID: "a1", AutoArchiveAt: &at}, {ID: "a2", AutoArchiveAt: &at}}}
	sched := &fakeArchiveScheduler{err: errors.New("redis down")}
	p = NewArchiveAnnouncementsProcessor(repo)
	p.SetScheduler(sched)
	require.NoError(t, p.RunAtStartup(context.Background()))
	assert.Len(t, sched.got, 2)

	// 積む先が無ければ落とすだけ。
	repo = &fakeAnnouncementArchiver{scheduled: []*model.Announcement{{ID: "a1", AutoArchiveAt: &at}}}
	require.NoError(t, NewArchiveAnnouncementsProcessor(repo).RunAtStartup(context.Background()))
	assert.Len(t, repo.calls, 1)
	assert.Empty(t, repo.listedAt)
}

// 起動時に積むジョブ (payload atStartup) は RunAtStartup を流す。期限の時刻のジョブ・
// 保険の cron・読めない payload は、期限の来た行を落とすだけ (#3994)。
func TestArchiveAnnouncements_HandleDispatchesTheStartupJob(t *testing.T) {
	at := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name    string
		task    driver.Task
		resched bool
	}{
		{"startup", driver.RawTask{TypeName: "t", Body: []byte(`{"atStartup":true}`)}, true},
		{"due job", driver.RawTask{TypeName: "t", Body: []byte(`{"announcementId":"a1","autoArchiveAt":1}`)}, false},
		{"cron", driver.RawTask{TypeName: "t"}, false},
		{"broken payload", driver.RawTask{TypeName: "t", Body: []byte(`{`)}, false},
		{"nil task", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeAnnouncementArchiver{scheduled: []*model.Announcement{{ID: "a1", AutoArchiveAt: &at}}}
			sched := &fakeArchiveScheduler{}
			p := NewArchiveAnnouncementsProcessor(repo)
			p.SetScheduler(sched)
			require.NoError(t, p.Handle(context.Background(), tc.task))
			assert.Len(t, repo.calls, 1, "always archives what is due")
			if tc.resched {
				assert.Len(t, sched.got, 1)
			} else {
				assert.Empty(t, sched.got)
			}
		})
	}
}
