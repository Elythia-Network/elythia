package processors

import (
	"context"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// ExpiredMutingPruner is the narrow interface the checkExpiredMutings job
// needs. repository.MutingRepository satisfies it (DeleteExpired).
//
// The returned slice holds the owner of every pruned row (muterId for user
// mutes, userId for channel mutes) with one entry per row.
type ExpiredMutingPruner interface {
	DeleteExpired(now time.Time) ([]string, error)
}

// RelationReloadPublisher notifies a user's live streaming connections that
// their mute/block snapshot must be rebuilt (#2400).
//
// 循環依存を避けるため interface で受け取る (実装は server の adapter)。
type RelationReloadPublisher interface {
	PublishMuteBlockReload(userID string)
}

// CheckExpiredMutingsProcessor physically removes user- and channel-muting
// rows whose expiresAt has passed, mirroring upstream
// CheckExpiredMutingsProcessorService (cron `*/5 * * * *`, #1563 / #1603)。
// read filter は既に期限切れ行を除外するので REST 経路への影響は無く、DB hygiene の
// ための能動 prune。mk-go には redis mute cache が無いため cache refresh は不要。
//
// ただし **streaming だけは prune の通知が要る** (#2453)。接続中の connection は
// mute 集合を接続時に 1 回読むだけなので、失効を伝えないと再接続まで mute された
// ままになる。利用者の操作 (unmute 等) には publisher が付いているが、期限切れは
// 誰も操作しないのでここが唯一の通知点。
type CheckExpiredMutingsProcessor struct {
	userMutes      ExpiredMutingPruner
	channelMutes   ExpiredMutingPruner
	relationReload RelationReloadPublisher
}

// NewCheckExpiredMutingsProcessor constructs a processor. Either pruner may be
// nil to disable that half (no-op) so callers can wire a subset.
func NewCheckExpiredMutingsProcessor(userMutes, channelMutes ExpiredMutingPruner) *CheckExpiredMutingsProcessor {
	return &CheckExpiredMutingsProcessor{userMutes: userMutes, channelMutes: channelMutes}
}

// SetRelationReloadPublisher wires the streaming reload publisher (#2453)。
// 未配線なら通知しない (= 従来どおり再接続まで stale)。
func (p *CheckExpiredMutingsProcessor) SetRelationReloadPublisher(pub RelationReloadPublisher) {
	p.relationReload = pub
}

// Handle implements the driver handler contract.
func (p *CheckExpiredMutingsProcessor) Handle(_ context.Context, _ driver.Task) error {
	now := time.Now()
	// 失敗しても nil を返して success 扱い。MaxRetry(0) で retry させない方針で、
	// 次回 cron (5 分後) を待てば自然に再 prune される。
	//
	// user mute と channel mute はどちらも同じ MuteBlockSnapshot に載るので、
	// 通知先は片方ずつ publish せず両方 prune してからまとめて 1 回にする。
	// 同じ利用者の user mute と channel mute が同時に失効したときに reload を
	// 2 回飛ばさないため (reload 1 回で snapshot 全体を取り直す)。
	affected := map[string]struct{}{}
	prune := func(kind string, r ExpiredMutingPruner) {
		if r == nil {
			return
		}
		owners, err := r.DeleteExpired(now)
		if err != nil {
			slog.Warn("checkExpiredMutings: prune failed", "kind", kind, "err", err)
			return
		}
		if len(owners) > 0 {
			slog.Info("checkExpiredMutings: pruned expired mutings", "kind", kind, "count", len(owners))
		}
		for _, id := range owners {
			if id != "" {
				affected[id] = struct{}{}
			}
		}
	}
	prune("user", p.userMutes)
	prune("channel", p.channelMutes)

	if p.relationReload == nil {
		return nil
	}
	// 失効した利用者の数だけ publish する。まとめて 1 メッセージにはしない。
	// reload の受け側 (stream.RefreshRelations) は**接続を持たない利用者を
	// snapshot 取得の前に弾く**ので、オフラインぶんは pubsub メッセージ 1 件で
	// 済み、DB 往復は起きない。実コストは「今つないでいる人のうち mute が
	// 失効した人」に比例する。
	for userID := range affected {
		p.relationReload.PublishMuteBlockReload(userID)
	}
	return nil
}
