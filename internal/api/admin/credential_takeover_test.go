package admin_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// **`reset-password` / `unset-mfa` は「乗っ取れる操作」(#3037)。**
//
// 前者は新しいパスワードを応答に載せて返し、後者は 2FA を外す。2 つを続けて
// 叩くと、対象アカウントとしてサインインできる状態が完成する。
//
// 以前は「対象が管理者」だけを見ていたので、
//
//   - モデレーターが **system アカウント** (`instance.actor` 等) のパスワードを
//     発行できた
//   - モデレーターが**他のモデレーター**を乗っ取れた。相手が持つ凍結・削除・
//     ロール付与をそのまま使える
func TestCredentialResets_ProtectedTargets(t *testing.T) {
	calls := map[string]func(*apiadmin.Handler, string, *model.User) *httptest.ResponseRecorder{
		"reset-password": func(h *apiadmin.Handler, id string, actor *model.User) *httptest.ResponseRecorder {
			return doPost(h.ResetPassword, `{"userId":"`+id+`"}`, actor)
		},
		"unset-mfa": func(h *apiadmin.Handler, id string, actor *model.User) *httptest.ResponseRecorder {
			return doPost(h.UnsetMfa, `{"userId":"`+id+`"}`, actor)
		},
	}

	for _, tt := range []struct {
		name      string
		target    *model.User
		adminIDs  []string
		modIDs    []string
		actorID   string
		wantDeny  bool
		wantDenyR string
	}{
		{
			name:     "system アカウント (モデレーターが実行)",
			target:   &model.User{ID: "sys", Username: "instance.actor"},
			actorID:  "mod1",
			wantDeny: true,
		},
		{
			// **管理者でも塞ぐ。** 人がサインインする前提の無いアカウントに
			// サインインできる資格情報を作らない。
			name:     "system アカウント (管理者が実行)",
			target:   &model.User{ID: "sys", Username: "relay.actor"},
			adminIDs: []string{"boss"},
			actorID:  "boss",
			wantDeny: true,
		},
		{
			name:     "他のモデレーター (モデレーターが実行)",
			target:   &model.User{ID: "mod2", Username: "mod2"},
			modIDs:   []string{"mod1", "mod2"},
			actorID:  "mod1",
			wantDeny: true,
		},
		{
			// インシデント対応の経路は残す。
			name:     "他のモデレーター (管理者が実行)",
			target:   &model.User{ID: "mod2", Username: "mod2"},
			adminIDs: []string{"boss"},
			modIDs:   []string{"mod2"},
			actorID:  "boss",
		},
		{
			name:     "管理者 (モデレーターが実行)",
			target:   &model.User{ID: "boss", Username: "boss"},
			adminIDs: []string{"boss"},
			modIDs:   []string{"mod1"},
			actorID:  "mod1",
			wantDeny: true,
		},
		{
			name:    "普通の利用者",
			target:  &model.User{ID: "u1", Username: "u1"},
			modIDs:  []string{"mod1"},
			actorID: "mod1",
		},
		{
			// 自分自身のリセットは従来どおり通す。
			name:     "自分自身 (管理者)",
			target:   &model.User{ID: "boss", Username: "boss"},
			adminIDs: []string{"boss"},
			actorID:  "boss",
		},
	} {
		for callName, call := range calls {
			t.Run(tt.name+"/"+callName, func(t *testing.T) {
				h := newCredentialResetHandler(t, tt.target, tt.adminIDs, tt.modIDs)
				rec := call(h, tt.target.ID, &model.User{ID: tt.actorID})

				if tt.wantDeny {
					assert.Equal(t, http.StatusBadRequest, rec.Code, "保護対象を触れている")
					assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
					return
				}
				assert.NotContains(t, rec.Body.String(), "ACCESS_DENIED",
					"通るべき対象を弾いている: %s", rec.Body.String())
			})
		}
	}
}

