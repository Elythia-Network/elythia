package signup_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/misc"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/password"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func newTestService(t *testing.T) (*signup.Service, *testutil.MockUserRepository, *testutil.MockMetaRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	svc := signup.NewService(userRepo, metaRepo, idGen)
	return svc, userRepo, metaRepo
}

func TestSignup_Success(t *testing.T) {
	svc, userRepo, _ := newTestService(t)
	result, err := svc.Signup("testuser", "password123", false, signup.UsernamePolicyPublic)
	require.NoError(t, err)
	assert.Equal(t, "testuser", result.User.Username)
	assert.NotEmpty(t, result.Token)
	assert.Len(t, userRepo.Users, 1)
	assert.Len(t, userRepo.Profiles, 1)

	// パスワードがハッシュ化されている
	profile := userRepo.Profiles[result.User.ID]
	require.NotNil(t, profile.Password)
	assert.True(t, strings.HasPrefix(*profile.Password, "$2"))
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(*profile.Password), []byte("password123")))
	cost, err := bcrypt.Cost([]byte(*profile.Password))
	require.NoError(t, err)
	assert.Equal(t, password.Cost(), cost)
}

func TestSignup_InitialSetup_SetsRootUser(t *testing.T) {
	svc, _, metaRepo := newTestService(t)
	result, err := svc.Signup("admin", "pass", true, signup.UsernamePolicyPublic)
	require.NoError(t, err)

	// rootUserId が設定される
	assert.NotNil(t, metaRepo.Meta.RootUserID)
	assert.Equal(t, result.User.ID, *metaRepo.Meta.RootUserID)
}

func TestSignup_NotInitialSetup_DoesNotSetRootUser(t *testing.T) {
	svc, _, metaRepo := newTestService(t)
	_, err := svc.Signup("user1", "pass", false, signup.UsernamePolicyPublic)
	require.NoError(t, err)
	assert.Nil(t, metaRepo.Meta.RootUserID)
}

func TestSignup_DuplicateUsername(t *testing.T) {
	svc, userRepo, _ := newTestService(t)
	userRepo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "taken",
		UsernameLower: "taken",
	}

	_, err := svc.Signup("taken", "pass", false, signup.UsernamePolicyPublic)
	assert.ErrorIs(t, err, signup.ErrUsernameAlreadyExists)
}

func TestSignup_EmptyUsername(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, err := svc.Signup("", "pass", false, signup.UsernamePolicyPublic)
	assert.ErrorIs(t, err, signup.ErrInvalidUsername)
}

func TestSignup_TooLongUsername(t *testing.T) {
	svc, _, _ := newTestService(t)
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'a'
	}
	_, err := svc.Signup(string(long), "pass", false, signup.UsernamePolicyPublic)
	assert.ErrorIs(t, err, signup.ErrInvalidUsername)
}

// upstream Misskey TS の `localUsernameSchema` は `^\w{1,20}$` (#800)。
// 境界 (= 20 char OK / 21 char NG) を明示する。リテラルではなく
// strings.Repeat で長さを構築することで、境界値が一目で判別できるよう
// にしている。
func TestSignup_UsernameLengthBoundary(t *testing.T) {
	cases := []struct {
		desc    string
		length  int
		wantErr error
	}{
		{"max_20_chars_accepted", 20, nil},
		{"over_21_chars_rejected", 21, signup.ErrInvalidUsername},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			svc, _, _ := newTestService(t)
			username := strings.Repeat("a", tc.length)
			_, err := svc.Signup(username, "pass", false, signup.UsernamePolicyPublic)
			if tc.wantErr == nil {
				require.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, tc.wantErr)
			}
		})
	}
}

