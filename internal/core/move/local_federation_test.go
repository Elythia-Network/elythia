package move_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/move"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type statusObserver struct {
	recorded []string
}

func (*statusObserver) IsExternallyAvailable(user *model.User) bool {
	return user != nil && user.FederationStatus == model.FederationStatusOn
}

func (o *statusObserver) ObserveExternalReference(userID, _, _ string) (bool, error) {
	o.recorded = append(o.recorded, userID)
	return true, nil
}

func TestMove_RejectsUnavailableLocalSourceAndDestination(t *testing.T) {
	for _, tc := range []struct {
		name        string
		source      model.FederationStatus
		destination model.FederationStatus
	}{
		{name: "source off", source: model.FederationStatusOff, destination: model.FederationStatusOn},
		{name: "source pending", source: model.FederationStatusPending, destination: model.FederationStatusOn},
		{name: "destination off", source: model.FederationStatusOn, destination: model.FederationStatusOff},
		{name: "destination pending", source: model.FederationStatusOn, destination: model.FederationStatusPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srcURI := "https://local.example/users/src"
			dstURI := "https://local.example/users/dst"
			dst := &model.User{
				ID: "dst", FederationStatus: tc.destination,
				AlsoKnownAs: strPtr(srcURI),
			}
			resolver := &fakeResolver{localIDs: map[string]string{dstURI: dst.ID}}
			deliverer := &fakeDeliverer{}
			svc, userRepo, _ := newService(resolver, deliverer)
			src := &model.User{ID: "src", URI: strPtr(srcURI), FederationStatus: tc.source}
			userRepo.Users[src.ID] = src
			userRepo.Users[dst.ID] = dst
			observer := &statusObserver{}
			svc.SetLocalFederationObserver(observer)

			err := svc.Move(src, dstURI)
			require.ErrorIs(t, err, move.ErrFederationDisabled)
			assert.Nil(t, src.MovedToURI, "a rejected move must not mutate the source")
			assert.Empty(t, deliverer.body, "a rejected move must not produce a Move activity")
			assert.Empty(t, observer.recorded, "a rejected move must not record either account as externally referenced")
		})
	}
}
