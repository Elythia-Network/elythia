package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// プラグインが管理するアカウント (#3468) の token は、プロセス内の呼び出し
// (MarkInternalCall) でだけ認証に使える。外から届いたリクエストでは、token が
// 正しくても無効な token と同じ 401 になる。普通のアカウントは今までどおり。
func TestAuthenticate_PluginManagedTokenOnlyInternal_RealDB(t *testing.T) {
	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	testutil.ApplyMigrations(db)

	plugin := "bot-plugin"
	mk := func(id, token string, managed *string) {
		t.Helper()
		require.NoError(t, db.Exec(`DELETE FROM "access_token" WHERE "userId" = ?`, id).Error)
		require.NoError(t, db.Exec(`DELETE FROM "user" WHERE "id" = ?`, id).Error)
		tok := token
		require.NoError(t, db.Create(&model.User{
			ID: id, Username: id, UsernameLower: id, Token: &tok, ManagedByPlugin: managed,
		}).Error)
		t.Cleanup(func() {
			db.Exec(`DELETE FROM "access_token" WHERE "userId" = ?`, id)
			db.Exec(`DELETE FROM "user" WHERE "id" = ?`, id)
		})
	}
	mk("u_managed_auth", "managedtok000016", &plugin)
	mk("u_normal_auth", "normaltoke000016", nil)
	// 手で UPDATE して空文字になった行も、管理するアカウントとして扱う
	// (model.User.IsPluginManaged)。拒否が外れる向きに倒さない。
	empty := ""
	mk("u_empty_managed", "emptymanaged0016", &empty)
	// 発行の経路は塞いであるが、DB に残った app token も外では通さない。
	require.NoError(t, db.Create(&model.AccessToken{
		ID: "at_managed_auth", Token: "managed-app-token", Hash: "managed-app-token", UserID: "u_managed_auth",
	}).Error)

	auth := NewAuthMiddleware(repository.NewUserRepository(db), repository.NewAccessTokenRepository(db))
	e := echo.New()
	send := func(token string, internal bool) (int, *model.User) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if internal {
			req = MarkInternalCall(req)
		}
		rec := httptest.NewRecorder()
		var got *model.User
		require.NoError(t, auth.Authenticate()(func(c echo.Context) error {
			got = GetUser(c)
			return c.String(http.StatusOK, "ok")
		})(e.NewContext(req, rec)))
		return rec.Code, got
	}

	cases := []struct {
		name     string
		token    string
		internal bool
		wantCode int
		wantUser string
	}{
		{"managed native token from outside", "managedtok000016", false, http.StatusUnauthorized, ""},
		{"managed native token in process", "managedtok000016", true, http.StatusOK, "u_managed_auth"},
		{"managed app token from outside", "managed-app-token", false, http.StatusUnauthorized, ""},
		{"empty managedByPlugin from outside", "emptymanaged0016", false, http.StatusUnauthorized, ""},
		{"normal native token from outside", "normaltoke000016", false, http.StatusOK, "u_normal_auth"},
		{"normal native token in process", "normaltoke000016", true, http.StatusOK, "u_normal_auth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, got := send(tc.token, tc.internal)
			assert.Equal(t, tc.wantCode, code)
			if tc.wantUser == "" {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.wantUser, got.ID)
		})
	}

	// cache に載った後 (in-process で一度通った後) でも、外からは通さない。
	code, got := send("managedtok000016", true)
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, got)
	code, got = send("managedtok000016", false)
	assert.Equal(t, http.StatusUnauthorized, code, "cache hit でも外からは拒否する")
	assert.Nil(t, got)
}