// `\w` 外の character (= [a-zA-Z0-9_] 以外) はすべて reject される。
func TestSignup_UsernameIllegalCharsRejected(t *testing.T) {
	cases := []struct {
		desc     string
		username string
	}{
		{"hyphen", "alice-bob"},
		{"dot", "alice.bob"},
		{"at_sign", "alice@bob"},
		{"middle_space", "alice bob"},
		{"non_ascii_japanese", "アリス"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			svc, _, _ := newTestService(t)
			_, err := svc.Signup(tc.username, "pass", false, signup.UsernamePolicyPublic)
			assert.ErrorIs(t, err, signup.ErrInvalidUsername)
		})
	}
}

func TestSignup_PreservedUsernameRejected(t *testing.T) {
	svc, _, metaRepo := newTestService(t)
	// meta.preservedUsernames に "admin" が入っている想定。
	metaRepo.Meta.PreservedUsernames = []string{"admin", "root", "System"}

	_, err := svc.Signup("admin", "pass", false, signup.UsernamePolicyPublic)
	assert.ErrorIs(t, err, signup.ErrUsernameReserved)

	// case-insensitive (slot "System" を大文字で登録、小文字で試行)
	_, err = svc.Signup("SYSTEM", "pass", false, signup.UsernamePolicyPublic)
	assert.ErrorIs(t, err, signup.ErrUsernameReserved)
}

func TestSignup_PreservedUsernameBypassedOnInitialSetup(t *testing.T) {
	// 初回セットアップ時は root / admin が予約ワードでも作成可能にする。
	// そうでないと本家デフォルトで admin / root が永久に作成できなくなる。
	svc, _, metaRepo := newTestService(t)
	metaRepo.Meta.PreservedUsernames = []string{"admin"}

	result, err := svc.Signup("admin", "pass", true, signup.UsernamePolicyPublic)
	require.NoError(t, err)
	assert.NotNil(t, result)
}

func TestSignup_PreservedUsernameAllowsOthers(t *testing.T) {
	svc, _, metaRepo := newTestService(t)
	metaRepo.Meta.PreservedUsernames = []string{"admin"}

	result, err := svc.Signup("alice", "pass", false, signup.UsernamePolicyPublic)
	require.NoError(t, err)
	assert.Equal(t, "alice", result.User.Username)
}

// --- Failing repo tests ---

type failingCreateUserRepo struct {
	*testutil.MockUserRepository
}

func (f *failingCreateUserRepo) Create(_ *model.User) error { return assert.AnError }

type failingCreateProfileRepo struct {
	*testutil.MockUserRepository
}

func (f *failingCreateProfileRepo) CreateProfile(_ *model.UserProfile) error { return assert.AnError }

func TestSignup_UserCreateError(t *testing.T) {
	repo := &failingCreateUserRepo{testutil.NewMockUserRepository()}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	svc := signup.NewService(repo, metaRepo, idGen)
	_, err := svc.Signup("user1", "pass", false, signup.UsernamePolicyPublic)
	assert.Error(t, err)
}

func TestSignup_ProfileCreateError(t *testing.T) {
	repo := &failingCreateProfileRepo{testutil.NewMockUserRepository()}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	svc := signup.NewService(repo, metaRepo, idGen)
	_, err := svc.Signup("user1", "pass", false, signup.UsernamePolicyPublic)
	assert.Error(t, err)
}

// 72 byte password は bcrypt の上限ぴったりで通る。73 byte は ErrPasswordTooLong を返す (#1075)。
func TestSignup_PasswordLengthBoundary(t *testing.T) {
	cases := []struct {
		desc   string
		length int
		want   error
	}{
		{"max_72_bytes_accepted", 72, nil},
		{"over_73_bytes_rejected", 73, signup.ErrPasswordTooLong},
		{"way_over_rejected", 200, signup.ErrPasswordTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			svc, _, _ := newTestService(t)
			pw := strings.Repeat("a", tc.length)
			_, err := svc.Signup("user1", pw, false, signup.UsernamePolicyPublic)
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, tc.want)
			}
		})
	}
}

