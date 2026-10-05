package backfill

import "github.com/elythia-network/elythia/internal/cli/cliflag"

// NoteTags implements "elythia backfill note-tags": it normalizes existing
// note.tags to the NFKC + lowercase form that NoteCreateService now stores for
// new notes (#1948-18 / #2013). It is needed because search-by-tag normalizes
// the query, so old uppercase / full-width tags would otherwise stop matching.
//
// 本番 (大きな note テーブル) では負荷を考慮し、-batch / -sleep-ms で速度を
// 絞り、maintenance window で段階実行すること。-from で中断地点から再開できる。
// 冪等なので途中失敗しても -from で安全に再開でき、再実行しても既正規化 note は
// touch しない。まず -dry-run で更新件数を見積もってから実行するのを推奨する。
//
// 性能上の注意: WHERE 句の `cardinality(tags) > 0` に対応する index は無いため、
// tagged note が疎なテーブルでは LIMIT 前に空 tag 行の heap filter が走り、各 batch の
// レイテンシが膨らみうる。事前に -dry-run で総走査コストを実測し、必要なら一時的に
// `CREATE INDEX CONCURRENTLY ... ON note (id) WHERE cardinality(tags) > 0` を貼って
// から実行 (完了後 DROP) することを検討する。
//
// なお >32 個の case-variant 重複 tag を持つ古い note では、backfill 結果が「今 fresh
// に作成した場合の値」と完全一致しないことがある (旧 Extract は case-insensitive dedup、
// 現 NormalizeNoteTags は case-sensitive dedup)。全 tag は正しく正規化され検索可能で、
// 余分に残るのは marginal tag のみ (検索上は寛容側) なのでデータ破損ではない。
//
//	elythia backfill note-tags -config .config/default.yml -dry-run
//	elythia backfill note-tags -config .config/default.yml -batch 1000 -sleep-ms 200
//	elythia backfill note-tags -config .config/default.yml -from <last-id>   # 再開
func NoteTags(args []string) int { return noteTags(defaultEnv(), args) }

func noteTags(e env, args []string) int {
	fs, cfgPath := newFlags(e, "note-tags")
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
	var totalScanned, totalUpdated int
	for {
		res, err := e.noteTagsBatch(db, cursor, *batchSize, *dryRun)
		if err != nil {
			e.logger.Printf("backfill batch (cursor=%q): %v", cursor, err)
			return 1
		}
		if res.Scanned == 0 {
			break
		}
		cursor = res.LastID
		totalScanned += res.Scanned
		totalUpdated += res.Updated
		e.logger.Printf("batch scanned=%d updated=%d cursor=%s (total scanned=%d updated=%d)",
			res.Scanned, res.Updated, cursor, totalScanned, totalUpdated)
		pause(e, *sleepMs)
	}
	e.logger.Printf("done [%s]: scanned=%d updated=%d",
		modeLabel(*dryRun, "dry-run (no writes)"), totalScanned, totalUpdated)
	return 0
}
