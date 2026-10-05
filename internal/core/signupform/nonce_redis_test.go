package signupform_test

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/signupform"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nonce の使い捨てを実 Redis で確かめる。**mock だと SetNX の atomicity が
// 再現されない** ので、1 枚を使い回せないという主張はここでしか押さえられない。
func TestRedisNonceStore_BurnIsSingleUse(t *testing.T) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		t.Skip("Redis unavailable:", err)
	}
	defer tr.Teardown(ctx)

	store := signupform.NewRedisNonceStore(tr.Client)
	require.NotNil(t, store)

	fresh, err := store.Burn(ctx, "nonce-1", time.Minute)
	require.NoError(t, err)
	assert.True(t, fresh, "初回は焼ける")

	fresh, err = store.Burn(ctx, "nonce-1", time.Minute)
	require.NoError(t, err)
	assert.False(t, fresh, "2 回目は焼けない")

	fresh, err = store.Burn(ctx, "nonce-2", time.Minute)
	require.NoError(t, err)
	assert.True(t, fresh, "別の nonce は独立している")
}

// 発行から検証までを実 Redis で通す (Issuer と store の組み合わせ)。
func TestIssuer_WithRedisNonceStore(t *testing.T) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		t.Skip("Redis unavailable:", err)
	}
	defer tr.Teardown(ctx)

	i := signupform.NewIssuer([]byte("secret"), signupform.NewRedisNonceStore(tr.Client))
	require.NotNil(t, i)
	token, err := i.Issue(signupform.PurposeApply)
	require.NoError(t, err)

	// 実時計なので最短滞在時間を待つ。**待たずに通ると gate が効いていない。**
	assert.ErrorIs(t, i.Verify(ctx, signupform.PurposeApply, token), signupform.ErrTokenTooSoon)
	time.Sleep(signupform.DefaultMinAge)
	require.NoError(t, i.Verify(ctx, signupform.PurposeApply, token))
	assert.ErrorIs(t, i.Verify(ctx, signupform.PurposeApply, token), signupform.ErrTokenUsed)
}

// 未配線の store は素通しにせず error を返す (fail-open にしない)。
func TestRedisNonceStore_NilIsNotFailOpen(t *testing.T) {
	assert.Nil(t, signupform.NewRedisNonceStore(nil))

	var store *signupform.RedisNonceStore
	fresh, err := store.Burn(context.Background(), "n", time.Minute)
	assert.False(t, fresh)
	assert.Error(t, err)
}
