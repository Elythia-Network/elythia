package user_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// resyncingResolver is a RemoteUserResolver that also re-syncs stored users.
type resyncingResolver struct {
	stubRemoteResolver
	resynced []string
	synced   *model.User
	err      error
}

func (r *resyncingResolver) ResyncIfStale(u *model.User) (*model.User, error) {
	r.resynced = append(r.resynced, u.ID)
	if r.err != nil {
		return nil, r.err
	}
	if r.synced != nil {
		return r.synced, nil
	}
	return u, nil
}

func newResyncService(t *testing.T) (*user.Service, *resyncingResolver) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	host := "remote.example"
	repo.Users["uR"] = &model.User{ID: "uR", Username: "remote", UsernameLower: "remote", Host: &host}
	repo.Users["uL"] = &model.User{ID: "uL", Username: "me", UsernameLower: "me"}
	svc := user.NewService(repo, nil, nil, nil)
	rs := &resyncingResolver{}
	svc.SetRemoteUserResolver(rs)
	return svc, rs
}

// users/show (ResolveByUsername) passes a stored remote user through the
// resolver's re-sync, as upstream resolveUser does, and returns the re-synced
// row.
func TestResolveByUsername_ResyncsStoredRemoteUser(t *testing.T) {
	svc, rs := newResyncService(t)
	host := "remote.example"
	updatedName := "Updated"
	rs.synced = &model.User{ID: "uR", Username: "remote", UsernameLower: "remote", Host: &host, Name: &updatedName}
	bundle, err := svc.ResolveByUsername("remote", &host)
	require.NoError(t, err)
	assert.Equal(t, []string{"uR"}, rs.resynced)
	require.NotNil(t, bundle.User.Name)
	assert.Equal(t, "Updated", *bundle.User.Name)
	assert.Empty(t, rs.calls, "DB にある利用者は WebFinger の新規解決に回さない")
}

// A failed re-sync is FAILED_TO_RESOLVE_REMOTE_USER (upstream users/show
// catches resolveUser's error).
func TestResolveByUsername_ResyncFailure(t *testing.T) {
	svc, rs := newResyncService(t)
	rs.err = errors.New("webfinger failed")
	host := "remote.example"
	_, err := svc.ResolveByUsername("remote", &host)
	require.ErrorIs(t, err, user.ErrFailedToResolveRemoteUser)
}

type nilResyncer struct{ stubRemoteResolver }

func (nilResyncer) ResyncIfStale(*model.User) (*model.User, error) { return nil, nil }

// A nil result from the re-sync is a failure too, not a nil dereference.
func TestResolveByUsername_ResyncNilUser(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	host := "remote.example"
	repo.Users["uR"] = &model.User{ID: "uR", Username: "remote", UsernameLower: "remote", Host: &host}
	svc := user.NewService(repo, nil, nil, nil)
	svc.SetRemoteUserResolver(&nilResyncer{})
	_, err := svc.ResolveByUsername("remote", &host)
	require.ErrorIs(t, err, user.ErrFailedToResolveRemoteUser)
}

// Without a host (a local lookup) and in ShowByUsername (used by
// users/followers / users/following, which upstream answers from the DB only)
// nothing is re-synced.
func TestResolveByUsername_NoResyncWithoutHostOrInShow(t *testing.T) {
	svc, rs := newResyncService(t)
	_, err := svc.ResolveByUsername("me", nil)
	require.NoError(t, err)
	host := "remote.example"
	_, err = svc.ShowByUsername("remote", &host)
	require.NoError(t, err)
	assert.Empty(t, rs.resynced)
}

// A resolver without re-sync support keeps returning the stored row.
func TestResolveByUsername_ResolverWithoutResync(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	host := "remote.example"
	repo.Users["uR"] = &model.User{ID: "uR", Username: "remote", UsernameLower: "remote", Host: &host}
	svc := user.NewService(repo, nil, nil, nil)
	svc.SetRemoteUserResolver(&stubRemoteResolver{})
	bundle, err := svc.ResolveByUsername("remote", &host)
	require.NoError(t, err)
	assert.Equal(t, "uR", bundle.User.ID)
}
