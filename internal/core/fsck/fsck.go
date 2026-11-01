// Package fsck reports and repairs drift between denormalized counters and
// the rows they summarise.
//
// `user.followersCount` / `followingCount` / `notesCount` と
// `note.repliesCount` / `renoteCount` / `clippedCount` は増減で維持されている。増減はベスト
// エフォート (戻り値を捨てる呼び出しがある) なので、失敗すればそのままずれる。
//
// **カウンタごとに数え方が違う** (#3927)。`followersCount` / `followingCount` /
// `repliesCount` は増やすだけでなく減らしもする、行の件数と一致させるつもりの値
// なので、どちら向きのずれも報告し、-fix は実件数で上書きする。`notesCount` と
// `renoteCount` は本家も削除で減らさない増えるだけの値なので、保存値が実件数より
// 小さいときだけを報告し、-fix は上げるだけにする (raiseOnly)。各カウンタの
// 数え方の根拠は counterChecks の各項目に書く。
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

// DriftTotal is the number of drifts of one counter, counted beyond the
// collection limit.
type DriftTotal struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Count  int64  `json:"count"`
}

// Report is one run's findings.
type Report struct {
	// Drifts holds at most Options.Limit drifts of each counter.
	Drifts []Drift `json:"drifts"`
	// Totals holds the real number of drifts of each counter that has any,
	// in check order. 上限で頭打ちになった Drifts の件数からは、本当の規模が
	// 分からないので別に数える (#3927)。
	Totals  []DriftTotal `json:"totals"`
	Orphans []Orphan     `json:"orphans"`
	// Repaired counts the drifts written back (Fix=true のときのみ非ゼロ)。
	// 書き戻すのは集めた Drifts だけなので、DriftTotal より小さいことがある。
	Repaired int `json:"repaired"`
}

// DriftTotal returns the number of drifts over every counter, beyond the
// collection limit.
func (r Report) DriftTotal() int64 {
	var n int64
	for _, t := range r.Totals {
		n += t.Count
	}
	return n
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
	// raiseOnly marks a counter that only ever grows in normal operation, so
	// only stored < actual is reported and -fix never lowers it.
	//
	// 増えるだけのカウンタは、保存値が実件数より大きいのが正常 (消した行の分が
	// 残る)。小さいのは増やすのに失敗した分なので、そこだけを上げる。
	raiseOnly bool
}

