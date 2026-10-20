package federation_test

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingPublickeyStore is a PublickeyStore over a map that counts reads and
// runs during inside the first one.
type countingPublickeyStore struct {
	keys   map[string]string
	reads  atomic.Int32
	during func()
}

func (s *countingPublickeyStore) Upsert(pk *model.UserPublickey) error {
	s.keys[pk.UserID] = pk.KeyPEM
	return nil
}

func (s *countingPublickeyStore) FindByUserID(userID string) (*model.UserPublickey, error) {
	s.reads.Add(1)
	pem, ok := s.keys[userID]
	if s.during != nil {
		d := s.during
		s.during = nil
		d()
	}
	if !ok {
		return nil, repository.ErrNotFound
	}
	return &model.UserPublickey{UserID: userID, KeyPEM: pem}, nil
}

// TestResolver_DropLocal pins that DropLocal drops the cached public keys of
// this process, so the next lookup reads the database again (#3732).
func TestResolver_DropLocal(t *testing.T) {
	r, _ := newResolver(t, sampleActor, nil)
	store := &countingPublickeyStore{keys: map[string]string{"u1": "OLD"}}
	r.SetPublickeyRepo(store)

	pem, err := r.PublicKeyForActor("u1")
	require.NoError(t, err)
	require.Equal(t, "OLD", pem)
	store.keys["u1"] = "NEW"
	pem, err = r.PublicKeyForActor("u1")
	require.NoError(t, err)
	require.Equal(t, "OLD", pem, "前提: 捨てるまでは cache が返る")

	r.DropLocal()
	pem, err = r.PublicKeyForActor("u1")
	require.NoError(t, err)
	assert.Equal(t, "NEW", pem, "DropLocal did not drop the public keys")
}

// TestResolver_DropLocalDuringReadIsNotCached pins that a key read from the
// database while DropLocal ran answers that one lookup but is not stored
// (#3732).
func TestResolver_DropLocalDuringReadIsNotCached(t *testing.T) {
	r, _ := newResolver(t, sampleActor, nil)
	store := &countingPublickeyStore{keys: map[string]string{"u1": "OLD"}}
	r.SetPublickeyRepo(store)
	store.during = func() {
		store.keys["u1"] = "NEW"
		r.DropLocal()
	}

	pem, err := r.PublicKeyForActor("u1")
	require.NoError(t, err)
	assert.Equal(t, "OLD", pem, "the in-flight read answers with what it read")
	pem, err = r.PublicKeyForActor("u1")
	require.NoError(t, err)
	assert.Equal(t, "NEW", pem, "the key read during DropLocal was cached")
	assert.Equal(t, int32(2), store.reads.Load())
}

// TestResolver_DropLocalForgetsMissingExtraKeys pins that DropLocal also drops
// the records of keyIds missing from user_publickey_extra (#3732).
func TestResolver_DropLocalForgetsMissingExtraKeys(t *testing.T) {
	r, _ := newResolver(t, sampleActor, nil)
	r.SetPublickeyRepo(&countingPublickeyStore{keys: map[string]string{"u1": "RSA"}})
	extra := &countingExtraStore{}
	r.SetPublickeyExtraRepo(extra)

	_, err := r.PublicKeyForKeyID("u1", "https://remote.example/u1#ed")
	require.NoError(t, err)
	_, err = r.PublicKeyForKeyID("u1", "https://remote.example/u1#ed")
	require.NoError(t, err)
	require.Equal(t, int32(1), extra.finds.Load(), "前提: 無いことを覚えている")

	r.DropLocal()
	_, err = r.PublicKeyForKeyID("u1", "https://remote.example/u1#ed")
	require.NoError(t, err)
	assert.Equal(t, int32(2), extra.finds.Load(), "DropLocal did not forget the missing extra keys")
}

// countingExtraStore is a PublickeyExtraStore that has no row and counts
// FindByUserAndKeyID.
type countingExtraStore struct {
	finds atomic.Int32
}

func (s *countingExtraStore) Upsert(*model.UserPublickeyExtra) error { return errors.New("unused") }
func (s *countingExtraStore) FindByUserAndKeyID(string, string) (*model.UserPublickeyExtra, error) {
	s.finds.Add(1)
	return nil, repository.ErrNotFound
}
func (s *countingExtraStore) ListByUserID(string) ([]model.UserPublickeyExtra, error) {
	return nil, nil
}
func (s *countingExtraStore) DeleteByKeyID(string, string) error { return nil }
