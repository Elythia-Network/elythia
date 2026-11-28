package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// countingSystemAccountRepo counts the lookups of the proxy resolver.
type countingSystemAccountRepo struct {
	*testutil.MockSystemAccountRepository
	finds int
}

func (r *countingSystemAccountRepo) FindByType(typ string) (*model.SystemAccount, error) {
	r.finds++
	return r.MockSystemAccountRepository.FindByType(typ)
}

// 見つかったプロキシの名前はプロセスに持ち、HTML を返すたびに引かない (#4276)。
// まだ無いうちは持たず、作られた後の要求で見える。
func TestProxyAccountResolver_KeepsTheFoundName(t *testing.T) {
	sa := &countingSystemAccountRepo{MockSystemAccountRepository: testutil.NewMockSystemAccountRepository()}
	users := testutil.NewMockUserRepository()
	names := newProxyAccountNames(sa, users)
	resolve := names.Resolve

	for range 2 {
		name, ok := resolve()
		assert.False(t, ok)
		assert.Empty(t, name)
	}
	assert.Equal(t, 2, sa.finds, "a missing proxy account must be looked up again")

	require.NoError(t, users.Create(&model.User{ID: "proxyuser", Username: "proxy.actor", UsernameLower: "proxy.actor"}))
	require.NoError(t, sa.Create(&model.SystemAccount{ID: "sa1", Type: "proxy", UserID: "proxyuser"}))
	for range 3 {
		name, ok := resolve()
		require.True(t, ok)
		assert.Equal(t, "proxy.actor", name)
	}
	assert.Equal(t, 3, sa.finds, "the name found once must be kept")

	// キャッシュの一覧から捨てたら、次は引き直す。
	names.DropLocal()
	name, ok := resolve()
	require.True(t, ok)
	assert.Equal(t, "proxy.actor", name)
	assert.Equal(t, 4, sa.finds, "DropLocal must forget the name")
	_, _ = resolve()
	assert.Equal(t, 4, sa.finds)

	// 利用者の行が引けないうちは見つかったことにしない。
	sa2 := &countingSystemAccountRepo{MockSystemAccountRepository: testutil.NewMockSystemAccountRepository()}
	require.NoError(t, sa2.Create(&model.SystemAccount{ID: "sa2", Type: "proxy", UserID: "missing"}))
	resolve2 := newProxyAccountNames(sa2, testutil.NewMockUserRepository()).Resolve
	for range 2 {
		_, ok := resolve2()
		assert.False(t, ok)
	}
	assert.Equal(t, 2, sa2.finds)
}

// gateSystemAccountRepo runs a function in the middle of FindByType.
type gateSystemAccountRepo struct {
	*testutil.MockSystemAccountRepository
	during func()
}

func (r *gateSystemAccountRepo) FindByType(typ string) (*model.SystemAccount, error) {
	if r.during != nil {
		r.during()
	}
	return r.MockSystemAccountRepository.FindByType(typ)
}

// 読んでいる途中で捨てられたら、読んだ名前を保存しない。
func TestProxyAccountNames_DropDuringLookupIsNotOverwritten(t *testing.T) {
	sa := &gateSystemAccountRepo{MockSystemAccountRepository: testutil.NewMockSystemAccountRepository()}
	users := testutil.NewMockUserRepository()
	require.NoError(t, users.Create(&model.User{ID: "proxyuser", Username: "proxy.actor", UsernameLower: "proxy.actor"}))
	require.NoError(t, sa.Create(&model.SystemAccount{ID: "sa1", Type: "proxy", UserID: "proxyuser"}))
	names := newProxyAccountNames(sa, users)
	sa.during = func() {
		sa.during = nil
		names.DropLocal()
	}
	_, ok := names.Resolve()
	require.True(t, ok)
	names.mu.Lock()
	defer names.mu.Unlock()
	assert.Nil(t, names.found, "a lookup raced by DropLocal must not be kept")
}
