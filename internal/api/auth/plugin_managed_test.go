package auth

import (
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

// auth/accept と miauth/gen-token は、プラグインが管理するアカウント (#3468) に
// app token を発行しない。そのアカウントの native token は外では通らないので、
// ここに来るのはプラグインの AsUser だけ。通すと外で使える資格情報が作れる。
func TestTokenIssuance_PluginManagedRefused(t *testing.T) {
	managed := "bot-plugin"
	for _, tc := range []struct {
		name    string
		managed *string
		accept  int
		genTok  int
	}{
		{name: "plugin managed", managed: &managed, accept: http.StatusBadRequest, genTok: http.StatusBadRequest},
		{name: "ordinary user", managed: nil, accept: http.StatusNoContent, genTok: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := newTestHandler()
			repo.apps["s1"] = &model.App{ID: "a1", Secret: "s1", Permission: model.StringArray{"read:account"}}
			repo.sessions["tok1"] = &model.AuthSession{ID: "sess1", Token: "tok1", AppID: "a1"}
			user := &model.User{ID: "u1", Username: "bot", ManagedByPlugin: tc.managed}

			rec := post(h.Accept, `{"token":"tok1"}`, user)
			assert.Equal(t, tc.accept, rec.Code, rec.Body.String())
			rec2 := post(h.GenToken, `{"permission":["read:account"]}`, user)
			assert.Equal(t, tc.genTok, rec2.Code, rec2.Body.String())
			if tc.managed != nil {
				assert.Contains(t, rec.Body.String(), "PLUGIN_MANAGED_ACCOUNT")
				assert.Contains(t, rec2.Body.String(), "PLUGIN_MANAGED_ACCOUNT")
				assert.Empty(t, repo.accessTokens, "token を作らない")
				assert.Nil(t, repo.sessions["tok1"].UserID, "session を承認済みにしない")
			} else {
				assert.Len(t, repo.accessTokens, 2)
			}
		})
	}
}
