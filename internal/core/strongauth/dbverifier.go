package strongauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-webauthn/webauthn/protocol"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/core/passwordguard"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// DBVerifier runs the same checks as Verifier against a database it opens
// for each check and closes afterwards. The backup service uses it, because
// it has no database pool of its own and must not keep a session on the
// database it is about to swap (#3463 (6c)).
//
// 照合のたびに role.Service とリポジトリを作り直すので、本体のキャッシュの
// ような古い値を持ち越さない。管理者の判定は本体と同じ role.Service で行う
// (条件付きロールなどを書き写すと食い違うため)。
type DBVerifier struct {
	// Open opens the database. The returned function closes it and is
	// called when the check ends.
	Open func(ctx context.Context) (*gorm.DB, func(), error)
	// IDGen is the ID generator of the server (role.Service needs one).
	IDGen id.Generator
	// Passkeys verifies passkey assertions. Nil accepts TOTP and backup
	// codes only.
	Passkeys Passkeys
	// Guard and Replay are shared with the main server through Redis.
	Guard  passwordguard.Guard
	Replay twofactor.ReplayGuard
}

// session is one check's view of the database.
type session struct {
	user     *model.User
	verifier *Verifier
	close    func()
}

func (v *DBVerifier) open(ctx context.Context, token, clientIP string) (*session, Subject, error) {
	if v.Open == nil || v.Guard == nil {
		return nil, Subject{}, unavailable(errors.New("strongauth: DBVerifier needs Open and Guard"))
	}
	db, closeDB, err := v.Open(ctx)
	if err != nil {
		return nil, Subject{}, unavailable(fmt.Errorf("open database: %w", err))
	}
	users := repository.NewUserRepository(db)
	// users.token (char(16)) だけを引く。app や MiAuth の token は access_token の行で
	// 解決されるので、ここでは見つからず、RequireSecure と同じく断る。
	u, err := users.FindByToken(token)
	if err != nil {
		closeDB()
		if errors.Is(err, repository.ErrNotFound) {
			return nil, Subject{}, refuse(ReasonNotNativeToken)
		}
		return nil, Subject{}, unavailable(fmt.Errorf("find user: %w", err))
	}
	if u.IsSuspended || u.IsDeleted {
		closeDB()
		return nil, Subject{}, refuse(ReasonNotAdmin)
	}
	roles := role.NewService(repository.NewRoleRepository(db), repository.NewRoleAssignmentRepository(db),
		repository.NewMetaRepository(db), v.IDGen)
	roles.SetUserRepo(users)
	deps := Deps{
		Roles:    roles,
		Profiles: profileStore{users},
		Guard:    v.Guard,
		Replay:   v.Replay,
	}
	if v.Passkeys != nil {
		deps.Keys = repository.NewUserSecurityKeyRepository(db)
		deps.Passkeys = v.Passkeys
	}
	inner, err := New(deps)
	if err != nil {
		closeDB()
		return nil, Subject{}, unavailable(err)
	}
	s := Subject{User: u, PresentedToken: token, ClientIP: clientIP}
	return &session{user: u, verifier: inner, close: closeDB}, s, nil
}

// Verify checks every condition for the user whose native session token is
// token, and returns that user. The database is closed before it returns.
func (v *DBVerifier) Verify(ctx context.Context, token, clientIP string, r Reauth) (*model.User, error) {
	if token == "" {
		return nil, refuse(ReasonNoCredential)
	}
	sess, subject, err := v.open(ctx, token, clientIP)
	if err != nil {
		return nil, err
	}
	defer sess.close()
	if err := sess.verifier.Verify(ctx, subject, r); err != nil {
		return nil, err
	}
	return sess.user, nil
}

// BeginPasskey starts a passkey challenge for the user of token.
func (v *DBVerifier) BeginPasskey(ctx context.Context, token, clientIP string) (*protocol.CredentialAssertion, error) {
	if token == "" {
		return nil, refuse(ReasonNoCredential)
	}
	sess, subject, err := v.open(ctx, token, clientIP)
	if err != nil {
		return nil, err
	}
	defer sess.close()
	return sess.verifier.BeginPasskey(ctx, subject)
}

// profileStore adapts the user repository to ProfileStore.
type profileStore struct{ users repository.UserRepository }

func (p profileStore) GetProfileErr(userID string) (*model.UserProfile, error) {
	return p.users.FindProfileByUserID(userID)
}

func (p profileStore) RemoveBackupCode(userID, code string) error {
	return p.users.RemoveBackupCode(userID, code)
}
