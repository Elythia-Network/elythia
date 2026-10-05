package backfill

import (
	"github.com/elythia-network/elythia/internal/cli/cliflag"
	"github.com/elythia-network/elythia/internal/maintenance"
)

// RemoteHost implements "elythia backfill remote-host": it normalizes every
// stored remote host to the form hostFromURI now produces (UTS#46 mapping +
// punycode, lowercase) — #2706. Rows stored by builds that punycoded without
// the UTS#46 mapping (`ｅｖｉｌ.example` → `xn--qi7ciaj2b.example`) are folded
// back as well.
//
// 既存行は `url.Parse` の生の host で保存されており、`Mixed.Example` のような表記の
// まま残る。読み取り側の両当たりは #2996 で撤去したので acct 解決からも引けず、連合ゲート
// (blocked / silenced host) や timeline の instance-mute は完全一致なので取りこぼす。
//
// **SQL migration では書けない。** PostgreSQL に IDNA 変換が無く、`lower()` だけでは
// `パイ.example` → `xn--eckve.example` を作れない (note-tags が NFKC で同じ理由)。
//
// 冪等なので途中で失敗しても再実行して安全。まず -dry-run で件数を見積もること。
// **-dry-run では conflicts を数えられない** (UPDATE を撃たないため。常に 0 が出る)。
// 衝突は本実行で初めて分かる。
// -table / -column で 1 組だけ流すこともできる (中断したところから再開する用途)。
//
// **conflicts が出たら手当てが要る。** 同じリモートが表記違いで 2 行に増えている
// 場合、正規化すると一意制約に当たる。マージは FK の張り替えが要るのでこのバッチでは
// やらない。衝突した行は key / host を個別にログへ出すので、それを見て判断すること。
//
//	elythia backfill remote-host -config .config/default.yml -dry-run
//	elythia backfill remote-host -config .config/default.yml -batch 1000 -sleep-ms 200
//	elythia backfill remote-host -config .config/default.yml -table user -from <last-key>
func RemoteHost(args []string) int { return remoteHost(defaultEnv(), args) }

func remoteHost(e env, args []string) int {
	fs, cfgPath := newFlags(e, "remote-host")
	batchSize := fs.Int("batch", 1000, "rows per keyset batch")
	sleepMs := fs.Int("sleep-ms", 100, "sleep between batches to limit DB load")
	fromKey := fs.String("from", "", "resume from this keyset value (exclusive); only meaningful with -table")
	table := fs.String("table", "", "restrict to one table (default: every table)")
	column := fs.String("column", "", "restrict to one column; requires -table when the table has several")
	dryRun := fs.Bool("dry-run", false, "count changes without writing")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}

	_, db, ok := open(e, *cfgPath)
	if !ok {
		return 1
	}

	targets := selectTargets(e.hostColumnList, *table, *column)
	if len(targets) == 0 {
		e.logger.Printf("no host column matches -table=%q -column=%q", *table, *column)
		return 1
	}
	// **絞り込んでいないのに -from を渡させない。** 複数の列を順に流す間、
	// 同じカーソルを全部に適用すると先頭以外を取りこぼす。
	if *fromKey != "" && len(targets) > 1 {
		e.logger.Printf("-from requires -table (and -column when ambiguous); %d columns matched", len(targets))
		return 1
	}

	var grandScanned, grandUpdated, grandConflicts int
	for _, col := range targets {
		cursor := *fromKey
		var scanned, updated, conflicts int
		for {
			res, err := e.hostBatch(db, col, cursor, *batchSize, *dryRun)
			if err != nil {
				e.logger.Printf("backfill %s.%s (cursor=%q): %v", col.Table, col.Column, cursor, err)
				return 1
			}
			if res.Scanned == 0 {
				break
			}
			// **カーソルが進まなければ止める。** 進まないまま回すと同じ batch を
			// 読み直して終わらない。hang より即死のほうが原因に近い
			// (#2714 review MEDIUM-3)。
			if res.LastKey <= cursor {
				e.logger.Printf("backfill %s.%s: cursor が進まない (cursor=%q lastKey=%q)",
					col.Table, col.Column, cursor, res.LastKey)
				return 1
			}
			cursor = res.LastKey
			scanned += res.Scanned
			updated += res.Updated
			conflicts += res.Conflicts
			for _, c := range res.ConflictKeys {
				e.logger.Printf("conflict %s.%s %s=%q host=%q -> %q (正規化すると一意制約に当たるので据え置き)",
					col.Table, col.Column, col.KeysetColumn, c.Key, c.Host, c.Normalized)
			}
			e.logger.Printf("%s.%s scanned=%d updated=%d conflicts=%d cursor=%s",
				col.Table, col.Column, scanned, updated, conflicts, cursor)
			pause(e, *sleepMs)
		}
		e.logger.Printf("%s.%s done: scanned=%d updated=%d conflicts=%d",
			col.Table, col.Column, scanned, updated, conflicts)
		grandScanned += scanned
		grandUpdated += updated
		grandConflicts += conflicts
	}

	e.logger.Printf("done [%s]: scanned=%d updated=%d conflicts=%d",
		modeLabel(*dryRun, "dry-run (no writes)"), grandScanned, grandUpdated, grandConflicts)
	if grandConflicts > 0 {
		e.logger.Printf("conflicts があるので手当てが要る: 表記違いで重複した行が残っている。" +
			"マージは FK の張り替えが必要なのでこのバッチでは行わない")
	}
	return 0
}

// selectTargets filters cols by the -table / -column flags.
func selectTargets(cols []maintenance.HostColumn, table, column string) []maintenance.HostColumn {
	var out []maintenance.HostColumn
	for _, c := range cols {
		if table != "" && c.Table != table {
			continue
		}
		if column != "" && c.Column != column {
			continue
		}
		out = append(out, c)
	}
	return out
}
