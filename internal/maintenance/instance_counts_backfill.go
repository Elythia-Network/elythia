package maintenance

import (
	"slices"
	"strings"

	"gorm.io/gorm"
)

// InstanceCountChange is one instance row whose stored counters differ from
// the recomputed ones.
type InstanceCountChange struct {
	Host     string
	OldNotes int
	NewNotes int
	OldUsers int
	NewUsers int
}

// InstanceCountsBackfillResult reports the outcome of one keyset batch.
type InstanceCountsBackfillResult struct {
	Scanned int    // instance rows inspected this batch
	LastID  string // greatest instance id seen; "" when no rows remained
	// Changes lists the rows whose notesCount / usersCount differed from the
	// recomputed values. In dry-run nothing was written; otherwise every
	// listed row was rewritten by this batch.
	Changes []InstanceCountChange
}

// instanceCountsAfterCount runs between the counting SELECT and the writing
// UPDATE. Tests replace it to observe that the two are separate statements.
var instanceCountsAfterCount = func() {}

// instanceCountsQuery counts the recomputed values for the instance ids bound
// to the single `?` placeholder.
//
// 値は本家の instance chart の tickMajor (`notes.total` = note を userHost で、
// `users.total` = user を host で数える) と同じ定義にする。renote も、削除済み
// (isDeleted) や凍結中の利用者も数える — 本家の集計列もそれらを区別せずに
// 増減している (NoteCreateService / ApPersonService.createPerson)。
//
// 件数は host ごとの相関 subquery で数える。note / user の全件を GROUP BY すると
// 走査が note 全体 (ローカルの投稿を含む) に及ぶが、相関 subquery なら
// note."userHost" / user.host の index の該当範囲だけを読む (どちらの index も
// 本家の初期 migration と mk-go の 000001 が作る)。
const instanceCountsQuery = `
SELECT i.id,
       i.host,
       i."notesCount" AS old_notes,
       i."usersCount" AS old_users,
       (SELECT COUNT(*) FROM "note" n WHERE n."userHost" = i.host)::int AS notes,
       (SELECT COUNT(*) FROM "user" u WHERE u.host = i.host)::int AS users
FROM "instance" i
WHERE i.id IN ?
ORDER BY i.id`

// instanceCountValue is one row of the VALUES list the writing UPDATE receives.
type instanceCountValue struct {
	ID    string
	Notes int
	Users int
}

// instanceCountsValues builds the VALUES placeholders and bind arguments for
// the writing UPDATE, ordered by instance id.
//
// **id の順に並べる。** 起動時の RecomputeFollowCounts も instance の複数行を
// 1 本で更新するので、バッチの最中に mk-go を再起動すると 2 本の UPDATE が
// 互いの行を待ち合い、まれに deadlock になりうる。渡す順を id に揃えておく
// (それでも行を取る順は planner 次第なので、docs/deployment.md では再起動と
// 重ねないよう書いている)。
func instanceCountsValues(vals []instanceCountValue) (string, []any) {
	sorted := slices.Clone(vals)
	slices.SortFunc(sorted, func(a, b instanceCountValue) int { return strings.Compare(a.ID, b.ID) })
	placeholders := make([]string, 0, len(sorted))
	args := make([]any, 0, len(sorted)*3)
	for _, v := range sorted {
		placeholders = append(placeholders, "(?::varchar, ?::int, ?::int)")
		args = append(args, v.ID, v.Notes, v.Users)
	}
	return strings.Join(placeholders, ", "), args
}

