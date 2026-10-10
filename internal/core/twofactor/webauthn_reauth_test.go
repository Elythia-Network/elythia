package twofactor

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReauth_ChallengeIsSeparateFromSignIn: a sign-in challenge must not be
// usable for a re-authentication (#3462), and starting one must not replace
// the other.
func TestReauth_ChallengeIsSeparateFromSignIn(t *testing.T) {
	svc := newSoftService(t)
	auth := newSoftAuthenticator(t)
	keys := []*model.UserSecurityKey{auth.securityKey(softUser.ID, 0)}
	ctx := context.Background()

	login, err := svc.BeginLogin(ctx, softUser, keys)
	require.NoError(t, err)
	reauth, err := svc.BeginReauth(ctx, softUser, keys)
	require.NoError(t, err)
	assert.NotEqual(t, login.Response.Challenge.String(), reauth.Response.Challenge.String())

	// サインイン用の challenge への assertion は、再認証には通らない。
	_, err = svc.FinishReauth(ctx, softUser, keys, auth.assert(login.Response.Challenge.String(), softUser.ID, true, 1))
	require.Error(t, err)
	// 再認証の challenge は1回で消える (上で使われた)。サインインの challenge は
	// 再認証の開始で上書きされていない。
	cred, err := svc.FinishLogin(ctx, softUser, keys, auth.assert(login.Response.Challenge.String(), softUser.ID, true, 2))
	require.NoError(t, err)
	assert.Equal(t, auth.credID, cred.ID)

	reauth, err = svc.BeginReauth(ctx, softUser, keys)
	require.NoError(t, err)
	_, err = svc.FinishLogin(ctx, softUser, keys, auth.assert(reauth.Response.Challenge.String(), softUser.ID, true, 3))
	assert.ErrorIs(t, err, ErrWebAuthnSessionNotFound, "the sign-in path cannot consume a re-authentication challenge")
	cred, err = svc.FinishReauth(ctx, softUser, keys, auth.assert(reauth.Response.Challenge.String(), softUser.ID, true, 4))
	require.NoError(t, err)
	assert.Equal(t, auth.credID, cred.ID)
}

// TestTakeSession_StoreFailureIsDistinguishable: callers must be able to tell
// a Redis failure from a missing or wrong challenge.
func TestTakeSession_StoreFailureIsDistinguishable(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	svc, err := NewWebAuthnService(softOrigin, "Misskey", rdb)
	require.NoError(t, err)
	ctx := context.Background()

	_, err = svc.FinishReauth(ctx, softUser, nil, nil)
	assert.ErrorIs(t, err, ErrWebAuthnSessionNotFound)
	assert.False(t, errors.Is(err, ErrWebAuthnSessionStore))

	mr.SetError("connection lost")
	_, err = svc.FinishReauth(ctx, softUser, nil, nil)
	assert.ErrorIs(t, err, ErrWebAuthnSessionStore)
	_, err = svc.FinishLogin(ctx, softUser, nil, nil)
	assert.ErrorIs(t, err, ErrWebAuthnSessionStore)
}
