// Package maintenance holds app-level batch jobs that cannot be expressed as
// pure SQL migrations (e.g. NFKC normalization). They are driven by standalone
// CLIs under cmd/ and are safe to run incrementally against a live DB.
package maintenance

import (
	"slices"

	"github.com/elythia-network/elythia/internal/misc/hashtag"
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// NoteTagsBackfillResult reports the outcome of one keyset batch.
type NoteTagsBackfillResult struct {
	Scanned int    // notes inspected this batch
	Updated int    // notes whose tags changed (0 when dryRun only counts)
	LastID  string // greatest note id seen; "" when no rows remained
}

// BackfillNoteTagsBatch normalizes one keyset batch of non-empty note.tags via
// hashtag.NormalizeNoteTags (NFKC + lowercase + >128 drop + 32 cap + dedup),
// matching what NoteCreateService now stores for new notes (#1948-18 / #2013).
//
// 既存 note は旧 hashtag.Extract で original-case のまま格納されており、search-by-tag
// の query が normalize されるため uppercase/全角 tag が match しなくなる回帰がある。
// これを batch で正規化する。fromID より大きい id の note を id ASC で batchSize 件
// 取り、変更があった行だけ UPDATE する。dryRun は UPDATE をせず件数のみ数える。
//
// 戻り値の LastID を次回の fromID に渡せば再開可能 (resumable)。LastID=="" は対象が
// 尽きたことを示す。NFKC は SQL で表現できないため app-level で行う。
func BackfillNoteTagsBatch(db *gorm.DB, fromID string, batchSize int, dryRun bool) (NoteTagsBackfillResult, error) {
	if batchSize <= 0 {
		batchSize = 1000
	}
	var notes []*model.Note
	q := db.Model(&model.Note{}).
		Select("id", "tags").
		Where("id > ? AND cardinality(tags) > 0", fromID).
		Order("id ASC").
		Limit(batchSize)
	if err := q.Find(&notes).Error; err != nil {
		return NoteTagsBackfillResult{}, err
	}
	res := NoteTagsBackfillResult{}
	for _, n := range notes {
		res.Scanned++
		res.LastID = n.ID
		normalized := hashtag.NormalizeNoteTags([]string(n.Tags))
		if slices.Equal([]string(n.Tags), normalized) {
			continue
		}
		res.Updated++
		if dryRun {
			continue
		}
		// nil は SQL NULL になり note.tags (NOT NULL) を壊すので空配列に倒す。
		newTags := model.StringArray(normalized)
		if newTags == nil {
			newTags = model.StringArray{}
		}
		if err := db.Model(&model.Note{}).Where("id = ?", n.ID).Update("tags", newTags).Error; err != nil {
			return res, err
		}
	}
	return res, nil
}
