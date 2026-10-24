package processors

import (
	"context"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// AnnouncementArchiver is the narrow interface the archiveAnnouncements job
// needs. repository.AnnouncementRepository satisfies it (ArchiveExpired).
type AnnouncementArchiver interface {
	ArchiveExpired(now time.Time) (int64, error)
}

// ArchiveAnnouncementsProcessor sets isActive to false on announcements whose
// autoArchiveAt has passed (#3809), the work upstream 2026.10.1 does in its
// per-announcement archiveAnnouncement job.
//
// 本家はお知らせごとに遅延ジョブを予約し、起動時に予約し直す。Elythia は定期処理で
// 期限を過ぎた行をまとめて落とすので、予約の取り消し・作り直しや、Redis から
// ジョブが消えたときの復元が要らない。API は読むときに期限を見ているので、ここが
// 遅れても止まっても見え方は変わらない (変わるのは DB の isActive と updatedAt)。
type ArchiveAnnouncementsProcessor struct {
	repo  AnnouncementArchiver
	clock func() time.Time
}

// NewArchiveAnnouncementsProcessor constructs a processor. A nil repo makes
// Handle a no-op.
func NewArchiveAnnouncementsProcessor(repo AnnouncementArchiver) *ArchiveAnnouncementsProcessor {
	return &ArchiveAnnouncementsProcessor{repo: repo, clock: time.Now}
}

// Handle implements the driver handler contract.
func (p *ArchiveAnnouncementsProcessor) Handle(_ context.Context, _ driver.Task) error {
	if p.repo == nil {
		return nil
	}
	n, err := p.repo.ArchiveExpired(p.clock())
	if err != nil {
		// 失敗しても success 扱いにする。MaxRetry(0) で再試行させず、次の回 (1 分後) が
		// 同じ行を拾う。
		slog.Warn("archiveAnnouncements: archive failed", "err", err)
		return nil
	}
	if n > 0 {
		slog.Info("archiveAnnouncements: archived announcements", "count", n)
	}
	return nil
}
