package admin_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v5"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/core/passwordguard"
	"github.com/elythia-network/elythia/internal/core/strongauth"
	"github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

const (
	purgeAdminID  = "admin1"
	purgeNo2faID  = "admin-no2fa"
	purgePassword = "correct horse"
)

type purgeRoles struct{}

func (purgeRoles) IsAdministrator(id string) bool { return id == purgeAdminID || id == purgeNo2faID }

type purgeProfiles struct{ m map[string]*model.UserProfile }

func (p *purgeProfiles) GetProfileErr(id string) (*model.UserProfile, error) { return p.m[id], nil }
func (p *purgeProfiles) RemoveBackupCode(string, string) error               { return nil }

// purgeVerifier builds the strongauth verifier of admin/backup/* over an
// administrator with TOTP and one without, and returns a function giving a
// valid TOTP code. The verifier does not refuse a used code; see
// purgeVerifierWithReplay.
func purgeVerifier(t *testing.T) (*strongauth.Verifier, func() string) {
	t.Helper()
	return newPurgeVerifier(t, false)
}

// purgeVerifierWithReplay is purgeVerifier refusing a TOTP code used once,
// as the server wires it.
func purgeVerifierWithReplay(t *testing.T) (*strongauth.Verifier, func() string) {
	t.Helper()
	return newPurgeVerifier(t, true)
}

func newPurgeVerifier(t *testing.T, replay bool) (*strongauth.Verifier, func() string) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	hash, err := bcrypt.GenerateFromPassword([]byte(purgePassword), bcrypt.MinCost)
	require.NoError(t, err)
	h := string(hash)
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "t", AccountName: "a"})
	require.NoError(t, err)
	secret := key.Secret()
	deps := strongauth.Deps{
		Roles: purgeRoles{},
		Profiles: &purgeProfiles{m: map[string]*model.UserProfile{
			purgeAdminID: {UserID: purgeAdminID, Password: &h, TwoFactorEnabled: true, TwoFactorSecret: &secret},
			purgeNo2faID: {UserID: purgeNo2faID, Password: &h},
		}},
		Guard: passwordguard.NewRedisGuard(rdb),
	}
	if replay {
		deps.Replay = twofactor.NewRedisReplayGuard(rdb)
	}
	v, err := strongauth.New(deps)
	require.NoError(t, err)
	return v, func() string {
		code, err := totp.GenerateCode(secret, time.Now())
		require.NoError(t, err)
		return code
	}
}

// doNativePost posts body as userID signed in with its native token, as the
// browser does.
func doNativePost(h func(*echo.Context) error, body, userID string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	tok := "native-" + userID
	c.Set(string(middleware.UserContextKey), &model.User{ID: userID, Token: &tok})
	c.Set(string(middleware.TokenContextKey), tok)
	_ = h(c)
	return rec
}

func purgeBody(password, token string) string {
	b, _ := json.Marshal(map[string]string{"password": password, "token": token})
	return string(b)
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), rec.Body.String())
	return env.Error.Code
}

// ローカルを消す処理は取り消せないので、DB のバックアップと同じ再認証を求める (#4184)。
func TestDriveStorageMigrationPurgeLocal_Reauthenticates(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDriveStorageMigrator{}
	h.SetDriveStorageMigrator(stub)
	v, code := purgeVerifier(t)

	// 再認証を配線し忘れたら、通さずに閉じる。
	rec := doNativePost(h.DriveStorageMigrationPurgeLocal, purgeBody(purgePassword, code()), purgeAdminID)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Zero(t, stub.purgeStarts)

	h.SetDriveStorageMigrationVerifier(v)
	repo := attachModLog(t, h)
	for _, tt := range []struct {
		name, body, user, code string
		status                 int
	}{
		{"no re-authentication", `{}`, purgeAdminID, "REAUTHENTICATION_REQUIRED", http.StatusForbidden},
		{"wrong password", purgeBody("wrong", code()), purgeAdminID, "REAUTHENTICATION_FAILED", http.StatusForbidden},
		{"wrong code", purgeBody(purgePassword, "000000"), purgeAdminID, "REAUTHENTICATION_FAILED", http.StatusForbidden},
		{"no second factor registered", purgeBody(purgePassword, ""), purgeNo2faID, "TWO_FACTOR_REQUIRED", http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := doNativePost(h.DriveStorageMigrationPurgeLocal, tt.body, tt.user)
			assert.Equal(t, tt.status, rec.Code, rec.Body.String())
			assert.Equal(t, tt.code, errorCode(t, rec))
		})
	}
	// アプリの token (users.token と一致しない) では流せない。
	rec = doPost(h.DriveStorageMigrationPurgeLocal, purgeBody(purgePassword, code()), &model.User{ID: purgeAdminID})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Zero(t, stub.purgeStarts, "a refused request started the purge")
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, repo.Snapshot(), "a refused request was logged")

	rec = doNativePost(h.DriveStorageMigrationPurgeLocal, purgeBody(purgePassword, code()), purgeAdminID)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, 1, stub.purgeStarts)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "purgeLocalDriveFiles", repo.Snapshot()[0].Type)
}

// 使ったワンタイムのコードは、もう一度送っても通さない (本番の配線は Replay を渡す)。
func TestDriveStorageMigrationPurgeLocal_RefusesUsedCode(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDriveStorageMigrator{}
	h.SetDriveStorageMigrator(stub)
	v, code := purgeVerifierWithReplay(t)
	h.SetDriveStorageMigrationVerifier(v)

	body := purgeBody(purgePassword, code())
	rec := doNativePost(h.DriveStorageMigrationPurgeLocal, body, purgeAdminID)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	rec = doNativePost(h.DriveStorageMigrationPurgeLocal, body, purgeAdminID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "TWO_FACTOR_CODE_ALREADY_USED", errorCode(t, rec))
	assert.Equal(t, 1, stub.purgeStarts, "a used code started the purge again")
}

// 4 つの処理を始めたら、モデレーションのログに残す。断ったときは残さない (#4184)。
func TestDriveStorageMigration_WritesModerationLog(t *testing.T) {
	for _, tt := range []struct {
		name    string
		call    func(h *apiadmin.Handler) func(*echo.Context) error
		logType string
	}{
		{"start", func(h *apiadmin.Handler) func(*echo.Context) error { return h.DriveStorageMigrationStart }, "migrateDriveFilesToObjectStorage"},
		{"verify", func(h *apiadmin.Handler) func(*echo.Context) error { return h.DriveStorageMigrationVerify }, "verifyMigratedDriveFiles"},
		{"restore", func(h *apiadmin.Handler) func(*echo.Context) error { return h.DriveStorageMigrationRestore }, "restoreMigratedDriveFiles"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, _, _, _ := newTestHandler(t)
			stub := &stubDriveStorageMigrator{}
			h.SetDriveStorageMigrator(stub)
			repo := attachModLog(t, h)
			require.Equal(t, http.StatusNoContent, doPost(tt.call(h), `{}`, adminUser).Code)
			require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
			assert.Equal(t, tt.logType, repo.Snapshot()[0].Type)

			h2, _, _, _ := newTestHandler(t)
			h2.SetDriveStorageMigrator(&stubDriveStorageMigrator{startErr: errors.New("redis down"), verifyErr: errors.New("redis down"), restoreErr: errors.New("redis down")})
			repo2 := attachModLog(t, h2)
			require.Equal(t, http.StatusInternalServerError, doPost(tt.call(h2), `{}`, adminUser).Code)
			time.Sleep(50 * time.Millisecond)
			assert.Empty(t, repo2.Snapshot())
		})
	}
}