// **長さ 16 は drop-in の制約。** Misskey TS の isNativeUserToken は長さだけで
// native token とアプリのアクセストークンを判別するので、伸ばすと TS に
// 引き渡したときアプリのトークンとして扱われる。
//
// 中身は英数字 62 文字集合であることも固定する。16 進に戻すと見た目は同じ
// 16 文字でも 64 bit しかなく、upstream (約 95 bit) より弱くなる。
func TestSignup_TokenShape(t *testing.T) {
	svc, _, _ := newTestService(t)

	seen := map[string]bool{}
	sawNonHex := false
	for range 40 {
		result, err := svc.Signup("user"+strconv.Itoa(len(seen)), "pass", false, signup.UsernamePolicyPublic)
		require.NoError(t, err)
		assert.Len(t, result.Token, misc.NativeTokenLength)
		assert.Regexp(t, `^[0-9a-zA-Z]{16}$`, result.Token)
		if strings.ContainsAny(result.Token, "ghijklmnopqrstuvwxyzGHIJKLMNOPQRSTUVWXYZ") {
			sawNonHex = true
		}
		assert.False(t, seen[result.Token], "同じ token が 2 度出た")
		seen[result.Token] = true
	}
	// 16 進に戻ると 0-9a-f しか出ない。40 本引いて一度も 16 進の外が出ないのは
	// 確率的にありえない (1 本あたり (16/62)^16 未満)。
	assert.True(t, sawNonHex, "16 進の範囲しか出ていない = 文字集合が狭まっている")
}

func TestSignup_WithKeypairRepo(t *testing.T) {
	svc, _, _ := newTestService(t)
	keypairRepo := testutil.NewMockUserKeypairRepository()
	svc.SetKeypairRepo(keypairRepo)

	result, err := svc.Signup("alice", "pass", false, signup.UsernamePolicyPublic)
	require.NoError(t, err)

	// Keypair created for the new user.
	k, ok := keypairRepo.Keypairs[result.User.ID]
	require.True(t, ok)
	assert.NotEmpty(t, k.PublicKey)
	assert.NotEmpty(t, k.PrivateKey)
}

type failingCreateKeypairRepo struct{}

func (f *failingCreateKeypairRepo) Create(_ *model.UserKeypair) error         { return assert.AnError }
func (f *failingCreateKeypairRepo) NormalizePrivateKeysToPKCS8() (int, error) { return 0, nil }
func (f *failingCreateKeypairRepo) FindByUserID(_ string) (*model.UserKeypair, error) {
	return nil, assert.AnError
}

func TestSignup_KeypairCreateError(t *testing.T) {
	svc, _, _ := newTestService(t)
	svc.SetKeypairRepo(&failingCreateKeypairRepo{})

	_, err := svc.Signup("alice", "pass", false, signup.UsernamePolicyPublic)
	assert.Error(t, err)
}

func TestSignup_WithKeypairExtraRepo(t *testing.T) {
	svc, _, _ := newTestService(t)
	keypairRepo := testutil.NewMockUserKeypairRepository()
	keypairExtraRepo := testutil.NewMockUserKeypairExtraRepository()
	svc.SetKeypairRepo(keypairRepo)
	svc.SetKeypairExtraRepo(keypairExtraRepo)

	result, err := svc.Signup("alice", "pass", false, signup.UsernamePolicyPublic)
	require.NoError(t, err)

	// Ed25519 鍵が併発行されている
	kx, ok := keypairExtraRepo.Get(result.User.ID)
	require.True(t, ok)
	assert.NotEmpty(t, kx.Ed25519PublicKey)
	assert.NotEmpty(t, kx.Ed25519PrivateKey)
}

// failingUpsertKeypairExtraRepo は Upsert だけ意図的に失敗させる test double。
// FindByUserID / Delete は呼ばれない想定だが、将来 helper が existence check で
// 呼んでも誤動作しないよう ErrNotFound / 成功を返す defense-in-depth な挙動にする。
type failingUpsertKeypairExtraRepo struct{}

