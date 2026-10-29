package repository

import (
	"errors"
	"fmt"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LocalFederationRepository performs account federation state writes while
// holding the user's row lock.
type LocalFederationRepository interface {
	FindUser(userID string) (*model.User, error)
	IsSystemAccount(userID string) (bool, error)
	ObserveExternalReference(userID, reason, host string, at time.Time) (*model.User, bool, error)
	ChangeStatus(userID string, next model.FederationStatus, at time.Time, rsa *model.UserKeypair, ed25519 *model.UserKeypairExtra) (*model.User, bool, error)
	WithLockedUser(userID string, fn func(tx *gorm.DB, user *model.User) error) error
}

type localFederationRepository struct {
	db *gorm.DB
}

// cachedLocalFederationRepository invalidates cached user rows after a
// federation-state transaction commits. The underlying repository keeps its
// row-locking and transaction boundaries; invalidation happens only after it
// returns a successful, actual change.
type cachedLocalFederationRepository struct {
	LocalFederationRepository
	users *CachedUserRepository
}

// NewCachedLocalFederationRepository wraps inner so federation-state changes
// invalidate local user caches and publish the existing cross-process
// invalidation event.
func NewCachedLocalFederationRepository(inner LocalFederationRepository, users *CachedUserRepository) LocalFederationRepository {
	return &cachedLocalFederationRepository{LocalFederationRepository: inner, users: users}
}

func (r *cachedLocalFederationRepository) ObserveExternalReference(userID, reason, host string, at time.Time) (*model.User, bool, error) {
	user, recorded, err := r.LocalFederationRepository.ObserveExternalReference(userID, reason, host, at)
	if err == nil && recorded && r.users != nil {
		r.users.Invalidate(userID)
	}
	return user, recorded, err
}

func (r *cachedLocalFederationRepository) ChangeStatus(userID string, next model.FederationStatus, at time.Time, rsa *model.UserKeypair, ed25519 *model.UserKeypairExtra) (*model.User, bool, error) {
	user, changed, err := r.LocalFederationRepository.ChangeStatus(userID, next, at, rsa, ed25519)
	if err == nil && changed && r.users != nil {
		r.users.Invalidate(userID)
	}
	return user, changed, err
}

// NewLocalFederationRepository creates a repository for local account federation state.
func NewLocalFederationRepository(db *gorm.DB) LocalFederationRepository {
	return &localFederationRepository{db: db}
}

func (r *localFederationRepository) FindUser(userID string) (*model.User, error) {
	if !storable(userID) {
		return nil, ErrNotFound
	}
	var user model.User
	if err := r.db.First(&user, "id = ?", userID).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

func (r *localFederationRepository) IsSystemAccount(userID string) (bool, error) {
	if !storable(userID) {
		return false, ErrNotFound
	}
	var count int64
	if err := r.db.Model(&model.SystemAccount{}).Where(`"userId" = ?`, userID).Count(&count).Error; err != nil {
		return false, err
	}
	return count != 0, nil
}

func (r *localFederationRepository) ObserveExternalReference(userID, reason, host string, at time.Time) (*model.User, bool, error) {
	if !storable(userID) {
		return nil, false, ErrNotFound
	}
	var user model.User
	recorded := false
	err := r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ?", userID).Error; err != nil {
			return err
		}
		if user.FederationStatus != model.FederationStatusOn || user.FirstExternalReferenceAt != nil {
			return nil
		}
		updates := map[string]any{
			"firstExternalReferenceAt":     at,
			"firstExternalReferenceReason": reason,
			"firstExternalReferenceHost":   host,
		}
		result := tx.Model(&model.User{}).
			Where(`id = ? AND "federationStatus" = ? AND "firstExternalReferenceAt" IS NULL`, userID, model.FederationStatusOn).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 1 {
			recorded = true
			user.FirstExternalReferenceAt = &at
			user.FirstExternalReferenceReason = &reason
			user.FirstExternalReferenceHost = &host
		}
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("local federation: observe external reference: %w", err)
	}
	return &user, recorded, nil
}

func (r *localFederationRepository) ChangeStatus(userID string, next model.FederationStatus, at time.Time, rsa *model.UserKeypair, ed25519 *model.UserKeypairExtra) (*model.User, bool, error) {
	if !storable(userID) {
		return nil, false, ErrNotFound
	}
	if next != model.FederationStatusOn && next != model.FederationStatusOff {
		return nil, false, errors.New("local federation: unsupported target status")
	}
	var user model.User
	changed := false
	err := r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ?", userID).Error; err != nil {
			return err
		}
		if user.FederationStatus == next {
			return nil
		}
		if next == model.FederationStatusOff && user.FirstExternalReferenceAt != nil {
			return nil
		}
		if user.FederationStatus != model.FederationStatusOn && user.FederationStatus != model.FederationStatusOff && user.FederationStatus != model.FederationStatusPending {
			return errors.New("local federation: account has unsupported current status")
		}
		if next == model.FederationStatusOn {
			if rsa == nil || rsa.UserID != userID {
				return errors.New("local federation: RSA keypair required when enabling federation")
			}
			if ed25519 != nil && ed25519.UserID != userID {
				return errors.New("local federation: Ed25519 keypair belongs to another user")
			}
			if err := tx.Create(rsa).Error; err != nil {
				return fmt.Errorf("local federation: create RSA keypair: %w", err)
			}
			if ed25519 != nil {
				if err := tx.Create(ed25519).Error; err != nil {
					return fmt.Errorf("local federation: create Ed25519 keypair: %w", err)
				}
			}
		} else {
			if err := tx.Model(&model.Note{}).Where(`"userId" = ? AND "localOnly" = FALSE`, userID).
				Update("localOnly", true).Error; err != nil {
				return fmt.Errorf("local federation: make existing notes local-only: %w", err)
			}
			if err := tx.Model(&model.NoteDraft{}).
				Where(`"userId" = ? AND "isActuallyScheduled" = TRUE AND "localOnly" = FALSE`, userID).
				Update("localOnly", true).Error; err != nil {
				return fmt.Errorf("local federation: make scheduled drafts local-only: %w", err)
			}
			if err := tx.Where(`"userId" = ?`, userID).Delete(&model.UserKeypair{}).Error; err != nil {
				return fmt.Errorf("local federation: delete RSA keypair: %w", err)
			}
			if err := tx.Where(`"userId" = ?`, userID).Delete(&model.UserKeypairExtra{}).Error; err != nil {
				return fmt.Errorf("local federation: delete Ed25519 keypair: %w", err)
			}
		}
		updates := map[string]any{"federationStatus": next}
		if next == model.FederationStatusOn {
			updates["federationEnabledAt"] = at
			user.FederationEnabledAt = &at
		}
		if err := tx.Model(&model.User{}).Where("id = ?", userID).Updates(updates).Error; err != nil {
			return fmt.Errorf("local federation: update account status: %w", err)
		}
		user.FederationStatus = next
		changed = true
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("local federation: change status: %w", err)
	}
	return &user, changed, nil
}

func (r *localFederationRepository) WithLockedUser(userID string, fn func(tx *gorm.DB, user *model.User) error) error {
	if !storable(userID) {
		return ErrNotFound
	}
	if fn == nil {
		return errors.New("local federation: callback is required")
	}
	return r.db.Transaction(func(tx *gorm.DB) error {
		var user model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ?", userID).Error; err != nil {
			return err
		}
		return fn(tx, &user)
	})
}
