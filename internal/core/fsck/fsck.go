// Package fsck reports and repairs drift between denormalized counters and
// the rows they summarise.
//
// `user.followersCount` / `followingCount` / `notesCount` と
// `note.repliesCount` / `renoteCount` / `clippedCount` は増減で維持されている。増減はベスト
// エフォート (戻り値を捨てる呼び出しがある) なので、失敗すればそのままずれる。
// `instance` 側には RecomputeFollowCounts があるのに user / note 側には無く、
// 一度ずれると管理者は SQL を手で書くしかなかった (#2473)。
//
// `note.clippedCount` は #1768 から clips/add-note と remove-note で増減している。
// それより前にクリップした行はカウンタが 0 のまま残っているので、実件数
// (`clip_note`) との突き合わせで直せる (#3291)。**平常時にもずれは出る** —
// クリップの削除 (利用者の削除による cascade を含む) では、本家と同じく
// カウンタを減らさないので、保存値が実件数より大きく残る。`-fix` で下げてよいが、
// 下げるとクリップが消えたリモートノートは掃除の対象に戻る。
// クリップ側の件数 (`clip.notesCount`) は非正規化しておらず (#2243)、検査の
// 対象ではない。
//
// `note.pageCount` は #3293 から、ページの作成・更新・削除で増減している
// (本家 PageService と同じ)。それより前に作ったページが参照するノートは
// migration 000107 で埋めたが、そちらは増やす向きにしか直さないので、参照が
// 無いのに値が残っている行 (TS 由来の古い値など) はここで下げる。
package fsck

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// Drift is one counter that disagrees with the rows it summarises.
type Drift struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	ID     string `json:"id"`
	Stored int64  `json:"stored"`
	Actual int64  `json:"actual"`
}

