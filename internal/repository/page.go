package repository

import (
	"encoding/json"
	"errors"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PageRepository provides data access for the `page` table.
type PageRepository interface {
	Create(p *model.Page) error
	FindByID(id string) (*model.Page, error)
	// FindManyByIDs returns pages for the given ID set in a single query.
	// Used by /api/i/page-likes to batch-resolve `like.pageId → page` and
	// avoid an N+1 lookup over the like list (#1136).
	FindManyByIDs(ids []string) ([]*model.Page, error)
	FindByUserAndName(userID, name string) (*model.Page, error)
	UpdateFields(pageID string, fields map[string]any) error
	Delete(p *model.Page) error
	// ListByUser returns pages owned by userID with cursor (sinceID/untilID)
	// or offset pagination. Cursor 指定時は offset 無視。
	ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Page, error)
	// ListPublicByUser returns only public pages owned by userID, used by
	// users/pages when viewer is not the owner. Same cursor semantics.
	ListPublicByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Page, error)
	// ListFeatured returns the most-liked public pages with cursor or
	// offset pagination.
	ListFeatured(sinceID, untilID string, limit, offset int) ([]*model.Page, error)
	IncrementCount(pageID, column string, delta int) error
}

type pageRepository struct {
	db *gorm.DB
}

// NewPageRepository creates a new PageRepository.
func NewPageRepository(db *gorm.DB) PageRepository {
	return &pageRepository{db: db}
}

// Create inserts p and increments pageCount of every note its content
// references, in one transaction (upstream PageService.create).
func (r *pageRepository) Create(p *model.Page) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(p).Error; err != nil {
			return err
		}
		return adjustNotePageCount(tx, referencedNoteIDs(p.Content), 1)
	})
}

func (r *pageRepository) FindByID(id string) (*model.Page, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var p model.Page
	if err := r.db.First(&p, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *pageRepository) FindManyByIDs(ids []string) ([]*model.Page, error) {
	ids = storableIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	var pages []*model.Page
	if err := r.db.Where("id IN ?", ids).Find(&pages).Error; err != nil {
		return nil, err
	}
	return pages, nil
}

// FindByUserAndName looks up a page by the (userId, name) pair which is the
// primary user-facing identity for a Page in Misskey.
func (r *pageRepository) FindByUserAndName(userID, name string) (*model.Page, error) {
	if !storable(userID) || !storable(name) {
		return nil, ErrNotFound
	}
	var p model.Page
	if err := r.db.Where("\"userId\" = ? AND name = ?", userID, name).First(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// UpdateFields applies fields to the page. When fields replaces the content,
// pageCount of notes the page stopped referencing is decremented and that of
// newly referenced notes is incremented, in the same transaction
// (upstream PageService.update).
//
// 旧 content は行をロックしてから読む。ロックせずに読むと、同じページへの並行な
// 更新が同じ旧 content を差分の基準にして、増減を二重に数える (本家も
// `for_no_key_update` でロックする)。
func (r *pageRepository) UpdateFields(pageID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	newContent, hasContent := fields["content"]
	if !hasContent {
		return r.db.Model(&model.Page{}).Where("id = ?", pageID).Updates(fields).Error
	}
	return r.db.Transaction(func(tx *gorm.DB) error {
		var cur model.Page
		err := tx.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).
			Select("id", "content").First(&cur, "id = ?", pageID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := tx.Model(&model.Page{}).Where("id = ?", pageID).Updates(fields).Error; err != nil {
			return err
		}
		before := referencedNoteIDs(cur.Content)
		after := referencedNoteIDs(contentBytes(newContent))
		if err := adjustNotePageCount(tx, idsMissingFrom(before, after), -1); err != nil {
			return err
		}
		return adjustNotePageCount(tx, idsMissingFrom(after, before), 1)
	})
}

// Delete removes the page and decrements pageCount of every note its content
// references, in one transaction (upstream PageService.delete).
//
// 引数の p.Content は呼び出し側が読んだ時点の値なので使わず、ロックして読み直した
// content で数える。消えていれば何もしない (二重に減らさない)。
func (r *pageRepository) Delete(p *model.Page) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		var cur model.Page
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "content").First(&cur, "id = ?", p.ID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := tx.Delete(&model.Page{}, "id = ?", p.ID).Error; err != nil {
			return err
		}
		return adjustNotePageCount(tx, referencedNoteIDs(cur.Content), -1)
	})
}

