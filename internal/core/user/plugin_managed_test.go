package user_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// プラグインが管理するアカウント (#3468) は isBot を外せない。外す指定だけを
// 拒否し、true を送る更新や isBot を含まない更新は通す。普通の利用者は外せる。
func TestUpdateProfile_PluginManagedMustStayBot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		managed *string
		in      user.UpdateInput
		wantErr bool
		wantBot bool
	}{
		{name: "managed clears isBot", managed: ptr("bot-plugin"), in: user.UpdateInput{IsBot: ptr(false), Name: ptr(ptr("x"))}, wantErr: true, wantBot: true},
		{name: "managed keeps isBot", managed: ptr("bot-plugin"), in: user.UpdateInput{IsBot: ptr(true)}, wantBot: true},
		{name: "managed other field", managed: ptr("bot-plugin"), in: user.UpdateInput{Name: ptr(ptr("x"))}, wantBot: true},
		{name: "ordinary clears isBot", managed: nil, in: user.UpdateInput{IsBot: ptr(false)}, wantBot: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, userRepo, _, _ := newFullSvc(t)
			userRepo.Users["u1"] = &model.User{ID: "u1", IsBot: true, ManagedByPlugin: tc.managed}
			userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}
			_, err := svc.UpdateProfile("u1", tc.in)
			if tc.wantErr {
				require.ErrorIs(t, err, user.ErrManagedAccountMustBeBot)
				assert.Nil(t, userRepo.Users["u1"].Name, "拒否したら他の項目も書かない")
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantBot, userRepo.Users["u1"].IsBot)
		})
	}
}
