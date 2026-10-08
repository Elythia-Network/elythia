package plugintest_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"
)

// フェイクの Accounts は、本番と同じ理由で拒否する (#3468)。他のプラグインの
// アカウントは ErrAccountNotFound、形式の違う名前は ErrInvalidUsername、使用済み
// (削除済みを含む) の名前は ErrUsernameUnavailable。
func TestAccounts_FakeScopesLikeHost(t *testing.T) {
	h := plugintest.New(t).WithName("bot")
	other := h.SeedAccount("other-plugin", "theirs")
	acc := h.Context().Accounts()
	ctx := context.Background()

	mine, err := acc.Create(ctx, "mine")
	require.NoError(t, err)
	assert.Equal(t, "mine", mine.Username)
	_, err = acc.Create(ctx, "Theirs")
	assert.ErrorIs(t, err, plugin.ErrUsernameUnavailable, "大文字小文字を区別せず重複を弾く")
	_, err = acc.Create(ctx, "bad name")
	assert.ErrorIs(t, err, plugin.ErrInvalidUsername)

	list, err := acc.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []plugin.Account{mine}, list, "自分のアカウントだけを返す")

	name := "Bot"
	assert.ErrorIs(t, acc.UpdateProfile(ctx, other.ID, plugin.ProfileUpdate{Name: &name}), plugin.ErrAccountNotFound)
	assert.ErrorIs(t, acc.Delete(ctx, other.ID), plugin.ErrAccountNotFound)

	desc := "hello"
	avatar := &plugin.Image{Data: []byte("png")}
	banner := &plugin.Image{Data: []byte("png2")}
	require.NoError(t, acc.UpdateProfile(ctx, mine.ID, plugin.ProfileUpdate{Name: &name, Description: &desc, Avatar: avatar, Banner: banner}))
	assert.Error(t, acc.UpdateProfile(ctx, mine.ID, plugin.ProfileUpdate{Avatar: &plugin.Image{}}), "空の画像は本番と同じく失敗する")

	require.NoError(t, acc.Delete(ctx, mine.ID))
	assert.ErrorIs(t, acc.Delete(ctx, mine.ID), plugin.ErrAccountNotFound, "消したものは見えない")
	_, err = acc.Create(ctx, "mine")
	assert.ErrorIs(t, err, plugin.ErrUsernameUnavailable, "消したアカウントの名前も使えない")
	list, err = acc.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)

	all := h.ManagedAccounts()
	require.Len(t, all, 2)
	assert.Equal(t, "other-plugin", all[0].Owner)
	got := all[1]
	assert.Equal(t, "bot", got.Owner)
	assert.True(t, got.Deleted)
	assert.Equal(t, &name, got.Name)
	assert.Equal(t, &desc, got.Description)
	assert.Equal(t, avatar, got.Avatar)
	assert.Equal(t, banner, got.Banner)
}

type stubAccounts struct{ plugin.Accounts }

func TestAccounts_WithAccountsOverrides(t *testing.T) {
	stub := stubAccounts{}
	h := plugintest.New(t).WithAccounts(stub)
	assert.Equal(t, stub, h.Context().Accounts())
}

func TestAccounts_SuspendedRefusesProfileUpdate(t *testing.T) {
	h := plugintest.New(t)
	acc, err := h.Context().Accounts().Create(context.Background(), "bot")
	require.NoError(t, err)
	h.SuspendAccount(acc.ID)
	name := "x"
	err = h.Context().Accounts().UpdateProfile(context.Background(), acc.ID, plugin.ProfileUpdate{Name: &name})
	assert.ErrorIs(t, err, plugin.ErrAccountSuspended)
	assert.Nil(t, h.ManagedAccounts()[0].Name)
}
