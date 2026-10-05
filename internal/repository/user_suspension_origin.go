package repository

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/elythia-network/elythia/internal/model"
)

// UserSuspensionOriginRepository records who decided a user's suspension
// state (#2973).
//
// **これが要る理由**: #2951 でリモート actor の `toot:suspended` を読むように
// したが、由来を持たないと**発信元が立て続けている間は `admin/unsuspend-user`
// の結果が次の actor refresh で無言で戻る**。Mastodon は `suspension_origin`
// (local / remote) でこれを解いている。
type UserSuspensionOriginRepository interface {
	// Origin returns the recorded origin for userID.
	// 記録が無ければ ("", nil) を返す (= どちらの判断でもない)。
	Origin(userID string) (string, error)
	// Set records the origin, replacing any previous value.
	Set(userID, origin string) error
	// Clear removes the record (ローカルの解除で由来を持つ意味が無くなるため)。
	Clear(userID string) error
}

type userSuspensionOriginRepository struct{ db *gorm.DB }

// NewUserSuspensionOriginRepository creates the repository.
func NewUserSuspensionOriginRepository(db *gorm.DB) UserSuspensionOriginRepository {
	return &userSuspensionOriginRepository{db: db}
}

func (r *userSuspensionOriginRepository) Origin(userID string) (string, error) {
	if userID == "" {
		return "", nil
	}
	var row model.UserSuspensionOrigin
	err := r.db.Where(`"userId" = ?`, userID).Take(&row).Error
	if err != nil {
		// **not-found は「記録が無い」で、DB 障害とは区別する** (#2792)。
		// 呼び出し元は前者を「どちらの判断でもない」として扱い、後者では
		// 判断を保留する (= 触らない) 必要がある。
		if IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return row.Origin, nil
}

func (r *userSuspensionOriginRepository) Set(userID, origin string) error {
	if userID == "" {
		return nil
	}
	row := &model.UserSuspensionOrigin{UserID: userID, Origin: origin, UpdatedAt: time.Now()}
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "userId"}},
		DoUpdates: clause.AssignmentColumns([]string{"origin", "updatedAt"}),
	}).Create(row).Error
}

func (r *userSuspensionOriginRepository) Clear(userID string) error {
	if userID == "" {
		return nil
	}
	return r.db.Where(`"userId" = ?`, userID).Delete(&model.UserSuspensionOrigin{}).Error
}