func (f *failingUpsertKeypairExtraRepo) Upsert(_ *model.UserKeypairExtra) error {
	return assert.AnError
}
func (f *failingUpsertKeypairExtraRepo) InsertIfAbsent(_ *model.UserKeypairExtra) (bool, error) {
	return false, assert.AnError
}
func (f *failingUpsertKeypairExtraRepo) FindByUserID(_ string) (*model.UserKeypairExtra, error) {
	return nil, testutil.ErrNotFound
}
func (f *failingUpsertKeypairExtraRepo) Delete(_ string) error { return nil }

func TestSignup_KeypairExtraUpsertError(t *testing.T) {
	svc, _, _ := newTestService(t)
	svc.SetKeypairRepo(testutil.NewMockUserKeypairRepository())
	svc.SetKeypairExtraRepo(&failingUpsertKeypairExtraRepo{})

	_, err := svc.Signup("alice", "pass", false, signup.UsernamePolicyPublic)
	assert.Error(t, err)
}

// --- Pending signup (#595) ---

// CreatePending 経路でも 73 byte 以上 password は ErrPasswordTooLong (#1075)。
func TestCreatePending_PasswordTooLong(t *testing.T) {
	svc, _, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)

	pw := strings.Repeat("a", 73)
	_, err := svc.CreatePending("user1", "user1@example.com", pw, nil)
	assert.ErrorIs(t, err, signup.ErrPasswordTooLong)
	// pending row も作成されない (early return)
	assert.Empty(t, pendingRepo.Rows)
}

func TestCreatePending_Success(t *testing.T) {
	svc, _, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)

	row, err := svc.CreatePending("alice", "alice@example.com", "secret123", nil)
	require.NoError(t, err)
	assert.Equal(t, "alice", row.Username)
	assert.Equal(t, "alice@example.com", row.Email)
	assert.NotEmpty(t, row.Code)
	// Password は bcrypt hash 済 (再 hash 防止)。
	assert.True(t, strings.HasPrefix(row.Password, "$2"))
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(row.Password), []byte("secret123")))
	cost, err := bcrypt.Cost([]byte(row.Password))
	require.NoError(t, err)
	assert.Equal(t, password.Cost(), cost)
	assert.Len(t, pendingRepo.Rows, 1)
}

func TestCreatePending_DuplicateUsername(t *testing.T) {
	svc, userRepo, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)
	require.NoError(t, userRepo.Create(&model.User{ID: "u1", Username: "alice", UsernameLower: "alice"}))

	_, err := svc.CreatePending("Alice", "a@example.com", "pw", nil)
	assert.ErrorIs(t, err, signup.ErrUsernameAlreadyExists)
}

func TestCreatePending_ReservedUsername(t *testing.T) {
	svc, _, metaRepo := newTestService(t)
	metaRepo.Meta.PreservedUsernames = []string{"admin"}
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)

	_, err := svc.CreatePending("admin", "a@example.com", "pw", nil)
	assert.ErrorIs(t, err, signup.ErrUsernameReserved)
}

func TestCreatePending_InvalidUsername(t *testing.T) {
	svc, _, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)

	_, err := svc.CreatePending("", "a@example.com", "pw", nil)
	assert.ErrorIs(t, err, signup.ErrInvalidUsername)
}

func TestPromotePending_Success(t *testing.T) {
	svc, userRepo, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)

	row, err := svc.CreatePending("bob", "bob@example.com", "pw123", nil)
	require.NoError(t, err)

	result, err := svc.PromotePending(row.Code)
	require.NoError(t, err)
	assert.Equal(t, "bob", result.User.Username)
	assert.NotEmpty(t, result.Token)
	// pending row が消費されている
	assert.Empty(t, pendingRepo.Rows)
	// profile に email + emailVerified=true がセット
	prof := userRepo.Profiles[result.User.ID]
	require.NotNil(t, prof)
	require.NotNil(t, prof.Email)
	assert.Equal(t, "bob@example.com", *prof.Email)
	assert.True(t, prof.EmailVerified)
	// Password ハッシュは Pending と完全一致 (再 hash されていない)
	require.NotNil(t, prof.Password)
	assert.Equal(t, row.Password, *prof.Password)
}

