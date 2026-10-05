package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
	"gorm.io/plugin/dbresolver"
)

// UserNotePiningRepository provides data access for the `user_note_pining` table.
type UserNotePiningRepository interface {
	Create(p *model.UserNotePining) error
	Delete(p *model.UserNotePining) error
	FindByPair(userID, noteID string) (*model.UserNotePining, error)
	ListByUser(userID string) ([]*model.UserNotePining, error)
	CountByUser(userID string) (int, error)
	// ReplaceByUser swaps every pin of userID for pins in one transaction.
	//
	// リモートの featured コレクションを取り込む経路で使う (#2552)。差分更新に
	// しないのは、**リモート側で外されたピンを残さない**ため。upstream
	// ApPersonService.updateFeatured も同じく全削除してから入れ直す。
	ReplaceByUser(userID string, pins []*model.UserNotePining) error
}

// UserNotePiningBatchReader lists the pins of many users in one query.
//
// 一覧の応答 (users/followers など) でピン留めを利用者ごとに引くと N+1 になる。
// 本家 UserEntityService.packMany もピン留めを IN でまとめて引く。
type UserNotePiningBatchReader interface {
	// ListByUsers returns the pins of userIDs ordered by id DESC.
	ListByUsers(userIDs []string) ([]*model.UserNotePining, error)
}

type userNotePiningRepository struct {
	db *gorm.DB
}

// NewUserNotePiningRepository creates a new UserNotePiningRepository.
func NewUserNotePiningRepository(db *gorm.DB) UserNotePiningRepository {
	return &userNotePiningRepository{db: db}
}

func (r *userNotePiningRepository) Create(p *model.UserNotePining) error {
	return r.db.Create(p).Error
}

func (r *userNotePiningRepository) Delete(p *model.UserNotePining) error {
	return r.db.Delete(p).Error
}

func (r *userNotePiningRepository) FindByPair(userID, noteID string) (*model.UserNotePining, error) {
	if !storable(userID) || !storable(noteID) {
		return nil, ErrNotFound
	}
	var p model.UserNotePining
	if err := r.db.Clauses(dbresolver.Write).
		Where("\"userId\" = ? AND \"noteId\" = ?", userID, noteID).
		First(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *userNotePiningRepository) ListByUser(userID string) ([]*model.UserNotePining, error) {
	var rows []*model.UserNotePining
	if err := r.db.Clauses(dbresolver.Write).
		Where("\"userId\" = ?", userID).
		Order("id DESC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *userNotePiningRepository) ListByUsers(userIDs []string) ([]*model.UserNotePining, error) {
	userIDs = storableIDs(userIDs)
	if len(userIDs) == 0 {
		return nil, nil
	}
	var rows []*model.UserNotePining
	if err := r.db.Clauses(dbresolver.Write).
		Where("\"userId\" IN ?", userIDs).
		Order("id DESC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *userNotePiningRepository) ReplaceByUser(userID string, pins []*model.UserNotePining) error {
	// 削除と挿入の間に他経路 (inbound Add) が割り込むと、ピンが一瞬消えた状態が
	// 見える。トランザクションに包んで外から見えないようにする。
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("\"userId\" = ?", userID).Delete(&model.UserNotePining{}).Error; err != nil {
			return err
		}
		if len(pins) == 0 {
			return nil
		}
		return tx.Create(pins).Error
	})
}

func (r *userNotePiningRepository) CountByUser(userID string) (int, error) {
	var count int64
	if err := r.db.Model(&model.UserNotePining{}).
		Where("\"userId\" = ?", userID).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return int(count), nil
}
