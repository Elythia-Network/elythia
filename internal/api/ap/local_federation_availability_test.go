package ap

import (
	"encoding/json"
	"net/http"
	"testing"

	corelocalfederation "github.com/elythia-network/elythia/internal/core/localfederation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type availabilityObserver struct {
	recorded []string
}

func (*availabilityObserver) IsExternallyAvailable(user *model.User) bool {
	return corelocalfederation.IsExternallyAvailable(user)
}

func (o *availabilityObserver) ObserveExternalReference(userID, _, _ string) (bool, error) {
	o.recorded = append(o.recorded, userID)
	return true, nil
}

func TestFollowCollections_LocalFederationUnavailableReturnsNotFound(t *testing.T) {
	collections := []struct {
		name string
		call func(*Handler, echo.Context) error
	}{
		{name: "followers", call: (*Handler).Followers},
		{name: "following", call: (*Handler).Following},
	}
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending} {
		t.Run(string(status), func(t *testing.T) {
			for _, collection := range collections {
				t.Run(collection.name, func(t *testing.T) {
					for _, page := range []string{"", "page=true"} {
						pageName := "index"
						if page != "" {
							pageName = "page"
						}
						t.Run(pageName, func(t *testing.T) {
							h, userRepo, _ := newHandlerWithFollowing(t)
							userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", FederationStatus: status}
							userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", FollowersVisibility: model.FollowingVisibilityPublic, FollowingVisibility: model.FollowingVisibilityPublic}
							observer := &availabilityObserver{}
							h.SetLocalFederationObserver(observer)

							c, rec := newReqQuery(t, "id", "u1", page)
							require.NoError(t, collection.call(h, c))
							assert.Equal(t, http.StatusNotFound, rec.Code)
							assert.Empty(t, rec.Body.String())
							assert.Empty(t, observer.recorded, "unavailable collections must not record an external reference")
						})
					}
				})
			}
		})
	}
}

func TestUser_BrowserFederationDisabledDoesNotObserve(t *testing.T) {
	for _, status := range []model.FederationStatus{model.FederationStatusOn, model.FederationStatusPending, model.FederationStatusOff} {
		t.Run(string(status), func(t *testing.T) {
			h, userRepo, _, _ := newHandler(t)
			userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", FederationStatus: status}
			observer := &availabilityObserver{}
			h.SetLocalFederationObserver(observer)

			c, rec := newBrowserReq(t, "id", "u1")
			require.NoError(t, h.User(c))
			assert.Equal(t, http.StatusFound, rec.Code)
			assert.Empty(t, observer.recorded, "ordinary HTML profile redirects do not observe ActivityPub references")
		})
	}
}

type systemActorObserver struct {
	systemID string
	observed []string
}

func (o *systemActorObserver) IsExternallyAvailable(user *model.User) bool {
	return user.ID == o.systemID || user.FederationStatus == model.FederationStatusOn
}

func (o *systemActorObserver) ObserveExternalReference(userID, _, _ string) (bool, error) {
	if userID == o.systemID {
		return false, nil
	}
	o.observed = append(o.observed, userID)
	return true, nil
}

func TestSystemActorRemainsServedWithoutReferenceObservation(t *testing.T) {
	h, userRepo, _, keypairRepo := newHandler(t)
	userRepo.Users["system"] = &model.User{ID: "system", Username: "instance.actor", FederationStatus: model.FederationStatusOn}
	keypairRepo.items["system"] = &model.UserKeypair{UserID: "system", PublicKey: "PUBKEY"}
	observer := &systemActorObserver{systemID: "system"}
	h.SetLocalFederationObserver(observer)

	c, rec := newReq(t, "id", "system")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "Application", body["type"])
	assert.Empty(t, observer.observed, "system actor access must not create a first-reference record")
}