// 承認制が有効なら、申請に紐付かない pending は昇格させない (#2804)。
//
// **`PendingSignupTTL` は 30 分。** 承認制へ切り替える直前 30 分に発行された確認
// メールは、申請 ID を持たないので #2576 の確定処理を通らず、ゲートが無いと承認を
// 経ずにアカウントになる。
func TestPromotePending_ApprovalRequiredRejectsUnappliedPending(t *testing.T) {
	svc, userRepo, metaRepo := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)

	// 切り替え前に発行された確認メールを再現する (承認制 OFF のうちに作る)。
	row, err := svc.CreatePending("late", "late@example.com", "pw123", nil)
	require.NoError(t, err)

	metaRepo.Meta.ApprovalRequiredForSignup = true

	_, err = svc.PromotePending(row.Code)
	require.ErrorIs(t, err, signup.ErrApplicationNotApproved)
	assert.Empty(t, userRepo.Users, "アカウントを作らない")
	// pending は残す。承認制を戻した運用者が状況を追えるようにする。
	assert.Len(t, pendingRepo.Rows, 1)
}

// 承認制が有効でも、申請に紐付く pending は従来どおり通る (#2804)。
func TestPromotePending_ApprovalRequiredAllowsAppliedPending(t *testing.T) {
	svc, userRepo, metaRepo := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)
	metaRepo.Meta.ApprovalRequiredForSignup = true

	appID := "app1"
	row, err := svc.CreatePendingForApplication("applied", "a@example.com", "pw123", nil, &appID)
	require.NoError(t, err)

	result, err := svc.PromotePending(row.Code)
	require.NoError(t, err)
	assert.Equal(t, "applied", result.User.Username)
	assert.Len(t, userRepo.Users, 1)
}

// meta が読めないときは通さない (#2804)。
//
// **DB 障害を承認の迂回路にしない。** ただし ErrApplicationNotApproved にも
// 丸めない — 障害をドメインの答えに化けさせると、運用側からは「承認されていない」
// としか見えなくなる (#2799 と同じ)。
func TestPromotePending_ApprovalGateFailsClosedWhenMetaUnavailable(t *testing.T) {
	svc, userRepo, metaRepo := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)

	row, err := svc.CreatePending("nometa", "n@example.com", "pw123", nil)
	require.NoError(t, err)

	metaRepo.Meta = nil // Fetch が error を返す

	_, err = svc.PromotePending(row.Code)
	require.Error(t, err)
	assert.NotErrorIs(t, err, signup.ErrApplicationNotApproved)
	assert.Empty(t, userRepo.Users, "アカウントを作らない")
}

func TestPromotePending_NotFound(t *testing.T) {
	svc, _, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)

	_, err := svc.PromotePending("ghost")
	assert.ErrorIs(t, err, signup.ErrPendingNotFound)
}

func TestPromotePending_UsernameClash(t *testing.T) {
	svc, userRepo, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)

	row, err := svc.CreatePending("clash", "c@example.com", "pw", nil)
	require.NoError(t, err)

	// pending 確定前に同名 user が登録されたケースを再現
	require.NoError(t, userRepo.Create(&model.User{ID: "u_clash", Username: "Clash", UsernameLower: "clash"}))

	_, err = svc.PromotePending(row.Code)
	assert.ErrorIs(t, err, signup.ErrUsernameAlreadyExists)
}

