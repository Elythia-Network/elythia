package repository

import (
	"fmt"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// IPLookupLogRepository stores and reads the IP lookup audit trail (#3106).
type IPLookupLogRepository interface {
	// Create appends one audit record.
	Create(l *model.IPLookupLog) error
	// List returns records newest first, capped at limit+1 so the caller can
	// tell whether another page exists.
	List(limit, offset int) ([]*model.IPLookupLog, error)
	// DeleteOlderThan prunes records created before t. Returns rows deleted.
	//
	// **保持期間が要る理由は記録の中身。** ここには照会に使った IP が入るので、
	// 永久に残すと `moderation_log` に IP を書くのと変わらなくなる (#3106)。
	DeleteOlderThan(t time.Time) (int64, error)
}

type ipLookupLogRepository struct {
	db *gorm.DB
	// lookupTimeout は一覧の読み取りに掛ける上限。既定は lookupStatementTimeout。
	// **照会本体と同じ扱いにする** — この一覧も IP が並ぶモデレーションの読み取りで、
	// ここだけ無制限だと doc の「負荷の上限」が endpoint ごとに嘘になる。
	lookupTimeout time.Duration
}

// NewIPLookupLogRepository constructs the default IPLookupLogRepository.
func NewIPLookupLogRepository(db *gorm.DB) IPLookupLogRepository {
	return &ipLookupLogRepository{db: db, lookupTimeout: lookupStatementTimeout}
}

func (r *ipLookupLogRepository) Create(l *model.IPLookupLog) error {
	if err := r.db.Create(l).Error; err != nil {
		return fmt.Errorf("ip_lookup_log create: %w", err)
	}
	return nil
}

func (r *ipLookupLogRepository) List(limit, offset int) ([]*model.IPLookupLog, error) {
	if limit <= 0 {
		limit = 1
	}
	if offset < 0 {
		offset = 0
	}
	var rows []*model.IPLookupLog
	// 並びは新しい順 → id 降順。`createdAt` が同じ行で順序が実行ごとに変わると、
	// ページを送ったときに同じ行が 2 回出たり抜けたりする。
	if err := withStatementTimeout(r.db, r.lookupTimeout, func(tx *gorm.DB) error {
		return tx.Order(`"createdAt" DESC, id DESC`).Limit(limit).Offset(offset).Find(&rows).Error
	}); err != nil {
		return nil, fmt.Errorf("ip_lookup_log list: %w", err)
	}
	return rows, nil
}

func (r *ipLookupLogRepository) DeleteOlderThan(t time.Time) (int64, error) {
	res := r.db.Where(`"createdAt" < ?`, t).Delete(&model.IPLookupLog{})
	if res.Error != nil {
		return 0, fmt.Errorf("ip_lookup_log prune: %w", res.Error)
	}
	return res.RowsAffected, nil
}
