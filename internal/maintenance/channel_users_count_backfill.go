package maintenance

import (
	"slices"
	"strings"

	"gorm.io/gorm"
)

// ChannelUsersCountChange is one channel row whose stored usersCount differs
// from the recomputed one.
type ChannelUsersCountChange struct {
	ID       string
	OldUsers int
	NewUsers int
}

// ChannelUsersCountBackfillResult reports the outcome of one keyset batch.
type ChannelUsersCountBackfillResult struct {
	Scanned int    // channel rows inspected this batch
	LastID  string // greatest channel id seen; "" when no rows remained
	// Changes lists the rows whose usersCount differed from the recomputed
	// value. In dry-run nothing was written; otherwise every listed row was
	// rewritten by this batch.
	Changes []ChannelUsersCountChange
}

// channelUsersCountAfterCount runs between the counting SELECT and the writing
// UPDATE. Tests replace it to observe that the two are separate statements.
var channelUsersCountAfterCount = func() {}

// channelUsersCountQuery counts the recomputed values for the channel ids bound
// to the single `?` placeholder.
//
// 値は「いまそのチャンネルに投稿がある利用者の数」。本家の usersCount は投稿の
// 作成で足すだけの累積値 (投稿を消して投稿し直すとまた増える) で、その履歴は
// DB に残らないので復元できない。
//
// 件数はチャンネルごとの相関 subquery で数える。note の
// "IDX_note_channelId_userId_id" ("channelId", "userId", id、channelId のある行だけの
// 部分 index、000140) の該当範囲だけを index only scan で読み、note 全体は走査しない。
const channelUsersCountQuery = `
SELECT c.id,
       c."usersCount" AS old_users,
       (SELECT COUNT(DISTINCT n."userId") FROM "note" n WHERE n."channelId" = c.id)::int AS users
FROM "channel" c
WHERE c.id IN ?
ORDER BY c.id`

// channelUsersCountValue is one row of the VALUES list the writing UPDATE
// receives.
type channelUsersCountValue struct {
	ID    string
	Users int
}

// channelUsersCountValues builds the VALUES placeholders and bind arguments for
// the writing UPDATE, ordered by channel id.
//
// 行のロックを取る順を揃えるため、id の順に並べる (instance-counts と同じ)。
func channelUsersCountValues(vals []channelUsersCountValue) (string, []any) {
	sorted := slices.Clone(vals)
	slices.SortFunc(sorted, func(a, b channelUsersCountValue) int { return strings.Compare(a.ID, b.ID) })
	placeholders := make([]string, 0, len(sorted))
	args := make([]any, 0, len(sorted)*2)
	for _, v := range sorted {
		placeholders = append(placeholders, "(?::varchar, ?::int)")
		args = append(args, v.ID, v.Users)
	}
	return strings.Join(placeholders, ", "), args
}

// BackfillChannelUsersCountBatch recomputes `channel.usersCount` for one keyset
// batch of channel rows and rewrites the rows whose stored value differs.
//
// #3369 まで Elythia は usersCount をフォロー・解除で増減しており、本家の
// 「チャンネルに投稿した人の数」と違う値が入っている。#3369 から投稿で数える
// ようになったが、積むのは差分なので過去の分は直らない。
//
// **数える文と書く文を分ける** (理由は BackfillInstanceCountsBatch と同じ。1 本の
// UPDATE ... FROM (集計) にすると、先に書いた行のロックを残りの集計の間握り
// 続けうる)。
//
// 冪等。値が既に正しい行は WHERE で外れ、書き込みもロックも起きない。`LastID` を
// 次回の fromID に渡せば再開できる。`dryRun` は UPDATE を撃たずに差分だけ返す。
func BackfillChannelUsersCountBatch(db *gorm.DB, fromID string, batchSize int, dryRun bool) (ChannelUsersCountBackfillResult, error) {
	batchSize = clampBatchSize(batchSize)

	var ids []string
	if err := db.Raw(
		`SELECT id FROM "channel" WHERE id > ? ORDER BY id ASC LIMIT ?`,
		fromID, batchSize,
	).Scan(&ids).Error; err != nil {
		return ChannelUsersCountBackfillResult{}, err
	}
	res := ChannelUsersCountBackfillResult{Scanned: len(ids)}
	if len(ids) == 0 {
		return res, nil
	}
	res.LastID = ids[len(ids)-1]

	type counted struct {
		ID       string
		OldUsers int
		Users    int
	}
	var rows []counted
	if err := db.Raw(channelUsersCountQuery, ids).Scan(&rows).Error; err != nil {
		return ChannelUsersCountBackfillResult{}, err
	}
	channelUsersCountAfterCount()

	var diffs []counted
	for _, r := range rows {
		if r.OldUsers != r.Users {
			diffs = append(diffs, r)
		}
	}
	if len(diffs) == 0 {
		return res, nil
	}

	if !dryRun {
		// 比較は書く時点の値に対して行うので、数えた後に投稿の hook が加算して
		// 既に一致した行は書かない。一致しない行は数えた時点の人数で上書きする
		// (その間に初めて投稿した人の分は失われる。docs/deployment.md の
		// backfill channel-users-count)。
		vals := make([]channelUsersCountValue, 0, len(diffs))
		for _, d := range diffs {
			vals = append(vals, channelUsersCountValue{ID: d.ID, Users: d.Users})
		}
		placeholders, args := channelUsersCountValues(vals)
		var written []string
		if err := db.Raw(`
UPDATE "channel" AS t
SET "usersCount" = v.users
FROM (VALUES `+placeholders+`) AS v(id, users)
WHERE t.id = v.id
  AND t."usersCount" IS DISTINCT FROM v.users
RETURNING t.id`, args...).Scan(&written).Error; err != nil {
			return ChannelUsersCountBackfillResult{}, err
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
		res.Changes = append(res.Changes, ChannelUsersCountChange{
			ID:       d.ID,
			OldUsers: d.OldUsers,
			NewUsers: d.Users,
		})
	}
	return res, nil
}