func TestPromotePending_Expired(t *testing.T) {
	// ParseTime が成功する形で expired を再現したいので、Create 後に row.ID を
	// `PendingSignupTTL` より前の ULID に書き換える。
	svc, _, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)

	row, err := svc.CreatePending("expuser", "exp@example.com", "pw", nil)
	require.NoError(t, err)

	// ULID を TTL より前の時刻で再生成 (idGen は newTestService で aidx 固定)。
	idGen, _ := id.NewGenerator("aidx")
	old := idGen.Generate(time.Now().Add(-signup.PendingSignupTTL - time.Minute))
	delete(pendingRepo.Rows, row.ID)
	row.ID = old
	pendingRepo.Rows[old] = row

	_, err = svc.PromotePending(row.Code)
	assert.ErrorIs(t, err, signup.ErrPendingExpired)
}

func TestPromotePending_WithKeypair(t *testing.T) {
	svc, _, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)
	keypairRepo := testutil.NewMockUserKeypairRepository()
	svc.SetKeypairRepo(keypairRepo)

	row, err := svc.CreatePending("kpuser", "kp@example.com", "pw", nil)
	require.NoError(t, err)

	result, err := svc.PromotePending(row.Code)
	require.NoError(t, err)
	_, ok := keypairRepo.Keypairs[result.User.ID]
	assert.True(t, ok, "keypair must be created during PromotePending")
}

func TestPromotePending_WithKeypairExtra(t *testing.T) {
	svc, _, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)
	svc.SetKeypairRepo(testutil.NewMockUserKeypairRepository())
	keypairExtraRepo := testutil.NewMockUserKeypairExtraRepository()
	svc.SetKeypairExtraRepo(keypairExtraRepo)

	row, err := svc.CreatePending("kpxuser", "kpx@example.com", "pw", nil)
	require.NoError(t, err)

	result, err := svc.PromotePending(row.Code)
	require.NoError(t, err)
	kx, ok := keypairExtraRepo.Get(result.User.ID)
	require.True(t, ok, "ed25519 keypair must be created during PromotePending")
	assert.Contains(t, kx.Ed25519PublicKey, "PUBLIC KEY")
}

func TestPromotePending_KeypairExtraUpsertError(t *testing.T) {
	svc, _, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)
	svc.SetKeypairRepo(testutil.NewMockUserKeypairRepository())
	svc.SetKeypairExtraRepo(&failingUpsertKeypairExtraRepo{})

	row, err := svc.CreatePending("kpxerr", "kpxerr@example.com", "pw", nil)
	require.NoError(t, err)

	_, err = svc.PromotePending(row.Code)
	assert.Error(t, err)
}

func TestPromotePending_WebhookHookFires(t *testing.T) {
	svc, _, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)
	hook := &recordingHook{}
	svc.SetWebhookHook(hook)

	row, err := svc.CreatePending("hookuser", "hook@example.com", "pw", nil)
	require.NoError(t, err)

	_, err = svc.PromotePending(row.Code)
	require.NoError(t, err)
	assert.Equal(t, 1, hook.calls, "OnUserCreated must fire after promotion")
}

// recordingHook は WebhookHook の呼び出し回数を数える。
type recordingHook struct{ calls int }

func (r *recordingHook) OnUserCreated(_ *model.User) { r.calls++ }

// invitationTicketID を渡すと pending row に保存され、PromotePending
// 完了時に SignupResult.InvitationTicketID として伝搬される (#600 item 5)。
func TestCreatePending_StoresInvitationTicketID(t *testing.T) {
	svc, _, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)

	tid := "ticket_abc"
	row, err := svc.CreatePending("inv", "inv@example.com", "pw", &tid)
	require.NoError(t, err)
	require.NotNil(t, row.InvitationTicketID)
	assert.Equal(t, "ticket_abc", *row.InvitationTicketID)
}

func TestPromotePending_PropagatesInvitationTicketID(t *testing.T) {
	svc, _, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)

	tid := "ticket_xyz"
	row, err := svc.CreatePending("invuser", "invu@example.com", "pw", &tid)
	require.NoError(t, err)

	result, err := svc.PromotePending(row.Code)
	require.NoError(t, err)
	require.NotNil(t, result.InvitationTicketID)
	assert.Equal(t, "ticket_xyz", *result.InvitationTicketID)
}

