package signin_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// signin / signin-flow は、プラグインが管理するアカウント (#3468) を居ない
// 利用者と同じ 404 にする。パスワードを入れた状態で試し、拒否が「パスワードが
// 無いから」で偶然に成り立っていないことを示す。普通の利用者は通る。
func TestSignin_PluginManagedRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		flow bool
		body string
	}{
		{name: "signin first step", body: `{"username":"bot"}`},
		{name: "signin with password", body: `{"username":"bot","password":"pass123"}`},
		{name: "signin-flow first step", flow: true, body: `{"username":"bot"}`},
		{name: "signin-flow with password", flow: true, body: `{"username":"bot","password":"pass123"}`},
		{name: "signin-flow with 2fa token", flow: true, body: `{"username":"bot","password":"pass123","token":"123456"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, managed := range []bool{true, false} {
				h, repo := newTestHandler(t)
				u := createTestUser(repo, "bot", "pass123")
				if managed {
					name := "bot-plugin"
					u.ManagedByPlugin = &name
				}
				fn := h.Signin
				if tc.flow {
					fn = h.SigninFlow
				}
				rec := doPost(fn, tc.body)
				if managed {
					assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
					assert.Contains(t, rec.Body.String(), "6cc579cc-885d-43d8-95c2-b8c7fc963280")
					assert.NotContains(t, rec.Body.String(), *u.Token)
				} else {
					assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				}
			}
		})
	}
}
