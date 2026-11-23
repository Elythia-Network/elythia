package admin_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// DeleteMigratedAccount (#4248) は admin/delete-account と同じ流れを通す。root・
// システムアカウント・リモートは消さず、root かどうか分からないときも消さない。
func TestDeleteMigratedAccount(t *testing.T) {
	remote := "remote.example"
	rootID := "meta-root"
	for _, tc := range []struct {
		name      string
		user      *model.User
		meta      *model.Meta
		fetchFail bool
	}{
		{name: "nil"},
		{name: "remote", user: &model.User{ID: "r1", Username: "r1", Host: &remote}},
		{name: "root flag", user: &model.User{ID: "root", Username: "root", IsRoot: true}},
		{name: "meta root", user: &model.User{ID: rootID, Username: "admin"}, meta: &model.Meta{ID: "x", RootUserID: &rootID}},
		{name: "system account", user: &model.User{ID: "s1", Username: "instance.actor"}},
		{name: "undetermined", user: &model.User{ID: "u1", Username: "carol"}, fetchFail: true},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			h, userRepo, metaRepo, _ := newTestHandler(t)
			stub := &stubDeleteAccountEnqueuer{}
			h.SetDeleteAccountEnqueuer(stub)
			metaRepo.Meta = tc.meta
			if tc.fetchFail {
				metaRepo.FetchErr = errors.New("db down")
			}
			if tc.user != nil {
				userRepo.Users[tc.user.ID] = tc.user
			}
			require.Error(t, h.DeleteMigratedAccount(tc.user))
			assert.Zero(t, stub.called)
			if tc.user != nil {
				assert.False(t, userRepo.Users[tc.user.ID].IsDeleted)
			}
		})
	}

	t.Run("deletes a local account", func(t *testing.T) {
		h, userRepo, _, _ := newTestHandler(t)
		stub := &stubDeleteAccountEnqueuer{}
		h.SetDeleteAccountEnqueuer(stub)
		inv := &stubUserTokenInvalidator{}
		h.SetUserTokenInvalidatorForTest(inv)
		carol := &model.User{ID: "u1", Username: "carol"}
		userRepo.Users["u1"] = carol

		require.NoError(t, h.DeleteMigratedAccount(carol))
		assert.True(t, userRepo.Users["u1"].IsDeleted)
		assert.True(t, userRepo.Users["u1"].IsSuspended)
		assert.Equal(t, []string{"u1"}, inv.calls, "token cache を落とす")
		assert.Equal(t, 1, stub.called)
		assert.Equal(t, "u1", stub.lastUserID)
	})

	t.Run("update failure", func(t *testing.T) {
		h, _, _, _ := newTestHandler(t)
		stub := &stubDeleteAccountEnqueuer{}
		h.SetDeleteAccountEnqueuer(stub)
		require.Error(t, h.DeleteMigratedAccount(&model.User{ID: "ghost", Username: "ghost"}))
		assert.Zero(t, stub.called)
	})

	// 後片付けのジョブを積めなければ失敗にする (convert が印を付けないように)。
	// プラグインの経路は、管理画面と同じく失敗にしない。
	t.Run("cascade not queued", func(t *testing.T) {
		h, userRepo, _, _ := newTestHandler(t)
		h.SetDeleteAccountEnqueuer(&stubDeleteAccountEnqueuer{err: errors.New("redis down")})
		carol := &model.User{ID: "u1", Username: "carol"}
		userRepo.Users["u1"] = carol
		err := h.DeleteMigratedAccount(carol)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "delete it again from the admin page")
		assert.True(t, userRepo.Users["u1"].IsDeleted, "flag は立っている")

		bot := &model.User{ID: "bot", Username: "bot", ManagedByPlugin: managedBy("bot-plugin")}
		userRepo.Users["bot"] = bot
		assert.NoError(t, h.DeletePluginManagedAccount(bot))
	})
}
