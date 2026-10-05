package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// SigninRepository provides data access for signin history.
type SigninRepository interface {
	Create(s *model.Signin) error
	ListByUserID(userID string, limit int, untilID, sinceID string) ([]*model.Signin, error)
}

type signinRepository struct {
	db *gorm.DB
}

// NewSigninRepository creates a new SigninRepository.
func NewSigninRepository(db *gorm.DB) SigninRepository {
	return &signinRepository{db: db}
}

func (r *signinRepository) Create(s *model.Signin) error {
	return r.db.Create(s).Error
}

func (r *signinRepository) ListByUserID(userID string, limit int, untilID, sinceID string) ([]*model.Signin, error) {
	q := r.db.Where(`"userId" = ?`, userID)
	if untilID != "" {
		q = q.Where(`"id" < ?`, untilID)
	}
	if sinceID != "" {
		q = q.Where(`"id" > ?`, sinceID)
	}
	q = q.Order(paginationOrder(sinceID, untilID, `"id"`)).Limit(limit)

	var rows []*model.Signin
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
