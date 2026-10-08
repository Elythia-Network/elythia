package oauth

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// プラグインが管理するアカウント (#3468) には認可コードを出さない。login_token は
// form の値で auth middleware を通らないので、ここで拒否しないと外から native
// token を持ち込んでアクセストークンを作れる。普通の利用者は今までどおり。
func TestDecision_PluginManagedAccountRefused(t *testing.T) {
	plugin := "bot-plugin"
	for _, tc := range []struct {
		name     string
		managed  *string
		wantCode int
	}{
		{name: "plugin managed", managed: &plugin, wantCode: http.StatusBadRequest},
		{name: "ordinary user", managed: nil, wantCode: http.StatusFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hn := newHarness(t)
			hn.users.byToken["native-tok"] = &model.User{ID: "u1", ManagedByPlugin: tc.managed}
			hn.store.txns["txn1"] = &transaction{
				ClientID: hn.clientID, RedirectURI: hn.redirect, Scopes: []string{"read:account"},
				CodeChallenge: challengeFor("v"), State: "st",
			}
			rec := hn.post(t, hn.h.Decision, url.Values{
				"transaction_id": {"txn1"}, "login_token": {"native-tok"},
			})
			require.Equal(t, tc.wantCode, rec.Code, rec.Body.String())
			if tc.managed != nil {
				assert.Empty(t, hn.store.grants, "認可コードを作らない")
				assert.Empty(t, rec.Header().Get("Location"))
			} else {
				assert.Len(t, hn.store.grants, 1)
			}
		})
	}
}
