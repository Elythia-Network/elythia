package repository

import (
	"github.com/shiroha-a/mk/internal/model"
	"gorm.io/gorm"
)

// SwSubscriptionRepository handles sw_subscription persistence.
type SwSubscriptionRepository interface {
	FindByUserAndEndpoint(userID, endpoint string) (*model.SwSubscription, error)
	// FindByUserEndpointAuthKey looks up a subscription by the full
	// (userId, endpoint, auth, publickey) tuple. sw/register uses this to detect
	// a genuine duplicate: a key rotation on the same endpoint must NOT match an
	// existing row, so the handler inserts a fresh subscription with the current
	// keys (upstream register.ts findOneBy 4-tuple、#1775)。
	FindByUserEndpointAuthKey(userID, endpoint, auth, publicKey string) (*model.SwSubscription, error)
	FindByUserID(userID string) ([]*model.SwSubscription, error)
	Create(sub *model.SwSubscription) error
	Update(sub *model.SwSubscription) error
	// FindByEndpointAuthKey returns every subscription matching the
	// (endpoint, auth, publickey) triple, narrowed to userID when it is non-nil
	// (upstream sw/unregister findBy). A value that can never be stored matches
	// nothing and yields an empty result.
	FindByEndpointAuthKey(userID *string, endpoint, auth, publicKey string) ([]*model.SwSubscription, error)
	DeleteByIDs(ids []string) error
	DeleteByUserAndEndpoint(userID, endpoint string) error
}

type swSubscriptionRepository struct {
	db *gorm.DB
}

// NewSwSubscriptionRepository creates a new SwSubscriptionRepository.
func NewSwSubscriptionRepository(db *gorm.DB) SwSubscriptionRepository {
	return &swSubscriptionRepository{db: db}
}

func (r *swSubscriptionRepository) FindByUserAndEndpoint(userID, endpoint string) (*model.SwSubscription, error) {
	if !storable(userID) || !storable(endpoint) {
		return nil, ErrNotFound
	}
	var sub model.SwSubscription
	if err := r.db.Where(`"userId" = ? AND "endpoint" = ?`, userID, endpoint).First(&sub).Error; err != nil {
		return nil, err
	}
	return &sub, nil
}

func (r *swSubscriptionRepository) FindByUserEndpointAuthKey(userID, endpoint, auth, publicKey string) (*model.SwSubscription, error) {
	if !storable(userID) || !storable(endpoint) || !storable(auth) || !storable(publicKey) {
		return nil, ErrNotFound
	}
	var sub model.SwSubscription
	if err := r.db.Where(`"userId" = ? AND "endpoint" = ? AND "auth" = ? AND "publickey" = ?`,
		userID, endpoint, auth, publicKey).First(&sub).Error; err != nil {
		return nil, err
	}
	return &sub, nil
}

func (r *swSubscriptionRepository) FindByUserID(userID string) ([]*model.SwSubscription, error) {
	var subs []*model.SwSubscription
	if err := r.db.Where(`"userId" = ?`, userID).Find(&subs).Error; err != nil {
		return nil, err
	}
	return subs, nil
}

func (r *swSubscriptionRepository) Create(sub *model.SwSubscription) error {
	return r.db.Create(sub).Error
}

func (r *swSubscriptionRepository) Update(sub *model.SwSubscription) error {
	return r.db.Save(sub).Error
}

func (r *swSubscriptionRepository) FindByEndpointAuthKey(userID *string, endpoint, auth, publicKey string) ([]*model.SwSubscription, error) {
	if !storable(endpoint) || !storable(auth) || !storable(publicKey) || (userID != nil && !storable(*userID)) {
		return []*model.SwSubscription{}, nil
	}
	q := r.db.Where(`"endpoint" = ? AND "auth" = ? AND "publickey" = ?`, endpoint, auth, publicKey)
	if userID != nil {
		q = q.Where(`"userId" = ?`, *userID)
	}
	var subs []*model.SwSubscription
	if err := q.Find(&subs).Error; err != nil {
		return nil, err
	}
	return subs, nil
}

func (r *swSubscriptionRepository) DeleteByIDs(ids []string) error {
	ids = storableIDs(ids)
	if len(ids) == 0 {
		return nil
	}
	return r.db.Where(`"id" IN ?`, ids).Delete(&model.SwSubscription{}).Error
}

func (r *swSubscriptionRepository) DeleteByUserAndEndpoint(userID, endpoint string) error {
	return r.db.Where(`"userId" = ? AND "endpoint" = ?`, userID, endpoint).Delete(&model.SwSubscription{}).Error
}
