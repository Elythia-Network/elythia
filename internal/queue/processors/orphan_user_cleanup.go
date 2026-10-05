package processors

import (
	"context"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// orphanUserBatchSize bounds one DELETE so a large backlog cannot hold the
// maintenance worker for an unbounded time.
const orphanUserBatchSize = 200

// orphanUserMaxBatches caps one run (= 200,000 rows).
const orphanUserMaxBatches = 1000

// orphanUserBatchPause spreads the I/O across the run.
const orphanUserBatchPause = 500 * time.Millisecond

// MinOrphanUserGraceDays is the floor applied to the configured grace period.
//
// ResolveActor は actorTTL (24 時間) を超えたときに再フェッチして
// `lastFetchedAt` を更新する。猶予をそれに近い値にすると、活動中の著者が
// 「削除 → 再フェッチ」を繰り返すチャーンになる。十分に離す。
const MinOrphanUserGraceDays = 7

// OrphanUserCleanerConfig holds the settings for the cleanup job.
type OrphanUserCleanerConfig struct {
	Enabled   bool
	GraceDays int
}

// OrphanUserCleaner is the repository surface the job needs.
type OrphanUserCleaner interface {
	DeleteOrphanRemoteUsers(graceDays, batchSize int) (int64, error)
}

// OrphanUserCleanupProcessor deletes relay-derived remote users that nothing
// references (#2340).
//
// リレー転送の LD-Signature 検証は creator の公開鍵を DB に載せる必要があるため
// (転送活動の唯一の認証手段)、その経路の著者だけは DB に残る。ノートを Redis に
// 逃がしても著者が積み上がるので、後追いで回収する。
//
// **対象はリレー経由で初めて観測した行だけ。** リレー購読前から居る行や、
// プロフィール閲覧・スレッド遡りで解決された行は、孤児であっても意図して観測
// したものなので消さない。
type OrphanUserCleanupProcessor struct {
	repo    OrphanUserCleaner
	cfg     func() OrphanUserCleanerConfig
	nowSlow func(time.Duration)
}

// NewOrphanUserCleanupProcessor constructs the processor. cfg は meta を都度
// 読むための関数 (起動時固定にすると管理画面の変更が再起動まで効かない)。
func NewOrphanUserCleanupProcessor(repo OrphanUserCleaner, cfg func() OrphanUserCleanerConfig) *OrphanUserCleanupProcessor {
	return &OrphanUserCleanupProcessor{repo: repo, cfg: cfg, nowSlow: time.Sleep}
}

// Handle implements the driver handler contract.
func (p *OrphanUserCleanupProcessor) Handle(ctx context.Context, _ driver.Task) error {
	if p == nil || p.repo == nil || p.cfg == nil {
		return nil
	}
	cfg := p.cfg()
	if !cfg.Enabled {
		return nil
	}
	grace := cfg.GraceDays
	if grace < MinOrphanUserGraceDays {
		// 設定が短すぎるとチャーンになるので下限で丸める。無効化ではなく丸めに
		// するのは、運用者が 0 を入れて意図せず毎時間削除される事故を防ぐため。
		grace = MinOrphanUserGraceDays
	}

	var total int64
	for i := 0; i < orphanUserMaxBatches; i++ {
		if ctx.Err() != nil {
			break
		}
		deleted, err := p.repo.DeleteOrphanRemoteUsers(grace, orphanUserBatchSize)
		if err != nil {
			slog.Error("orphanUserCleanup: delete failed", "err", err)
			return err
		}
		total += deleted
		if deleted < orphanUserBatchSize {
			break
		}
		p.nowSlow(orphanUserBatchPause)
	}
	if total > 0 {
		slog.Info("orphanUserCleanup: completed", "deleted", total, "graceDays", grace)
	}
	return nil
}
