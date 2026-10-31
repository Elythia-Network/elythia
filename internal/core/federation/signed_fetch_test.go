package federation

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubFetchKeys returns a fixed PEM per actor ID and records the lookups.
type stubFetchKeys struct {
	pems  map[string]string
	err   error
	calls int
}

func (s *stubFetchKeys) PublicKeyForKeyID(actorID, _ string) (string, error) {
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	pem, ok := s.pems[actorID]
	if !ok {
		return "", errors.New("no key")
	}
	return pem, nil
}

type signedFetchFixture struct {
	auth  *SignedFetchAuthenticator
	users *testutil.MockUserRepository
	keys  *stubFetchKeys
	key   *activitypub.PrivateKey
}

const signedFetchHost = "local.example"

func newSignedFetchFixture(t *testing.T) *signedFetchFixture {
	t.Helper()
	privPEM, pubPEM, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/bob#main-key", privPEM)
	require.NoError(t, err)
	users := testutil.NewMockUserRepository()
	host, uri := "remote.example", "https://remote.example/users/bob"
	users.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &host, URI: &uri}
	keys := &stubFetchKeys{pems: map[string]string{"bob": pubPEM}}
	return &signedFetchFixture{
		auth:  NewSignedFetchAuthenticator(users, keys, signedFetchHost),
		users: users,
		keys:  keys,
		key:   key,
	}
}

func signedGet(t *testing.T, key *activitypub.PrivateKey, target string, headers ...string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Accept", "application/activity+json")
	if len(headers) == 0 {
		headers = []string{"(request-target)", "host", "date", "accept"}
	}
	require.NoError(t, activitypub.SignRequest(req, key, "", headers))
	return req
}

func TestSignedFetchAuthenticator_ReturnsKnownRemoteSigner(t *testing.T) {
	f := newSignedFetchFixture(t)
	signer, err := f.auth.AuthenticateFetch(signedGet(t, f.key, "https://"+signedFetchHost+"/notes/n1"))
	require.NoError(t, err)
	assert.Equal(t, "bob", signer.ID)
}

func TestSignedFetchAuthenticator_Rejects(t *testing.T) {
	target := "https://" + signedFetchHost + "/notes/n1"
	t.Run("no signature", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		_, err := f.auth.AuthenticateFetch(httptest.NewRequest(http.MethodGet, target, nil))
		assert.Error(t, err)
	})
	t.Run("signed for another host", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		_, err := f.auth.AuthenticateFetch(signedGet(t, f.key, "https://other.example/notes/n1"))
		assert.ErrorIs(t, err, activitypub.ErrInboxHostMismatch)
		assert.Zero(t, f.keys.calls, "no key lookup before the admission passes")
	})
	t.Run("host not signed", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		_, err := f.auth.AuthenticateFetch(signedGet(t, f.key, target, "(request-target)", "date"))
		assert.ErrorIs(t, err, activitypub.ErrInboxHostUnsigned)
	})
	t.Run("unknown actor", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		delete(f.users.Users, "bob")
		_, err := f.auth.AuthenticateFetch(signedGet(t, f.key, target))
		assert.ErrorIs(t, err, ErrSignedFetchUnknownSigner)
		assert.Zero(t, f.keys.calls)
	})
	t.Run("user lookup failure is not unknown signer", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		f.users.FindByURIErr = errors.New("db down")
		_, err := f.auth.AuthenticateFetch(signedGet(t, f.key, target))
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrSignedFetchUnknownSigner)
	})
	t.Run("local user", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		f.users.Users["bob"].Host = nil
		_, err := f.auth.AuthenticateFetch(signedGet(t, f.key, target))
		assert.ErrorIs(t, err, ErrSignedFetchUnknownSigner)
	})
	t.Run("key lookup failure", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		f.keys.err = errors.New("no key")
		_, err := f.auth.AuthenticateFetch(signedGet(t, f.key, target))
		assert.Error(t, err)
	})
	t.Run("signed with another key", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		_, otherPub, err := activitypub.GenerateRSAKeypair()
		require.NoError(t, err)
		f.keys.pems["bob"] = otherPub
		_, err = f.auth.AuthenticateFetch(signedGet(t, f.key, target))
		assert.Error(t, err)
	})
	t.Run("path changed after signing", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		req := signedGet(t, f.key, target)
		req.URL.Path = "/notes/n2"
		_, err := f.auth.AuthenticateFetch(req)
		assert.Error(t, err)
	})
}
