package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MutingRepository provides data access for the `muting` table.
type MutingRepository interface {
	Create(m *model.Muting) error
	Delete(m *model.Muting) error
	FindByPair(muterID, muteeID string) (*model.Muting, error)
	Exists(muterID, muteeID string) (bool, error)
	// ListByMuter supports cursor (sinceID/untilID) and offset pagination.
	// Cursor 指定時は offset を無視 (upstream makePaginationQuery と一致)。
	ListByMuter(muterID, sinceID, untilID string, limit, offset int) ([]*model.Muting, error)
	// ListMuteeIDs returns the muteeIDs of active (non-expired) mute rows
	// for muterID. timeline endpoint で muted user の note を除外する
	// filter 用 (#874)。
	ListMuteeIDs(muterID string) ([]string, error)
	// ListByMutee returns the active (non-expired) mute rows whose muteeId
	// equals the given user (= who mutes this user).
	//
	// アカウント移行のミュート引き継ぎ (#2419) 用。upstream copyMutings は
	// `findBy([{muteeId, expiresAt: IsNull()}, {muteeId, expiresAt: MoreThan(now)}])`
	// で期限切れを除外する。expiresAt をそのまま移行先へ引き継ぐため、ID だけ
	// でなく行そのものを返す。
	ListByMutee(muteeID string) ([]*model.Muting, error)
	// ListAllMuteeIDs returns the muteeIDs of ALL mute rows for muterID,
	// including temporarily-expired ones. export-following の excludeMuting は
	// upstream が expiry filter なしの mutingsRepository.findBy({muterId}) を使う
	// ため、active-only の ListMuteeIDs ではなく本メソッドで一致させる (#1555)。
	ListAllMuteeIDs(muterID string) ([]string, error)
	// DeleteExpired physically removes muting rows whose expiresAt has
	// passed. Read filters (ListMuteeIDs/Exists) already exclude them; this
	// is the active prune run by the checkExpiredMutings cron (#1563).
	//
	// Returns the muterId of every deleted row — **one entry per row**, so
	// duplicates appear when a single user had several mutes expire at once
	// and len() equals the old RowsAffected return.
	//
	// 行を数えるだけでなく所有者を返すのは、streaming の snapshot を取り直させる
	// ため (#2453)。接続中の connection は mute 集合を接続時に 1 回だけ読むので、
	// 失効を通知しないと再接続まで mute されたままになる。distinct にしないのは
	// 呼び出し側の prune 件数ログの意味を保つため。
	DeleteExpired(now time.Time) ([]string, error)
}

type mutingRepository struct {
	db *gorm.DB
}

// NewMutingRepository creates a new MutingRepository.
func NewMutingRepository(db *gorm.DB) MutingRepository {
	return &mutingRepository{db: db}
}

func (r *mutingRepository) Create(m *model.Muting) error {
	return r.db.Create(m).Error
}

func (r *mutingRepository) Delete(m *model.Muting) error {
	return r.db.Delete(m).Error
}

