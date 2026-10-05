package systemaccount_test

import (
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/systemaccount"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// saMock wraps testutil.MockSystemAccountRepository to translate the
// testutil-level ErrNotFound sentinel into the gorm.ErrRecordNotFound that
// the production gorm repository would return. Service 側が gorm の
// sentinel で分岐するため必須のアダプタ。
type saMock struct {
	*testutil.MockSystemAccountRepository
}

func newSAMock() *saMock {
	return &saMock{MockSystemAccountRepository: testutil.NewMockSystemAccountRepository()}
}

func (s *saMock) FindByType(typ string) (*model.SystemAccount, error) {
	sa, err := s.MockSystemAccountRepository.FindByType(typ)
	if errors.Is(err, testutil.ErrNotFound) {
		return nil, gorm.ErrRecordNotFound
	}
	return sa, err
}

// saFailCreate は Create だけ常にエラーを返す SystemAccountRepository モック。
type saFailCreate struct {
	*saMock
	err error
}

func (f *saFailCreate) Create(sa *model.SystemAccount) error { return f.err }

// keypairFailCreate は Create が常にエラーを返す UserKeypairRepository モック。
type keypairFailCreate struct {
	*testutil.MockUserKeypairRepository
	err error
}

func (k *keypairFailCreate) Create(kp *model.UserKeypair) error { return k.err }

// userFailProfile は CreateProfile だけエラーを返す。
type userFailProfile struct {
	*testutil.MockUserRepository
	err error
}

func (u *userFailProfile) CreateProfile(p *model.UserProfile) error { return u.err }

func newService(t *testing.T) (*systemaccount.Service, *testutil.MockUserRepository, *testutil.MockUserKeypairRepository, *saMock) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	keypairRepo := testutil.NewMockUserKeypairRepository()
	saRepo := newSAMock()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := systemaccount.NewService(userRepo, keypairRepo, saRepo, idGen)
	svc.SetClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })
	return svc, userRepo, keypairRepo, saRepo
}

func TestFetch_CreatesRowsOnFirstCall(t *testing.T) {
	svc, userRepo, keypairRepo, saRepo := newService(t)

	user, err := svc.Fetch("relay")
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, "relay.actor", user.Username)
	assert.True(t, user.IsBot)
	assert.False(t, user.IsExplorable)
	assert.Nil(t, user.Host) // 必ずローカル

	// 各 repo に行が作られていることを確認
	require.Len(t, userRepo.Users, 1)
	require.Len(t, keypairRepo.Keypairs, 1)
	require.Len(t, saRepo.Accounts, 1)
	kp := keypairRepo.Keypairs[user.ID]
	require.NotNil(t, kp)
	assert.Contains(t, kp.PrivateKey, "PRIVATE KEY")
	assert.Contains(t, kp.PublicKey, "PUBLIC KEY")
}

func TestFetch_ReturnsExistingOnSecondCall(t *testing.T) {
	svc, userRepo, _, _ := newService(t)

	user1, err := svc.Fetch("relay")
	require.NoError(t, err)
	user2, err := svc.Fetch("relay")
	require.NoError(t, err)
	assert.Equal(t, user1.ID, user2.ID)
	// user はひとつだけ
	assert.Len(t, userRepo.Users, 1)
}

func TestFetch_EmptyKindRejected(t *testing.T) {
	svc, _, _, _ := newService(t)
	_, err := svc.Fetch("")
	assert.Error(t, err)
}

func TestFetch_ErrorBubblesFromSystemAccountCreate(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	keypairRepo := testutil.NewMockUserKeypairRepository()
	saRepo := &saFailCreate{
		saMock: newSAMock(),
		err:    errors.New("db down"),
	}
	idGen, _ := id.NewGenerator("aidx")
	svc := systemaccount.NewService(userRepo, keypairRepo, saRepo, idGen)

	_, err := svc.Fetch("relay")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "db down")
}

func TestFetch_ErrorBubblesFromKeypairCreate(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	keypairRepo := &keypairFailCreate{
		MockUserKeypairRepository: testutil.NewMockUserKeypairRepository(),
		err:                       errors.New("keypair db down"),
	}
	saRepo := newSAMock()
	idGen, _ := id.NewGenerator("aidx")
	svc := systemaccount.NewService(userRepo, keypairRepo, saRepo, idGen)

	_, err := svc.Fetch("relay")
	assert.Error(t, err)
}

func TestFetch_ErrorBubblesFromProfileCreate(t *testing.T) {
	userRepo := &userFailProfile{
		MockUserRepository: testutil.NewMockUserRepository(),
		err:                errors.New("profile db down"),
	}
	keypairRepo := testutil.NewMockUserKeypairRepository()
	saRepo := newSAMock()
	idGen, _ := id.NewGenerator("aidx")
	svc := systemaccount.NewService(userRepo, keypairRepo, saRepo, idGen)

	_, err := svc.Fetch("relay")
	assert.Error(t, err)
}

