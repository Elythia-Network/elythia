package federation_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestUserMentionResolver_WithUserLookup(t *testing.T) {
	userRepo := &failingUserRepoMR{MockUserRepository: testutil.NewMockUserRepository(), err: assert.AnError}
	r := federation.NewUserMentionResolver(userRepo, activitypub.NewURLBuilder("https://example.com"))
	r.SetLocalFederationObserver(localFederationAvailabilityStub{})
	users := map[string]*model.User{
		"on":  {ID: "on", Username: "alice", FederationStatus: model.FederationStatusOn},
		"off": {ID: "off", Username: "bob", FederationStatus: model.FederationStatusOff},
	}
	derived := r.WithUserLookup(func(userID string) (*model.User, error) {
		if u, ok := users[userID]; ok {
			return u, nil
		}
		return nil, gorm.ErrRecordNotFound
	})

	name, uri, err := derived.ResolveMention("on")
	require.NoError(t, err, "the lookup answers instead of the failing repository")
	assert.Equal(t, "@alice", name)
	assert.Equal(t, "https://example.com/users/on", uri)

	_, _, err = derived.ResolveMention("off")
	assert.ErrorIs(t, err, activitypub.ErrMentionUserNotFound, "the derived resolver keeps the local-only check")
	_, _, err = derived.ResolveMention("ghost")
	assert.ErrorIs(t, err, activitypub.ErrMentionUserNotFound)

	_, _, err = r.ResolveMention("on")
	assert.ErrorIs(t, err, assert.AnError, "the original resolver still reads the repository")
}
