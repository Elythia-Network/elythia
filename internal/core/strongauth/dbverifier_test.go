package strongauth

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/core/passwordguard"
	"github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

type dbFixture struct {
	db     *gorm.DB
	v      *DBVerifier
	secret string
	opened int
	closed int
}

func newDBFixture(t *testing.T) *dbFixture {
	t.Helper()
	db, err := testutil.OpenTestDB()
	if err != nil {
		t.Skipf("PostgreSQL test DB unavailable: %v", err)
	}
	testutil.ApplyMigrations(db)
	for _, table := range []string{"role_assignment", "role", "user_security_key", "user_profile", `"user"`} {
		require.NoError(t, db.Exec("DELETE FROM "+table).Error)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "test", AccountName: "admin"})
	require.NoError(t, err)
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	f := &dbFixture{db: db, secret: key.Secret()}
	f.v = &DBVerifier{
		Open: func(context.Context) (*gorm.DB, func(), error) {
			f.opened++
			return db, func() { f.closed++ }, nil
		},
		IDGen:  gen,
		Guard:  passwordguard.NewRedisGuard(rdb),
		Replay: twofactor.NewRedisReplayGuard(rdb),
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	h := string(hash)
	for _, u := range []struct {
		id, token string
		suspended bool
	}{{"dbadmin", "tokadmin00000001", false}, {"dbmod", "tokmod0000000001", false}, {"dbsuspended", "toksusp000000001", true}} {
		tok := u.token
		require.NoError(t, db.Create(&model.User{
			ID: u.id, Username: u.id, UsernameLower: u.id, Token: &tok, IsSuspended: u.suspended,
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		}).Error)
		require.NoError(t, db.Create(&model.UserProfile{
			UserID: u.id, Password: &h, TwoFactorEnabled: true, TwoFactorSecret: &f.secret,
		}).Error)
	}
	require.NoError(t, db.Create(&model.Role{ID: "roleadmin", Name: "admin", IsAdministrator: true}).Error)
	require.NoError(t, db.Create(&model.Role{ID: "rolemod", Name: "mod", IsModerator: true}).Error)
	for _, a := range [][2]string{{"ra1", "dbadmin"}, {"ra2", "dbsuspended"}} {
		require.NoError(t, db.Create(&model.RoleAssignment{ID: a[0], UserID: a[1], RoleID: "roleadmin"}).Error)
	}
	require.NoError(t, db.Create(&model.RoleAssignment{ID: "ra3", UserID: "dbmod", RoleID: "rolemod"}).Error)
	return f
}

func (f *dbFixture) code(t *testing.T) string {
	t.Helper()
	c, err := totp.GenerateCode(f.secret, time.Now())
	require.NoError(t, err)
	return c
}

func TestDBVerifier_Verify(t *testing.T) {
	f := newDBFixture(t)
	ctx := context.Background()

	u, err := f.v.Verify(ctx, "tokadmin00000001", "192.0.2.1", Reauth{Password: password, Token: f.code(t)})
	require.NoError(t, err)
	assert.Equal(t, "dbadmin", u.ID)

	cases := []struct {
		name  string
		token string
		want  Reason
	}{
		{"moderator", "tokmod0000000001", ReasonNotAdmin},
		{"suspended administrator", "toksusp000000001", ReasonNotAdmin},
		{"unknown or app token", "tokapp0000000001", ReasonNotNativeToken},
		{"no token", "", ReasonNoCredential},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.v.Verify(ctx, tc.token, "192.0.2.1", Reauth{Password: password, Token: "000000"})
			assert.Equal(t, tc.want, ReasonOf(err))
		})
	}
	_, err = f.v.Verify(ctx, "tokadmin00000001", "192.0.2.1", Reauth{Password: "wrong", Token: "000000"})
	assert.Equal(t, ReasonFailed, ReasonOf(err))
	// 照合のたびに開いた DB を、どの結果でも閉じる (「no token」は開かない)。
	assert.Equal(t, f.opened, f.closed)
	assert.Equal(t, 5, f.opened)
}

// TestDBVerifier_SeesRoleChangesAtOnce checks that each check reads the roles
// again: an administrator role assigned after a refused check counts at once.
func TestDBVerifier_SeesRoleChangesAtOnce(t *testing.T) {
	f := newDBFixture(t)
	ctx := context.Background()
	_, err := f.v.Verify(ctx, "tokmod0000000001", "192.0.2.1", Reauth{Password: password, Token: f.code(t)})
	require.Equal(t, ReasonNotAdmin, ReasonOf(err))
	require.NoError(t, f.db.Create(&model.RoleAssignment{ID: "ra4", UserID: "dbmod", RoleID: "roleadmin"}).Error)
	_, err = f.v.Verify(ctx, "tokmod0000000001", "192.0.2.1", Reauth{Password: password, Token: f.code(t)})
	require.NoError(t, err)
}