func TestPromotePending_NoTicketIDReturnsNil(t *testing.T) {
	svc, _, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)

	row, err := svc.CreatePending("noinv", "noinv@example.com", "pw", nil)
	require.NoError(t, err)

	result, err := svc.PromotePending(row.Code)
	require.NoError(t, err)
	assert.Nil(t, result.InvitationTicketID, "non-invitation pending では nil で伝搬する")
}

// #2080: used_usernames (削除済 account の username) は Signup で ErrUsernameUsed を
// 返す (existing → used → preserved の順、upstream SignupService:87 相当)。
func TestSignup_UsedUsername(t *testing.T) {
	svc, _, _ := newTestService(t)
	usedRepo := testutil.NewMockUsedUsernameRepository()
	usedRepo.Usernames["taken"] = true
	svc.SetUsedUsernameRepo(usedRepo)

	_, err := svc.Signup("taken", "password123", false, signup.UsernamePolicyPublic)
	assert.ErrorIs(t, err, signup.ErrUsernameUsed)

	// 大文字違いも lowercase 照合で弾く。
	_, err = svc.Signup("TAKEN", "password123", false, signup.UsernamePolicyPublic)
	assert.ErrorIs(t, err, signup.ErrUsernameUsed)

	// repo 未配線なら skip (後方互換) — 別 svc で確認。
	svc2, _, _ := newTestService(t)
	_, err = svc2.Signup("taken", "password123", false, signup.UsernamePolicyPublic)
	assert.NoError(t, err, "repo 未配線時は used_usernames を見ない")
}

// #2106 N23: Signup は used_username に lowercase username を記録する
// (account 削除後の同名 username 再取得を恒久的に防ぐ guard のため)。
func TestSignup_RecordsUsedUsername(t *testing.T) {
	svc, _, _ := newTestService(t)
	usedRepo := testutil.NewMockUsedUsernameRepository()
	svc.SetUsedUsernameRepo(usedRepo)

	_, err := svc.Signup("NewUser", "password123", false, signup.UsernamePolicyPublic)
	require.NoError(t, err)

	exists, err := usedRepo.Exists("newuser")
	require.NoError(t, err)
	assert.True(t, exists, "signup は used_username に lowercase username を記録する")
}

// errUsedUsernameRepo always fails Create to exercise the best-effort error path.
type errUsedUsernameRepo struct{}

func (errUsedUsernameRepo) Create(string) error         { return errors.New("used_username create failed") }
func (errUsedUsernameRepo) Exists(string) (bool, error) { return false, nil }

// #2106 N23: used_username 記録失敗は best-effort で握り (signup は成功)。
func TestSignup_UsedUsernameCreateErrorIsBestEffort(t *testing.T) {
	svc, _, _ := newTestService(t)
	svc.SetUsedUsernameRepo(errUsedUsernameRepo{})
	result, err := svc.Signup("besteffort", "password123", false, signup.UsernamePolicyPublic)
	require.NoError(t, err, "used_username 記録失敗でも signup は成功する")
	require.NotNil(t, result)
}

// #2106 N23: non-tx (mock) promote 経路でも used_username に記録する。
func TestPromotePending_NoTxRecordsUsedUsername(t *testing.T) {
	svc, _, _ := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)
	usedRepo := testutil.NewMockUsedUsernameRepository()
	svc.SetUsedUsernameRepo(usedRepo)

	row, err := svc.CreatePending("PendUser", "pend@example.com", "password123", nil)
	require.NoError(t, err)
	_, err = svc.PromotePending(row.Code)
	require.NoError(t, err)

	exists, err := usedRepo.Exists("penduser")
	require.NoError(t, err)
	assert.True(t, exists, "non-tx promote も used_username に記録する")
}

