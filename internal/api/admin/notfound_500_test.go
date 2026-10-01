package admin_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	apiadmin "github.com/shiroha-a/mk/internal/api/admin"
	"github.com/shiroha-a/mk/internal/core/role"
	"github.com/shiroha-a/mk/internal/core/signup"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/testutil"
)

// failingUserRepo makes every FindByID look like a database failure rather
// than a missing row.
type failingUserRepo struct {
	*testutil.MockUserRepository
	err error
}

func (r *failingUserRepo) FindByID(string) (*model.User, error) { return nil, r.err }

// FindProfileByEmail も落とす。`admin/accounts/find-by-email` は profile を先に
// 引くので、FindByID だけ落としても手前で 400 に落ちる。
func (r *failingUserRepo) FindProfileByEmail(string) (*model.UserProfile, error) {
	return nil, r.err
}

// profileOnlyRepo resolves the profile but fails the user lookup, so the second
// guard in AccountsFindByEmail is reachable.
type profileOnlyRepo struct {
	*testutil.MockUserRepository
	err error
}

func (r *profileOnlyRepo) FindProfileByEmail(string) (*model.UserProfile, error) {
	return &model.UserProfile{UserID: "u1"}, nil
}

func (r *profileOnlyRepo) FindByID(string) (*model.User, error) { return nil, r.err }

func handlerWithFailingUserLookupOnly(t *testing.T, err error) *apiadmin.Handler {
	t.Helper()
	userRepo := &profileOnlyRepo{MockUserRepository: testutil.NewMockUserRepository(), err: err}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	idGen, _ := id.NewGenerator("aidx")
	return apiadmin.NewHandler(
		signup.NewService(userRepo, metaRepo, idGen),
		role.NewService(roleRepo, assignRepo, metaRepo, idGen),
		metaRepo, userRepo, idGen,
	)
}

// handlerWithFailingUserRepo builds an admin handler whose user lookups fail
// with a database error.
func handlerWithFailingUserRepo(t *testing.T, err error) *apiadmin.Handler {
	t.Helper()
	userRepo := &failingUserRepo{MockUserRepository: testutil.NewMockUserRepository(), err: err}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	roleRepo := testutil.NewMockRoleRepository()
	// **role を seed する。** roles/assign と roles/unassign は user を引く前に
	// role の編集権限を見るので、role が無いと手前で NO_SUCH_ROLE の 400 になり
	// user lookup に到達しない。
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Target: model.RoleTargetManual, CanEditMembersByModerator: true}
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	idGen, _ := id.NewGenerator("aidx")
	return apiadmin.NewHandler(
		signup.NewService(userRepo, metaRepo, idGen),
		role.NewService(roleRepo, assignRepo, metaRepo, idGen),
		metaRepo, userRepo, idGen,
	)
}

// **DB 障害を not-found に丸めないこと** (#2792)。
//
// repository は GORM の error をそのまま返すので、`err != nil` をまとめて
// not-found 扱いにすると接続断まで「そんな利用者は居ない」になる。クライアント
// からは区別できず、監視でも 5xx が立たない。
//
// upstream は `.findOneBy` の結果が `null` かどうかで判定するので、DB 障害は
// 例外として 500 に化ける。こちらもそれに合わせる。
func TestAdmin_DBFailureIsNot4xx(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")

	h := handlerWithFailingUserRepo(t, dbErr)

	for _, tt := range []struct {
		name string
		run  func() int
	}{
		{"admin/show-user", func() int {
			return doPost(h.ShowUser, `{"userId":"u1"}`, adminUser).Code
		}},
		{"admin/suspend-user", func() int {
			return doPost(h.SuspendUser, `{"userId":"u1"}`, adminUser).Code
		}},
		{"admin/unsuspend-user", func() int {
			return doPost(h.UnsuspendUser, `{"userId":"u1"}`, adminUser).Code
		}},
		{"admin/reset-password", func() int {
			return doPost(h.ResetPassword, `{"userId":"u1"}`, adminUser).Code
		}},
		{"admin/unset-mfa", func() int {
			return doPost(h.UnsetMfa, `{"userId":"u1"}`, adminUser).Code
		}},
		{"admin/roles/assign", func() int {
			return doPost(h.RolesAssign, `{"userId":"u1","roleId":"r1"}`, adminUser).Code
		}},
		{"admin/roles/unassign", func() int {
			return doPost(h.RolesUnassign, `{"userId":"u1","roleId":"r1"}`, adminUser).Code
		}},
		{"admin/accounts/find-by-email", func() int {
			return doPost(h.AccountsFindByEmail, `{"email":"a@example.test"}`, adminUser).Code
		}},
		// **2 つ目の guard も通す。** profile が引けたあと user を引くので、
		// profile だけ成功させないと `FindByID` 側の guard に到達しない。
		{"admin/accounts/find-by-email (user lookup)", func() int {
			ph := handlerWithFailingUserLookupOnly(t, dbErr)
			return doPost(ph.AccountsFindByEmail, `{"email":"a@example.test"}`, adminUser).Code
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, http.StatusInternalServerError, tt.run(),
				"DB 障害が 4xx に化けている (#2792)")
		})
	}

	// forward-abuse-user-report は abuse report を引く。**別の repo なので上の
	// failingUserRepo では守れない** (実際、guard を外す変異が生き残った)。
	t.Run("admin/forward-abuse-user-report", func(t *testing.T) {
		fh, _, _, _ := newTestHandler(t)
		fh.SetAbuseRepo(&failingAbuseRepo{err: dbErr})

		rec := doPost(fh.ForwardAbuseUserReport, `{"reportId":"r1"}`, adminUser)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"DB 障害が 4xx に化けている (#2792)")
	})

	// resolve-abuse-user-report も abuse report を引く。以前は引かずに
	// UpdateFields の失敗を 404 にしていた (#3259)。
	t.Run("admin/resolve-abuse-user-report", func(t *testing.T) {
		rh, _, _, _ := newTestHandler(t)
		rh.SetAbuseRepo(&failingAbuseRepo{err: dbErr})

		rec := doPost(rh.ResolveAbuseReport, `{"reportId":"r1"}`, adminUser)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"DB 障害が 4xx に化けている (#2792)")
	})

	// promo/create は user ではなく note を引く。**別の repo なので上の
	// failingUserRepo では守れない** (実際、guard を外す変異が生き残った)。
	t.Run("admin/promo/create", func(t *testing.T) {
		ph, _, _, _ := newTestHandler(t)
		ph.SetPromoNoteRepo(&stubPromoRepo{})
		ph.SetNoteFinder(failingNoteFinder{err: dbErr})

		rec := doPost(ph.PromoCreate, `{"noteId":"n1","expiresAt":1}`, adminUser)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"DB 障害が 4xx に化けている (#2792)")
	})
}

// failingNoteFinder makes every note lookup look like a database failure.
type failingNoteFinder struct{ err error }

func (f failingNoteFinder) FindByID(string) (*model.Note, error) { return nil, f.err }

// failingAbuseRepo makes every abuse-report lookup look like a database failure.
type failingAbuseRepo struct {
	repository.AbuseReportRepository
	err error
}

func (r *failingAbuseRepo) FindByID(string) (*model.AbuseUserReport, error) { return nil, r.err }
