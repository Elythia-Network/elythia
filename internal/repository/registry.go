package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// RegistryRepository provides data access for the `registry_item` table.
type RegistryRepository interface {
	// Get returns the value for a key+scope+domain combination.
	Get(userID, key string, scope []string, domain *string) (*model.RegistryItem, error)
	// Set upserts a registry item.
	Set(item *model.RegistryItem) error
	// GetAll returns all items for the user+scope+domain.
	GetAll(userID string, scope []string, domain *string) ([]*model.RegistryItem, error)
	// KeysWithType returns all keys with their value types for user+scope+domain.
	KeysWithType(userID string, scope []string, domain *string) (map[string]string, error)
	// Remove deletes a registry item.
	Remove(userID, key string, scope []string, domain *string) error
	// ScopesWithDomain returns the distinct (scope, domain) pairs that userID has.
	// i/registry/scopes-with-domain で返す要素のベースになる。
	ScopesWithDomain(userID string) ([]model.RegistryScopeDomain, error)
}

type registryRepository struct {
	db *gorm.DB
}

// NewRegistryRepository creates a new RegistryRepository.
func NewRegistryRepository(db *gorm.DB) RegistryRepository {
	return &registryRepository{db: db}
}

func (r *registryRepository) scopeQuery(q *gorm.DB, scope []string, domain *string) *gorm.DB {
	q = q.Where("scope = ?", model.StringArray(scope))
	if domain == nil {
		q = q.Where("domain IS NULL")
	} else {
		q = q.Where("domain = ?", *domain)
	}
	return q
}

func (r *registryRepository) Get(userID, key string, scope []string, domain *string) (*model.RegistryItem, error) {
	if !storable(userID) || !storable(key) || !allStorable(scope) || (domain != nil && !storable(*domain)) {
		return nil, ErrNotFound
	}
	var item model.RegistryItem
	q := r.db.Where("\"userId\" = ? AND key = ?", userID, key)
	q = r.scopeQuery(q, scope, domain)
	if err := q.First(&item).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *registryRepository) Set(item *model.RegistryItem) error {
	// 既存のアイテムを検索
	var existing model.RegistryItem
	q := r.db.Where("\"userId\" = ? AND key = ?", item.UserID, item.Key)
	q = r.scopeQuery(q, item.Scope, item.Domain)
	if err := q.First(&existing).Error; err == nil {
		// 既存アイテムを更新
		return r.db.Model(&existing).Updates(map[string]any{
			"value":     item.Value,
			"updatedAt": time.Now(),
		}).Error
	}
	// 新規作成
	return r.db.Create(item).Error
}

func (r *registryRepository) GetAll(userID string, scope []string, domain *string) ([]*model.RegistryItem, error) {
	// 列に入らない値はどの行とも一致しえない (#3025)。**引く前に弾く** —
	// 比較の右辺に載せるとクエリごと落ちて 500 になる。
	if !storable(userID) || !allStorable(scope) || (domain != nil && !storable(*domain)) {
		return nil, nil
	}
	var items []*model.RegistryItem
	q := r.db.Where("\"userId\" = ?", userID)
	q = r.scopeQuery(q, scope, domain)
	if err := q.Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

func (r *registryRepository) KeysWithType(userID string, scope []string, domain *string) (map[string]string, error) {
	// 列に入らない値はどの行とも一致しえない (#3025)。**引く前に弾く** —
	// 比較の右辺に載せるとクエリごと落ちて 500 になる。
	if !storable(userID) || !allStorable(scope) || (domain != nil && !storable(*domain)) {
		return nil, nil
	}
	items, err := r.GetAll(userID, scope, domain)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(items))
	for _, item := range items {
		result[item.Key] = detectJSONType(item.Value)
	}
	return result, nil
}

func (r *registryRepository) Remove(userID, key string, scope []string, domain *string) error {
	// 列に入らない値はどの行とも一致しえない (#3025)。**引く前に弾く** —
	// 比較の右辺に載せるとクエリごと落ちて 500 になる。
	if !storable(userID) || !storable(key) || !allStorable(scope) || (domain != nil && !storable(*domain)) {
		return nil
	}
	q := r.db.Where("\"userId\" = ? AND key = ?", userID, key)
	q = r.scopeQuery(q, scope, domain)
	return q.Delete(&model.RegistryItem{}).Error
}

func (r *registryRepository) ScopesWithDomain(userID string) ([]model.RegistryScopeDomain, error) {
	var items []*model.RegistryItem
	if err := r.db.
		Select(`DISTINCT "scope", "domain"`).
		Where(`"userId" = ?`, userID).
		Find(&items).Error; err != nil {
		return nil, err
	}
	out := make([]model.RegistryScopeDomain, 0, len(items))
	for _, it := range items {
		out = append(out, model.RegistryScopeDomain{Scope: []string(it.Scope), Domain: it.Domain})
	}
	return out, nil
}

// detectJSONType returns the JSON type name for a value.
func detectJSONType(value []byte) string {
	if len(value) == 0 {
		return "null"
	}
	switch value[0] {
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	case '[':
		return "array"
	case '{':
		return "object"
	default:
		return "number"
	}
}
