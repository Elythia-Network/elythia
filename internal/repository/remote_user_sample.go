package repository

import (
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/model"
)

// RemoteUserSampler picks a known user of a remote host, for diagnosing
// federation with that host (#3055).
//
// UserRepository の interface には足さない。手書きの test fake が複数パッケージに
// 散っていて、診断のためだけに全部へ実装を足すことになる。
type RemoteUserSampler struct {
	db *gorm.DB
}

// NewRemoteUserSampler constructs a RemoteUserSampler.
func NewRemoteUserSampler(db *gorm.DB) *RemoteUserSampler {
	return &RemoteUserSampler{db: db}
}

// FindRecentByHost returns the user of host we fetched most recently, or
// ErrNotFound when we know none.
//
// **最近取得したものを選ぶ。** 相手側で消えたアカウントを選ぶと、疎通に問題が
// 無くても actor の検査が 410 で落ちて診断を誤らせる。凍結 / 削除済みも除く。
func (r *RemoteUserSampler) FindRecentByHost(host string) (*model.User, error) {
	if !storable(host) || host == "" {
		return nil, ErrNotFound
	}
	var users []*model.User
	err := hostMatch(r.db, host).
		Where(`"isDeleted" = false AND "isSuspended" = false`).
		Order(`"lastFetchedAt" DESC NULLS LAST`).
		Limit(1).
		Find(&users).Error
	if err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, ErrNotFound
	}
	return users[0], nil
}
