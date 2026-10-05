package oauth

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedisStore_Integration exercises the Redis-backed Store end to end,
// including the single-use transaction (GETDEL), atomic grant claim (SETNX),
// and grant-token recording used for replay revocation (#1899).
func TestRedisStore_Integration(t *testing.T) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		t.Skipf("Redis not available: %v", err)
	}
	defer tr.Teardown(ctx)
	tr.FlushAll(ctx)

	s := NewRedisStore(tr.Client)

	// transaction: put → take (single use) → take again is ErrNotFound。
	txn := &transaction{ClientID: "https://c/", RedirectURI: "https://c/cb", Scopes: []string{"read:account"}, CodeChallenge: "ch", State: "st"}
	require.NoError(t, s.PutTransaction(ctx, "tx1", txn))
	got, err := s.TakeTransaction(ctx, "tx1")
	require.NoError(t, err)
	assert.Equal(t, "https://c/", got.ClientID)
	assert.Equal(t, "st", got.State)
	_, err = s.TakeTransaction(ctx, "tx1")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = s.TakeTransaction(ctx, "missing")
	assert.ErrorIs(t, err, ErrNotFound)

	// grant: put → get。
	g := &grant{ClientID: "https://c/", UserID: "u1", RedirectURI: "https://c/cb", CodeChallenge: "ch", Scopes: []string{"read:account"}}
	require.NoError(t, s.PutGrant(ctx, "code1", g))
	gotG, err := s.GetGrant(ctx, "code1")
	require.NoError(t, err)
	assert.Equal(t, "u1", gotG.UserID)
	_, err = s.GetGrant(ctx, "missing")
	assert.ErrorIs(t, err, ErrNotFound)

	// claim: first true, replay false。
	first, err := s.ClaimGrant(ctx, "code1")
	require.NoError(t, err)
	assert.True(t, first)
	second, err := s.ClaimGrant(ctx, "code1")
	require.NoError(t, err)
	assert.False(t, second)

	// grant token: empty → set → get。
	tid, err := s.GetGrantToken(ctx, "code1")
	require.NoError(t, err)
	assert.Empty(t, tid)
	require.NoError(t, s.PutGrantToken(ctx, "code1", "tokABC"))
	tid, err = s.GetGrantToken(ctx, "code1")
	require.NoError(t, err)
	assert.Equal(t, "tokABC", tid)
}