// BackfillInstanceCountsBatch recomputes `instance.notesCount` /
// `instance.usersCount` for one keyset batch of instance rows and rewrites the
// rows whose stored values differ.
//
// #3330 まで mk-go は 2 列を動かしておらず、それより前に作られた行は
// notesCount が 0、usersCount が行を作ったときの 1 のまま残っている。#3330 から
// 増減を積むようになったが、積むのは差分なので過去の分は埋まらない。
//
// 本家の集計列は累積値で、利用者の物理削除 (DeleteAccountProcessorService) では
// note も user も引かないため、長く動いた本家の値は実件数より大きくなりうる。
// その履歴は DB に残らないので復元できず、ここでは本家自身が chart の total に
// 使う「いまの実件数」を正とする。
//
// **数える文と書く文を分ける。** 1 本の UPDATE ... FROM (集計) にすると、
// MATERIALIZED の CTE でも planner が Nested Loop を選べば「1 行数える → 書いて
// ロック → 次を数える」が交互に進み、先に書いた行のロックを残りの集計の間
// 握り続ける (投稿の多い host が後ろにあると、instance 行を更新する本体の処理が
// その間待たされる)。先に SELECT で数え終え、書く文は数えた値を VALUES で
// 渡すだけにする。ロックを持つのは書く文の間だけになる。
//
// 冪等。値が既に正しい行は WHERE で外れ、書き込みもロックも起きない。`LastID` を
// 次回の fromID に渡せば再開できる。`dryRun` は UPDATE を撃たずに差分だけ返す。
func BackfillInstanceCountsBatch(db *gorm.DB, fromID string, batchSize int, dryRun bool) (InstanceCountsBackfillResult, error) {
	batchSize = clampBatchSize(batchSize)

	var ids []string
	if err := db.Raw(
		`SELECT id FROM "instance" WHERE id > ? ORDER BY id ASC LIMIT ?`,
		fromID, batchSize,
	).Scan(&ids).Error; err != nil {
		return InstanceCountsBackfillResult{}, err
	}
	res := InstanceCountsBackfillResult{Scanned: len(ids)}
	if len(ids) == 0 {
		return res, nil
	}
	res.LastID = ids[len(ids)-1]

	type counted struct {
		ID       string
		Host     string
		OldNotes int
		OldUsers int
		Notes    int
		Users    int
	}
	var rows []counted
	if err := db.Raw(instanceCountsQuery, ids).Scan(&rows).Error; err != nil {
		return InstanceCountsBackfillResult{}, err
	}
	instanceCountsAfterCount()

	var diffs []counted
	for _, r := range rows {
		if r.OldNotes != r.Notes || r.OldUsers != r.Users {
			diffs = append(diffs, r)
		}
	}
	if len(diffs) == 0 {
		return res, nil
	}

	if !dryRun {
		// 書く文は数えた値を受け取るだけで集計を含まない。比較は書く時点の
		// 値に対して行うので、数えた後に CounterBuffer が書いて既に一致した行は
		// 書かない。一致しない行は数えた時点の件数で上書きする (その間の増減は
		// 失われる。docs/deployment.md の backfill-instance-counts)。
		vals := make([]instanceCountValue, 0, len(diffs))
		for _, d := range diffs {
			vals = append(vals, instanceCountValue{ID: d.ID, Notes: d.Notes, Users: d.Users})
		}
		placeholders, args := instanceCountsValues(vals)
		var written []string
		if err := db.Raw(`
UPDATE "instance" AS t
SET "notesCount" = v.notes, "usersCount" = v.users
FROM (VALUES `+placeholders+`) AS v(id, notes, users)
WHERE t.id = v.id
  AND (t."notesCount" IS DISTINCT FROM v.notes OR t."usersCount" IS DISTINCT FROM v.users)
RETURNING t.id`, args...).Scan(&written).Error; err != nil {
			return InstanceCountsBackfillResult{}, err
		}
		done := make(map[string]bool, len(written))
		for _, id := range written {
			done[id] = true
		}
		kept := diffs[:0]
		for _, d := range diffs {
			if done[d.ID] {
				kept = append(kept, d)
			}
		}
		diffs = kept
	}

	for _, d := range diffs {
		res.Changes = append(res.Changes, InstanceCountChange{
			Host:     d.Host,
			OldNotes: d.OldNotes,
			NewNotes: d.Notes,
			OldUsers: d.OldUsers,
			NewUsers: d.Users,
		})
	}
	return res, nil
}
