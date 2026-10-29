package i

import (
	"net/http"
	"testing"

	corelocalfederation "github.com/elythia-network/elythia/internal/core/localfederation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

type moveAvailabilityObserver struct {
	recorded []string
}

func (*moveAvailabilityObserver) IsExternallyAvailable(user *model.User) bool {
	return corelocalfederation.IsExternallyAvailable(user)
}

func (o *moveAvailabilityObserver) ObserveExternalReference(userID, _, _ string) (bool, error) {
	o.recorded = append(o.recorded, userID)
	return true, nil
}

func TestMove_LocalFederationUnavailableSourceIsRejected(t *testing.T) {
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending} {
		t.Run(string(status), func(t *testing.T) {
			h, _ := newExtraHandler(t)
			user := *stubUser
			user.FederationStatus = status
			observer := &moveAvailabilityObserver{}
			h.SetLocalFederationObserver(observer)

			rec := postExtra(h.Move, `{"moveToAccount":"https://remote.example/users/destination"}`, &user)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "FEDERATION_DISABLED")
			assert.Empty(t, observer.recorded, "a disabled source must be rejected before move resolution or observation")
		})
	}
}