// referencedNoteIDs returns the distinct note IDs a page content references.
// Mirrors upstream PageService.collectReferencedNotes: `note` blocks whose
// `note` is a string, descending into `section` blocks' `children`.
// Unreadable content references nothing.
func referencedNoteIDs(content []byte) []string {
	var blocks []any
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	var walk func([]any)
	walk = func(bs []any) {
		for _, b := range bs {
			m, ok := b.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "note":
				if id, ok := m["note"].(string); ok {
					if _, dup := seen[id]; !dup {
						seen[id] = struct{}{}
						out = append(out, id)
					}
				}
			case "section":
				if children, ok := m["children"].([]any); ok {
					walk(children)
				}
			}
		}
	}
	walk(blocks)
	return out
}

// contentBytes converts the value UpdateFields received for `content`.
// page_service は jsonb 列へ渡すために string にして渡す (#3037)。
func contentBytes(v any) []byte {
	switch c := v.(type) {
	case string:
		return []byte(c)
	case []byte:
		return c
	case json.RawMessage:
		return c
	case datatypes.JSON:
		return c
	default:
		return nil
	}
}

// idsMissingFrom returns the IDs in a that are not in b.
func idsMissingFrom(a, b []string) []string {
	in := make(map[string]struct{}, len(b))
	for _, id := range b {
		in[id] = struct{}{}
	}
	var out []string
	for _, id := range a {
		if _, ok := in[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// adjustNotePageCount adds delta to pageCount of the given notes, keeping it
// within the smallint range (0..32767).
//
// 0 で止めるのは他の note のカウンタと同じ (#3291)。上限で止めるのは、列の範囲を
// 超えてページの作成・更新ごと失敗させないため。**int に広げてから足す** —
// smallint のまま足すと LEAST に届く前に smallint の範囲外で落ちる。存在しない
// ノートの id は 0 行の更新になるだけ。
func adjustNotePageCount(tx *gorm.DB, noteIDs []string, delta int) error {
	noteIDs = storableIDs(noteIDs)
	if len(noteIDs) == 0 {
		return nil
	}
	return tx.Model(&model.Note{}).Where("id IN ?", noteIDs).
		UpdateColumn("pageCount", gorm.Expr(`LEAST(GREATEST("pageCount"::int + ?::int, 0), 32767)`, delta)).Error
}

// ListByUser returns the pages owned by userID with cursor (sinceID/untilID)
// or offset pagination. cursor mode はupstream `makePaginationQuery` 同様
// `id` で sort + filter する (元実装の `updatedAt DESC` は frontend
// Paginator の untilId/sinceId と整合しないため変更)。
func (r *pageRepository) ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Page, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.Where(`"userId" = ?`, userID)
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	q = q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit)
	if sinceID == "" && untilID == "" && offset > 0 {
		q = q.Offset(offset)
	}
	var rows []*model.Page
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *pageRepository) ListPublicByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Page, error) {
	// 列に入らない値はどの行とも一致しえない (#3025)。**引く前に弾く** —
	// 比較の右辺に載せるとクエリごと落ちて 500 になる。
	if !storable(userID) || !storable(sinceID) || !storable(untilID) {
		return nil, nil
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.Where(`"userId" = ? AND visibility = ?`, userID, string(model.PageVisibilityPublic))
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	q = q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit)
	if sinceID == "" && untilID == "" && offset > 0 {
		q = q.Offset(offset)
	}
	var rows []*model.Page
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ListFeatured returns the most-liked public pages. cursor (sinceID/untilID)
// 指定時はそれを優先。指定なしは likedCount DESC + offset で従来通り。
func (r *pageRepository) ListFeatured(sinceID, untilID string, limit, offset int) ([]*model.Page, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	// upstream pages/featured は public かつ likedCount>0 の page のみを返す
	// (#1548)。0 like の public page は featured に出さない。
	q := r.db.Where("visibility = ?", string(model.PageVisibilityPublic)).
		Where(`"likedCount" > 0`)
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" || untilID != "" {
		q = q.Order(paginationOrder(sinceID, untilID, "id"))
	} else {
		q = q.Order(`"likedCount" DESC`)
	}
	q = q.Limit(limit)
	if sinceID == "" && untilID == "" && offset > 0 {
		q = q.Offset(offset)
	}
	var rows []*model.Page
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// IncrementCount adjusts a counter column on the page row by delta.
func (r *pageRepository) IncrementCount(pageID, column string, delta int) error {
	return r.db.Model(&model.Page{}).
		Where("id = ?", pageID).
		UpdateColumn(column, gorm.Expr("\""+column+"\" + ?", delta)).Error
}
