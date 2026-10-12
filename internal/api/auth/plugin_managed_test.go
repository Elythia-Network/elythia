package auth

import (
	"errors"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
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

// miauth/:session/check は、プラグインが管理するアカウントの token を払い出さない。
// gen-token / accept が作らないので本来は行が無いが、それより前のデータなどで行が
// あっても、資格情報を外へ出さない。取得済みにもしない。
func TestMiAuthCheck_PluginManagedOwnerRefused(t *testing.T) {
	h, repo := newTestHandler()
	sess := "sess-managed"
	repo.accessTokens["k"] = &model.AccessToken{ID: "at1", Token: "bot-token", UserID: "bot1", Session: &sess}
	managed := "bot-plugin"
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["bot1"] = &model.User{ID: "bot1", Username: "bot", ManagedByPlugin: &managed}

	rec := postMiAuthCheck(h, sess, userRepo)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "bot-token")
	assert.JSONEq(t, `{"ok":false}`, rec.Body.String())
	assert.False(t, repo.accessTokens["k"].Fetched, "取得済みにしない")
}

// 持ち主を読めない障害のときも払い出さず、取得済みにしない (polling で取り直せる)。
// 持ち主の行が無いだけなら、今までどおり払い出す (user は付けない)。
func TestMiAuthCheck_OwnerLookup(t *testing.T) {
	t.Run("lookup error", func(t *testing.T) {
		h, repo := newTestHandler()
		sess := "sess-err"
		repo.accessTokens["k"] = &model.AccessToken{ID: "at1", Token: "tok", UserID: "u1", Session: &sess}
		userRepo := testutil.NewMockUserRepository()
		userRepo.FindErr = errors.New("connection refused")

		rec := postMiAuthCheck(h, sess, userRepo)
		assert.JSONEq(t, `{"ok":false}`, rec.Body.String())
		assert.False(t, repo.accessTokens["k"].Fetched)
	})
	t.Run("owner not found", func(t *testing.T) {
		h, repo := newTestHandler()
		sess := "sess-gone"
		repo.accessTokens["k"] = &model.AccessToken{ID: "at1", Token: "tok", UserID: "gone", Session: &sess}

		rec := postMiAuthCheck(h, sess, testutil.NewMockUserRepository())
		assert.JSONEq(t, `{"ok":true,"token":"tok"}`, rec.Body.String())
		assert.True(t, repo.accessTokens["k"].Fetched)
	})
}

// auth/session/userkey も、プラグインが管理するアカウントの token を払い出さない。
// 居ない session と同じ応答にし、session は消さない。
func TestSessionUserkey_PluginManagedOwnerRefused(t *testing.T) {
	h, repo := newTestHandler()
	repo.apps["s1"] = &model.App{ID: "a1", Secret: "s1"}
	managed := "bot-plugin"
	userID := "bot1"
	user := &model.User{ID: "bot1", Username: "bot", ManagedByPlugin: &managed}
	repo.sessions["tok1"] = &model.AuthSession{ID: "sess1", Token: "tok1", AppID: "a1", UserID: &userID, User: user}
	appID := "a1"
	repo.accessTokens["a1:bot1"] = &model.AccessToken{ID: "at1", Token: "bot-token", AppID: &appID, UserID: "bot1"}

	rec := post(h.SessionUserkey, `{"appSecret":"s1","token":"tok1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_SESSION")
	assert.NotContains(t, rec.Body.String(), "bot-token")
	assert.NotEmpty(t, repo.sessions, "session を消さない")
}
