package localfederation

import (
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type memoryRepository struct {
	users             map[string]*model.User
	systemAccounts    map[string]bool
	observations      int
	lastReason        string
	lastHost          string
	lastReferenceTime time.Time
}

func (r *memoryRepository) FindUser(userID string) (*model.User, error) {
	u, ok := r.users[userID]
	if !ok {
		return nil, errors.New("not found")
	}
	return u, nil
}

func (r *memoryRepository) IsSystemAccount(userID string) (bool, error) {
	return r.systemAccounts[userID], nil
}

func (r *memoryRepository) ObserveExternalReference(userID, reason, host string, at time.Time) (*model.User, bool, error) {
	u := r.users[userID]
	if u.FederationStatus != model.FederationStatusOn {
		return u, false, nil
	}
	if u.FirstExternalReferenceAt != nil {
		return u, false, nil
	}
	r.observations++
	r.lastReason, r.lastHost, r.lastReferenceTime = reason, host, at
	u.FirstExternalReferenceAt = &at
	u.FirstExternalReferenceReason = &reason
	u.FirstExternalReferenceHost = &host
	return u, true, nil
}

func (r *memoryRepository) ChangeStatus(userID string, next model.FederationStatus, at time.Time, rsa *model.UserKeypair, ed25519 *model.UserKeypairExtra) (*model.User, bool, error) {
	u := r.users[userID]
	if u.FederationStatus == next {
		return u, false, nil
	}
	if next == model.FederationStatusOff && u.FirstExternalReferenceAt != nil {
		return u, false, nil
	}
	u.FederationStatus = next
	if next == model.FederationStatusOn {
		u.FederationEnabledAt = &at
	}
	return u, true, nil
}

func (r *memoryRepository) WithLockedUser(userID string, fn func(tx *gorm.DB, user *model.User) error) error {
	return fn(nil, r.users[userID])
}

func TestIsExternallyAvailable(t *testing.T) {
	tests := []struct {
		name string
		user *model.User
		want bool
	}{
		{name: "local enabled", user: &model.User{FederationStatus: model.FederationStatusOn}, want: true},
		{name: "local pending", user: &model.User{FederationStatus: model.FederationStatusPending}},
		{name: "local disabled", user: &model.User{FederationStatus: model.FederationStatusOff}},
		{name: "remote", user: &model.User{FederationStatus: model.FederationStatusOn, Host: stringPointer("remote.example")}, want: true},
		{name: "nil", user: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsExternallyAvailable(tt.user))
		})
	}
}

func TestServiceObserveExternalReferenceRejectsPendingAndOff(t *testing.T) {
	for _, status := range []model.FederationStatus{model.FederationStatusPending, model.FederationStatusOff} {
		t.Run(string(status), func(t *testing.T) {
			repo := &memoryRepository{users: map[string]*model.User{
				"local": {ID: "local", FederationStatus: status},
			}}
			svc := NewService(repo, false)
			recorded, err := svc.ObserveExternalReference("local", ExternalReferenceReasonActor, "peer.example")
			require.ErrorIs(t, err, ErrFederationDisabled)
			assert.False(t, recorded)
			assert.Zero(t, repo.observations)
		})
	}
}

func TestServiceObserveExternalReferenceSkipsKnownReference(t *testing.T) {
	observedAt := time.Date(2026, 10, 26, 2, 0, 0, 0, time.UTC)
	repo := &memoryRepository{users: map[string]*model.User{
		"local": {ID: "local", FederationStatus: model.FederationStatusOn, FirstExternalReferenceAt: &observedAt},
	}}
	svc := NewService(repo, false)

	recorded, err := svc.ObserveExternalReferenceForUser(repo.users["local"], ExternalReferenceReasonActor, "peer.example")
	require.NoError(t, err)
	assert.False(t, recorded)
	assert.Zero(t, repo.observations)
}

func stringPointer(value string) *string { return &value }
