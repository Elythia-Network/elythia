package repository

import (
	"errors"

	"github.com/shiroha-a/mk/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RevokeDeletedLocalCredentials idempotently revokes a retained account's
// authentication and delivery credentials, preserving identity and content.
func (r *userRepository) RevokeDeletedLocalCredentials(userID string) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		var user model.User
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ?", userID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !user.IsLocal() || !user.IsDeleted {
			return errors.New("credential cleanup requires a deleted local account")
		}
		if err := tx.Model(&model.User{}).Where("id = ?", userID).Update("token", nil).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.UserProfile{}).Where(`"userId" = ?`, userID).Updates(map[string]any{
			"password": nil, "emailVerifyCode": nil,
			"twoFactorTempSecret": nil, "twoFactorSecret": nil,
			"twoFactorBackupSecret": model.StringArray{}, "twoFactorEnabled": false,
			"securityKeysAvailable": false, "usePasswordLessLogin": false,
		}).Error; err != nil {
			return err
		}
		for _, credential := range []any{
			&model.AccessToken{}, &model.AuthSession{}, &model.UserSecurityKey{},
			&model.PasswordResetRequest{}, &model.SwSubscription{}, &model.Webhook{},
		} {
			if err := tx.Where(`"userId" = ?`, userID).Delete(credential).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