func TestDBVerifier_OpenFailure(t *testing.T) {
	v := &DBVerifier{
		Open:  func(context.Context) (*gorm.DB, func(), error) { return nil, nil, errors.New("down") },
		Guard: erringGuard{},
	}
	_, err := v.Verify(context.Background(), "tokadmin00000001", "", Reauth{Password: password, Token: "1"})
	assert.Equal(t, ReasonUnavailable, ReasonOf(err))
	_, err = (&DBVerifier{}).Verify(context.Background(), "tokadmin00000001", "", Reauth{})
	assert.Equal(t, ReasonUnavailable, ReasonOf(err))
}

// TestDBVerifier_RefusesMisshapenTokensWithoutOpening checks that a token
// that cannot be a native session token never opens the database.
func TestDBVerifier_RefusesMisshapenTokensWithoutOpening(t *testing.T) {
	opened := 0
	v := &DBVerifier{
		Open: func(context.Context) (*gorm.DB, func(), error) {
			opened++
			return nil, nil, errors.New("down")
		},
		Guard: erringGuard{},
	}
	for _, tok := range []string{"short", "tokadmin000000012", "an-app-token-that-is-much-longer-than-sixteen"} {
		_, err := v.Verify(context.Background(), tok, "", Reauth{Password: password, Token: "1"})
		assert.Equal(t, ReasonNotNativeToken, ReasonOf(err), tok)
		_, err = v.BeginPasskey(context.Background(), tok, "")
		assert.Equal(t, ReasonNotNativeToken, ReasonOf(err), tok)
	}
	_, err := v.BeginPasskey(context.Background(), "", "")
	assert.Equal(t, ReasonNoCredential, ReasonOf(err))
	assert.Zero(t, opened)
}

// TestDBVerifier_CapsConcurrentChecks checks that a check over MaxChecks is
// refused at once without opening the database, and that a slot comes back
// when a check ends, even a failed one.
func TestDBVerifier_CapsConcurrentChecks(t *testing.T) {
	entered := make(chan struct{})
	unblock := make(chan struct{})
	var opened atomic.Int32
	v := &DBVerifier{
		Open: func(context.Context) (*gorm.DB, func(), error) {
			if opened.Add(1) == 1 {
				close(entered)
				<-unblock
			}
			return nil, nil, errors.New("down")
		},
		Guard:     erringGuard{},
		MaxChecks: 1,
	}
	done := make(chan error)
	go func() {
		_, err := v.Verify(context.Background(), "tokadmin00000001", "", Reauth{})
		done <- err
	}()
	<-entered
	_, err := v.Verify(context.Background(), "tokadmin00000001", "", Reauth{})
	var se *Error
	require.ErrorAs(t, err, &se)
	assert.Equal(t, ReasonRateLimited, se.Reason)
	assert.Equal(t, 2*time.Second, se.RetryAfter)
	assert.EqualValues(t, 1, opened.Load(), "the refused check does not open the database")
	close(unblock)
	assert.Equal(t, ReasonUnavailable, ReasonOf(<-done))
	_, err = v.Verify(context.Background(), "tokadmin00000001", "", Reauth{})
	assert.Equal(t, ReasonUnavailable, ReasonOf(err), "the slot is back")
	assert.EqualValues(t, 2, opened.Load())
}

func TestDBVerifier_BeginPasskey(t *testing.T) {
	f := newDBFixture(t)
	ctx := context.Background()
	_, err := f.v.BeginPasskey(ctx, "tokadmin00000001", "")
	assert.Equal(t, ReasonPasskeyUnavailable, ReasonOf(err), "no Passkeys configured")
	_, err = f.v.BeginPasskey(ctx, "", "")
	assert.Equal(t, ReasonNoCredential, ReasonOf(err))
	_, err = f.v.BeginPasskey(ctx, "tokmod0000000001", "")
	assert.Equal(t, ReasonNotAdmin, ReasonOf(err))

	f.v.Passkeys = &fakePasskeys{}
	require.NoError(t, f.db.Create(&model.UserSecurityKey{ID: "AQID", UserID: "dbadmin", PublicKey: "pk", Name: "key"}).Error)
	assertion, err := f.v.BeginPasskey(ctx, "tokadmin00000001", "")
	require.NoError(t, err)
	assert.Equal(t, []byte("chal"), []byte(assertion.Response.Challenge))
	assert.Equal(t, f.opened, f.closed)
}

// TestDBVerifier_BackupCodeIsConsumedInTheDatabase checks that a backup code
// used through the DB-backed verifier is removed from user_profile.
func TestDBVerifier_BackupCodeIsConsumedInTheDatabase(t *testing.T) {
	f := newDBFixture(t)
	ctx := context.Background()
	require.NoError(t, f.db.Model(&model.UserProfile{}).Where(`"userId" = ?`, "dbadmin").
		Update("twoFactorBackupSecret", model.StringArray{"BACKUPCODE0001", "BACKUPCODE0002"}).Error)
	_, err := f.v.Verify(ctx, "tokadmin00000001", "192.0.2.1", Reauth{Password: password, Token: "BACKUPCODE0001"})
	require.NoError(t, err)
	var p model.UserProfile
	require.NoError(t, f.db.Where(`"userId" = ?`, "dbadmin").First(&p).Error)
	assert.Equal(t, model.StringArray{"BACKUPCODE0002"}, p.TwoFactorBackupSecret)
}
