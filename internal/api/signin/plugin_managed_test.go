package signin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// プラグインが管理するアカウント (#3468) は、パスキーの検証が通っても
// ログインさせない。パスワード無しログインを有効にした profile を持たせ、
// 拒否が「設定が無いから」で偶然に成り立っていないことを示す。同じ条件の
// 普通の利用者は通る。
func TestFinishPasskeySignin_PluginManagedRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		managed *string
		want    int
	}{
		{name: "plugin managed", managed: ptr("bot-plugin"), want: http.StatusForbidden},
		{name: "ordinary user", managed: nil, want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			tok := "Tk-managed"
			user := &model.User{ID: "u1", Token: &tok, ManagedByPlugin: tc.managed}
			repo.Users["u1"] = user
			repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", UsePasswordLessLogin: true}
			h := NewHandler(repo)
			c := newCtx()
			rec := c.Response().Writer.(*httptest.ResponseRecorder)
			require.NoError(t, h.finishPasskeySignin(c, user, makeStubWebauthnCred()))
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
			if tc.managed != nil {
				var resp map[string]any
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				assert.Equal(t, "652f899f-66d4-490e-993e-6606c8ec04c3", resp["error"].(map[string]any)["id"],
					"利用者が居ないのと同じ応答")
				assert.NotContains(t, rec.Body.String(), tok)
			}
		})
	}
}

func ptr(s string) *string { return &s }
