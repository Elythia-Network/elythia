package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// user.token は char(16) で、PostgreSQL の char 比較は末尾の空白を無視する。
// 実 DB では "<token> " も同じ利用者に解決されるので、middleware が保存値との
// 完全一致を見ていないと、空白付きの文字列が別の tokenCache キー / 別の
// WebSocket 失効鍵として通ってしまう (i/regenerate-token で閉じない)。
func TestAuthenticate_NativeTokenWithTrailingSpaceIsRejected_RealDB(t *testing.T) {
	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	testutil.ApplyMigrations(db)
	require.NoError(t, db.Exec(`DELETE FROM "user" WHERE "id" = ?`, "u_trailing_space").Error)

	const tok = "trailspacetok016"
	require.Len(t, tok, 16)
	tokCopy := tok
	require.NoError(t, db.Create(&model.User{
		ID:            "u_trailing_space",
		Username:      "trailing_space",
		UsernameLower: "trailing_space",
		Token:         &tokCopy,
	}).Error)
	t.Cleanup(func() { db.Exec(`DELETE FROM "user" WHERE "id" = ?`, "u_trailing_space") })

	userRepo := repository.NewUserRepository(db)
	// 前提の確認: repository 自体は char の意味論で空白付きを同じ行に解決する。
	u, err := userRepo.FindByToken(tok + " ")
	require.NoError(t, err, "char(16) の比較は末尾空白を無視する (この前提が崩れたらテストを見直す)")
	require.Equal(t, "u_trailing_space", u.ID)

	auth := NewAuthMiddleware(userRepo, repository.NewAccessTokenRepository(db))
	e := echo.New()
	send := func(token string) (*httptest.ResponseRecorder, *model.User) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		var got *model.User
		require.NoError(t, auth.Authenticate()(func(c echo.Context) error {
			got = GetUser(c)
			return c.String(http.StatusOK, "ok")
		})(e.NewContext(req, rec)))
		return rec, got
	}

	rec, got := send(tok + " ")
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "空白付きの native token は認証失敗にする")
	assert.Nil(t, got)
	assert.Equal(t, 0, auth.tokenCache.len(), "空白付きの文字列を cache の別キーとして積まない")

	rec, got = send(tok)
	assert.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, got)
	assert.Equal(t, "u_trailing_space", got.ID)
}
