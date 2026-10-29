package federation

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/localfederation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type actorReferenceObserver struct {
	unavailable map[string]bool
	recorded    []string
	err         error
}

func (o *actorReferenceObserver) IsExternallyAvailable(user *model.User) bool {
	return user.FederationStatus == model.FederationStatusOn && !o.unavailable[user.ID]
}

func (o *actorReferenceObserver) ObserveExternalReference(userID, reason, host string) (bool, error) {
	o.recorded = append(o.recorded, userID+":"+reason+":"+host)
	return true, o.err
}

func TestFilterLocalActorReferences(t *testing.T) {
	urls := activitypub.NewURLBuilder("https://example.com")
	aliasOn := &model.User{ID: "alias-on", FederationStatus: model.FederationStatusOn}
	aliasOff := &model.User{ID: "alias-off", FederationStatus: model.FederationStatusOff}
	aliasPending := &model.User{ID: "alias-pending", FederationStatus: model.FederationStatusPending}
	aliasSuspended := &model.User{ID: "alias-suspended", FederationStatus: model.FederationStatusOn, IsSuspended: true}
	aliasDeleted := &model.User{ID: "alias-deleted", FederationStatus: model.FederationStatusOn, IsDeleted: true}
	aliases := urls.UserURI(aliasOn.ID) + "," + urls.UserURI(aliasOff.ID) + "," + urls.UserURI(aliasPending.ID) + "," + urls.UserURI(aliasSuspended.ID) + "," + urls.UserURI(aliasDeleted.ID) + "," + urls.UserURI("missing") + ",https://remote.example/users/alice"
	move := urls.UserURI(aliasOff.ID)
	owner := &model.User{ID: "owner", AlsoKnownAs: &aliases, MovedToURI: &move}
	observer := &actorReferenceObserver{}

	filtered, err := FilterLocalActorReferences(owner, urls, func(id string) (*model.User, error) {
		return map[string]*model.User{"alias-on": aliasOn, "alias-off": aliasOff, "alias-pending": aliasPending, "alias-suspended": aliasSuspended, "alias-deleted": aliasDeleted}[id], nil
	}, observer, "actor", nil)

	require.NoError(t, err)
	require.NotNil(t, filtered.AlsoKnownAs)
	assert.Equal(t, urls.UserURI(aliasOn.ID)+",https://remote.example/users/alice", *filtered.AlsoKnownAs)
	assert.Nil(t, filtered.MovedToURI)
	assert.Equal(t, []string{"alias-on:actor:"}, observer.recorded)
	assert.Equal(t, aliases, *owner.AlsoKnownAs, "the source user is not mutated")
}

func TestFilterLocalActorReferencesLookupFailureDoesNotObserveEarlierAliases(t *testing.T) {
	urls := activitypub.NewURLBuilder("https://example.com")
	alias := &model.User{ID: "alias", FederationStatus: model.FederationStatusOn}
	aliases := urls.UserURI(alias.ID) + "," + urls.UserURI("broken")
	owner := &model.User{ID: "owner", AlsoKnownAs: &aliases}
	observer := &actorReferenceObserver{}
	lookupErr := errors.New("database unavailable")

	_, err := FilterLocalActorReferences(owner, urls, func(id string) (*model.User, error) {
		if id == "broken" {
			return nil, lookupErr
		}
		return alias, nil
	}, observer, "actor", nil)

	assert.ErrorIs(t, err, lookupErr)
	assert.Empty(t, observer.recorded)
}

func TestFilterLocalActorReferencesRecordErrorAndConcurrentDisable(t *testing.T) {
	urls := activitypub.NewURLBuilder("https://example.com")
	alias := &model.User{ID: "alias", FederationStatus: model.FederationStatusOn}
	aliases := urls.UserURI(alias.ID)
	owner := &model.User{ID: "owner", AlsoKnownAs: &aliases}

	observer := &actorReferenceObserver{err: errors.New("record failed")}
	_, err := FilterLocalActorReferences(owner, urls, func(string) (*model.User, error) { return alias, nil }, observer, "delivery", nil)
	assert.ErrorIs(t, err, observer.err)

	observer = &actorReferenceObserver{err: localfederation.ErrFederationDisabled}
	filtered, err := FilterLocalActorReferences(owner, urls, func(string) (*model.User, error) { return alias, nil }, observer, "delivery", nil)
	require.NoError(t, err)
	assert.Nil(t, filtered.AlsoKnownAs)
}
