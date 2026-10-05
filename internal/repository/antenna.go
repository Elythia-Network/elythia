package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// AntennaRepository provides data access for the `antenna` table.
type AntennaRepository interface {
	Create(a *model.Antenna) error
	FindByID(id string) (*model.Antenna, error)
	UpdateFields(antennaID string, fields map[string]any) error
	Delete(a *model.Antenna) error
	ListByUser(userID string) ([]*model.Antenna, error)
	// CountByUser returns the number of antennas owned by userID. Used by
	// antennaLimit policy gate so we don't have to fetch all rows just to
	// count them (#1029 PR-1 follow-up).
	CountByUser(userID string) (int64, error)
	ListAllActive() ([]*model.Antenna, error)
	// DeactivateUnusedSince sets isActive=false on every active antenna whose
	// lastUsedAt is older than the given cutoff. Used by the `clean` cron to
	// auto-deactivate antennas that have not been read/updated in a while
	// (upstream CleanProcessorService, #1604). Returns rows deactivated.
	DeactivateUnusedSince(cutoff time.Time) (int64, error)
}

type antennaRepository struct {
	db *gorm.DB
}

// NewAntennaRepository creates a new AntennaRepository.
func NewAntennaRepository(db *gorm.DB) AntennaRepository {
	return &antennaRepository{db: db}
}

func (r *antennaRepository) Create(a *model.Antenna) error {
	return r.db.Create(a).Error
}

func (r *antennaRepository) FindByID(id string) (*model.Antenna, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var a model.Antenna
	if err := r.db.First(&a, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// UpdateFields applies a map of column → value updates to the antenna row.
func (r *antennaRepository) UpdateFields(antennaID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.Model(&model.Antenna{}).Where("id = ?", antennaID).Updates(fields).Error
}

func (r *antennaRepository) Delete(a *model.Antenna) error {
	return r.db.Delete(a).Error
}

// ListByUser returns the antennas owned by the given user, ordered by id desc.
func (r *antennaRepository) ListByUser(userID string) ([]*model.Antenna, error) {
	var rows []*model.Antenna
	if err := r.db.Where("\"userId\" = ?", userID).
		Order("id DESC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// CountByUser returns the number of antennas owned by the given user.
func (r *antennaRepository) CountByUser(userID string) (int64, error) {
	var count int64
	if err := r.db.Model(&model.Antenna{}).
		Where(`"userId" = ?`, userID).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// DeactivateUnusedSince sets isActive=false on active antennas not used since
// the cutoff. Upstream blindly updates all rows older than the cutoff; we add
// `isActive = true` so RowsAffected reflects only the antennas this run newly
// deactivated and we skip rewriting already-inactive rows.
func (r *antennaRepository) DeactivateUnusedSince(cutoff time.Time) (int64, error) {
	res := r.db.Model(&model.Antenna{}).
		Where(`"lastUsedAt" < ? AND "isActive" = ?`, cutoff, true).
		Update("isActive", false)
	return res.RowsAffected, res.Error
}

// ListAllActive returns every antenna with isActive=true.
//
// **この層は毎回 DB を引く。** note ごとの呼び出しは #2752 で
// CachedAntennaRepository が吸収するので、ここに直接叩く経路を足すときは
// キャッシュを迂回することになる点に注意する (router は wrapper を配線する)。
func (r *antennaRepository) ListAllActive() ([]*model.Antenna, error) {
	var rows []*model.Antenna
	if err := r.db.Where("\"isActive\" = ?", true).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
