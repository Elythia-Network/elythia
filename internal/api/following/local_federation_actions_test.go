package following

import (
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

type localFederationActionObserver struct {
	available bool
	recorded  int
}

func (s *localFederationActionObserver) IsExternallyAvailable(*model.User) bool { return s.available }
func (s *localFederationActionObserver) ObserveExternalReference(string, string, string) (bool, error) {
	s.recorded++
	return true, nil
}

func TestCreate_LocalOnlyAccountCannotFollowRemoteUser(t *testing.T) {
	h, users := newTestHandler(t)
	host := "remote.example"
	users.Users["remote"] = &model.User{ID: "remote", Username: "bob", Host: &host}
	h.SetLocalFederationObserver(&localFederationActionObserver{})
	rec := postJSON(h.Create, `{"userId":"remote"}`, &model.User{ID: "local", FederationStatus: model.FederationStatusOff})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_AlreadyFollowingDoesNotRecordExternalReference(t *testing.T) {
	h, users := newTestHandler(t)
	alice := addUser(users, "alice", false)
	host := "remote.example"
	remote := &model.User{ID: "remote", Username: "bob", Host: &host}
	users.Users[remote.ID] = remote
	first := postJSON(h.Create, `{"userId":"remote"}`, alice)
	assert.Equal(t, http.StatusOK, first.Code)

	alice.FederationStatus = model.FederationStatusOn
	observer := &localFederationActionObserver{available: true}
	h.SetLocalFederationObserver(observer)
	second := postJSON(h.Create, `{"userId":"remote"}`, alice)
	assert.Equal(t, http.StatusBadRequest, second.Code)
	assert.Zero(t, observer.recorded)
}