func (r *mutingRepository) FindByPair(muterID, muteeID string) (*model.Muting, error) {
	if !storable(muterID) || !storable(muteeID) {
		return nil, ErrNotFound
	}
	var m model.Muting
	if err := r.db.Where("\"muterId\" = ? AND \"muteeId\" = ?", muterID, muteeID).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

// Exists reports whether muter has an active (non-expired) mute on mutee.
func (r *mutingRepository) Exists(muterID, muteeID string) (bool, error) {
	var count int64
	now := time.Now()
	if err := r.db.Model(&model.Muting{}).
		Where("\"muterId\" = ? AND \"muteeId\" = ?", muterID, muteeID).
		Where("\"expiresAt\" IS NULL OR \"expiresAt\" > ?", now).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *mutingRepository) ListByMuter(muterID, sinceID, untilID string, limit, offset int) ([]*model.Muting, error) {
	q := r.db.Where(`"muterId" = ?`, muterID)
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
	var rows []*model.Muting
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ListMuteeIDs returns muteeIDs where muterID has an active (non-expired)
// mute. timeline endpoint で muted user の note を除外する filter 用 (#874)。
func (r *mutingRepository) ListMuteeIDs(muterID string) ([]string, error) {
	if muterID == "" {
		return nil, nil
	}
	var ids []string
	now := time.Now()
	if err := r.db.Model(&model.Muting{}).
		Where(`"muterId" = ?`, muterID).
		Where(`"expiresAt" IS NULL OR "expiresAt" > ?`, now).
		Pluck(`"muteeId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *mutingRepository) ListByMutee(muteeID string) ([]*model.Muting, error) {
	if muteeID == "" {
		return nil, nil
	}
	var rows []*model.Muting
	now := time.Now()
	if err := r.db.
		Where(`"muteeId" = ?`, muteeID).
		Where(`"expiresAt" IS NULL OR "expiresAt" > ?`, now).
		Order("id").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ListAllMuteeIDs returns every muteeId for muterID with no expiry filter
// (upstream export-following が使う raw mutingsRepository.findBy 相当)。
func (r *mutingRepository) ListAllMuteeIDs(muterID string) ([]string, error) {
	if muterID == "" {
		return nil, nil
	}
	var ids []string
	if err := r.db.Model(&model.Muting{}).
		Where(`"muterId" = ?`, muterID).
		Pluck(`"muteeId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *mutingRepository) DeleteExpired(now time.Time) ([]string, error) {
	// DELETE ... RETURNING で 1 文にする。SELECT してから DELETE すると、その間に
	// 別の prune (cron の重複起動や手動 unmute) が走った分を取りこぼす。
	var deleted []model.Muting
	res := r.db.
		Clauses(clause.Returning{Columns: []clause.Column{{Name: "muterId"}}}).
		Where(`"expiresAt" IS NOT NULL AND "expiresAt" < ?`, now).
		Delete(&deleted)
	if res.Error != nil {
		return nil, res.Error
	}
	muterIDs := make([]string, 0, len(deleted))
	for _, m := range deleted {
		muterIDs = append(muterIDs, m.MuterID)
	}
	return muterIDs, nil
}

// RenoteMutingRepository provides data access for the `renote_muting` table.
type RenoteMutingRepository interface {
	Create(m *model.RenoteMuting) error
	Delete(m *model.RenoteMuting) error
	FindByPair(muterID, muteeID string) (*model.RenoteMuting, error)
	Exists(muterID, muteeID string) (bool, error)
	// ListByMuter supports cursor (sinceID/untilID) and offset pagination.
	ListByMuter(muterID, sinceID, untilID string, limit, offset int) ([]*model.RenoteMuting, error)
	// ListMuteeIDs returns the muteeIDs of renote-mute rows for muterID.
	// timeline endpoint で renote-mute されたユーザーの pure renote を
	// 除外する filter 用 (#903)。MutingRepository.ListMuteeIDs と同 shape。
	ListMuteeIDs(muterID string) ([]string, error)
}

type renoteMutingRepository struct {
	db *gorm.DB
}

// NewRenoteMutingRepository creates a new RenoteMutingRepository.
func NewRenoteMutingRepository(db *gorm.DB) RenoteMutingRepository {
	return &renoteMutingRepository{db: db}
}

func (r *renoteMutingRepository) Create(m *model.RenoteMuting) error {
	return r.db.Create(m).Error
}

func (r *renoteMutingRepository) Delete(m *model.RenoteMuting) error {
	return r.db.Delete(m).Error
}

func (r *renoteMutingRepository) FindByPair(muterID, muteeID string) (*model.RenoteMuting, error) {
	if !storable(muterID) || !storable(muteeID) {
		return nil, ErrNotFound
	}
	var m model.RenoteMuting
	if err := r.db.Where("\"muterId\" = ? AND \"muteeId\" = ?", muterID, muteeID).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *renoteMutingRepository) Exists(muterID, muteeID string) (bool, error) {
	var count int64
	if err := r.db.Model(&model.RenoteMuting{}).
		Where("\"muterId\" = ? AND \"muteeId\" = ?", muterID, muteeID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// ListMuteeIDs returns muteeIDs of all renote-mute rows for muterID.
// renote_muting には expiresAt が無いので active filter は不要 (= 解除は
// row delete で完結)。MutingRepository.ListMuteeIDs と同 shape (#903)。
func (r *renoteMutingRepository) ListMuteeIDs(muterID string) ([]string, error) {
	if muterID == "" {
		return nil, nil
	}
	var ids []string
	if err := r.db.Model(&model.RenoteMuting{}).
		Where(`"muterId" = ?`, muterID).
		Pluck(`"muteeId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *renoteMutingRepository) ListByMuter(muterID, sinceID, untilID string, limit, offset int) ([]*model.RenoteMuting, error) {
	q := r.db.Where(`"muterId" = ?`, muterID)
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
	var rows []*model.RenoteMuting
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
