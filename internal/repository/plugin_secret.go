package repository

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/elythia-network/elythia/internal/model"
)

// PluginSecretRepository stores the encrypted secret values of server plugins
// (#3470). 暗号化と復号は呼び出し側 (core/pluginsecret) の責務で、ここは
// 暗号文を出し入れするだけ。
type PluginSecretRepository struct {
	db *gorm.DB
}

// NewPluginSecretRepository constructs a PluginSecretRepository.
func NewPluginSecretRepository(db *gorm.DB) *PluginSecretRepository {
	return &PluginSecretRepository{db: db}
}

// FindByName returns one secret, or ErrNotFound.
func (r *PluginSecretRepository) FindByName(ctx context.Context, pluginName, name string) (*model.PluginSecret, error) {
	if !storable(pluginName) || !storable(name) {
		return nil, ErrNotFound
	}
	var s model.PluginSecret
	if err := r.db.WithContext(ctx).
		Where(`"pluginName" = ? AND "name" = ?`, pluginName, name).
		Take(&s).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

// ListByPlugin returns every secret of one plugin, ordered by name.
func (r *PluginSecretRepository) ListByPlugin(ctx context.Context, pluginName string) ([]*model.PluginSecret, error) {
	if !storable(pluginName) {
		return nil, nil
	}
	var out []*model.PluginSecret
	if err := r.db.WithContext(ctx).
		Where(`"pluginName" = ?`, pluginName).
		Order(`"name" ASC`).
		Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// Upsert inserts the secret or replaces the existing one.
func (r *PluginSecretRepository) Upsert(ctx context.Context, s *model.PluginSecret) error {
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "pluginName"}, {Name: "name"}},
		DoUpdates: clause.AssignmentColumns([]string{"ciphertext", "updatedAt"}),
	}).Create(s).Error
}

// Delete removes one secret. 無い行を消しても成功にする (呼び出し側の
// 「消えている状態にする」と一致させるため)。
func (r *PluginSecretRepository) Delete(ctx context.Context, pluginName, name string) error {
	if !storable(pluginName) || !storable(name) {
		return nil
	}
	return r.db.WithContext(ctx).
		Where(`"pluginName" = ? AND "name" = ?`, pluginName, name).
		Delete(&model.PluginSecret{}).Error
}
