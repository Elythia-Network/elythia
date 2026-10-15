package backfill

import (
	"github.com/elythia-network/elythia/internal/cli/cliflag"
	"github.com/elythia-network/elythia/internal/maintenance"
)

// HashtagMentions implements "elythia backfill hashtag-mentions": it drops,
// from the hashtag table, the users that were counted as mentioning a tag only
// through followers / specified notes. Builds that counted every visibility
// left those entries behind; newer builds count public and home notes only.
//
// 対象は公開範囲を限ったタグ付きのノートだけなので、多くの場合は短く終わる。
// まず -dry-run で消す件数を見てから実行するのを推奨する。冪等で、-from で
// 中断地点から再開できる。
//
//	elythia backfill hashtag-mentions -config .config/default.yml -dry-run
//	elythia backfill hashtag-mentions -config .config/default.yml -batch 1000 -sleep-ms 100
//	elythia backfill hashtag-mentions -config .config/default.yml -from <last-id>   # 再開
func HashtagMentions(args []string) int { return hashtagMentions(defaultEnv(), args) }

func hashtagMentions(e env, args []string) int {
	fs, cfgPath := newFlags(e, "hashtag-mentions")
	batchSize := fs.Int("batch", 1000, "notes per keyset batch")
	sleepMs := fs.Int("sleep-ms", 100, "sleep between batches to limit DB load")
	fromID := fs.String("from", "", "resume from note id (exclusive)")
	dryRun := fs.Bool("dry-run", false, "count changes without writing")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}

	_, db, ok := open(e, *cfgPath)
	if !ok {
		return 1
	}

	cursor := *fromID
	// 同じ (作者, タグ) の組が複数のバッチに出ても 1 回だけ扱うよう、実行全体で持つ。
	seen := maintenance.HashtagMentionsSeen{}
	var totalScanned, totalRemoved, totalDeleted int
	for {
		res, err := e.hashtagMentionsBatch(db, cursor, *batchSize, *dryRun, seen)
		if err != nil {
			e.logger.Printf("backfill batch (cursor=%q): %v", cursor, err)
			return 1
		}
		if res.Scanned == 0 {
			break
		}
		cursor = res.LastID
		totalScanned += res.Scanned
		totalRemoved += res.Removed
		totalDeleted += res.Deleted
		e.logger.Printf("batch scanned=%d removed=%d deleted=%d cursor=%s (total scanned=%d removed=%d deleted=%d)",
			res.Scanned, res.Removed, res.Deleted, cursor, totalScanned, totalRemoved, totalDeleted)
		pause(e, *sleepMs)
	}
	e.logger.Printf("done [%s]: scanned=%d removed=%d deleted=%d",
		modeLabel(*dryRun, "dry-run (no writes)"), totalScanned, totalRemoved, totalDeleted)
	return 0
}
