package localfederation

import (
	"errors"
	"fmt"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"gorm.io/gorm"
)

var (
	ErrFederationDisabled          = errors.New("local federation is disabled")
	ErrFederationAlreadyReferenced = errors.New("account has already been externally referenced")
	ErrSystemAccount               = errors.New("system accounts cannot change federation status")
	ErrNotLocalAccount             = errors.New("federation status is only available for local accounts")
	ErrInvalidFederationStatus     = errors.New("invalid federation status")
)

type ExternalReferenceReason string

const (
	ExternalReferenceReasonMigration  = "migration"
	ExternalReferenceReasonWebFinger  = "webfinger"
	ExternalReferenceReasonActor      = "actor"
	ExternalReferenceReasonCollection = "collection"
	ExternalReferenceReasonNote       = "note"
	ExternalReferenceReasonInbox      = "inbox"
	ExternalReferenceReasonDelivery   = "delivery"
)

// StatusSnapshot is returned by the authenticated self-service endpoints.
type StatusSnapshot struct {
	FederationStatus             model.FederationStatus `json:"federationStatus"`
	FederationEnabledAt          *time.Time             `json:"federationEnabledAt"`
	FirstExternalReferenceAt     *time.Time             `json:"firstExternalReferenceAt"`
	FirstExternalReferenceReason *string                `json:"firstExternalReferenceReason"`
	FirstExternalReferenceHost   *string                `json:"firstExternalReferenceHost"`
	IsSystemAccount              bool                   `json:"isSystemAccount"`
}

// Service coordinates local-account federation state and its key lifecycle.
type Service struct {
	repo           repository.LocalFederationRepository
	ed25519Enabled bool
	clock          func() time.Time
}

func NewService(repo repository.LocalFederationRepository, ed25519Enabled bool) *Service {
	return &Service{repo: repo, ed25519Enabled: ed25519Enabled, clock: time.Now}
}

func (s *Service) SetClock(now func() time.Time) {
	if now != nil {
		s.clock = now
	}
}

// IsExternallyAvailable reports whether a local user may be served to remote
// servers. Remote users are unaffected by this local-account feature.
func IsExternallyAvailable(user *model.User) bool {
	if user == nil {
		return false
	}
	return !user.IsLocal() || user.FederationStatus == model.FederationStatusOn
}

func (s *Service) IsExternallyAvailable(user *model.User) bool {
	return IsExternallyAvailable(user)
}

// ObserveExternalReference records the first observed federation event. The
// repository locks the user row so disabling and observation serialize.
func (s *Service) ObserveExternalReference(userID string, reason string, host string) (bool, error) {
	isSystem, err := s.repo.IsSystemAccount(userID)
	if err != nil {
		return false, fmt.Errorf("local federation: check system account: %w", err)
	}
	if isSystem {
		return false, nil
	}
	user, err := s.repo.FindUser(userID)
	if err != nil {
		return false, fmt.Errorf("local federation: find account for observation: %w", err)
	}
	if !user.IsLocal() {
		return false, nil
	}
	user, recorded, err := s.repo.ObserveExternalReference(userID, string(reason), host, s.clock())
	if err != nil {
		return false, err
	}
	if user.FederationStatus != model.FederationStatusOn {
		return false, ErrFederationDisabled
	}
	return recorded, nil
}

// ObserveExternalReferenceForUser avoids a repository call for a user already
// known to be referenced. That fact is permanent: referenced accounts cannot
// be switched off.
func (s *Service) ObserveExternalReferenceForUser(user *model.User, reason string, host string) (bool, error) {
	if user == nil {
		return false, repository.ErrNotFound
	}
	if !s.IsExternallyAvailable(user) {
		return false, ErrFederationDisabled
	}
	if !user.IsLocal() || user.FirstExternalReferenceAt != nil {
		return false, nil
	}
	return s.ObserveExternalReference(user.ID, reason, host)
}