// Orphan is a row referencing a missing parent.
type Orphan struct {
	Table string `json:"table"`
	// Reason describes what is missing.
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

// Report is one run's findings.
type Report struct {
	Drifts  []Drift  `json:"drifts"`
	Orphans []Orphan `json:"orphans"`
	// Repaired counts the drifts written back (Fix=true のときのみ非ゼロ)。
	Repaired int `json:"repaired"`
}

// OK reports whether nothing needs attention.
func (r Report) OK() bool { return len(r.Drifts) == 0 && len(r.Orphans) == 0 }

// Options controls one run.
type Options struct {
	// Fix writes corrected counters back. **既定は false。**
	Fix bool
	// Limit bounds how many drifts of each kind are collected. 0 で既定値。
	// 大規模インスタンスで全件を持つとメモリを食うので上限を設ける。
	Limit int
}

// DefaultLimit bounds per-check reporting.
const DefaultLimit = 1000

// counterCheck describes one denormalized counter and how to recompute it.
//
// **SQL は 1 本にまとめる。** 行ごとに数え直すと大規模インスタンスで N+1 に
// なり、実行できない道具になる。
type counterCheck struct {
	table  string
	column string
	// query returns (id, stored, actual) for rows whose counter disagrees.
	query string
}

// counterChecks are the counters fsck knows how to verify.
var counterChecks = []counterCheck{
	{
		table: "user", column: "followersCount",
		query: `SELECT u.id AS id, u."followersCount" AS stored, COALESCE(f.n, 0) AS actual
		        FROM "user" u
		        LEFT JOIN (SELECT "followeeId" AS uid, COUNT(*) AS n FROM "following" GROUP BY "followeeId") f
		          ON f.uid = u.id
		        WHERE u."followersCount" <> COALESCE(f.n, 0)`,
	},
	{
		table: "user", column: "followingCount",
		query: `SELECT u.id AS id, u."followingCount" AS stored, COALESCE(f.n, 0) AS actual
		        FROM "user" u
		        LEFT JOIN (SELECT "followerId" AS uid, COUNT(*) AS n FROM "following" GROUP BY "followerId") f
		          ON f.uid = u.id
		        WHERE u."followingCount" <> COALESCE(f.n, 0)`,
	},
	{
		table: "user", column: "notesCount",
		query: `SELECT u.id AS id, u."notesCount" AS stored, COALESCE(n.n, 0) AS actual
		        FROM "user" u
		        LEFT JOIN (SELECT "userId" AS uid, COUNT(*) AS n FROM "note" GROUP BY "userId") n
		          ON n.uid = u.id
		        WHERE u."notesCount" <> COALESCE(n.n, 0)`,
	},
	{
		table: "note", column: "repliesCount",
		query: `SELECT t.id AS id, t."repliesCount" AS stored, COALESCE(r.n, 0) AS actual
		        FROM "note" t
		        LEFT JOIN (SELECT "replyId" AS pid, COUNT(*) AS n FROM "note" WHERE "replyId" IS NOT NULL GROUP BY "replyId") r
		          ON r.pid = t.id
		        WHERE t."repliesCount" <> COALESCE(r.n, 0)`,
	},
	{
		table: "note", column: "renoteCount",
		query: `SELECT t.id AS id, t."renoteCount" AS stored, COALESCE(r.n, 0) AS actual
		        FROM "note" t
		        LEFT JOIN (SELECT "renoteId" AS pid, COUNT(*) AS n FROM "note" WHERE "renoteId" IS NOT NULL GROUP BY "renoteId") r
		          ON r.pid = t.id
		        WHERE t."renoteCount" <> COALESCE(r.n, 0)`,
	},
	{
		// 列は smallint なので、実件数は列に入る上限で頭打ちにする。そうしないと
		// 32767 件を超えるノートは -fix の書き戻しが範囲外で失敗し続ける。
		table: "note", column: "clippedCount",
		query: `SELECT t.id AS id, t."clippedCount" AS stored, LEAST(COALESCE(c.n, 0), 32767) AS actual
		        FROM "note" t
		        LEFT JOIN (SELECT "noteId" AS nid, COUNT(*) AS n FROM "clip_note" GROUP BY "noteId") c
		          ON c.nid = t.id
		        WHERE t."clippedCount" <> LEAST(COALESCE(c.n, 0), 32767)`,
	},
	{
		// 参照の集め方は本家 PageService.collectReferencedNotes と同じ
		// (repository の referencedNoteIDs)。`type: note` の `note` (文字列) を、
		// `type: section` の `children` を再帰的にたどって集め、ページごとに重複を
		// 除く。content が配列でないページは何も参照しない。
		table: "note", column: "pageCount",
		query: `WITH RECURSIVE blocks AS (
		          SELECT p.id AS pid, b.value AS blk
		          FROM "page" p,
		               jsonb_array_elements(CASE WHEN jsonb_typeof(p.content) = 'array' THEN p.content ELSE '[]'::jsonb END) b
		          UNION ALL
		          SELECT blocks.pid, c.value
		          FROM blocks,
		               jsonb_array_elements(CASE WHEN blocks.blk->>'type' = 'section' AND jsonb_typeof(blocks.blk->'children') = 'array'
		                                         THEN blocks.blk->'children' ELSE '[]'::jsonb END) c
		        ), refs AS (
		          SELECT DISTINCT pid, blk->>'note' AS nid FROM blocks
		          WHERE blk->>'type' = 'note' AND jsonb_typeof(blk->'note') = 'string'
		        )
		        SELECT t.id AS id, t."pageCount" AS stored, LEAST(COALESCE(r.n, 0), 32767) AS actual
		        FROM "note" t
		        LEFT JOIN (SELECT nid, COUNT(*) AS n FROM refs GROUP BY nid) r ON r.nid = t.id
		        WHERE t."pageCount" <> LEAST(COALESCE(r.n, 0), 32767)`,
	},
}

// orphanCheck counts rows whose parent no longer exists.
type orphanCheck struct {
	table  string
	reason string
	query  string
}

// orphanChecks are report-only. **削除は復元できないので自動で消さない。**
var orphanChecks = []orphanCheck{
	{
		table: "note", reason: "userId が存在しない",
		query: `SELECT COUNT(*) FROM "note" n WHERE NOT EXISTS (SELECT 1 FROM "user" u WHERE u.id = n."userId")`,
	},
	{
		table: "drive_file", reason: "userId が存在しない",
		query: `SELECT COUNT(*) FROM "drive_file" d WHERE d."userId" IS NOT NULL
		          AND NOT EXISTS (SELECT 1 FROM "user" u WHERE u.id = d."userId")`,
	},
	{
		table: "following", reason: "followerId または followeeId が存在しない",
		query: `SELECT COUNT(*) FROM "following" f
		        WHERE NOT EXISTS (SELECT 1 FROM "user" u WHERE u.id = f."followerId")
		           OR NOT EXISTS (SELECT 1 FROM "user" u WHERE u.id = f."followeeId")`,
	},
}

// Run executes every check.
func Run(ctx context.Context, db *gorm.DB, opts Options) (Report, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	var rep Report

	for _, c := range counterChecks {
		rows, err := collectDrift(ctx, db, c, limit)
		if err != nil {
			return rep, err
		}
		rep.Drifts = append(rep.Drifts, rows...)
		if !opts.Fix {
			continue
		}
		for _, d := range rows {
			if err := repair(ctx, db, d); err != nil {
				return rep, err
			}
			rep.Repaired++
		}
	}

	for _, c := range orphanChecks {
		var n int64
		if err := db.WithContext(ctx).Raw(c.query).Scan(&n).Error; err != nil {
			return rep, fmt.Errorf("fsck: %s の孤児検査: %w", c.table, err)
		}
		if n > 0 {
			rep.Orphans = append(rep.Orphans, Orphan{Table: c.table, Reason: c.reason, Count: n})
		}
	}
	return rep, nil
}

// collectDrift runs one counter check.
func collectDrift(ctx context.Context, db *gorm.DB, c counterCheck, limit int) ([]Drift, error) {
	var rows []struct {
		ID     string
		Stored int64
		Actual int64
	}
	if err := db.WithContext(ctx).Raw(c.query+" LIMIT ?", limit).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("fsck: %s.%s の検査: %w", c.table, c.column, err)
	}
	out := make([]Drift, 0, len(rows))
	for _, r := range rows {
		out = append(out, Drift{
			Table: c.table, Column: c.column,
			ID: r.ID, Stored: r.Stored, Actual: r.Actual,
		})
	}
	return out, nil
}

// repair writes the recomputed value back.
//
// 列名は counterChecks の固定値なので識別子として埋め込んでよい。**id は
// プレースホルダで渡す** (行データ由来なので)。
func repair(ctx context.Context, db *gorm.DB, d Drift) error {
	sql := fmt.Sprintf(`UPDATE %q SET %q = ? WHERE id = ?`, d.Table, d.Column)
	if err := db.WithContext(ctx).Exec(sql, d.Actual, d.ID).Error; err != nil {
		return fmt.Errorf("fsck: %s.%s の修正 (%s): %w", d.Table, d.Column, d.ID, err)
	}
	return nil
}