// newCredentialResetHandler wires a handler where adminIDs / modIDs hold the
// corresponding role.
func newCredentialResetHandler(t *testing.T, target *model.User, adminIDs, modIDs []string) *apiadmin.Handler {
	t.Helper()
	h, users, _, roles, assigns := newTestHandlerWithAssign(t)
	users.Users[target.ID] = target

	roles.Roles["admin-role"] = &model.Role{ID: "admin-role", Target: model.RoleTargetManual, IsAdministrator: true}
	roles.Roles["mod-role"] = &model.Role{ID: "mod-role", Target: model.RoleTargetManual, IsModerator: true}
	for _, id := range adminIDs {
		require.NoError(t, assigns.Create(&model.RoleAssignment{ID: "a-" + id, UserID: id, RoleID: "admin-role"}))
	}
	for _, id := range modIDs {
		require.NoError(t, assigns.Create(&model.RoleAssignment{ID: "m-" + id, UserID: id, RoleID: "mod-role"}))
	}
	h.SetSecurityKeyRepo(newFakeSecurityKeyRepo())
	return h
}

// flakyUserRepo returns the user once, then fails — reproducing the window
// where the DB drops between the handler's lookup and isProtectedAccount's.
type flakyUserRepo struct {
	*testutil.MockUserRepository
	calls int
}

func (r *flakyUserRepo) FindByID(id string) (*model.User, error) {
	r.calls++
	if r.calls > 1 {
		return nil, errors.New("db is down")
	}
	return r.MockUserRepository.FindByID(id)
}

// **system アカウントの判定は handler が引いた行だけで完結する。**
//
// 以前は `isProtectedAccount` が `FindByID` で引き直しており、その失敗を
// 「保護対象ではない」と扱っていたので、**DB の瞬断の窓で system アカウントの
// パスワードを発行できた** (#3037)。レビュー 2 周目で引き直しを廃し、
// handler が既に持っている行だけを見る形にしたので窓そのものが無い。
func TestCredentialResets_SystemAccountStaysProtectedWhenLookupFails(t *testing.T) {
	users := testutil.NewMockUserRepository()
	sys := &model.User{ID: "sys", Username: "instance.actor"}
	users.Users["sys"] = sys

	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	roleSvc := corerole.NewService(roleRepo, assignRepo, metaRepo, idGen)
	flaky := &flakyUserRepo{MockUserRepository: users}
	h := apiadmin.NewHandler(signup.NewService(flaky, metaRepo, idGen), roleSvc, metaRepo, flaky, idGen)

	rec := doPost(h.ResetPassword, `{"userId":"sys"}`, &model.User{ID: "mod1"})

	assert.Equal(t, http.StatusBadRequest, rec.Code, "瞬断の窓で system アカウントを触れている")
	assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
	// **引き直さないことまで固定する。** 2 回目を足すとその失敗を
	// 「保護対象ではない」と読む経路が復活する。
	assert.Equal(t, 1, flaky.calls, "行を引き直している (瞬断の窓が戻る)")
}

// failingAssignmentRepo makes the role lookup fail while every other read
// keeps working — the partial outage this guard has to survive.
type failingAssignmentRepo struct {
	*testutil.MockRoleAssignmentRepository
}

func (r *failingAssignmentRepo) ListByUser(string) ([]*model.RoleAssignment, error) {
	return nil, errors.New("assignment lookup is down")
}

// failingMetaRepo makes only Fetch fail.
//
// 本番の `metaRepository.Fetch` は行が無ければ作り直すので、**返る error は
// 必ず実障害**。だから「読めなかった」を「root ではない」と読み替えてはいけない。
type failingMetaRepo struct {
	repository.MetaRepository
	fail bool
}

func (r *failingMetaRepo) Fetch() (*model.Meta, error) {
	if r.fail {
		return nil, errors.New("meta is down")
	}
	return r.MetaRepository.Fetch()
}

// takeoverFixture wires a handler whose role lookup and meta read can each be
// broken independently.
type takeoverFixture struct {
	h        *apiadmin.Handler
	metaRepo *failingMetaRepo
}

