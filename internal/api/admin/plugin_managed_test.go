package admin_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func managedBy(name string) *string { return &name }

// admin/reset-password は、プラグインが管理するアカウント (#3468) にパスワードを
// 作らない。応答に新しいパスワードが載るので、通すとログインできる相手になる。
func TestResetPasswordAdmin_PluginManagedRefused(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["bot"] = &model.User{ID: "bot", Username: "bot", ManagedByPlugin: managedBy("bot-plugin")}
	userRepo.Profiles["bot"] = &model.UserProfile{UserID: "bot"}

	rec := doPost(h.ResetPassword, `{"userId":"bot"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp struct {
		Error struct {
			Code string `json:"code"`
			ID   string `json:"id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "PLUGIN_MANAGED_ACCOUNT", resp.Error.Code)
	assert.NotContains(t, rec.Body.String(), `"password"`)
	assert.Nil(t, userRepo.Profiles["bot"].Password, "パスワードを書かない")
}

// admin/show-user は管理しているプラグインの名前を返す。普通のアカウントは null。
func TestShowUser_ManagedByPlugin(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["bot"] = &model.User{ID: "bot", Username: "bot", ManagedByPlugin: managedBy("bot-plugin")}
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "u1"}

	for _, tc := range []struct {
		id   string
		want any
	}{{"bot", "bot-plugin"}, {"u1", nil}} {
		rec := doPost(h.ShowUser, `{"userId":"`+tc.id+`"}`, &model.User{ID: "admin1"})
		require.Equal(t, http.StatusOK, rec.Code)
		var got map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		v, ok := got["managedByPlugin"]
		assert.True(t, ok, tc.id)
		assert.Equal(t, tc.want, v, tc.id)
	}
}

// DeletePluginManagedAccount は admin/delete-account と同じ流れを通す。管理する
// アカウント以外 (普通の利用者・root・リモート) は受け付けない。
func TestDeletePluginManagedAccount(t *testing.T) {
	remote := "remote.example"
	for _, tc := range []struct {
		name string
		user *model.User
	}{
		{"nil", nil},
		{"ordinary", &model.User{ID: "u1"}},
		{"root", &model.User{ID: "root", IsRoot: true}},
		{"remote managed", &model.User{ID: "r1", Host: &remote, ManagedByPlugin: managedBy("bot-plugin")}},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			h, userRepo, _, _ := newTestHandler(t)
			stub := &stubDeleteAccountEnqueuer{}
			h.SetDeleteAccountEnqueuer(stub)
			if tc.user != nil {
				userRepo.Users[tc.user.ID] = tc.user
			}
			require.Error(t, h.DeletePluginManagedAccount(tc.user))
			assert.Zero(t, stub.called)
			if tc.user != nil {
				assert.False(t, userRepo.Users[tc.user.ID].IsDeleted)
			}
		})
	}

	t.Run("deletes a managed account", func(t *testing.T) {
		h, userRepo, _, _ := newTestHandler(t)
		stub := &stubDeleteAccountEnqueuer{}
		h.SetDeleteAccountEnqueuer(stub)
		inv := &stubUserTokenInvalidator{}
		h.SetUserTokenInvalidator(inv)
		bot := &model.User{ID: "bot", Username: "bot", ManagedByPlugin: managedBy("bot-plugin")}
		userRepo.Users["bot"] = bot

		require.NoError(t, h.DeletePluginManagedAccount(bot))
		assert.True(t, userRepo.Users["bot"].IsDeleted)
		assert.True(t, userRepo.Users["bot"].IsSuspended)
		assert.Equal(t, []string{"bot"}, inv.calls, "token cache を落とす")
		assert.Equal(t, 1, stub.called)
		assert.Equal(t, "bot", stub.lastUserID)
	})

	t.Run("update failure", func(t *testing.T) {
		h, _, _, _ := newTestHandler(t)
		stub := &stubDeleteAccountEnqueuer{}
		h.SetDeleteAccountEnqueuer(stub)
		// repo に居ない利用者は UPDATE が失敗する。
		require.Error(t, h.DeletePluginManagedAccount(&model.User{ID: "ghost", ManagedByPlugin: managedBy("bot-plugin")}))
		assert.Zero(t, stub.called, "フラグを立てられなければ削除のジョブも積まない")
	})
}

// 初回セットアップの窓は、プラグインが管理するアカウントしか居なければ開いた
// ままにする (#3468)。普通のローカル利用者が居れば閉じる。
func TestAccountsCreate_InitialSetupIgnoresPluginManaged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		managed *string
		want    int
	}{
		{name: "only a managed account", managed: managedBy("bot-plugin"), want: http.StatusOK},
		{name: "an ordinary account", managed: nil, want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, userRepo, metaRepo, _ := newTestHandler(t)
			metaRepo.Meta = &model.Meta{ID: "x"}
			userRepo.Users["existing"] = &model.User{ID: "existing", Username: "existing", UsernameLower: "existing", ManagedByPlugin: tc.managed}
			rec := doPost(h.AccountsCreate, `{"username":"first_admin","password":"admin-pass"}`, nil)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}
}
