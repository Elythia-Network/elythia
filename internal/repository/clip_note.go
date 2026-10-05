package repository

import (
	"strings"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// ClipNoteRepository provides data access for the `clip_note` table.
type ClipNoteRepository interface {
	Create(cn *model.ClipNote) error
	Delete(cn *model.ClipNote) error
	// DeleteByPair removes the (clipID, noteID) entry and returns how many rows
	// were actually deleted (0 when the note was not in the clip).
	DeleteByPair(clipID, noteID string) (int64, error)
	FindByPair(clipID, noteID string) (*model.ClipNote, error)
	ListByClip(clipID string, untilID, sinceID string, limit int) ([]*model.ClipNote, error)
	// ListByClipVisible is ListByClip with a visibility push-down: clip entries
	// whose referenced note the viewer cannot see (per core/note.CanSeeNote) are
	// excluded before LIMIT. viewerID 空文字は匿名 (public/home のみ)。clips/notes
	// で post-fetch filter によるページ過少充填と per-note の follow 判定 N+1 を
	// 避けるため (#1418 review)。export 経路は全件必要なので ListByClip を使う。
	// searchWords は各語を note.text / note.cw への ILIKE 部分一致 (OR) として
	// AND 結合する (#1562、upstream clips/notes の search param 相当)。空 slice
	// は無条件。
	ListByClipVisible(clipID, viewerID, untilID, sinceID string, limit int, searchWords []string) ([]*model.ClipNote, error)
	// CountByClip returns the number of notes in the given clip. Used by
	// noteEachClipsLimit policy gate (#1029 PR-1 follow-up).
	CountByClip(clipID string) (int64, error)
	// ListClipIDsByNote returns the distinct clipIds that contain noteID,
	// across all clips/owners. notes/clips が「この note を含む public clip」を
	// 引くのに使う (upstream clipNotesRepository.findBy({noteId}))。
	ListClipIDsByNote(noteID string) ([]string, error)
}

type clipNoteRepository struct {
	db *gorm.DB
}

// NewClipNoteRepository creates a new ClipNoteRepository.
func NewClipNoteRepository(db *gorm.DB) ClipNoteRepository {
	return &clipNoteRepository{db: db}
}

func (r *clipNoteRepository) Create(cn *model.ClipNote) error {
	return r.db.Create(cn).Error
}

func (r *clipNoteRepository) Delete(cn *model.ClipNote) error {
	return r.db.Delete(cn).Error
}

// DeleteByPair は削除した行数を返す。clips/remove-note は「実際に消えた
// ときだけ clippedCount を減らす」ために件数を見る (本家 4682d44cae)。
// FindByPair → Delete の 2 段だと、同じ組を並行に消した 2 つの要求が両方
// 見つけて両方減らし、カウンタが負になりうる。DELETE の RowsAffected なら、
// 後から来た方は先の行ロックが解けた後に 0 行になる。
func (r *clipNoteRepository) DeleteByPair(clipID, noteID string) (int64, error) {
	if !storable(clipID) || !storable(noteID) {
		return 0, nil
	}
	res := r.db.Where(`"clipId" = ? AND "noteId" = ?`, clipID, noteID).Delete(&model.ClipNote{})
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}

func (r *clipNoteRepository) FindByPair(clipID, noteID string) (*model.ClipNote, error) {
	if !storable(clipID) || !storable(noteID) {
		return nil, ErrNotFound
	}
	var cn model.ClipNote
	if err := r.db.Where("\"clipId\" = ? AND \"noteId\" = ?", clipID, noteID).First(&cn).Error; err != nil {
		return nil, err
	}
	return &cn, nil
}

// CountByClip returns the number of notes in the given clip.
func (r *clipNoteRepository) CountByClip(clipID string) (int64, error) {
	var count int64
	if err := r.db.Model(&model.ClipNote{}).
		Where(`"clipId" = ?`, clipID).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func (r *clipNoteRepository) ListClipIDsByNote(noteID string) ([]string, error) {
	var ids []string
	if err := r.db.Model(&model.ClipNote{}).
		Where(`"noteId" = ?`, noteID).
		Distinct(`"clipId"`).
		Pluck(`"clipId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

// ListByClip returns the entries for a clip with since/until pagination on
// the referenced note id (clip_note.noteId == note.id). Order flips to ASC when
// only sinceID is supplied, matching paginationOrder (upstream
// QueryService.makePaginationQuery on the note builder parity, #1950).
func (r *clipNoteRepository) ListByClip(clipID string, untilID, sinceID string, limit int) ([]*model.ClipNote, error) {
	return r.listByClip(clipID, "", false, untilID, sinceID, limit, nil)
}

// ListByClipVisible is ListByClip with the visibility push-down enabled.
func (r *clipNoteRepository) ListByClipVisible(clipID, viewerID, untilID, sinceID string, limit int, searchWords []string) ([]*model.ClipNote, error) {
	return r.listByClip(clipID, viewerID, true, untilID, sinceID, limit, searchWords)
}

func (r *clipNoteRepository) listByClip(clipID, viewerID string, filterVisibility bool, untilID, sinceID string, limit int, searchWords []string) ([]*model.ClipNote, error) {
	// 列に入らない文字は保存された値に現れないので、一致しえない (#3025)。
	// **引く前に弾く** — LIKE のパターンに載せるとクエリごと落ちて 500 になる。
	if !storable(clipID) || !storable(viewerID) || !allStorable(searchWords) {
		return nil, nil
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.Where("\"clipId\" = ?", clipID)
	if filterVisibility {
		// clip_note は author 混在のため、note を相関 EXISTS で join して
		// visibility を LIMIT 前に絞る。条件は core/note.CanSeeNote と一致 (#1454)。
		q = applyViewerVisibilityExists(q, `"clip_note"."noteId"`, viewerID)
	}
	// search 各語を text / cw への ILIKE 部分一致 (語内 OR、語間 AND) で絞る
	// (#1562、upstream clips/notes.ts:103-110)。LIMIT 前に適用しないと
	// ページ過少充填になるため visibility と同じく push down する。同一 note
	// 行への相関なので、語ごとに EXISTS を重ねず単一 EXISTS 内で AND する
	// (subplan が語数分走るのを避ける)。
	if len(searchWords) > 0 {
		var cond strings.Builder
		args := make([]any, 0, len(searchWords)*2)
		cond.WriteString(`EXISTS (SELECT 1 FROM "note" sn WHERE sn."id" = "clip_note"."noteId"`)
		for _, word := range searchWords {
			like := "%" + escapeLike(word) + "%"
			cond.WriteString(` AND (sn."text" ILIKE ? OR sn."cw" ILIKE ?)`)
			args = append(args, like, like)
		}
		cond.WriteString(`)`)
		q = q.Where(cond.String(), args...)
	}
	// upstream clips/notes は makePaginationQuery を note builder に対して掛けるため
	// sinceId/untilId は note.id と比較され note.id DESC (作成順, 新しい note が先頭)
	// で並ぶ。client は最後に返った note.id を untilId として渡すので、比較列も
	// 並び替え列も note.id でなければならない。clip_note.noteId は note.id (FK) と
	// 同値なので、join せず noteId 列でページネーションすれば等価になる。clip_note.id
	// (AddNote 時に独立採番される PK) で比較していた旧実装は、client が渡す note.id と
	// 無関係な ULID を比較してページ窓が壊れていた (#1950)。
	if untilID != "" {
		q = q.Where(`"clip_note"."noteId" < ?`, untilID)
	}
	if sinceID != "" {
		q = q.Where(`"clip_note"."noteId" > ?`, sinceID)
	}
	var rows []*model.ClipNote
	if err := q.Order(paginationOrder(sinceID, untilID, `"clip_note"."noteId"`)).Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// escapeLike escapes LIKE/ILIKE metacharacters so user input matches
// literally (upstream sqlLikeEscape 相当)。PostgreSQL の既定 escape 文字は
// backslash なので \ 自身もエスケープする。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
