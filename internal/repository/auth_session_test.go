package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthSessionRepository_Full(t *testing.T) {
	repo := NewAuthSessionRepository(testDB)
	user := insertTestUser(t, "u_as_1", "authuser")
	defer cleanupUser(t, user.ID)

	// App作成
	app := &model.App{
		ID:          "app_as_1",
		Secret:      "secret_as_1",
		Name:        "TestApp",
		Description: "test app",
		Permission:  model.StringArray{"read:account"},
	}
	require.NoError(t, repo.CreateApp(app))
	defer testDB.Exec(`DELETE FROM "app" WHERE id = ?`, app.ID)

	// FindAppBySecret
	found, err := repo.FindAppBySecret("secret_as_1")
	require.NoError(t, err)
	assert.Equal(t, "TestApp", found.Name)

	// FindAppBySecret - not found
	_, err = repo.FindAppBySecret("ghost")
	assert.Error(t, err)

	// Session作成
	session := &model.AuthSession{
		ID:    "sess_as_1",
		Token: "token_as_1",
		AppID: app.ID,
	}
	require.NoError(t, repo.CreateSession(session))
	defer testDB.Exec(`DELETE FROM "auth_session" WHERE id = ?`, session.ID)

	// FindSessionByToken
	s, err := repo.FindSessionByToken("token_as_1")
	require.NoError(t, err)
	assert.Equal(t, "sess_as_1", s.ID)
	assert.NotNil(t, s.App)

	// FindSessionByToken - not found
	_, err = repo.FindSessionByToken("ghost")
	assert.Error(t, err)

	// FindSessionByTokenAndAppID
	s2, err := repo.FindSessionByTokenAndAppID("token_as_1", app.ID)
	require.NoError(t, err)
	assert.Equal(t, "sess_as_1", s2.ID)

	// FindSessionByTokenAndAppID - wrong app
	_, err = repo.FindSessionByTokenAndAppID("token_as_1", "wrong_app")
	assert.Error(t, err)

	// UpdateSessionUserID
	require.NoError(t, repo.UpdateSessionUserID("sess_as_1", user.ID))
	s3, err := repo.FindSessionByToken("token_as_1")
	require.NoError(t, err)
	require.NotNil(t, s3.UserID)
	assert.Equal(t, user.ID, *s3.UserID)

	// AccessToken作成
	accessToken := &model.AccessToken{
		ID:         "at_as_1",
		Token:      "rawtoken_as_1",
		Hash:       "hash_as_1",
		UserID:     user.ID,
		AppID:      &app.ID,
		Permission: model.StringArray{"read:account"},
	}
	require.NoError(t, repo.CreateAccessToken(accessToken))
	defer testDB.Exec(`DELETE FROM "access_token" WHERE id = ?`, accessToken.ID)

	// FindAccessTokenByAppAndUser
	at, err := repo.FindAccessTokenByAppAndUser(app.ID, user.ID)
	require.NoError(t, err)
	assert.Equal(t, "rawtoken_as_1", at.Token)

	// FindAccessTokenByAppAndUser - not found
	_, err = repo.FindAccessTokenByAppAndUser("ghost", user.ID)
	assert.Error(t, err)

	// MiAuth session token (#1224): session 紐付き token を作成して
	// FindAccessTokenBySession / MarkAccessTokenFetched を検証する。
	miSession := "misess_as_1"
	miToken := &model.AccessToken{
		ID: "at_as_mi", Token: "mirawtoken", Hash: "mihash", UserID: user.ID,
		Session: &miSession, Permission: model.StringArray{"read:account"},
	}
	require.NoError(t, repo.CreateAccessToken(miToken))
	defer testDB.Exec(`DELETE FROM "access_token" WHERE id = ?`, miToken.ID)

	miFound, err := repo.FindAccessTokenBySession(miSession)
	require.NoError(t, err)
	assert.Equal(t, "mirawtoken", miFound.Token)
	assert.False(t, miFound.Fetched)

	won, err := repo.MarkAccessTokenFetched(miToken.ID)
	require.NoError(t, err)
	assert.True(t, won, "first MarkAccessTokenFetched must win the false->true transition")
	refetched, err := repo.FindAccessTokenBySession(miSession)
	require.NoError(t, err)
	assert.True(t, refetched.Fetched, "MarkAccessTokenFetched must persist the one-time flag")

	// 2 度目はもう fetched=true なので遷移に負ける (won=false)。
	wonAgain, err := repo.MarkAccessTokenFetched(miToken.ID)
	require.NoError(t, err)
	assert.False(t, wonAgain, "second MarkAccessTokenFetched must lose: already fetched")

	// FindAccessTokenBySession - not found
	_, err = repo.FindAccessTokenBySession("ghost-session")
	assert.Error(t, err)

	// DeleteSession
	require.NoError(t, repo.DeleteSession("sess_as_1"))
	_, err = repo.FindSessionByToken("token_as_1")
	assert.Error(t, err)
}