// #2106 N20: meta.prohibitedWordsForNameOfUser に該当する username を signup で弾く。
func TestSignup_ProhibitedUsername(t *testing.T) {
	svc, _, metaRepo := newTestService(t)
	metaRepo.Meta.ProhibitedWordsForNameOfUser = []string{"badname"}
	_, err := svc.Signup("badname123", "pass", false, signup.UsernamePolicyPublic)
	assert.ErrorIs(t, err, signup.ErrUsernameUsed)
}

// #2106 N20: 初回セットアップ (isInitialSetup=true / rootUserId==null) では prohibited
// チェックを skip する (upstream は rootUserId!=null のときのみ評価)。
func TestSignup_ProhibitedUsernameAllowedOnInitialSetup(t *testing.T) {
	svc, _, metaRepo := newTestService(t)
	metaRepo.Meta.ProhibitedWordsForNameOfUser = []string{"badname"}
	result, err := svc.Signup("badname123", "pass", true, signup.UsernamePolicyPublic)
	require.NoError(t, err)
	require.NotNil(t, result)
}

// #2106 N20: CreatePending (email 確認経路) でも prohibited username を弾く。
func TestCreatePending_ProhibitedUsername(t *testing.T) {
	svc, _, metaRepo := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)
	metaRepo.Meta.ProhibitedWordsForNameOfUser = []string{"badname"}
	_, err := svc.CreatePending("badname123", "x@example.com", "pass", nil)
	assert.ErrorIs(t, err, signup.ErrUsernameUsed)
}

// **`PendingSignupTTL` は upstream と同じ 30 分 (#3037)。**
//
// `SignupApiService.ts:254` が `idService.parse(pendingUser.id).date + 30 分` を
// 過ぎた pending を弾く。mk-go は 24h (48 倍) で、しかも doc コメントが
// 「Misskey TS 実装では明示的な TTL は無い」と**事実と逆のこと**を書いて
// いたので、広げている自覚がどこにも残っていなかった。
func TestPendingSignupTTL_MatchesUpstream(t *testing.T) {
	assert.Equal(t, 30*time.Minute, signup.PendingSignupTTL)
}

// 「受け付けない」の間は、承認済みの申請に紐付く確認待ちも止める (#3186)。
//
// **申請に紐付く pending を使うのが要点。** 承認制のゲート (checkApprovalGate) は
// 申請 ID を持つ pending を素通しするので、紐付かない pending で試すと承認制の
// ゲートが代わりに止めてしまい、こちらのゲートを外しても落ちない。
func TestPromotePending_RegistrationClosedRejectsAppliedPending(t *testing.T) {
	svc, userRepo, metaRepo := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)

	appID := "app1"
	row, err := svc.CreatePendingForApplication("applied", "a@example.com", "pw123", nil, &appID)
	require.NoError(t, err)

	metaRepo.Meta.RegistrationClosed = true
	_, err = svc.PromotePending(row.Code)
	require.ErrorIs(t, err, signup.ErrRegistrationClosed)
	assert.Empty(t, userRepo.Users, "アカウントを作らない")
	assert.Len(t, pendingRepo.Rows, 1, "解除後に完了できるよう行は残す")
}

// meta が読めないときは通さない。承認制のゲートを素通りする (申請に紐付く) pending でも。
func TestPromotePending_RegistrationGateFailsClosedWhenMetaUnavailable(t *testing.T) {
	svc, userRepo, metaRepo := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)

	appID := "app1"
	row, err := svc.CreatePendingForApplication("applied", "a@example.com", "pw123", nil, &appID)
	require.NoError(t, err)

	metaRepo.Meta = nil // Fetch が error を返す
	_, err = svc.PromotePending(row.Code)
	require.Error(t, err)
	assert.NotErrorIs(t, err, signup.ErrRegistrationClosed, "障害を「閉じている」に化けさせない")
	assert.Empty(t, userRepo.Users, "アカウントを作らない")
}
