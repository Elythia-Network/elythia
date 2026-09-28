package repository

import (
	"gorm.io/gorm"

	"github.com/shiroha-a/mk/internal/model"
)

// FederationRuleRepository stores the mk-go-only federation rules (#3090).
type FederationRuleRepository struct {
	db *gorm.DB
}

// NewFederationRuleRepository constructs a FederationRuleRepository.
func NewFederationRuleRepository(db *gorm.DB) *FederationRuleRepository {
	return &FederationRuleRepository{db: db}
}

// List returns every rule in evaluation order (position, then id).
func (r *FederationRuleRepository) List() ([]*model.FederationRule, error) {
	var rules []*model.FederationRule
	if err := r.db.Order(`"position" ASC, "id" ASC`).Find(&rules).Error; err != nil {
		return nil, err
	}
	return rules, nil
}

// FindByID returns the rule, or ErrNotFound.
func (r *FederationRuleRepository) FindByID(id string) (*model.FederationRule, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var rule model.FederationRule
	if err := r.db.Where(`"id" = ?`, id).Take(&rule).Error; err != nil {
		return nil, err
	}
	return &rule, nil
}

// Count returns the number of rules.
func (r *FederationRuleRepository) Count() (int64, error) {
	var n int64
	err := r.db.Model(&model.FederationRule{}).Count(&n).Error
	return n, err
}

// Create inserts a rule.
func (r *FederationRuleRepository) Create(rule *model.FederationRule) error {
	return r.db.Create(rule).Error
}

// Update overwrites every column of an existing rule. 行が無ければ ErrNotFound。
func (r *FederationRuleRepository) Update(rule *model.FederationRule) error {
	// Save は行が無いと INSERT に倒れるので使わない (消された直後の更新で
	// ルールが復活する)。Select("*") で false / 空配列 / nil も書く。
	res := r.db.Model(&model.FederationRule{}).Where(`"id" = ?`, rule.ID).
		Select("*").Omit("id", "createdAt").Updates(rule)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a rule. 行が無ければ ErrNotFound。
func (r *FederationRuleRepository) Delete(id string) error {
	if !storable(id) {
		return ErrNotFound
	}
	res := r.db.Where(`"id" = ?`, id).Delete(&model.FederationRule{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
