package processors

import (
	"context"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// orphanAttachmentBatchSize bounds one pass so a large backlog cannot hold the
// maintenance worker for an unbounded time.
const orphanAttachmentBatchSize = 200

// orphanAttachmentMaxBatches caps one run (= 200,000 rows).
const orphanAttachmentMaxBatches = 1000

// orphanAttachmentBatchPause spreads the I/O across the run.
const orphanAttachmentBatchPause = 500 * time.Millisecond

// MinOrphanAttachmentGrace is the floor applied to the computed grace period.
//
// **猶予は「まだ印が付いていない窓」を覆うためのもの**で、ephemeral note の
// 寿命そのものを覆う役ではない。添付行を作る `upsertAttachments` と、印を打つ
// `ephemeral.Store.PutNote` は別の処理なので、その間に掃除が走ると印の無い
// 行を消してしまう。窓は 1 リクエストぶんだが、余裕を持って日単位で取る。
const MinOrphanAttachmentGrace = 7 * 24 * time.Hour

// orphanAttachmentTTLFactor scales the configured ephemeral TTL when it is
// long enough to exceed the floor.
//
// **TTL は寿命の上限ではない。** `Store.Touch` (`/api/notes/show` の経路) が
// TTL を打ち直すので、開かれ続けるノートは無限に生き延びる。したがって猶予をいくら伸ばしても
// 「TTL より長ければ安全」にはならず、生存判定は Redis の印 (LiveFileIDs) が
// 担う。この倍率は、印の書き込みが落ちた構成での取りこぼしを減らすための
// 保険にすぎない。
const orphanAttachmentTTLFactor = 8

// OrphanAttachmentCleanerConfig holds the settings for the cleanup job.
type OrphanAttachmentCleanerConfig struct {
	// Enabled gates the whole job. 呼び出し側が
	// 「ephemeral relay notes が有効」または「リレー孤児 user 掃除が有効」で
	// 立てる。どちらも owner 無しのリモート添付を作る側。
	Enabled bool
	// EphemeralTTL is the configured ephemeral note TTL (0 なら既定扱い)。
	EphemeralTTL time.Duration
}

// OrphanAttachmentCleaner is the repository surface the job needs.
type OrphanAttachmentCleaner interface {
	ListOrphanRemoteAttachmentCandidates(cutoffID, afterID string, limit int) ([]string, error)
	DeleteByIDs(ids []string) (int64, error)
}

// EphemeralFileChecker reports which drive file IDs a live ephemeral note still
// references. Implemented by ephemeral.Store.
type EphemeralFileChecker interface {
	LiveFileIDs(ctx context.Context, ids []string) (map[string]bool, error)
}

// CutoffIDFunc builds an ID for the given time under the configured ID scheme.
// 設定の generator をそのまま使う (aidx 固定にしない)。
//
// **共有 generator をそのまま渡さないこと。** `ulid` の monotonic reader は
// 過去時刻で呼ぶと内部の基準時刻を巻き戻すので、他の採番と同じインスタンスを
// 使うと直後の単調性が崩れる。呼び出し側で専用のインスタンスを作る。
//
// 設定の `id` を運用途中で変えた場合、旧方式で採番された行は新方式の cutoff と
// 辞書順で比較できない。方式ごとに「常に対象外」か「年齢を無視して対象」の
// どちらかに倒れるので、変えるなら掃除を止めてから行う。
type CutoffIDFunc func(t time.Time) string

// OrphanAttachmentCleanupProcessor deletes owner-less remote link attachments
// that neither a database note nor a live ephemeral note references (#2722).
//
// 著者が materialize されていないリモート添付は owner 無しで保存される
// (#2717)。ephemeral note 自体は Redis の TTL で消えるのに、この drive_file 行
// には寿命が無く、リレー購読の量に比例して積み上がる。
//
// **「owner 無し = ゴミ」ではない。** materialize (`ephemeral.Materializer`) は
// drive_file.userId を埋めないし、`upsertAttachments` の dedup は既存行を
// **年齢に関係なく**再利用する。行の作成時刻から猶予を測るだけでは、8 日前の
// 行が今日届いた ephemeral note に結び直された場合にその晩で消える。
// 表示中の添付を守るのは次の 2 つ:
//
//   - DB 側: repository の「どの note からも参照されていない」述語
//   - Redis 側: ephemeral note が生きている間だけ立つ印 (LiveFileIDs)
type OrphanAttachmentCleanupProcessor struct {
	repo      OrphanAttachmentCleaner
	ephemeral EphemeralFileChecker
	cfg       func() OrphanAttachmentCleanerConfig
	cutoffID  CutoffIDFunc
	now       func() time.Time
	nowSlow   func(time.Duration)
}

// NewOrphanAttachmentCleanupProcessor constructs the processor. cfg は meta を
// 都度読むための関数 (起動時固定にすると管理画面の変更が再起動まで効かない)。
func NewOrphanAttachmentCleanupProcessor(repo OrphanAttachmentCleaner, ephemeral EphemeralFileChecker, cfg func() OrphanAttachmentCleanerConfig, cutoffID CutoffIDFunc) *OrphanAttachmentCleanupProcessor {
	return &OrphanAttachmentCleanupProcessor{
		repo: repo, ephemeral: ephemeral, cfg: cfg, cutoffID: cutoffID,
		now: time.Now, nowSlow: time.Sleep,
	}
}

// grace returns the retention window for owner-less remote attachments.
func (p *OrphanAttachmentCleanupProcessor) grace(cfg OrphanAttachmentCleanerConfig) time.Duration {
	scaled := cfg.EphemeralTTL * orphanAttachmentTTLFactor
	if scaled > MinOrphanAttachmentGrace {
		return scaled
	}
	return MinOrphanAttachmentGrace
}

// Handle implements the driver handler contract.
func (p *OrphanAttachmentCleanupProcessor) Handle(ctx context.Context, _ driver.Task) error {
	if p == nil || p.repo == nil || p.cfg == nil || p.cutoffID == nil {
		return nil
	}
	cfg := p.cfg()
	if !cfg.Enabled {
		return nil
	}
	grace := p.grace(cfg)
	cutoff := p.cutoffID(p.now().Add(-grace))
	if cutoff == "" {
		// generator が空を返すことは無いが、空 cutoff は repository 側で
		// 「全件が対象」に化けうる形なので念のため止める。
		slog.Warn("orphanAttachmentCleanup: empty cutoff id, skipping")
		return nil
	}

	var total int64
	after := ""
	for i := 0; i < orphanAttachmentMaxBatches; i++ {
		if ctx.Err() != nil {
			break
		}
		candidates, err := p.repo.ListOrphanRemoteAttachmentCandidates(cutoff, after, orphanAttachmentBatchSize)
		if err != nil {
			slog.Error("orphanAttachmentCleanup: list failed", "err", err)
			return err
		}
		if len(candidates) == 0 {
			break
		}
		// **cursor は「消した行」ではなく「見た行」で進める。** 残す行で
		// 止めると次のバッチが同じ行から始まり、実行が終わらない。
		after = candidates[len(candidates)-1]

		deletable, err := p.excludeLive(ctx, candidates)
		if err != nil {
			slog.Error("orphanAttachmentCleanup: ephemeral check failed", "err", err)
			return err
		}
		if len(deletable) > 0 {
			deleted, err := p.repo.DeleteByIDs(deletable)
			if err != nil {
				slog.Error("orphanAttachmentCleanup: delete failed", "err", err)
				return err
			}
			total += deleted
		}
		if len(candidates) < orphanAttachmentBatchSize {
			break
		}
		p.nowSlow(orphanAttachmentBatchPause)
	}
	if total > 0 {
		slog.Info("orphanAttachmentCleanup: completed", "deleted", total, "grace", grace)
	}
	return nil
}

// excludeLive drops the IDs a live ephemeral note still references.
func (p *OrphanAttachmentCleanupProcessor) excludeLive(ctx context.Context, ids []string) ([]string, error) {
	if p.ephemeral == nil {
		// **未配線なら 1 件も消さない。** ephemeral store が無い構成では
		// この形の行も作られないので、消す物が無いのが正しい。「印が無い =
		// ゴミ」と倒すと、配線を落とした瞬間に全部消える。
		return nil, nil
	}
	live, err := p.ephemeral.LiveFileIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if live[id] {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}
