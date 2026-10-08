package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// PluginAccountRepository reads the local accounts that plugins manage
// (user.managedByPlugin, #3468).
type PluginAccountRepository interface {
	// ListByPlugin returns the live (not deleted) local accounts managed by
	// the named plugin, oldest first.
	ListByPlugin(plugin string) ([]*model.User, error)
}

type pluginAccountRepository struct {
	db *gorm.DB
}

// NewPluginAccountRepository creates a PluginAccountRepository.
func NewPluginAccountRepository(db *gorm.DB) PluginAccountRepository {
	return &pluginAccountRepository{db: db}
}

// ListByPlugin は削除済み (isDeleted) を返さない。削除はジョブで後から行が
// 消えるので、その間に一覧へ出すとプラグインが消したはずのアカウントを
// 操作しにいく。
//
// 件数の上限は置かない。管理するアカウントはプラグインが自分で作るもので、
// 利用者の数に比例して増えるものではないため。
func (r *pluginAccountRepository) ListByPlugin(plugin string) ([]*model.User, error) {
	if !storable(plugin) {
		return nil, nil
	}
	var users []*model.User
	err := r.db.
		Where(`"managedByPlugin" = ? AND "host" IS NULL AND "isDeleted" = FALSE`, plugin).
		Order("id ASC").
		Find(&users).Error
	if err != nil {
		return nil, err
	}
	return users, nil
}
