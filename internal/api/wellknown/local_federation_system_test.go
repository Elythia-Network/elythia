package wellknown

import (
	"net/http"
	"testing"

	corelocalfederation "github.com/elythia-network/elythia/internal/core/localfederation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type systemWebfingerObserver struct {
	systemID string
	observed []string
}

func (o *systemWebfingerObserver) IsExternallyAvailable(user *model.User) bool {
	return user.ID == o.systemID || corelocalfederation.IsExternallyAvailable(user)
}

func (o *systemWebfingerObserver) ObserveExternalReference(userID, _, _ string) (bool, error) {
	if userID == o.systemID {
		return false, nil
	}
	o.observed = append(o.observed, userID)
	return true, nil
}

func TestWebfinger_SystemAccountRemainsDiscoverableWithoutReferenceObservation(t *testing.T) {
	h, repo := newHandler(t)
	repo.Users["system"] = &model.User{
		ID: "system", Username: "instance.actor", UsernameLower: "instance.actor",
		FederationStatus: model.FederationStatusOn,
	}
	observer := &systemWebfingerObserver{systemID: "system"}
	h.SetLocalFederationObserver(observer)

	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:instance.actor@example.com")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "acct:instance.actor@example.com")
	assert.Empty(t, observer.observed, "system-account discovery must not record a first external reference")
}
