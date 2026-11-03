package processors

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// AnnouncementArchiver is the narrow interface the archiveAnnouncements job
// needs. repository.AnnouncementRepository satisfies it.
type AnnouncementArchiver interface {
	ArchiveExpired(now time.Time) (int64, error)
	ListScheduledArchives(now time.Time) ([]*model.Announcement, error)
}

// AnnouncementArchiveScheduler schedules the archive job of one announcement
// at its autoArchiveAt (#3994). queue.Client satisfies it.
type AnnouncementArchiveScheduler interface {
	ScheduleAnnouncementArchive(id string, at time.Time) error
}

// ArchiveAnnouncementsProcessor sets isActive to false on announcements whose
// autoArchiveAt has passed (#3809), the work upstream 2026.10.1 does in its
// per-announcement archiveAnnouncement job.
//
// 本家と同じく、お知らせごとに期限の時刻の遅延ジョブを積む (#3994)。ジョブは payload
// のお知らせだけでなく、その時点で期限を過ぎた行をまとめて落とす。期限は UPDATE の
// 時点で保存されている値と比べるので、期限を変えた・消した・お知らせを消した後に
// 古いジョブが走っても、その行には何もしない。同じ行のジョブが重なっても、2 回目は
// 落とす行が無い。API は読むときにも期限を見ているので、ここが遅れても止まっても
// 見え方は変わらない (変わるのは DB の isActive と updatedAt)。
type ArchiveAnnouncementsProcessor struct {
	repo      AnnouncementArchiver
	scheduler AnnouncementArchiveScheduler
	clock     func() time.Time
}

// NewArchiveAnnouncementsProcessor constructs a processor. A nil repo makes
// Handle and RunAtStartup no-ops.
func NewArchiveAnnouncementsProcessor(repo AnnouncementArchiver) *ArchiveAnnouncementsProcessor {
	return &ArchiveAnnouncementsProcessor{repo: repo, clock: time.Now}
}

// SetScheduler sets where RunAtStartup schedules the archive jobs of the
// announcements not yet due (#3994). nil disables the scheduling.
func (p *ArchiveAnnouncementsProcessor) SetScheduler(s AnnouncementArchiveScheduler) {
	p.scheduler = s
}

// Handle implements the driver handler contract. The job the startup puts
// (payload atStartup) runs RunAtStartup.
//
// 失敗は error で返し、queue に試し直させる (#3994)。毎分の cron だった頃は次の回が
// 拾ったが、今は期限の時刻に 1 回しか走らない。
func (p *ArchiveAnnouncementsProcessor) Handle(ctx context.Context, t driver.Task) error {
	if p.repo == nil {
		return nil
	}
	if atStartup(t) {
		return p.RunAtStartup(ctx)
	}
	return p.archive(p.clock())
}

// atStartup reports whether t is the job put at startup (payload
// `{"atStartup":true}`, #3994). 期限の時刻のジョブ・保険の cron (payload 無し)・
// 読めない payload は、どれも期限の来た行を処理するだけにする。
func atStartup(t driver.Task) bool {
	if t == nil || len(t.Payload()) == 0 {
		return false
	}
	var p struct {
		AtStartup bool `json:"atStartup"`
	}
	return json.Unmarshal(t.Payload(), &p) == nil && p.AtStartup
}

func (p *ArchiveAnnouncementsProcessor) archive(now time.Time) error {
	n, err := p.repo.ArchiveExpired(now)
	if err != nil {
		return fmt.Errorf("archiveAnnouncements: archive: %w", err)
	}
	if n > 0 {
		slog.Info("archiveAnnouncements: archived announcements", "count", n)
	}
	return nil
}

// RunAtStartup archives the announcements whose autoArchiveAt passed while no
// job ran (the process was down, or a job was lost or never scheduled), and
// schedules again the jobs of the announcements not yet due (#3994).
//
// 本家 AnnouncementService.onModuleInit (Redis のジョブが失われても予約を復元
// する) と同じ役目。複数のプロセスが同時に流しても、落とす UPDATE は一度しか
// 効かず、積むジョブは queue 側で重ならない。
func (p *ArchiveAnnouncementsProcessor) RunAtStartup(_ context.Context) error {
	if p.repo == nil {
		return nil
	}
	now := p.clock()
	if err := p.archive(now); err != nil {
		return err
	}
	if p.scheduler == nil {
		return nil
	}
	rows, err := p.repo.ListScheduledArchives(now)
	if err != nil {
		return fmt.Errorf("archiveAnnouncements: list scheduled: %w", err)
	}
	for _, a := range rows {
		if a.AutoArchiveAt == nil {
			continue
		}
		if err := p.scheduler.ScheduleAnnouncementArchive(a.ID, *a.AutoArchiveAt); err != nil {
			// 1 件の失敗で残りを止めない。積めなかった行は保険の日次の cron が落とす。
			slog.Warn("archiveAnnouncements: schedule", "announcementId", a.ID, "err", err)
		}
	}
	return nil
}