func newTakeoverFixture(t *testing.T, target *model.User, breakAssignments bool) *takeoverFixture {
	t.Helper()

	users := testutil.NewMockUserRepository()
	users.Users[target.ID] = target
	users.Users["mod1"] = &model.User{ID: "mod1", Username: "mod1"}

	inner := testutil.NewMockMetaRepository()
	inner.Meta = &model.Meta{ID: "x"}
	metaRepo := &failingMetaRepo{MetaRepository: inner}

	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	var assignments repository.RoleAssignmentRepository = assignRepo
	if breakAssignments {
		assignments = &failingAssignmentRepo{MockRoleAssignmentRepository: assignRepo}
	}
	roleSvc := corerole.NewService(roleRepo, assignments, metaRepo, idGen)
	h := apiadmin.NewHandler(signup.NewService(users, metaRepo, idGen), roleSvc, metaRepo, users, idGen)
	h.SetSecurityKeyRepo(newFakeSecurityKeyRepo())
	return &takeoverFixture{h: h, metaRepo: metaRepo}
}

// **ロールを引けない窓で乗っ取らせない (#3037 レビュー 2 周目)。**
//
// `IsAdministrator` / `IsModerator` は判定できないときに false を返すので、
// 素で使うと `assignmentRepo.ListByUser` が一時的に失敗する窓で**他の管理者を
// 乗っ取れる**。`RolePrivileges` が error を返し、handler が 500 に倒すこと
// (#2792) をここで固定する。
//
// **この形のテストが無かったため、fail-open に戻す変異が素通りしていた。**
func TestCredentialResets_RoleLookupFailureIsNotTreatedAsUnprivileged(t *testing.T) {
	for name, call := range credentialTakeoverCalls() {
		t.Run(name, func(t *testing.T) {
			target := &model.User{ID: "victim", Username: "victim"}

			// 対照: ロールを引けるなら通る (「常に 500」の実装では緑にならない)。
			ok := newTakeoverFixture(t, target, false)
			rec := call(ok.h, "victim", &model.User{ID: "mod1"})
			require.Less(t, rec.Code, 300, "正常時に通っていない: %s", rec.Body.String())

			broken := newTakeoverFixture(t, target, true)
			rec = call(broken.h, "victim", &model.User{ID: "mod1"})

			assert.Equal(t, http.StatusInternalServerError, rec.Code,
				"ロールを引けない窓で資格情報を発行している: %s", rec.Body.String())
		})
	}
}

// **root は `meta.rootUserId` にしか居ないことがある (#3037 レビュー 2 周目)。**
//
// `isRoot` 列が入った migration より前に作られた root は `isRoot=false` のまま
// で、本番の root がまさにそれ。meta を読めない窓で「root ではない」と扱うと、
// **モデレーターが root のパスワードを発行できる**。
func TestCredentialResets_RootStaysProtectedWhenMetaIsUnreadable(t *testing.T) {
	for name, call := range credentialTakeoverCalls() {
		t.Run(name, func(t *testing.T) {
			// `isRoot` は false。root であることは meta にしか書かれていない。
			target := &model.User{ID: "root1", Username: "root1"}
			fx := newTakeoverFixture(t, target, false)
			rootID := "root1"
			fx.metaRepo.MetaRepository.(*testutil.MockMetaRepository).Meta.RootUserID = &rootID

			// 対照: meta を読めるなら root として弾く。
			rec := call(fx.h, "root1", &model.User{ID: "mod1"})
			require.Equal(t, http.StatusBadRequest, rec.Code, "root を弾けていない: %s", rec.Body.String())
			require.Contains(t, rec.Body.String(), "ACCESS_DENIED")

			fx.metaRepo.fail = true
			rec = call(fx.h, "root1", &model.User{ID: "mod1"})

			assert.Equal(t, http.StatusInternalServerError, rec.Code,
				"meta を読めない窓で root の資格情報を発行している: %s", rec.Body.String())
		})
	}
}

func credentialTakeoverCalls() map[string]func(*apiadmin.Handler, string, *model.User) *httptest.ResponseRecorder {
	return map[string]func(*apiadmin.Handler, string, *model.User) *httptest.ResponseRecorder{
		"reset-password": func(h *apiadmin.Handler, id string, actor *model.User) *httptest.ResponseRecorder {
			return doPost(h.ResetPassword, `{"userId":"`+id+`"}`, actor)
		},
		"unset-mfa": func(h *apiadmin.Handler, id string, actor *model.User) *httptest.ResponseRecorder {
			return doPost(h.UnsetMfa, `{"userId":"`+id+`"}`, actor)
		},
	}
}