// counterChecks are the counters fsck knows how to verify.
var counterChecks = []counterCheck{
	{
		// 本家はフォローの成立で 1 増やし (UserFollowingService.insertFollowingDoc)、
		// 解除で 1 減らす (decrementFollowing)。Elythia も同じ
		// (following.Service、行の作成・削除と同じトランザクション)。行の件数と
		// 一致させるつもりの値なので、どちら向きのずれも報告する。
		//
		// **移行したアカウントが絡むフォローは数えない。** 本家も Elythia も、
		// どちらかが移行済み (movedToUri がある) なら増減を飛ばし、移行の時点で
		// 旧アカウントと関係のあった相手のカウントを 1 ずつ減らす (行は残す。
		// AccountMoveService.adjustFollowingCounts / move.Service)。実件数は、
		// 本家が移行済みの相手との解除のときに数え直す式 (decrementFollowing の
		// else 側) と同じく、相手が移行済みでない行だけを数え、移行済みの利用者
		// 自身は数え直さない。
		//
		// 本家でも行を直接消す経路 (利用者の削除で following が cascade で消える)
		// では相手のカウントを減らさないので、保存値が大きいずれは平常時にも出る。
		// 下げるのは本家が数え直すときと同じ値なので、-fix で直してよい。
		table: "user", column: "followersCount",
		query: `SELECT u.id AS id, u."followersCount" AS stored, COALESCE(f.n, 0) AS actual
		        FROM "user" u
		        LEFT JOIN (SELECT f."followeeId" AS uid, COUNT(*) AS n
		                   FROM "following" f
		                   JOIN "user" o ON o.id = f."followerId"
		                   WHERE COALESCE(o."movedToUri", '') = ''
		                   GROUP BY f."followeeId") f
		          ON f.uid = u.id
		        WHERE COALESCE(u."movedToUri", '') = '' AND u."followersCount" <> COALESCE(f.n, 0)`,
	},
	{
		// followersCount と同じ (相手はフォロー先)。
		table: "user", column: "followingCount",
		query: `SELECT u.id AS id, u."followingCount" AS stored, COALESCE(f.n, 0) AS actual
		        FROM "user" u
		        LEFT JOIN (SELECT f."followerId" AS uid, COUNT(*) AS n
		                   FROM "following" f
		                   JOIN "user" o ON o.id = f."followeeId"
		                   WHERE COALESCE(o."movedToUri", '') = ''
		                   GROUP BY f."followerId") f
		          ON f.uid = u.id
		        WHERE COALESCE(u."movedToUri", '') = '' AND u."followingCount" <> COALESCE(f.n, 0)`,
	},
	{
		// 本家は投稿のたびに 1 増やし (NoteCreateService.incNotesCountOfUser)、
		// 削除では減らさない (NoteDeleteService が減らすのは instance の notesCount
		// だけ)。Elythia のローカルの投稿も同じ (note.CreateService.Create)。消した投稿の
		// 分が残る増えるだけの値なので、保存値が実件数より小さいときだけを報告する。
		//
		// **リモートの利用者も見る。** 本家はリモートの投稿の受信でも増やし、Elythia も
		// #3928 から取り込みで増やす。それより前に取り込んだ投稿の分は数えていないので、
		// 「記録が小さい」と出て、-fix で取り込んだ件数まで上がる。
		table: "user", column: "notesCount", raiseOnly: true,
		query: `SELECT u.id AS id, u."notesCount" AS stored, COALESCE(n.n, 0) AS actual
		        FROM "user" u
		        LEFT JOIN (SELECT "userId" AS uid, COUNT(*) AS n FROM "note" GROUP BY "userId") n
		          ON n.uid = u.id
		        WHERE u."notesCount" < COALESCE(n.n, 0)`,
	},
	{
		// 本家は返信の作成で 1 増やし (NoteCreateService.saveReply、自分への返信も
		// bot の返信も数える)、削除で 1 減らす (NoteDeleteService)。Elythia も
		// ローカルの投稿・resolver が取り込む返信で増やし、DeleteService で減らす。
		// 行の件数と一致させるつもりの値なので、どちら向きのずれも報告する。
		//
		// 本家でも利用者の削除 (DeleteAccountProcessorService) は投稿の行を直接
		// 消して返信先のカウントを減らさないので、保存値が大きいずれは平常時にも
		// 出る。
		table: "note", column: "repliesCount",
		query: `SELECT t.id AS id, t."repliesCount" AS stored, COALESCE(r.n, 0) AS actual
		        FROM "note" t
		        LEFT JOIN (SELECT "replyId" AS pid, COUNT(*) AS n FROM "note" WHERE "replyId" IS NOT NULL GROUP BY "replyId") r
		          ON r.pid = t.id
		        WHERE t."repliesCount" <> COALESCE(r.n, 0)`,
	},
	{
		// 本家は、リノート (引用を含む) の作成で、リノートした人が対象の作者でも
		// bot でもないときだけ 1 増やし (NoteCreateService の
		// `data.renote.userId !== user.id && !user.isBot` → incRenoteCount)、
		// 削除では減らさない。Elythia もローカルの投稿・inbox の Announce・
		// resolver が取り込む引用で同じ条件で増やすので、同じ条件で数えた件数を
		// 実件数とし、notesCount と同じく保存値が小さいときだけを報告する。
		//
		// Elythia は受信した Undo(Announce) で、増やしたときと同じ条件で 1 減らす
		// (federation.handleUndoAnnounce、本家との意図的な乖離)。そのときは
		// リノートの行も同時に消すので、保存値と実件数が 1 ずつ下がり、
		// 「保存値 >= 実件数」の関係は崩れない。
		//
		// 崩れるのは、リノートした後に、リノートした人の isBot が変わったときだけ
		// (判定は今の isBot で行うので)。bot のときにリノートして後で bot を
		// やめると、数えなかったリノートを数えるので「小さい」と出て -fix で
		// 足される。逆向き (後で bot になった) は「大きい」側なので報告しない。
		// どちらも本家の値と同じにはならないが、作成時の isBot は DB に残らない。
		//
		// 対象の作者は、非正規化した renoteUserId で見る (本家も Elythia もリノートの
		// 作成のたびに埋める)。note 同士を結合すると、統計が古いときに planner が
		// 索引を使わない nested loop を選び、検査が何分も返らなかった。renoteUserId
		// が空の行は数えない — 数えすぎると -fix が本家より大きい値に上げてしまう
		// ので、迷う行は「小さい」と出さない側に倒す。
		//
		// 列は smallint なので、実件数は列に入る上限で頭打ちにする (clippedCount
		// と同じ理由)。
		table: "note", column: "renoteCount", raiseOnly: true,
		query: `SELECT t.id AS id, t."renoteCount" AS stored, LEAST(COALESCE(r.n, 0), 32767) AS actual
		        FROM "note" t
		        LEFT JOIN (SELECT rn."renoteId" AS pid, COUNT(*) AS n
		                   FROM "note" rn
		                   JOIN "user" ru ON ru.id = rn."userId"
		                   WHERE rn."renoteId" IS NOT NULL AND rn."userId" <> rn."renoteUserId" AND NOT ru."isBot"
		                   GROUP BY rn."renoteId") r
		          ON r.pid = t.id
		        WHERE t."renoteCount" < LEAST(COALESCE(r.n, 0), 32767)`,
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
		rows, total, err := collectDrift(ctx, db, c, limit)
		if err != nil {
			return rep, err
		}
		rep.Drifts = append(rep.Drifts, rows...)
		if total > 0 {
			rep.Totals = append(rep.Totals, DriftTotal{Table: c.table, Column: c.column, Count: total})
		}
		if !opts.Fix {
			continue
		}
		for _, d := range rows {
			if err := repair(ctx, db, d, c.raiseOnly); err != nil {
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

// collectDrift runs one counter check. It returns at most limit drifts and
// the total number of drifts.
//
// 総数は窓関数で同じクエリから取る。COUNT(*) OVER () は LIMIT の前に評価
// されるので、集計の走査を 2 回せずに上限を超えた分まで数えられる。id で
// 並べるのは、上限で切ったときにどの行を集めるかを実行ごとに変えないため。
func collectDrift(ctx context.Context, db *gorm.DB, c counterCheck, limit int) ([]Drift, int64, error) {
	var rows []struct {
		ID     string
		Stored int64
		Actual int64
		Total  int64
	}
	q := `SELECT d.id, d.stored, d.actual, COUNT(*) OVER () AS total FROM (` + c.query + `) d ORDER BY d.id LIMIT ?`
	if err := db.WithContext(ctx).Raw(q, limit).Scan(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("fsck: %s.%s の検査: %w", c.table, c.column, err)
	}
	out := make([]Drift, 0, len(rows))
	var total int64
	for _, r := range rows {
		out = append(out, Drift{
			Table: c.table, Column: c.column,
			ID: r.ID, Stored: r.Stored, Actual: r.Actual,
		})
		total = r.Total
	}
	return out, total, nil
}

// repair writes the recomputed value back.
//
// 列名は counterChecks の固定値なので識別子として埋め込んでよい。**id は
// プレースホルダで渡す** (行データ由来なので)。
//
// raiseOnly のカウンタは GREATEST で書く。検査から書き戻しまでの間にも
// サーバーは動いていて値が増えうるので、集めた時点の実件数で上書きすると
// その間の増分を消してしまう。
func repair(ctx context.Context, db *gorm.DB, d Drift, raiseOnly bool) error {
	sql := fmt.Sprintf(`UPDATE %q SET %q = ? WHERE id = ?`, d.Table, d.Column)
	if raiseOnly {
		sql = fmt.Sprintf(`UPDATE %q SET %q = GREATEST(%q, ?) WHERE id = ?`, d.Table, d.Column, d.Column)
	}
	if err := db.WithContext(ctx).Exec(sql, d.Actual, d.ID).Error; err != nil {
		return fmt.Errorf("fsck: %s.%s の修正 (%s): %w", d.Table, d.Column, d.ID, err)
	}
	return nil
}
