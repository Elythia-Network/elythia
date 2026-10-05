package signupapplication

import (
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

var errBoom = errors.New("boom")

// stubRepo lets a test force each repository call to fail, so the wrappers'
// error paths are exercised without breaking the shared connection.
//
// DB() は本物を返す。transition がトランザクションを開けないと、その中の分岐に
// 到達できないため。
type stubRepo struct {
	createErrs  []error // 呼び出しごとに返すエラー (足りなければ nil)
	createCalls int
	findErr     error
	findByIDErr error
	lockErr     error
	updateErr   error
	listErr     error
	countErr    error
	expireErr   error

	found  *model.SignupApplication
	locked *model.SignupApplication
}

func (s *stubRepo) Create(*model.SignupApplication) error {
	defer func() { s.createCalls++ }()
	if s.createCalls < len(s.createErrs) {
		return s.createErrs[s.createCalls]
	}
	return nil
}

func (s *stubRepo) FindByID(string) (*model.SignupApplication, error) {
	if s.findByIDErr != nil {
		return nil, s.findByIDErr
	}
	return &model.SignupApplication{ID: "x"}, nil
}

func (s *stubRepo) FindByClaimCodeHash(string) (*model.SignupApplication, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}
	if s.found != nil {
		return s.found, nil
	}
	return nil, gorm.ErrRecordNotFound
}

func (s *stubRepo) FindByIDForUpdateTx(*gorm.DB, string) (*model.SignupApplication, error) {
	if s.lockErr != nil {
		return nil, s.lockErr
	}
	if s.locked != nil {
		return s.locked, nil
	}
	return &model.SignupApplication{ID: "x", Status: model.SignupApplicationPending}, nil
}

func (s *stubRepo) UpdateFieldsTx(*gorm.DB, string, map[string]any) error { return s.updateErr }

func (s *stubRepo) List(string, int, int) ([]*model.SignupApplication, error) {
	return nil, s.listErr
}

func (s *stubRepo) Count(string) (int, error) { return 0, s.countErr }

func (s *stubRepo) ExpireStale(time.Time) (int, error) { return 0, s.expireErr }

func (s *stubRepo) DB() *gorm.DB { return testDB }

func newStubService(t *testing.T, repo *stubRepo) *Service {
	t.Helper()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return NewService(repo, idGen)
}

// hash 衝突は crypto/rand なので実質起きないが、**黙って別の申請を上書きしない**
// ように採り直す。
func TestApply_RetriesOnCodeCollision(t *testing.T) {
	repo := &stubRepo{createErrs: []error{repository.ErrSignupApplicationCodeCollision}}
	svc := newStubService(t, repo)

	app, code, err := svc.Apply(testAnswers())
	require.NoError(t, err)
	assert.NotEmpty(t, code)
	assert.NotNil(t, app)
	assert.Equal(t, 2, repo.createCalls, "1 度衝突したら採り直す")
}

// 何度も衝突するなら諦める。**無限に回すと、DB が壊れているときに固まる。**
func TestApply_GivesUpAfterRepeatedCollisions(t *testing.T) {
	errs := make([]error, claimCodeAttempts)
	for i := range errs {
		errs[i] = repository.ErrSignupApplicationCodeCollision
	}
	svc := newStubService(t, &stubRepo{createErrs: errs})

	_, _, err := svc.Apply(testAnswers())
	assert.Error(t, err)
}

func TestErrorPaths(t *testing.T) {
	t.Run("Apply wraps a create failure", func(t *testing.T) {
		svc := newStubService(t, &stubRepo{createErrs: []error{errBoom}})
		_, _, err := svc.Apply(testAnswers())
		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("ByClaimCode propagates a lookup failure", func(t *testing.T) {
		svc := newStubService(t, &stubRepo{findErr: errBoom})
		_, err := svc.ByClaimCode("code")
		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("Get propagates a failure", func(t *testing.T) {
		svc := newStubService(t, &stubRepo{findByIDErr: errBoom})
		_, err := svc.Get("x")
		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("List propagates a failure", func(t *testing.T) {
		svc := newStubService(t, &stubRepo{listErr: errBoom})
		_, err := svc.List(repository.SignupApplicationFilterAll, 10, 0)
		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("Count propagates a failure", func(t *testing.T) {
		svc := newStubService(t, &stubRepo{countErr: errBoom})
		_, err := svc.Count(repository.SignupApplicationFilterAll)
		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("ExpireStale propagates a failure", func(t *testing.T) {
		svc := newStubService(t, &stubRepo{expireErr: errBoom})
		_, err := svc.ExpireStale()
		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("transition propagates a lock failure", func(t *testing.T) {
		svc := newStubService(t, &stubRepo{lockErr: errBoom})
		assert.ErrorIs(t, svc.Approve("x", "mod"), errBoom)
	})

	t.Run("transition propagates an update failure", func(t *testing.T) {
		repo := &stubRepo{
			updateErr: errBoom,
			locked: &model.SignupApplication{
				ID: "x", Status: model.SignupApplicationPending,
				ExpiresAt: time.Now().Add(time.Hour),
			},
		}
		svc := newStubService(t, repo)
		svc.SetClock(func() time.Time { return time.Now().Add(-time.Hour) })
		assert.ErrorIs(t, svc.Approve("x", "mod"), errBoom)
	})
}

// 期限切れの掃除は、ロックを取った時点で別経路が既に処理していたら何もしない。
// **ここで無条件に上書きすると、承認済みの申請を期限切れに落としうる。**
func TestByClaimCode_SkipsWhenAlreadyHandled(t *testing.T) {
	now := time.Now().UTC()
	stale := &model.SignupApplication{
		ID: "x", Status: model.SignupApplicationPending, ExpiresAt: now.Add(-time.Hour),
	}
	handled := &model.SignupApplication{
		ID: "x", Status: model.SignupApplicationCompleted, ExpiresAt: now.Add(-time.Hour),
	}
	repo := &stubRepo{found: stale, locked: handled, updateErr: errBoom}
	svc := newStubService(t, repo)
	svc.SetClock(func() time.Time { return now })

	// updateErr を仕込んであるので、更新に入ったらエラーが返る。返らない =
	// 何もしなかった、ということ。
	got, err := svc.ByClaimCode("code")
	require.NoError(t, err)
	require.NotNil(t, got)
}