// **「相手が特権を持っていないことを確かめてから触る」判定は全部 fail-closed
// にする (#3037 レビュー 2 周目)。**
//
// `IsAdministrator` / `IsModerator` は判定できないときに false を返すので、
// 素で使うと `assignmentRepo.ListByUser` が一時的に失敗する窓で判定が
// 「特権なし」に倒れる。1 周目では資格情報のリセット 2 本だけを直しており、
// 同じ形が 3 箇所残っていた。
func TestTargetPrivilegeChecksAreFailClosed(t *testing.T) {
	t.Run("suspend-user", func(t *testing.T) {
		target := &model.User{ID: "mod2", Username: "mod2"}

		ok := newTakeoverFixture(t, target, false)
		rec := doPost(ok.h.SuspendUser, `{"userId":"mod2"}`, &model.User{ID: "mod1"})
		require.Less(t, rec.Code, 300, "正常時に凍結できていない: %s", rec.Body.String())

		broken := newTakeoverFixture(t, target, true)
		rec = doPost(broken.h.SuspendUser, `{"userId":"mod2"}`, &model.User{ID: "mod1"})
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"ロールを引けない窓でモデレーターを凍結できている: %s", rec.Body.String())
	})

	t.Run("show-user", func(t *testing.T) {
		target := &model.User{ID: "boss", Username: "boss"}

		ok := newTakeoverFixture(t, target, false)
		rec := doPost(ok.h.ShowUser, `{"userId":"boss"}`, &model.User{ID: "mod1"})
		require.Less(t, rec.Code, 300, "正常時に閲覧できていない: %s", rec.Body.String())

		broken := newTakeoverFixture(t, target, true)
		rec = doPost(broken.h.ShowUser, `{"userId":"boss"}`, &model.User{ID: "mod1"})
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"ロールを引けない窓で管理者の詳細が見えている: %s", rec.Body.String())
	})
}

// **root は `meta.rootUserId` にしか居ないことがある — 凍結と閲覧でも同じ
// (#3037 レビュー 3 周目)。**
//
// 2 周目は資格情報のリセット 2 本だけを `targetIsRoot` に替えた。`SuspendUser` /
// `ShowUser` は `RolePrivileges` を通すが、あれが内部で呼ぶ `isRootUser` は
// **meta の失敗を握り潰して `user.isRoot` へ落ちる**。`isRoot` 列が入った
// migration より前に作られた root は `false` のままなので、meta を読めない窓では
// `(false, false, nil)` = 「特権なし」が返り、handler に倒す材料が渡らなかった。
func TestRootStaysProtectedWhenMetaIsUnreadable(t *testing.T) {
	for name, call := range map[string]func(*apiadmin.Handler, string, *model.User) *httptest.ResponseRecorder{
		"suspend-user": func(h *apiadmin.Handler, id string, actor *model.User) *httptest.ResponseRecorder {
			return doPost(h.SuspendUser, `{"userId":"`+id+`"}`, actor)
		},
		"show-user": func(h *apiadmin.Handler, id string, actor *model.User) *httptest.ResponseRecorder {
			return doPost(h.ShowUser, `{"userId":"`+id+`"}`, actor)
		},
	} {
		t.Run(name, func(t *testing.T) {
			// `isRoot` は false。root であることは meta にしか書かれていない。
			target := &model.User{ID: "root1", Username: "root1"}
			fx := newTakeoverFixture(t, target, false)
			rootID := "root1"
			fx.metaRepo.MetaRepository.(*testutil.MockMetaRepository).Meta.RootUserID = &rootID

			// 対照: meta を読めるなら root として弾く。
			rec := call(fx.h, "root1", &model.User{ID: "mod1"})
			require.Equal(t, http.StatusBadRequest, rec.Code, "root を弾けていない: %s", rec.Body.String())
			require.Contains(t, rec.Body.String(), "ACCESS_DENIED")

			fx.metaRepo.fail = true
			rec = call(fx.h, "root1", &model.User{ID: "mod1"})

			assert.Equal(t, http.StatusInternalServerError, rec.Code,
				"meta を読めない窓で root を触れている: %s", rec.Body.String())
		})
	}
}