func (s *Service) GetStatus(userID string) (StatusSnapshot, error) {
	user, err := s.repo.FindUser(userID)
	if err != nil {
		return StatusSnapshot{}, fmt.Errorf("local federation: find account status: %w", err)
	}
	isSystem, err := s.repo.IsSystemAccount(userID)
	if err != nil {
		return StatusSnapshot{}, fmt.Errorf("local federation: check system account: %w", err)
	}
	if !user.IsLocal() {
		return StatusSnapshot{}, ErrNotLocalAccount
	}
	if isSystem {
		user.FederationStatus = model.FederationStatusOn
	}
	return snapshot(user, isSystem), nil
}

func (s *Service) ChangeStatus(userID string, next model.FederationStatus) (StatusSnapshot, error) {
	if next != model.FederationStatusOn && next != model.FederationStatusOff {
		return StatusSnapshot{}, ErrInvalidFederationStatus
	}
	user, err := s.repo.FindUser(userID)
	if err != nil {
		return StatusSnapshot{}, fmt.Errorf("local federation: find account to change: %w", err)
	}
	if !user.IsLocal() {
		return StatusSnapshot{}, ErrNotLocalAccount
	}
	isSystem, err := s.repo.IsSystemAccount(userID)
	if err != nil {
		return StatusSnapshot{}, fmt.Errorf("local federation: check system account: %w", err)
	}
	if isSystem {
		return StatusSnapshot{}, ErrSystemAccount
	}
	if next == model.FederationStatusOff && user.FirstExternalReferenceAt != nil {
		return StatusSnapshot{}, ErrFederationAlreadyReferenced
	}
	if user.FederationStatus == next {
		return snapshot(user, false), nil
	}

	var rsa *model.UserKeypair
	var ed25519 *model.UserKeypairExtra
	if next == model.FederationStatusOn {
		privateKey, publicKey, err := activitypub.GenerateRSAKeypair()
		if err != nil {
			return StatusSnapshot{}, fmt.Errorf("local federation: generate RSA keypair: %w", err)
		}
		rsa = &model.UserKeypair{UserID: userID, PublicKey: publicKey, PrivateKey: privateKey}
		if s.ed25519Enabled {
			privateKey, publicKey, err = activitypub.GenerateEd25519Keypair()
			if err != nil {
				return StatusSnapshot{}, fmt.Errorf("local federation: generate Ed25519 keypair: %w", err)
			}
			ed25519 = &model.UserKeypairExtra{UserID: userID, Ed25519PublicKey: publicKey, Ed25519PrivateKey: privateKey}
		}
	}

	at := s.clock()
	updated, changed, err := s.repo.ChangeStatus(userID, next, at, rsa, ed25519)
	if err != nil {
		return StatusSnapshot{}, err
	}
	if !changed && next == model.FederationStatusOff && updated.FirstExternalReferenceAt != nil {
		return StatusSnapshot{}, ErrFederationAlreadyReferenced
	}
	isSystem, err = s.repo.IsSystemAccount(userID)
	if err != nil {
		return StatusSnapshot{}, fmt.Errorf("local federation: check system account: %w", err)
	}
	if isSystem {
		updated.FederationStatus = model.FederationStatusOn
	}
	return snapshot(updated, isSystem), nil
}

// WithLockedUser shares the same row lock used by state changes and reference
// observations. The callback must use tx for writes that need this ordering.
func (s *Service) WithLockedUser(userID string, fn func(tx *gorm.DB, user *model.User) error) error {
	return s.repo.WithLockedUser(userID, fn)
}

func snapshot(user *model.User, isSystem bool) StatusSnapshot {
	return StatusSnapshot{
		FederationStatus:             user.FederationStatus,
		FederationEnabledAt:          user.FederationEnabledAt,
		FirstExternalReferenceAt:     user.FirstExternalReferenceAt,
		FirstExternalReferenceReason: user.FirstExternalReferenceReason,
		FirstExternalReferenceHost:   user.FirstExternalReferenceHost,
		IsSystemAccount:              isSystem,
	}
}
