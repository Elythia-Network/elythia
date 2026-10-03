package repository

import (
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedRetainedCredentials(t *testing.T, uid string) []any {
	t.Helper()
	u := insertTestUser(t, uid, uid)
	t.Cleanup(func() { cleanupUser(t, uid) })
	require.NoError(t, testDB.Model(u).Update("isDeleted", true).Error)
	secret := "secret"
	profile := &model.UserProfile{UserID: uid, Password: &secret, EmailVerifyCode: &secret,
		TwoFactorSecret: &secret, TwoFactorTempSecret: &secret, TwoFactorBackupSecret: model.StringArray{"backup"},
		TwoFactorEnabled: true, SecurityKeysAvailable: true, UsePasswordLessLogin: true, Description: &secret}
	require.NoError(t, testDB.Create(profile).Error)
	app := &model.App{ID: uid + "app", Secret: "appsecret", Name: "test", Description: "test", Permission: model.StringArray{}}
	require.NoError(t, testDB.Create(app).Error)
	t.Cleanup(func() { require.NoError(t, testDB.Delete(app).Error) })
	rows := []any{
		&model.AccessToken{ID: uid + "token", UserID: uid, Token: secret, Hash: uid},
		&model.AuthSession{ID: uid + "session", UserID: &uid, Token: secret, AppID: app.ID},
		&model.UserSecurityKey{ID: uid + "key", UserID: uid, Name: "key", PublicKey: secret, LastUsed: time.Now()},
		&model.PasswordResetRequest{ID: uid + "reset", UserID: uid, Token: uid + "resetsecret"},
		&model.SwSubscription{ID: uid + "push", UserID: uid, Endpoint: "https://example.invalid/push", Auth: secret, PublicKey: secret},
		&model.Webhook{ID: uid + "hook", UserID: uid, Name: "hook", URL: "https://example.invalid/hook", Secret: secret},
	}
	for _, row := range rows {
		require.NoError(t, testDB.Create(row).Error)
	}
	return rows
}

func TestRevokeDeletedLocalCredentials(t *testing.T) {
	rows := seedRetainedCredentials(t, "credtarget")
	other := seedRetainedCredentials(t, "credother")
	clip := &model.Clip{ID: "credtargetclip", UserID: "credtarget", Name: "retained", IsPublic: true}
	require.NoError(t, testDB.Create(clip).Error)
	repo := NewUserRepository(testDB)
	require.NoError(t, repo.RevokeDeletedLocalCredentials("credtarget"))
	require.NoError(t, repo.RevokeDeletedLocalCredentials("credtarget"))
	for i, row := range rows {
		var count int64
		require.NoError(t, testDB.Model(row).Where(`"userId" = ?`, "credtarget").Count(&count).Error)
		assert.Zero(t, count, "%T", row)
		require.NoError(t, testDB.Model(other[i]).Where(`"userId" = ?`, "credother").Count(&count).Error)
		assert.EqualValues(t, 1, count, "other owner %T", row)
	}
	u, err := repo.FindByID("credtarget")
	require.NoError(t, err)
	assert.Nil(t, u.Token)
	assert.Equal(t, "credtarget", u.Username)
	var profile model.UserProfile
	require.NoError(t, testDB.First(&profile, `"userId" = ?`, u.ID).Error)
	assert.Nil(t, profile.Password)
	assert.Nil(t, profile.EmailVerifyCode)
	assert.Nil(t, profile.TwoFactorSecret)
	assert.Nil(t, profile.TwoFactorTempSecret)
	assert.Empty(t, profile.TwoFactorBackupSecret)
	assert.False(t, profile.TwoFactorEnabled)
	assert.False(t, profile.SecurityKeysAvailable)
	assert.False(t, profile.UsePasswordLessLogin)
	require.NotNil(t, profile.Description)
	assert.Equal(t, "secret", *profile.Description)
	var retained model.Clip
	require.NoError(t, testDB.First(&retained, "id = ?", clip.ID).Error)
	assert.True(t, retained.IsPublic)
	require.NoError(t, repo.RevokeDeletedLocalCredentials("absentcredentials"))
}

func TestRevokeDeletedLocalCredentialsRollsBack(t *testing.T) {
	rows := seedRetainedCredentials(t, "credrollback")
	require.NoError(t, testDB.Exec(`CREATE FUNCTION reject_retained_webhook_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF OLD."userId" = 'credrollback' THEN RAISE EXCEPTION 'credential cleanup failure'; END IF; RETURN OLD; END $$`).Error)
	t.Cleanup(func() {
		require.NoError(t, testDB.Exec(`DROP FUNCTION reject_retained_webhook_delete() CASCADE`).Error)
	})
	require.NoError(t, testDB.Exec(`CREATE TRIGGER reject_retained_webhook_delete BEFORE DELETE ON webhook
FOR EACH ROW EXECUTE FUNCTION reject_retained_webhook_delete()`).Error)
	require.Error(t, NewUserRepository(testDB).RevokeDeletedLocalCredentials("credrollback"))
	for _, row := range rows {
		var count int64
		require.NoError(t, testDB.Model(row).Where(`"userId" = ?`, "credrollback").Count(&count).Error)
		assert.EqualValues(t, 1, count, "%T", row)
	}
	u, err := NewUserRepository(testDB).FindByID("credrollback")
	require.NoError(t, err)
	assert.NotNil(t, u.Token)
	var profile model.UserProfile
	require.NoError(t, testDB.First(&profile, `"userId" = ?`, u.ID).Error)
	assert.NotNil(t, profile.Password)
	assert.NotNil(t, profile.TwoFactorSecret)
}

func TestRevokeDeletedLocalCredentialsRejectsActiveAndRemote(t *testing.T) {
	seedRetainedCredentials(t, "credguard")
	repo := NewUserRepository(testDB)
	require.NoError(t, testDB.Model(&model.User{}).Where("id = ?", "credguard").Update("isDeleted", false).Error)
	require.Error(t, repo.RevokeDeletedLocalCredentials("credguard"))
	require.NoError(t, testDB.Model(&model.User{}).Where("id = ?", "credguard").Updates(map[string]any{"isDeleted": true, "host": "remote.example"}).Error)
	require.Error(t, repo.RevokeDeletedLocalCredentials("credguard"))
	u, err := repo.FindByID("credguard")
	require.NoError(t, err)
	assert.NotNil(t, u.Token)
}