// saFailLookup は FindByType で gorm.ErrRecordNotFound 以外のエラーを返して
// その分岐がきちんと上位に伝わることを確認する。
type saFailLookup struct {
	*saMock
	err error
}

func (f *saFailLookup) FindByType(typ string) (*model.SystemAccount, error) { return nil, f.err }

func TestFetch_ErrorBubblesFromUnexpectedLookup(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	keypairRepo := testutil.NewMockUserKeypairRepository()
	saRepo := &saFailLookup{
		saMock: newSAMock(),
		err:    errors.New("other db error"),
	}
	idGen, _ := id.NewGenerator("aidx")
	svc := systemaccount.NewService(userRepo, keypairRepo, saRepo, idGen)

	_, err := svc.Fetch("relay")
	require.Error(t, err)
	assert.NotErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestSetClock_NilIgnored(t *testing.T) {
	svc, _, _, _ := newService(t)
	// nil を渡しても panic しない
	svc.SetClock(nil)
	_, err := svc.Fetch("relay")
	require.NoError(t, err)
}

// Previous Fetch が user 行は作れたが system_account 作成に失敗した状態を
// 模し、次の Fetch で orphan を検出して残りの行を埋めて復旧できることを確認。
// Devin review #171 対応。
func TestFetch_RecoversFromOrphanUser(t *testing.T) {
	svc, userRepo, keypairRepo, saRepo := newService(t)

	// 1 回目: system_account の Create を失敗させる
	originalRepo := saRepo.MockSystemAccountRepository
	failing := &saFailCreate{saMock: saRepo, err: errors.New("tx rolled back")}
	svc2 := systemaccount.NewService(userRepo, keypairRepo, failing, idGenFor(t))
	svc2.SetClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })
	_, err := svc2.Fetch("relay")
	require.Error(t, err)

	// user 行だけ残っているが system_account は無い
	require.Len(t, userRepo.Users, 1)
	require.Empty(t, originalRepo.Accounts)

	// 2 回目 (failing ではない普通の saRepo で) → orphan 検出 → recovery 成功
	user, err := svc.Fetch("relay")
	require.NoError(t, err)
	assert.Equal(t, "relay.actor", user.Username)
	// 新しい user は作られず既存を再利用
	assert.Len(t, userRepo.Users, 1)
	assert.Len(t, originalRepo.Accounts, 1)
}

// idGenFor は tests で idGen が必要な場合の小さなヘルパ。
func idGenFor(t *testing.T) id.Generator {
	t.Helper()
	g, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return g
}

func TestFetch_GeneratesEd25519KeypairWhenWired(t *testing.T) {
	svc, _, _, _ := newService(t)
	extra := testutil.NewMockUserKeypairExtraRepository()
	svc.SetKeypairExtraRepo(extra)

	user, err := svc.Fetch("relay")
	require.NoError(t, err)
	kx, ok := extra.Get(user.ID)
	require.True(t, ok)
	assert.Contains(t, kx.Ed25519PublicKey, "PUBLIC KEY")
	assert.Contains(t, kx.Ed25519PrivateKey, "PRIVATE KEY")
}

func TestFetch_SkipsEd25519WhenExtraRepoNotWired(t *testing.T) {
	svc, _, keypairRepo, _ := newService(t)

	user, err := svc.Fetch("relay")
	require.NoError(t, err)
	// RSA 鍵だけ作られる (extra repo 未配線で skip される)
	_, ok := keypairRepo.Keypairs[user.ID]
	assert.True(t, ok)
}

func TestFetch_Ed25519IdempotentAcrossSecondCall(t *testing.T) {
	svc, _, _, _ := newService(t)
	extra := testutil.NewMockUserKeypairExtraRepository()
	svc.SetKeypairExtraRepo(extra)

	user, err := svc.Fetch("relay")
	require.NoError(t, err)
	first, ok := extra.Get(user.ID)
	require.True(t, ok)
	firstPub := first.Ed25519PublicKey

	// 2 回目 Fetch しても鍵は変わらない (idempotent)
	_, err = svc.Fetch("relay")
	require.NoError(t, err)
	second, ok := extra.Get(user.ID)
	require.True(t, ok)
	assert.Equal(t, firstPub, second.Ed25519PublicKey)
}

type failingExtraUpsert struct {
	*testutil.MockUserKeypairExtraRepository
	err error
}

func (f *failingExtraUpsert) Upsert(_ *model.UserKeypairExtra) error { return f.err }

func TestFetch_ErrorBubblesFromEd25519KeypairCreate(t *testing.T) {
	svc, _, _, _ := newService(t)
	extra := &failingExtraUpsert{
		MockUserKeypairExtraRepository: testutil.NewMockUserKeypairExtraRepository(),
		err:                            errors.New("ed25519 db down"),
	}
	svc.SetKeypairExtraRepo(extra)

	_, err := svc.Fetch("relay")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ed25519")
}
