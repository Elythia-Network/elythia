package maintenance

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// HashtagMentionsSeen records the (author, tag) pairs a backfill run has
// already handled. Passing the same value to every batch of one run makes a
// pair that appears in several batches count once.
type HashtagMentionsSeen map[[2]string]struct{}

// HashtagMentionsBackfillResult reports the outcome of one keyset batch.
type HashtagMentionsBackfillResult struct {
	Scanned int    // followers / specified notes with tags inspected this batch
	Removed int    // (tag, user) entries dropped from the mentioned* arrays (would be, when dryRun)
	Deleted int    // hashtag rows deleted because no user was left (0 when dryRun)
	LastID  string // greatest note id seen; "" when no rows remained
}

// hashtagMentionCondition selects the hashtag row of a tag that lists a user
// as mentioning it while the user has no public / home note carrying the tag.
// Its placeholders take hashtagMentionArgs.
const hashtagMentionCondition = `"name" = ? AND ? = ANY("mentionedUserIds")
  AND NOT EXISTS (
    SELECT 1 FROM "note" n
    WHERE n."userId" = ? AND n."tags" @> ARRAY[?]::varchar[] AND n."visibility" IN ('public', 'home')
  )`

// hashtagMentionArgs returns the values for hashtagMentionCondition.
func hashtagMentionArgs(user, tag string) []any { return []any{tag, user, user, tag} }

// BackfillHashtagMentionsBatch removes, from the hashtag table's mentioned*
// arrays and counts, the users that were recorded only through followers /
// specified notes. It walks one keyset batch of such notes (id ASC, after
// fromID) and, for every (author, tag) pair, drops the author from the tag's
// mentioned users unless the author has a public or home note with the tag.
// A hashtag row left with no mentioned and no attached user is deleted.
//
// 以前は公開範囲を限ったノートのタグも hashtag テーブルに数えていた。本家と同じく
// public / home のノートだけを数える形に揃える。消すのは「公開範囲を限ったノートが
// あり、public / home のノートが無い」組だけにする。hashtag テーブルはノートを消しても
// 減らない (本家も同じ) ので、ノートから数え直すと、消したノートの分まで落ちてしまう。
// public / home のノートを消していて、公開範囲を限ったノートだけが残っている利用者は、
// 本家より少なく数える側に倒れる。
//
// 判定と更新は 1 文で行うが、READ COMMITTED では副問い合わせ (NOT EXISTS) は文の
// 開始時点のスナップショットで評価される。このため、文が始まる前に commit された
// public / home のノートがある組は消さないが、文の実行中に同じ利用者が public の
// ノートでタグを使うと、外れることがある (次にそのタグを public / home のノートで
// 使えば、また数えられる)。
//
// seen は実行全体で 1 つを渡す (nil ならこのバッチの中だけで持つ)。同じ組が複数の
// バッチに出ても、dryRun の数を重複させないため。dryRun は消す組を数えるだけで
// 書き込まない。冪等なので、途中で止めても LastID から再開できる。
func BackfillHashtagMentionsBatch(db *gorm.DB, fromID string, batchSize int, dryRun bool, seen HashtagMentionsSeen) (HashtagMentionsBackfillResult, error) {
	if batchSize <= 0 {
		batchSize = 1000
	}
	var notes []*model.Note
	if err := db.Model(&model.Note{}).
		Select("id", "userId", "tags").
		Where(`id > ? AND "visibility" IN ('followers', 'specified') AND cardinality(tags) > 0`, fromID).
		Order("id ASC").
		Limit(batchSize).
		Find(&notes).Error; err != nil {
		return HashtagMentionsBackfillResult{}, err
	}
	res := HashtagMentionsBackfillResult{}
	if seen == nil {
		seen = HashtagMentionsSeen{}
	}
	for _, n := range notes {
		res.Scanned++
		res.LastID = n.ID
		for _, tag := range n.Tags {
			key := [2]string{n.UserID, tag}
			if _, done := seen[key]; tag == "" || done {
				continue
			}
			seen[key] = struct{}{}
			p := struct{ user, tag string }{user: n.UserID, tag: tag}
			if dryRun {
				var count int64
				if err := db.Raw(`SELECT count(*) FROM "hashtag" WHERE `+hashtagMentionCondition, hashtagMentionArgs(p.user, p.tag)...).Scan(&count).Error; err != nil {
					return res, err
				}
				res.Removed += int(count)
				continue
			}
			args := append([]any{p.user, p.user, p.user, p.user, p.user}, hashtagMentionArgs(p.user, p.tag)...)
			// 数が配列とずれている行 (以前の版の並行な記録など) で負にしないよう、0 で止める。
			upd := db.Exec(`UPDATE "hashtag" SET
  "mentionedUserIds" = array_remove("mentionedUserIds", ?),
  "mentionedUsersCount" = GREATEST("mentionedUsersCount" - 1, 0),
  "mentionedLocalUserIds" = array_remove("mentionedLocalUserIds", ?),
  "mentionedLocalUsersCount" = GREATEST("mentionedLocalUsersCount" - CASE WHEN ? = ANY("mentionedLocalUserIds") THEN 1 ELSE 0 END, 0),
  "mentionedRemoteUserIds" = array_remove("mentionedRemoteUserIds", ?),
  "mentionedRemoteUsersCount" = GREATEST("mentionedRemoteUsersCount" - CASE WHEN ? = ANY("mentionedRemoteUserIds") THEN 1 ELSE 0 END, 0)
WHERE `+hashtagMentionCondition, args...)
			if upd.Error != nil {
				return res, upd.Error
			}
			if upd.RowsAffected == 0 {
				continue
			}
			res.Removed += int(upd.RowsAffected)
			// 誰も使っていない行は名前ごと消す。名前そのものが一覧と検索に出るため。
			del := db.Exec(`DELETE FROM "hashtag" WHERE "name" = ? AND cardinality("mentionedUserIds") = 0 AND cardinality("attachedUserIds") = 0`, p.tag)
			if del.Error != nil {
				return res, del.Error
			}
			res.Deleted += int(del.RowsAffected)
		}
	}
	return res, nil
}
