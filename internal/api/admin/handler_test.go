package admin_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	apiadmin "github.com/shiroha-a/mk/internal/api/admin"
	coredrive "github.com/shiroha-a/mk/internal/core/drive"
	"github.com/shiroha-a/mk/internal/core/moderationlog"
	"github.com/shiroha-a/mk/internal/core/role"
	"github.com/shiroha-a/mk/internal/core/signup"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/shiroha-a/mk/internal/core/iplookuplog"
)

const adminInternalErrorJSON = `{"error":{"message":"Internal error.","code":"INTERNAL_ERROR","id":"5d37dbcb-891e-41ca-a3d6-e690c97775ac","kind":"server"}}`

var adminNoSuchRoleJSON = map[string]string{
	"show":   `{"error":{"message":"No such role.","code":"NO_SUCH_ROLE","id":"07dc7d34-c0d8-49b7-96c6-db3ce64ee0b3","kind":"client"}}`,
	"update": `{"error":{"message":"No such role.","code":"NO_SUCH_ROLE","id":"cd23ef55-09ad-428a-ac61-95a45e124b32","kind":"client"}}`,
	"delete": `{"error":{"message":"No such role.","code":"NO_SUCH_ROLE","id":"de0d6ecd-8e0a-4253-88ff-74bc89ae3d45","kind":"client"}}`,
	"assign": `{"error":{"message":"No such role.","code":"NO_SUCH_ROLE","id":"6503c040-6af4-4ed9-bf07-f2dd16678eab","kind":"client"}}`,
	"users":  `{"error":{"message":"No such role.","code":"NO_SUCH_ROLE","id":"224eff5e-2488-4b18-b3e7-f50d94421648","kind":"client"}}`,
}

type adminRoleLookup struct {
	role *model.Role
	err  error
}

type adminRoleRepository struct {
	repository.RoleRepository
	lookups   []adminRoleLookup
	findCalls int
	updateErr error
	deleteErr error
}

func (r *adminRoleRepository) FindByID(string) (*model.Role, error) {
	result := r.lookups[min(r.findCalls, len(r.lookups)-1)]
	r.findCalls++
	return result.role, result.err
}

func (r *adminRoleRepository) UpdateFields(id string, fields map[string]any) error {
	if r.updateErr != nil {
		return r.updateErr
	}
	return r.RoleRepository.UpdateFields(id, fields)
}

func (r *adminRoleRepository) Delete(id string) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	return r.RoleRepository.Delete(id)
}

func adminHandlerWithRoleRepository(t *testing.T, roleRepo repository.RoleRepository, base *testutil.MockRoleRepository) (*apiadmin.Handler, *testutil.MockUserRepository, *testutil.MockRoleAssignmentRepository) {
	t.Helper()
	users := testutil.NewMockUserRepository()
	assignments := testutil.NewMockRoleAssignmentRepository(base)
	return assignmentAdminHandler(t, roleRepo, users, assignments), users, assignments
}

func assertAdminInternalError(t *testing.T, rec *httptest.ResponseRecorder, rawError, roleID string) {
	t.Helper()
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, adminInternalErrorJSON, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), rawError)
	assert.NotContains(t, rec.Body.String(), roleID)
}

func TestRolesShow_RolePersistenceFailure(t *testing.T) {
	wantErr := errors.New("show role SELECT failed")
	base := testutil.NewMockRoleRepository()
	repo := &adminRoleRepository{RoleRepository: base, lookups: []adminRoleLookup{{err: wantErr}}}
	h, _, _ := adminHandlerWithRoleRepository(t, repo, base)
	rec := doPost(h.RolesShow, `{"roleId":"secret-role-id"}`, nil)
	assertAdminInternalError(t, rec, wantErr.Error(), "secret-role-id")
}

func TestRolesUpdate_PrelookupPersistenceFailure(t *testing.T) {
	wantErr := errors.New("update role SELECT failed")
	base := testutil.NewMockRoleRepository()
	repo := &adminRoleRepository{RoleRepository: base, lookups: []adminRoleLookup{{err: wantErr}}}
	h, _, _ := adminHandlerWithRoleRepository(t, repo, base)
	rec := doPost(h.RolesUpdate, `{"roleId":"secret-role-id","name":"new"}`, nil)
	assertAdminInternalError(t, rec, wantErr.Error(), "secret-role-id")
}

func TestRolesUpdate_InternalLookupFailure(t *testing.T) {
	wantErr := errors.New("update existence SELECT failed")
	existing := &model.Role{ID: "secret-role-id", Name: "old"}
	base := testutil.NewMockRoleRepository()
	base.Roles[existing.ID] = existing
	repo := &adminRoleRepository{
		RoleRepository: base,
		lookups:        []adminRoleLookup{{role: existing}, {err: wantErr}},
	}
	h, _, _ := adminHandlerWithRoleRepository(t, repo, base)
	rec := doPost(h.RolesUpdate, `{"roleId":"secret-role-id","name":"new"}`, nil)
	assertAdminInternalError(t, rec, wantErr.Error(), "secret-role-id")
}

func TestRolesUpdate_OperationFailure(t *testing.T) {
	wantErr := errors.New("role UPDATE failed")
	existing := &model.Role{ID: "secret-role-id", Name: "old"}
	base := testutil.NewMockRoleRepository()
	base.Roles[existing.ID] = existing
	repo := &adminRoleRepository{RoleRepository: base, lookups: []adminRoleLookup{{role: existing}}, updateErr: wantErr}
	h, _, _ := adminHandlerWithRoleRepository(t, repo, base)
	rec := doPost(h.RolesUpdate, `{"roleId":"secret-role-id","name":"new"}`, nil)
	assertAdminInternalError(t, rec, wantErr.Error(), "secret-role-id")
}

func TestRolesDelete_LookupFailure(t *testing.T) {
	wantErr := errors.New("delete role SELECT failed")
	base := testutil.NewMockRoleRepository()
	repo := &adminRoleRepository{RoleRepository: base, lookups: []adminRoleLookup{{err: wantErr}}}
	h, _, _ := adminHandlerWithRoleRepository(t, repo, base)
	rec := doPost(h.RolesDelete, `{"roleId":"secret-role-id"}`, nil)
	assertAdminInternalError(t, rec, wantErr.Error(), "secret-role-id")
}

func TestRolesDelete_OperationFailure(t *testing.T) {
	wantErr := errors.New("role DELETE failed")
	existing := &model.Role{ID: "secret-role-id"}
	base := testutil.NewMockRoleRepository()
	base.Roles[existing.ID] = existing
	repo := &adminRoleRepository{RoleRepository: base, lookups: []adminRoleLookup{{role: existing}}, deleteErr: wantErr}
	h, _, _ := adminHandlerWithRoleRepository(t, repo, base)
	rec := doPost(h.RolesDelete, `{"roleId":"secret-role-id"}`, nil)
	assertAdminInternalError(t, rec, wantErr.Error(), "secret-role-id")
}

func TestRolesDelete_BestEffortSnapshot(t *testing.T) {
	existing := &model.Role{ID: "secret-role-id"}
	base := testutil.NewMockRoleRepository()
	base.Roles[existing.ID] = existing
	repo := &adminRoleRepository{
		RoleRepository: base,
		lookups: []adminRoleLookup{
			{err: errors.New("snapshot failed")},
			{role: existing},
		},
	}
	h, _, _ := adminHandlerWithRoleRepository(t, repo, base)
	rec := doPost(h.RolesDelete, `{"roleId":"secret-role-id"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
}

func TestRolesAssign_SecondLookupFailure(t *testing.T) {
	wantErr := errors.New("assign role SELECT failed")
	existing := &model.Role{ID: "secret-role-id", CanEditMembersByModerator: true}
	base := testutil.NewMockRoleRepository()
	base.Roles[existing.ID] = existing
	repo := &adminRoleRepository{
		RoleRepository: base,
		lookups:        []adminRoleLookup{{role: existing}, {err: wantErr}},
	}
	h, users, assignments := adminHandlerWithRoleRepository(t, repo, base)
	users.Users["target-user"] = &model.User{ID: "target-user"}
	rec := doPost(h.RolesAssign, `{"userId":"target-user","roleId":"secret-role-id"}`, nil)
	assertAdminInternalError(t, rec, wantErr.Error(), "secret-role-id")
	exists, err := assignments.Exists("target-user", "secret-role-id")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestRolesUsers_RolePersistenceFailure(t *testing.T) {
	wantErr := errors.New("users role SELECT failed")
	base := testutil.NewMockRoleRepository()
	repo := &adminRoleRepository{RoleRepository: base, lookups: []adminRoleLookup{{err: wantErr}}}
	h, _, _ := adminHandlerWithRoleRepository(t, repo, base)
	rec := doPost(h.RolesUsers, `{"roleId":"secret-role-id"}`, nil)
	assertAdminInternalError(t, rec, wantErr.Error(), "secret-role-id")
}

func TestRolesHandlers_WrappedRoleNotFound(t *testing.T) {
	wrappedNotFound := fmt.Errorf("classified role: %w", role.ErrRoleNotFound)
	existing := &model.Role{ID: "secret-role-id", CanEditMembersByModerator: true}
	tests := []struct {
		name     string
		lookups  []adminRoleLookup
		call     func(*apiadmin.Handler) *httptest.ResponseRecorder
		response string
	}{
		{"show", []adminRoleLookup{{err: wrappedNotFound}}, func(h *apiadmin.Handler) *httptest.ResponseRecorder {
			return doPost(h.RolesShow, `{"roleId":"secret-role-id"}`, nil)
		}, adminNoSuchRoleJSON["show"]},
		{"update prelookup", []adminRoleLookup{{err: wrappedNotFound}}, func(h *apiadmin.Handler) *httptest.ResponseRecorder {
			return doPost(h.RolesUpdate, `{"roleId":"secret-role-id","name":"new"}`, nil)
		}, adminNoSuchRoleJSON["update"]},
		{"update internal lookup", []adminRoleLookup{{role: existing}, {err: wrappedNotFound}}, func(h *apiadmin.Handler) *httptest.ResponseRecorder {
			return doPost(h.RolesUpdate, `{"roleId":"secret-role-id","name":"new"}`, nil)
		}, adminNoSuchRoleJSON["update"]},
		{"delete", []adminRoleLookup{{role: existing}, {err: wrappedNotFound}}, func(h *apiadmin.Handler) *httptest.ResponseRecorder {
			return doPost(h.RolesDelete, `{"roleId":"secret-role-id"}`, nil)
		}, adminNoSuchRoleJSON["delete"]},
		{"assign", []adminRoleLookup{{role: existing}, {err: wrappedNotFound}}, func(h *apiadmin.Handler) *httptest.ResponseRecorder {
			return doPost(h.RolesAssign, `{"userId":"target-user","roleId":"secret-role-id"}`, nil)
		}, adminNoSuchRoleJSON["assign"]},
		{"users", []adminRoleLookup{{err: wrappedNotFound}}, func(h *apiadmin.Handler) *httptest.ResponseRecorder {
			return doPost(h.RolesUsers, `{"roleId":"secret-role-id"}`, nil)
		}, adminNoSuchRoleJSON["users"]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := testutil.NewMockRoleRepository()
			base.Roles[existing.ID] = existing
			repo := &adminRoleRepository{RoleRepository: base, lookups: tt.lookups}
			h, users, _ := adminHandlerWithRoleRepository(t, repo, base)
			users.Users["target-user"] = &model.User{ID: "target-user"}
			rec := tt.call(h)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.JSONEq(t, tt.response, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "secret-role-id")
		})
	}
}

func newTestHandler(t *testing.T) (*apiadmin.Handler, *testutil.MockUserRepository, *testutil.MockMetaRepository, *testutil.MockRoleRepository) {
	t.Helper()
	h, userRepo, metaRepo, roleRepo, _ := newTestHandlerWithAssign(t)
	return h, userRepo, metaRepo, roleRepo
}

// newTestHandlerWithAssign exposes the role-assignment mock alongside the
// standard handler dependencies. Used by tests that need to attach
// moderator / admin roles to the viewer (#1148 で moderator gate を伴う
// admin/drive/show-file 等で必要)。新規 test は本 helper を使い、既存
// test は (assign を必要としない場合) newTestHandler 互換 wrapper を継続。
func newTestHandlerWithAssign(t *testing.T) (*apiadmin.Handler, *testutil.MockUserRepository, *testutil.MockMetaRepository, *testutil.MockRoleRepository, *testutil.MockRoleAssignmentRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	idGen, _ := id.NewGenerator("aidx")
	signupSvc := signup.NewService(userRepo, metaRepo, idGen)
	roleSvc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	h := apiadmin.NewHandler(signupSvc, roleSvc, metaRepo, userRepo, idGen)
	return h, userRepo, metaRepo, roleRepo, assignRepo
}

// newTestHandlerNoRoles builds a handler without a role service, for the
// fail-closed paths that must not disclose anything when the policy cannot be
// evaluated (#3114)。
func newTestHandlerNoRoles(t *testing.T) (*apiadmin.Handler, *testutil.MockUserRepository, *testutil.MockMetaRepository, id.Generator) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	signupSvc := signup.NewService(userRepo, metaRepo, idGen)
	return apiadmin.NewHandler(signupSvc, nil, metaRepo, userRepo, idGen), userRepo, metaRepo, idGen
}

func doPost(h func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	_ = h(c)
	return rec
}

// adminUser is the throwaway admin principal shared across the per-handler
// test files (forward_abuse_report_test.go, server_stats_test.go, etc.).
var adminUser = &model.User{ID: "admin1"}

// assertError is a trivial error used to exercise error branches of handlers
// without pulling in a real mock that fails. Shared by tests that need a
// repository to surface a generic failure (e.g. ResetPassword fallback paths,
// AbuseReport recipient repo errors, emoji import enqueue failures).
type assertError struct{}

func (assertError) Error() string { return "stub failure" }

// setupDriveFileHandler returns a handler with DriveFileRepo wired and
// optional seed rows. boilerplate (handler 構築 + repo 生成 + seed +
// SetDriveFileRepo) を 1 行に圧縮する (#761)。drive_emoji_test 以外の
// admin test (例: moderation_test の DeleteAllFilesOfUser) からも利用
// できるよう handler_test.go に置く。戻り値の repo を直接 mutate して
// EmojiReferencedURLs 等の追加設定も可能。
func setupDriveFileHandler(t *testing.T, seed ...*model.DriveFile) (*apiadmin.Handler, *testutil.MockDriveFileRepository) {
	t.Helper()
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockDriveFileRepository()
	for _, df := range seed {
		require.NoError(t, repo.Create(df))
	}
	h.SetDriveFileRepo(repo)
	return h, repo
}

// setupAbuseReportHandler returns a handler with AbuseRepo wired and
// optional seed rows. moderation_test / forward_abuse_report_test の両方
// で UpdateAbuseUserReport / ForwardAbuseUserReport の modlog 検証に
// 使う (#761 Phase 2)。
func setupAbuseReportHandler(t *testing.T, seed ...*model.AbuseUserReport) (*apiadmin.Handler, *testutil.MockAbuseReportRepository) {
	t.Helper()
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockAbuseReportRepository()
	for _, r := range seed {
		require.NoError(t, repo.Create(r))
	}
	h.SetAbuseRepo(repo)
	return h, repo
}

// TestSetDriveFileRepo / TestSetAdminDB exist only to ensure the public
// setters keep compiling; the real wiring is exercised end-to-end by the
// per-handler tests.

func TestSetDriveFileRepo(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetDriveFileRepo(testutil.NewMockDriveFileRepository())
}

func TestSetAdminDB(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetAdminDB(nil)
}

// --- AccountsCreate ---

func TestAccountsCreate_InitialSetup(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	// rootUserId=nil → 初回セットアップ
	rec := doPost(h.AccountsCreate, `{"username":"admin","password":"pass123"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "admin", resp["username"])
	assert.NotEmpty(t, resp["token"])

	// rootUserId が設定された
	assert.NotNil(t, metaRepo.Meta.RootUserID)
}

func TestAccountsCreate_NotInitialSetup_RequiresAdmin(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rootID := "root1"
	metaRepo.Meta.RootUserID = &rootID

	// 認証なし → ACCESS_DENIED
	rec := doPost(h.AccountsCreate, `{"username":"user2","password":"pass"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAccountsCreate_AsRootUser(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rootID := "root1"
	metaRepo.Meta.RootUserID = &rootID

	rootUser := &model.User{ID: "root1", Username: "root"}
	rec := doPost(h.AccountsCreate, `{"username":"user2","password":"pass"}`, rootUser)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// upstream 2026.7.0 (#17783): root 本人でなくても administrator role を持つ
// user はアカウント作成 API を使える。
func TestAccountsCreate_AsNonRootAdministrator_Allowed(t *testing.T) {
	h, userRepo, metaRepo, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	rootID := "root1"
	metaRepo.Meta.RootUserID = &rootID
	userRepo.Users["adm1"] = &model.User{ID: "adm1", Username: "adm"}
	roleRepo.Roles["admrole"] = &model.Role{ID: "admrole", Name: "Admin", IsAdministrator: true}
	assignRepo.Assignments["adm1:admrole"] = &model.RoleAssignment{ID: "a1", UserID: "adm1", RoleID: "admrole"}

	rec := doPost(h.AccountsCreate, `{"username":"user2","password":"pass"}`, userRepo.Users["adm1"])
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAccountsCreate_AsNonRoot_Denied(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rootID := "root1"
	metaRepo.Meta.RootUserID = &rootID

	otherUser := &model.User{ID: "other", Username: "other"}
	rec := doPost(h.AccountsCreate, `{"username":"user2","password":"pass"}`, otherUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAccountsCreate_InvalidJSON(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.AccountsCreate, `invalid`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAccountsCreate_DuplicateUsername(t *testing.T) {
	// **認証済み admin で叩く。** 以前はここを未認証で叩いて 409 を期待していたが、
	// それは「利用者が既に居るのに初回セットアップ扱いで到達できる」という
	// 脆弱な挙動そのものを固定していた (下の
	// TestAccountsCreate_InitialSetupClosesOnceAUserExists を参照)。
	h, userRepo, metaRepo, _ := newTestHandler(t)
	rootID := "root1"
	metaRepo.Meta.RootUserID = &rootID
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "taken", UsernameLower: "taken"}

	rootUser := &model.User{ID: "root1", Username: "root"}
	rec := doPost(h.AccountsCreate, `{"username":"taken","password":"pass"}`, rootUser)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

// 初回セットアップの窓は、ローカル利用者が 1 人でも居れば閉じる。
//
// **`meta.rootUserId` だけを条件にすると窓が閉じない。** あれを書くのは signup
// service の `isInitialSetup` 分岐だけで、公開 `/api/signup` は本番でそこを
// 通らない。運営者が登録を開放して通常の signup で最初のアカウントを作ると
// `rootUserId` は永久に NULL のままになり、**この endpoint が未認証のまま
// 開き続ける**。`update-meta` は `rootUserId` を落とすので手で閉じることも
// できない。
func TestAccountsCreate_InitialSetupClosesOnceAUserExists(t *testing.T) {
	t.Run("利用者が居れば未認証は拒否", func(t *testing.T) {
		h, userRepo, metaRepo, _ := newTestHandler(t)
		metaRepo.Meta.RootUserID = nil // 窓が開いている想定
		userRepo.Users["u1"] = &model.User{ID: "u1", Username: "someone", UsernameLower: "someone"}

		rec := doPost(h.AccountsCreate, `{"username":"attacker","password":"pass1234"}`, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
		for _, u := range userRepo.Users {
			assert.NotEqual(t, "attacker", u.UsernameLower, "アカウントが作られている")
		}
	})

	// **弾きすぎていないことを見る。** 常に拒否する実装でも上は緑になる。
	t.Run("誰も居なければ従来どおり通る", func(t *testing.T) {
		h, _, metaRepo, _ := newTestHandler(t)
		metaRepo.Meta.RootUserID = nil

		rec := doPost(h.AccountsCreate, `{"username":"firstadmin","password":"pass1234"}`, nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.NotNil(t, metaRepo.Meta.RootUserID, "rootUserId が設定されて窓が閉じる")
	})

	// 数えられないときは初回セットアップ扱いにしない (fail-closed)。
	// **ACCESS_DENIED に潰さない** — 「権限が足りない」と誤読されるため。
	t.Run("数えられなければ 500", func(t *testing.T) {
		h, userRepo, metaRepo, _ := newTestHandler(t)
		metaRepo.Meta.RootUserID = nil
		userRepo.CountLocalUsersErr = assert.AnError

		rec := doPost(h.AccountsCreate, `{"username":"x","password":"pass1234"}`, nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}

func TestAccountsCreate_MetaFetchError(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta = nil // Fetch will error
	rec := doPost(h.AccountsCreate, `{"username":"admin","password":"pass"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- ShowUser ---

func TestShowUser_Success(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	uid := idGen.Generate(time.Now())
	userRepo.Users[uid] = &model.User{ID: uid, Username: "test", IsExplorable: true, AvatarDecorations: []byte("[]")}
	userRepo.Profiles[uid] = &model.UserProfile{
		UserID:             uid,
		AutoAcceptFollowed: true,
		PublicReactions:    true,
		MutedWords:         []byte("[]"),
		HardMutedWords:     []byte("[]"),
		MutedInstances:     []byte("[]"),
	}

	rec := doPost(h.ShowUser, `{"userId":"`+uid+`"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// upstream show-user は profile / role 由来の field だけを返す。
	// id / createdAt / publicReactions / securityKeysList のような
	// UserLite / UserDetailed / MeDetailed 由来の field は含まない (#2287)。
	assert.NotContains(t, resp, "id")
	assert.NotContains(t, resp, "createdAt")
	assert.NotContains(t, resp, "publicReactions")
	assert.NotContains(t, resp, "securityKeysList")
	assert.NotContains(t, resp, "isAdmin")
	assert.NotNil(t, resp["policies"])
	assert.NotNil(t, resp["roles"])
	assert.Equal(t, true, resp["autoAcceptFollowed"])
	assert.Equal(t, false, resp["isSuspended"])
	// signins / roleAssigns は repo / assignment 未配線でも nil ではなく
	// 空配列で返る (frontend の `.map(...)` が落ちない shape compat)。
	assert.Equal(t, []any{}, resp["signins"])
	assert.Equal(t, []any{}, resp["roleAssigns"])
}

// failingSigninRepo は signin lookup が error を返すケースを再現する
// repository.SigninRepository 実装。admin/show-user の fallback 経路を突く。
type failingSigninRepo struct{}

func (failingSigninRepo) Create(*model.Signin) error { return assertError{} }
func (failingSigninRepo) ListByUserID(string, int, string, string) ([]*model.Signin, error) {
	return nil, assertError{}
}

// countingSigninRepo proves that an admin/show-user response which opts out of
// sign-in history does not merely redact the rows after querying them. The
// repository itself must remain untouched so opening the surrounding page is
// not an IP lookup.
type countingSigninRepo struct {
	calls   int
	signins []*model.Signin
}

func (*countingSigninRepo) Create(*model.Signin) error { return nil }

func (r *countingSigninRepo) ListByUserID(string, int, string, string) ([]*model.Signin, error) {
	r.calls++
	return r.signins, nil
}

// **`signins` の `ip` は `canSearchIpHistory` を持つ相手にだけ返す** (#3114)。
//
// ここは `signin` テーブルのログイン IP をそのまま返す口で、`admin/ip/*` が
// 要求する policy を通らなかった。#3104 が「IP とアカウントの対応は既定で
// モデレーターに開かない」と決めた以上、同じ種類の情報をモデレーター権限だけで
// 全件返すのは、mk-go が自分で作った権限境界と食い違う。
func TestShowUser_SigninIPsRequirePolicy(t *testing.T) {
	setup := func(t *testing.T) (*apiadmin.Handler, string, string, *testutil.MockMetaRepository, *testutil.MockRoleRepository, *testutil.MockRoleAssignmentRepository, *stubAuditRepo) {
		t.Helper()
		h, userRepo, metaRepo, roleRepo, assignRepo := newTestHandlerWithAssign(t)
		idGen, _ := id.NewGenerator("aidx")
		uid := idGen.Generate(time.Now())
		userRepo.Users[uid] = &model.User{ID: uid, Username: "test", AvatarDecorations: []byte("[]")}
		userRepo.Profiles[uid] = &model.UserProfile{
			UserID: uid, MutedWords: []byte("[]"), HardMutedWords: []byte("[]"), MutedInstances: []byte("[]"),
		}
		signinRepo := testutil.NewMockSigninRepository()
		sid := idGen.Generate(time.Now())
		// **`headers` に nginx が付ける IP を入れる。** これが無いと
		// `PackSignin` が `{}` を返すので、**漏れる唯一のフィールドだけが
		// テスト対象から外れる** (敵対的レビューで実測)。
		signinRepo.Signins = []*model.Signin{{ID: sid, UserID: uid, IP: "203.0.113.5", Success: true,
			Headers: []byte(`{"X-Real-Ip":["203.0.113.5"],"X-Forwarded-For":["203.0.113.5, 10.0.0.1"]}`)}}
		h.SetSigninRepo(signinRepo)
		audit := &stubAuditRepo{}
		h.SetIPLookupAudit(iplookuplog.NewService(audit, idGen))
		return h, uid, sid, metaRepo, roleRepo, assignRepo, audit
	}
	readSignin := func(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		signins, ok := resp["signins"].([]any)
		require.True(t, ok)
		require.Len(t, signins, 1)
		return signins[0].(map[string]any)
	}

	t.Run("policy が無ければ ip を伏せる", func(t *testing.T) {
		h, uid, sid, _, _, _, audit := setup(t)
		rec := doPost(h.ShowUser, `{"userId":"`+uid+`"}`, &model.User{ID: "mod1"})
		require.Equal(t, http.StatusOK, rec.Code)
		entry := readSignin(t, rec)
		// **key は消さず空文字にする。** json-schema が `ip` を
		// optional:false / nullable:false と宣言している。
		assert.Equal(t, "", entry["ip"], "policy が無い相手に IP を返している")
		// **`headers` も伏せる。** 本番構成の nginx が `X-Real-IP` /
		// `X-Forwarded-For` を必ず付けるので、`ip` だけ潰しても隣のキーで読める。
		assert.Equal(t, map[string]any{}, entry["headers"], "headers から IP が読める")
		raw, _ := json.Marshal(entry)
		assert.NotContains(t, string(raw), "203.0.113.5", "応答のどこかに IP が残っている")
		// 伏せた応答は開示が起きていないので記録しない。
		assert.Empty(t, audit.written)
		// 他の列はそのまま。
		assert.Equal(t, sid, entry["id"])
		assert.Equal(t, true, entry["success"])
		assert.NotNil(t, entry["createdAt"])
	})

	t.Run("policy を持つモデレーターには返し、監査に残す", func(t *testing.T) {
		h, uid, _, _, roleRepo, assignRepo, audit := setup(t)
		roleRepo.Roles["iprole"] = &model.Role{ID: "iprole", Name: "IP",
			Policies: datatypes.JSON([]byte(`{"canSearchIpHistory":{"useDefault":false,"priority":1,"value":true}}`))}
		assignRepo.Assignments["mod1:iprole"] = &model.RoleAssignment{ID: "as1", UserID: "mod1", RoleID: "iprole"}

		rec := doPost(h.ShowUser, `{"userId":"`+uid+`"}`, &model.User{ID: "mod1"})
		require.Equal(t, http.StatusOK, rec.Code)
		entry := readSignin(t, rec)
		assert.Equal(t, "203.0.113.5", entry["ip"])
		// policy があるときは headers もそのまま。
		assert.NotEqual(t, map[string]any{}, entry["headers"])
		// **返したときだけ記録する。**
		require.Len(t, audit.written, 1, "IP を返したのに監査に残していない")
		got := audit.written[0]
		assert.Equal(t, "mod1", got.UserID)
		assert.Equal(t, model.IPLookupKindSignins, got.Kind)
		assert.Equal(t, uid, got.TargetUserID)
		assert.Equal(t, 1, got.ResultCount)
		// 共有キャッシュに残さない (IP が載る応答なので #3106 と同じ扱い)。
		assert.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
	})

	t.Run("withSignins false なら signin を取得せず監査にも残さない", func(t *testing.T) {
		h, uid, _, _, roleRepo, assignRepo, audit := setup(t)
		roleRepo.Roles["iprole"] = &model.Role{ID: "iprole", Name: "IP",
			Policies: datatypes.JSON([]byte(`{"canSearchIpHistory":{"useDefault":false,"priority":1,"value":true}}`))}
		assignRepo.Assignments["mod1:iprole"] = &model.RoleAssignment{ID: "as1", UserID: "mod1", RoleID: "iprole"}
		repo := &countingSigninRepo{signins: []*model.Signin{{
			ID: "signin1", UserID: uid, Success: true,
		}}}
		h.SetSigninRepo(repo)

		rec := doPost(h.ShowUser, `{"userId":"`+uid+`","withSignins":false}`, &model.User{ID: "mod1"})
		require.Equal(t, http.StatusOK, rec.Code)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, []any{}, resp["signins"])
		assert.Equal(t, 0, repo.calls, "IP 欄を開いていないのに signin repository を読んでいる")
		assert.Empty(t, audit.written, "IP を返していないのに監査に残している")
		assert.Contains(t, rec.Header().Get("Cache-Control"), "no-store", "既存の管理情報の cache 保護を弱めている")
		assert.NotContains(t, rec.Body.String(), "__signinsLoaded")
	})

	t.Run("withSignins true は従来通り取得して監査に残す", func(t *testing.T) {
		h, uid, _, _, roleRepo, assignRepo, audit := setup(t)
		roleRepo.Roles["iprole"] = &model.Role{ID: "iprole", Name: "IP",
			Policies: datatypes.JSON([]byte(`{"canSearchIpHistory":{"useDefault":false,"priority":1,"value":true}}`))}
		assignRepo.Assignments["mod1:iprole"] = &model.RoleAssignment{ID: "as1", UserID: "mod1", RoleID: "iprole"}

		rec := doPost(h.ShowUser, `{"userId":"`+uid+`","withSignins":true}`, &model.User{ID: "mod1"})
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "203.0.113.5", readSignin(t, rec)["ip"])
		require.Len(t, audit.written, 1)
		assert.Equal(t, model.IPLookupKindSignins, audit.written[0].Kind)
		assert.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
	})

	// **判定できないときは伏せる側へ倒す** (fail-closed)。roleService が未配線
	// だったり、呼び出し元が特定できないときに「policy を確かめられなかった」を
	// 「持っている」に倒すと、配線忘れがそのまま IP の開示になる。
	t.Run("呼び出し元が特定できなければ伏せる", func(t *testing.T) {
		h, uid, _, _, _, _, audit := setup(t)
		rec := doPost(h.ShowUser, `{"userId":"`+uid+`"}`, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "", readSignin(t, rec)["ip"], "利用者不明でも IP を返している")
		assert.Empty(t, audit.written)
	})

	t.Run("roleService が未配線なら伏せる", func(t *testing.T) {
		// roleService を持たない handler。policy を確かめる手段が無い。
		h, userRepo, _, _ := newTestHandlerNoRoles(t)
		idGen, _ := id.NewGenerator("aidx")
		uid := idGen.Generate(time.Now())
		userRepo.Users[uid] = &model.User{ID: uid, Username: "test", AvatarDecorations: []byte("[]")}
		userRepo.Profiles[uid] = &model.UserProfile{
			UserID: uid, MutedWords: []byte("[]"), HardMutedWords: []byte("[]"), MutedInstances: []byte("[]"),
		}
		signinRepo := testutil.NewMockSigninRepository()
		signinRepo.Signins = []*model.Signin{{ID: idGen.Generate(time.Now()), UserID: uid, IP: "203.0.113.5", Success: true}}
		h.SetSigninRepo(signinRepo)

		rec := doPost(h.ShowUser, `{"userId":"`+uid+`"}`, &model.User{ID: "mod1"})
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "", readSignin(t, rec)["ip"], "roleService 未配線で IP を返している")
	})

	// **引けなかった照会は監査に残さない** (#3114 レビュー)。残すと
	// `ip_lookup_log` 上で「本当に 0 件だった」と「DB が落ちていて何も
	// 返していない」が区別できなくなる (`admin/get-user-ips` と同じ判断)。
	t.Run("signins を引けなければ監査に残さない", func(t *testing.T) {
		h, uid, _, _, roleRepo, assignRepo, audit := setup(t)
		roleRepo.Roles["iprole"] = &model.Role{ID: "iprole", Name: "IP",
			Policies: datatypes.JSON([]byte(`{"canSearchIpHistory":{"useDefault":false,"priority":1,"value":true}}`))}
		assignRepo.Assignments["mod1:iprole"] = &model.RoleAssignment{ID: "as1", UserID: "mod1", RoleID: "iprole"}
		h.SetSigninRepo(&failingSigninRepo{})

		rec := doPost(h.ShowUser, `{"userId":"`+uid+`"}`, &model.User{ID: "mod1"})
		require.Equal(t, http.StatusOK, rec.Code)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp["signins"])
		assert.Empty(t, audit.written, "引けていないのに監査に残している")
		// **内部連絡用のキーを wire へ出さない。**
		assert.NotContains(t, rec.Body.String(), "__signinsLoaded")
	})

	t.Run("管理者は policy を短絡して返る", func(t *testing.T) {
		h, uid, _, metaRepo, _, _, audit := setup(t)
		// `HasRolePolicy` は administrator を短絡する。root を管理者にする。
		root := "root1"
		metaRepo.Meta = &model.Meta{ID: "x", RootUserID: &root}
		rec := doPost(h.ShowUser, `{"userId":"`+uid+`"}`, &model.User{ID: "root1"})
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "203.0.113.5", readSignin(t, rec)["ip"])
		require.Len(t, audit.written, 1)
	})
}

func TestShowUser_SigninsErrorFallback(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	uid := idGen.Generate(time.Now())
	userRepo.Users[uid] = &model.User{ID: uid, Username: "test", AvatarDecorations: []byte("[]")}
	userRepo.Profiles[uid] = &model.UserProfile{
		UserID: uid, MutedWords: []byte("[]"), HardMutedWords: []byte("[]"), MutedInstances: []byte("[]"),
	}
	h.SetSigninRepo(failingSigninRepo{})

	rec := doPost(h.ShowUser, `{"userId":"`+uid+`"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// lookup 失敗時は空配列に fallback する。
	assert.Equal(t, []any{}, resp["signins"])
}

func TestShowUser_WithRoleAssigns(t *testing.T) {
	h, userRepo, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	idGen, _ := id.NewGenerator("aidx")
	uid := idGen.Generate(time.Now())
	userRepo.Users[uid] = &model.User{ID: uid, Username: "test", AvatarDecorations: []byte("[]")}
	userRepo.Profiles[uid] = &model.UserProfile{
		UserID: uid, MutedWords: []byte("[]"), HardMutedWords: []byte("[]"), MutedInstances: []byte("[]"),
	}

	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Active"}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "Expired"}
	future := time.Now().Add(time.Hour)
	expired := time.Now().Add(-time.Hour)
	aid := idGen.Generate(time.Now())
	assignRepo.Assignments[uid+":r1"] = &model.RoleAssignment{ID: aid, UserID: uid, RoleID: "r1", ExpiresAt: &future}
	assignRepo.Assignments[uid+":r2"] = &model.RoleAssignment{ID: "a2", UserID: uid, RoleID: "r2", ExpiresAt: &expired}

	rec := doPost(h.ShowUser, `{"userId":"`+uid+`"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assigns, ok := resp["roleAssigns"].([]any)
	require.True(t, ok)
	// 期限切れ (r2) は upstream getUserAssigns と同じく除外される。
	require.Len(t, assigns, 1)
	entry := assigns[0].(map[string]any)
	assert.Equal(t, "r1", entry["roleId"])
	assert.NotNil(t, entry["createdAt"])
	assert.NotNil(t, entry["expiresAt"])
}

// admin/show-user は role policy 由来の isSilenced を返す (canPublicNote を
// 否定する role を持つ user は true)。旧実装は false 固定だった。
func TestShowUser_IsSilencedFromRolePolicy(t *testing.T) {
	h, userRepo, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	uid := "silenced1"
	userRepo.Users[uid] = &model.User{ID: uid, Username: "muted", AvatarDecorations: []byte("[]")}
	userRepo.Profiles[uid] = &model.UserProfile{
		UserID: uid, MutedWords: []byte("[]"), HardMutedWords: []byte("[]"), MutedInstances: []byte("[]"),
	}
	// canPublicNote=false の role を割り当てる → isSilenced=true。
	roleRepo.Roles["silrole"] = &model.Role{ID: "silrole", Name: "Silenced", Policies: datatypes.JSON([]byte(`{"canPublicNote":{"useDefault":false,"priority":1,"value":false}}`))}
	assignRepo.Assignments[uid+":silrole"] = &model.RoleAssignment{ID: "as1", UserID: uid, RoleID: "silrole"}

	rec := doPost(h.ShowUser, `{"userId":"`+uid+`"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["isSilenced"])
}

func TestShowUser_NotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.ShowUser, `{"userId":"ghost"}`, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestShowUser_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.ShowUser, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- ShowUsers ---

func TestShowUsers_Success(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "a"}
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "b"}

	rec := doPost(h.ShowUsers, `{"limit":10}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 2)
}

// admin/show-users が profile 取得を per-row FindProfileByUserID で
// 引いていた N+1 を FindProfilesByUserIDs 1 batch に置換した (#300 1-4)。
// 5 user 検証で per-row が 0 回、batch が 1 回 + size=5 で呼ばれることを
// 担保する。
type countingAdminUserRepo struct {
	*testutil.MockUserRepository
	findProfileByUserIDCalls    int
	findProfilesByUserIDsCalls  int
	findProfilesByUserIDsBucket int
}

func (c *countingAdminUserRepo) FindProfileByUserID(id string) (*model.UserProfile, error) {
	c.findProfileByUserIDCalls++
	return c.MockUserRepository.FindProfileByUserID(id)
}

func (c *countingAdminUserRepo) FindProfilesByUserIDs(ids []string) ([]*model.UserProfile, error) {
	c.findProfilesByUserIDsCalls++
	c.findProfilesByUserIDsBucket += len(ids)
	return c.MockUserRepository.FindProfilesByUserIDs(ids)
}

func TestShowUsers_BatchFetchesProfiles(t *testing.T) {
	repo := &countingAdminUserRepo{MockUserRepository: testutil.NewMockUserRepository()}
	for i := 0; i < 5; i++ {
		uid := fmt.Sprintf("au%d", i)
		repo.Users[uid] = &model.User{ID: uid, Username: uid}
		desc := fmt.Sprintf("d%d", i)
		repo.Profiles[uid] = &model.UserProfile{UserID: uid, Description: &desc}
	}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	idGen, _ := id.NewGenerator("aidx")
	signupSvc := signup.NewService(repo, metaRepo, idGen)
	roleSvc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	h := apiadmin.NewHandler(signupSvc, roleSvc, metaRepo, repo, idGen)

	rec := doPost(h.ShowUsers, `{"limit":10}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 5)

	assert.Equal(t, 0, repo.findProfileByUserIDCalls,
		"per-row FindProfileByUserID must not be called (N+1 must be eliminated)")
	assert.Equal(t, 1, repo.findProfilesByUserIDsCalls,
		"FindProfilesByUserIDs should be called exactly once per request")
	assert.Equal(t, 5, repo.findProfilesByUserIDsBucket,
		"all 5 user IDs should be coalesced into a single batch")
}

func TestShowUsers_WithFilter(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "a", IsSuspended: true}
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "b"}

	rec := doPost(h.ShowUsers, `{"state":"suspended","limit":10}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
}

// frontend (instance-info.vue) は admin/show-users に hostname を渡して
// 「特定リモートサーバーに属するユーザー」だけを取りに来る (#469)。
// 過去はこのフィールドが handler の req struct に無く、無視されて全
// remote が返るバグがあった。回帰防止に hostname narrowing を検証する。
func TestShowUsers_FilterByHostname(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	hostA := "a.example"
	hostB := "b.example"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Host: &hostA}
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "bob", Host: &hostB}
	userRepo.Users["u3"] = &model.User{ID: "u3", Username: "local"}

	rec := doPost(h.ShowUsers, `{"origin":"remote","hostname":"a.example","limit":10}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "alice", resp[0]["username"])
}

// --- SuspendUser / UnsuspendUser ---

func TestSuspendUser_Success(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "target"}

	rec := doPost(h.SuspendUser, `{"userId":"u1"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestSuspendUser_NotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.SuspendUser, `{"userId":"ghost"}`, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSuspendUser_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.SuspendUser, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// upstream suspend-user.ts: モデレーター role を持つ対象は凍結できない。
func TestSuspendUser_RejectsModerator(t *testing.T) {
	h, userRepo, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	userRepo.Users["mod1"] = &model.User{ID: "mod1", Username: "mod"}
	roleRepo.Roles["modrole"] = &model.Role{ID: "modrole", Name: "Mod", IsModerator: true}
	assignRepo.Assignments["mod1:modrole"] = &model.RoleAssignment{ID: "a1", UserID: "mod1", RoleID: "modrole"}

	rec := doPost(h.SuspendUser, `{"userId":"mod1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.False(t, userRepo.Users["mod1"].IsSuspended, "moderator must not be suspended")
}

// root アカウントは凍結できない。
func TestSuspendUser_RejectsRoot(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["root"] = &model.User{ID: "root", IsRoot: true}

	rec := doPost(h.SuspendUser, `{"userId":"root"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.False(t, userRepo.Users["root"].IsSuspended)
}

// IsAdministrator role (IsModerator=false / IsAdministrator=true) を持つ対象も
// 凍結できない (IsModerator が admin role を含むため)。
func TestSuspendUser_RejectsAdministrator(t *testing.T) {
	h, userRepo, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	userRepo.Users["adm1"] = &model.User{ID: "adm1", Username: "adm"}
	roleRepo.Roles["admrole"] = &model.Role{ID: "admrole", Name: "Admin", IsAdministrator: true}
	assignRepo.Assignments["adm1:admrole"] = &model.RoleAssignment{ID: "a1", UserID: "adm1", RoleID: "admrole"}

	rec := doPost(h.SuspendUser, `{"userId":"adm1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.False(t, userRepo.Users["adm1"].IsSuspended, "administrator must not be suspended")
}

func TestUnsuspendUser_Success(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", IsSuspended: true}

	rec := doPost(h.UnsuspendUser, `{"userId":"u1"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestUnsuspendUser_NotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.UnsuspendUser, `{"userId":"ghost"}`, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// --- #965: admin が target user を suspend/unsuspend したとき、target の
// 全 token cache entry を即時 invalidate する。SuspendUser / UnsuspendUser
// 成功時に UserTokenInvalidator が呼ばれることを確認する。

// stubUserTokenInvalidator captures InvalidateTokensForUser calls.
type stubUserTokenInvalidator struct {
	calls []string
}

func (s *stubUserTokenInvalidator) InvalidateTokensForUser(userID string) {
	s.calls = append(s.calls, userID)
}

func TestSuspendUser_InvalidatesTargetTokenCache(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "target"}
	inv := &stubUserTokenInvalidator{}
	h.SetUserTokenInvalidator(inv)

	rec := doPost(h.SuspendUser, `{"userId":"u1"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, []string{"u1"}, inv.calls,
		"SuspendUser 成功時は target の全 token cache を invalidate するべき")
}

func TestUnsuspendUser_InvalidatesTargetTokenCache(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", IsSuspended: true}
	inv := &stubUserTokenInvalidator{}
	h.SetUserTokenInvalidator(inv)

	rec := doPost(h.UnsuspendUser, `{"userId":"u1"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, []string{"u1"}, inv.calls,
		"UnsuspendUser 成功時も cache 内 stale isSuspended=true を消すために invalidate するべき")
}

// invalidator 未配線時は handler が panic / fail せず通常レスポンスを返す
// (test 直叩き / router 配線忘れ時の defensive)。
func TestSuspendUser_NoInvalidatorIsNoop(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "target"}
	rec := doPost(h.SuspendUser, `{"userId":"u1"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, userRepo.Users["u1"].IsSuspended,
		"invalidator 未配線でも core suspend 動作は止まらない")
}

// #1759: suspend / delete-account は対象 user の AP Delete を、unsuspend は
// Undo(Delete) を federation hook 経由で配信する。
type stubUserModFed struct {
	deleted  []string
	restored []string
}

func (s *stubUserModFed) OnUserDeleted(u *model.User)  { s.deleted = append(s.deleted, u.ID) }
func (s *stubUserModFed) OnUserRestored(u *model.User) { s.restored = append(s.restored, u.ID) }

func TestSuspendUser_DeliversAPDelete(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "target"}
	fed := &stubUserModFed{}
	h.SetUserModerationFederationHook(fed)
	rec := doPost(h.SuspendUser, `{"userId":"u1"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"u1"}, fed.deleted)
	assert.Empty(t, fed.restored)
}

func TestUnsuspendUser_DeliversUndoDelete(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", IsSuspended: true}
	fed := &stubUserModFed{}
	h.SetUserModerationFederationHook(fed)
	rec := doPost(h.UnsuspendUser, `{"userId":"u1"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"u1"}, fed.restored)
	assert.Empty(t, fed.deleted)
}

func TestDeleteAccount_DeliversAPDelete(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "target"}
	fed := &stubUserModFed{}
	h.SetUserModerationFederationHook(fed)
	rec := doPost(h.DeleteAccount, `{"userId":"u1"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"u1"}, fed.deleted)
}

// #1759: suspend は双方向 followRequest を削除し、outgoing follow を全 unfollow する
// (incoming follower は触らない、upstream unFollowAll は followerId=user のみ)。
func TestSuspendUser_CleansUpRelations(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "target"}

	followRepo := testutil.NewMockFollowingRepository()
	followRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "u1", FolloweeID: "a"}
	followRepo.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "u1", FolloweeID: "b"}
	followRepo.Followings["f3"] = &model.Following{ID: "f3", FollowerID: "other", FolloweeID: "u1"} // incoming
	h.SetFollowingRepo(followRepo)
	enq := &stubUnfollowEnqueuer{}
	h.SetUnfollowEnqueuer(enq)

	frRepo := testutil.NewMockFollowRequestRepository()
	frRepo.Requests["r1"] = &model.FollowRequest{ID: "r1", FollowerID: "u1", FolloweeID: "x"} // outgoing req
	frRepo.Requests["r2"] = &model.FollowRequest{ID: "r2", FollowerID: "y", FolloweeID: "u1"} // incoming req
	frRepo.Requests["r3"] = &model.FollowRequest{ID: "r3", FollowerID: "p", FolloweeID: "q"}  // unrelated
	h.SetFollowRequestRepo(frRepo)

	rec := doPost(h.SuspendUser, `{"userId":"u1"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	// 双方向 followRequest 削除 (無関係は残る)。
	assert.NotContains(t, frRepo.Requests, "r1")
	assert.NotContains(t, frRepo.Requests, "r2")
	assert.Contains(t, frRepo.Requests, "r3")

	// outgoing follow のみ unfollow される。本家 unFollowAll と同じく silent。
	require.Len(t, enq.pairs, 2)
	assert.Equal(t, []bool{true, true}, enq.silent)
	set := map[[2]string]bool{}
	for _, p := range enq.pairs {
		set[p] = true
	}
	assert.True(t, set[[2]string{"u1", "a"}])
	assert.True(t, set[[2]string{"u1", "b"}])
}

// SuspendUser が target を見つけられない (= UpdateUser 前に NotFound) と
// invalidate は呼ばない。404 を返した時点で cache に target の entry が
// 存在する保証もないので、空打ちを避ける。
func TestSuspendUser_NotFoundDoesNotInvalidate(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	inv := &stubUserTokenInvalidator{}
	h.SetUserTokenInvalidator(inv)

	rec := doPost(h.SuspendUser, `{"userId":"ghost"}`, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, inv.calls, "target 不在のとき invalidate は呼ばれない")
}

// --- AdminMeta / UpdateMeta ---

func TestAdminMeta_Success(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.AdminMeta, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// **admin/meta のテーマは生文字列のまま返す (#2583)。** ブランディング画面の
// テキストエリアは管理者が入力したものをそのまま出す必要があり、JSON に変換
// すると書いた形式 (JSON5 / 整形 / コメント) が失われる。本家 admin/meta.ts も
// raw を返している。変換するのは /api/meta と SSR 埋め込み meta だけ。
func TestAdminMeta_ThemeStaysRaw(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	theme := `{id:'6c445bcb',base:'dark',props:{bg:'#000'}} // 手書きのコメント`
	metaRepo.Meta = &model.Meta{ID: "x", DefaultDarkTheme: &theme}

	rec := doPost(h.AdminMeta, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, theme, resp["defaultDarkTheme"])
}

func TestAdminMeta_FetchError(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta = nil
	rec := doPost(h.AdminMeta, `{}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// fetcher 配線済なら proxyAccountId が proxy system user の ID で埋まる。
// frontend admin/settings 画面が users/show でこれを引くため必須 (#348)。
func TestAdminMeta_ProxyAccountIDFromSystemAccount(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	proxy := &model.User{ID: "u-proxy-meta", Username: "proxy.actor", UsernameLower: "proxy.actor"}
	h.SetSystemAccountFetcher(&stubSystemAccountFetcher{user: proxy})
	rec := doPost(h.AdminMeta, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"proxyAccountId":"u-proxy-meta"`)
}

// fetcher 未配線なら proxyAccountId は null (フォロー実装まで safety fallback)。
func TestAdminMeta_ProxyAccountIDNullWhenFetcherMissing(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.AdminMeta, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"proxyAccountId":null`)
}

func TestUpdateMeta_Success(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"name":"My Instance"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// upstream 2026.7.0: sensitive-detector 接続設定の integer は minimum:1
// (ajv 相当の validation)。
func TestUpdateMeta_SensitiveDetectorNumericMinimum(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)

	rec := doPost(h.UpdateMeta, `{"sensitiveMediaDetectionTimeout":0}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec = doPost(h.UpdateMeta, `{"sensitiveMediaDetectionMaxImagesPerRequest":-1}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec = doPost(h.UpdateMeta, `{"sensitiveMediaDetectionTimeout":1.5}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "non-integer must be rejected")

	// JSON null は NOT NULL 列を壊すので 400 で弾く (upstream ajv 相当)。
	rec = doPost(h.UpdateMeta, `{"sensitiveMediaDetectionTimeout":null}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "null must be rejected")

	// 空文字は null 化して「未設定」に戻す (upstream update-meta.ts)。
	apiURL := "https://old.example"
	metaRepo.Meta.SensitiveMediaDetectionAPIURL = &apiURL
	rec = doPost(h.UpdateMeta, `{"sensitiveMediaDetectionApiUrl":""}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Nil(t, metaRepo.Meta.SensitiveMediaDetectionAPIURL, "空文字は null に正規化")

	rec = doPost(h.UpdateMeta, `{"sensitiveMediaDetectionTimeout":30000,"sensitiveMediaDetectionMaxImagesPerRequest":8,"sensitiveMediaDetectionApiUrl":"https://detector.example.test"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 30000, metaRepo.Meta.SensitiveMediaDetectionTimeout)
	assert.Equal(t, 8, metaRepo.Meta.SensitiveMediaDetectionMaxImagesPerRequest)
	require.NotNil(t, metaRepo.Meta.SensitiveMediaDetectionAPIURL)
	assert.Equal(t, "https://detector.example.test", *metaRepo.Meta.SensitiveMediaDetectionAPIURL)
}

// admin/meta は sensitive-detector 4 フィールドを公開する (2026.7.0)。
func TestAdminMeta_SensitiveDetectorFields(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	apiURL := "https://detector.example.test"
	metaRepo.Meta.SensitiveMediaDetectionAPIURL = &apiURL
	metaRepo.Meta.SensitiveMediaDetectionTimeout = 60000
	metaRepo.Meta.SensitiveMediaDetectionMaxImagesPerRequest = 4
	metaRepo.Meta.URLPreviewSensitiveList = model.StringArray{"nsfw.example"}

	rec := doPost(h.AdminMeta, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, apiURL, resp["sensitiveMediaDetectionApiUrl"])
	assert.Nil(t, resp["sensitiveMediaDetectionApiKey"])
	assert.EqualValues(t, 60000, resp["sensitiveMediaDetectionTimeout"])
	assert.EqualValues(t, 4, resp["sensitiveMediaDetectionMaxImagesPerRequest"])
	assert.NotNil(t, resp["urlPreviewSensitiveList"])
}

// frontend が送る `tosUrl` alias は DB column `termsOfServiceUrl` に
// translate されて update される (#348)。
func TestUpdateMeta_TosUrlAliasIsTranslated(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"tosUrl":"https://example.test/tos"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	// mock は update 直後の値を Meta struct に反映するので
	// TermsOfServiceURL が埋まっていれば translate 成功。
	require.NotNil(t, metaRepo.Meta.TermsOfServiceURL)
	assert.Equal(t, "https://example.test/tos", *metaRepo.Meta.TermsOfServiceURL)
}

func TestUpdateMeta_SwPublickeyAliasIsTranslated(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"swPublickey":"KEY"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, metaRepo.Meta.SwPublicKey)
	assert.Equal(t, "KEY", *metaRepo.Meta.SwPublicKey)
}

// deliverSuspendedSoftware は jsonb 列。JSON decode された []any を
// coerceMetaJSONBFields が []byte に marshal して永続化できること (#1732)。
func TestUpdateMeta_DeliverSuspendedSoftware(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"deliverSuspendedSoftware":[{"software":"mastodon","versionRange":"*"}]}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotEmpty(t, metaRepo.Meta.DeliverSuspendedSoftware, "jsonb 列が永続化される")
	var entries []model.SuspendedSoftwareEntry
	require.NoError(t, json.Unmarshal(metaRepo.Meta.DeliverSuspendedSoftware, &entries))
	require.Len(t, entries, 1)
	assert.Equal(t, "mastodon", entries[0].Software)
	assert.Equal(t, "*", entries[0].VersionRange)
}

// #1846: clientOptions は object 形 jsonb 列。decoded map を coerce せず repo に
// 渡すと本番 Postgres で型不一致 500 になるため、handler→repo の経路で datatypes.JSON
// に正規化され永続化されることを確認する (deliverSuspendedSoftware と同じ動機)。
func TestUpdateMeta_ClientOptions(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"clientOptions":{"entrancePageStyle":"simple","foo":1}}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotEmpty(t, metaRepo.Meta.ClientOptions, "object 形 jsonb 列が永続化される")
	var obj map[string]any
	require.NoError(t, json.Unmarshal(metaRepo.Meta.ClientOptions, &obj))
	assert.Equal(t, "simple", obj["entrancePageStyle"])
	assert.Equal(t, float64(1), obj["foo"])
}

// #1851: clientOptions は upstream と同じく既存値に shallow merge する。partial
// update で未指定キーが消えず、incoming キーが既存値を上書きする。
func TestUpdateMeta_ClientOptionsMerge(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	// 既存 clientOptions: keep="A" (未指定で保持) / override="old" (上書き対象)。
	metaRepo.Meta.ClientOptions = datatypes.JSON([]byte(`{"keep":"A","override":"old"}`))

	rec := doPost(h.UpdateMeta, `{"clientOptions":{"override":"new","added":"B"}}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	var obj map[string]any
	require.NoError(t, json.Unmarshal(metaRepo.Meta.ClientOptions, &obj))
	assert.Equal(t, "A", obj["keep"], "未指定の既存キーは保持される")
	assert.Equal(t, "new", obj["override"], "incoming が既存キーを上書きする")
	assert.Equal(t, "B", obj["added"], "incoming の新規キーが追加される")
}

// #1851: clientOptions:null は incoming 無し = 既存維持 (upstream の
// {...existing, ...null} と同じ spread セマンティクス)。
func TestUpdateMeta_ClientOptionsNullKeepsExisting(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta.ClientOptions = datatypes.JSON([]byte(`{"keep":"A"}`))

	rec := doPost(h.UpdateMeta, `{"clientOptions":null}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	var obj map[string]any
	require.NoError(t, json.Unmarshal(metaRepo.Meta.ClientOptions, &obj))
	assert.Equal(t, "A", obj["keep"], "clientOptions:null は既存を維持する")
}

// JSON で送られてくる array は []any{...} に decode されるが、そのまま
// repo.Update に流すと lib/pq が varchar[] 列に書けず "expression is of
// type record" で UPDATE 全体が落ちる。handler 側の coerceMetaArrayFields
// が []any → model.StringArray に変換することで永続化できることを確認 (#590)。
//
// このテストは MockMetaRepository の Update が array 型を反映するよう
// 拡張した上で成立する。実 DB 側は repository/meta_test.go の
// TestMetaRepository_Update_FederationHosts でカバー。
func TestUpdateMeta_FederationHostsArray(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta,
		`{"federation":"specified","federationHosts":["allowed.example","trusted.example"]}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "specified", metaRepo.Meta.Federation)
	assert.Equal(t,
		[]string{"allowed.example", "trusted.example"},
		[]string(metaRepo.Meta.FederationHosts))
}

// blockedHosts / silencedHosts も同じ varchar[] 列なので同じ変換経路を
// 通す。代表的な host モデレーション設定をすべて 1 リクエストで保存する
// 統合テスト。
func TestUpdateMeta_HostListArrays(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta,
		`{"blockedHosts":["bad.example"],"silencedHosts":["noisy.example"]}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"bad.example"}, []string(metaRepo.Meta.BlockedHosts))
	assert.Equal(t, []string{"noisy.example"}, []string(metaRepo.Meta.SilencedHosts))
}

// 空配列も正しく永続化される (= リスト解除動作)。空 []any はゼロ要素の
// model.StringArray に変換される必要がある。
func TestUpdateMeta_EmptyHostArrayClearsList(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	// 事前に値を持たせる
	metaRepo.Meta.BlockedHosts = []string{"oldblock.example"}

	rec := doPost(h.UpdateMeta, `{"blockedHosts":[]}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, []string(metaRepo.Meta.BlockedHosts))
}

// PR #1108 sweep: upstream enum 制約付き field の silent corruption 防止。
// sensitiveMediaDetection が enum 外の値で送られたら 400 reject。
func TestUpdateMeta_InvalidSensitiveMediaDetectionRejected(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta.SensitiveMediaDetection = "none"

	rec := doPost(h.UpdateMeta, `{"sensitiveMediaDetection":"purple"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// 不正値は DB に書かれない
	assert.Equal(t, "none", metaRepo.Meta.SensitiveMediaDetection)
}

// federation が drastic な network 遮断を引き起こす enum なので silent
// fallback を絶対に許さない (admin の typo で 全 federation が止まる
// 事故を防ぐ)。
func TestUpdateMeta_InvalidFederationRejected(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"federation":"weird"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 正常値はそのまま通る (regression guard: enum 検証が誤って正常値も
// reject していないことを確認)。
func TestUpdateMeta_ValidEnumValuesAccepted(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta,
		`{"sensitiveMediaDetection":"all","sensitiveMediaDetectionSensitivity":"high","federation":"specified","ugcVisibilityForVisitor":"local"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "all", metaRepo.Meta.SensitiveMediaDetection)
	assert.Equal(t, "high", metaRepo.Meta.SensitiveMediaDetectionSensitivity)
}

// JSON null は coerceMetaArrayFields が空配列に揃える (#590 review #2)。
// varchar[] 列は migration で NOT NULL DEFAULT '{}' なので、null を素通し
// すると real repo で制約違反になり UPDATE 全体が rollback する。admin の
// 「リスト解除」操作を確実に成功させるため、handler 側で nil → 空配列に
// coerce してから repo に渡す。
func TestUpdateMeta_NullArrayClearsList(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta.BlockedHosts = []string{"oldblock.example"}

	rec := doPost(h.UpdateMeta, `{"blockedHosts":null}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, []string(metaRepo.Meta.BlockedHosts),
		"null は coerce 後に空配列で永続化されるべき")
}

// metaArrayColumns に列挙されていない field の null は触らない (= 既存挙動
// を保つ)。例: rootUserId (nullable string) は null 渡しで本当に nil 化
// したい用途があるため、coerce が誤発火しないことを保証。
func TestUpdateMeta_NullForNonArrayColumnIsNotTouched(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rootID := "u1"
	metaRepo.Meta.RootUserID = &rootID

	// proxyAccountId は nullable string で nil 化を許容する設計。null で
	// クリアできる挙動を pre-existing テストで確認できているので、coerce
	// 後でも壊れないことを担保。
	rec := doPost(h.UpdateMeta, `{"proxyAccountId":null}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Nil(t, metaRepo.Meta.ProxyAccountID,
		"非 array 列の null は素通しされ、ポインタ列は nil 化される")
}

// Service Worker を有効化する request で keys が空なら backend が
// auto-generate して DB に persist すること (#492)。frontend からは
// toggle ON + 空欄保存で完結し、リロードすると生成済の鍵が表示される
// 想定。
func TestUpdateMeta_VAPIDAutoGenerateOnEnable(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"enableServiceWorker":true,"swPublicKey":"","swPrivateKey":""}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, metaRepo.Meta.SwPublicKey)
	require.NotNil(t, metaRepo.Meta.SwPrivateKey)
	pub, priv := *metaRepo.Meta.SwPublicKey, *metaRepo.Meta.SwPrivateKey
	assert.NotEmpty(t, pub)
	assert.NotEmpty(t, priv)
	// VAPID public key は base64url(65 byte) ≒ 87 文字。少なくとも
	// 「なんらかの長い rand 値」になっていることを sanity check する。
	assert.GreaterOrEqual(t, len(pub), 80)
	assert.GreaterOrEqual(t, len(priv), 40)
	assert.NotEqual(t, pub, priv)
}

// 既に運用者が外部生成した鍵を持っている場合は触らないこと
// (上書きすると push subscription が無効化されるため)。
func TestUpdateMeta_VAPIDDoesNotOverwriteExistingKeys(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	existingPub := "EXISTING_PUB"
	existingPriv := "EXISTING_PRIV"
	metaRepo.Meta.EnableServiceWorker = true
	metaRepo.Meta.SwPublicKey = &existingPub
	metaRepo.Meta.SwPrivateKey = &existingPriv

	rec := doPost(h.UpdateMeta, `{"enableServiceWorker":true}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, metaRepo.Meta.SwPublicKey)
	assert.Equal(t, existingPub, *metaRepo.Meta.SwPublicKey)
	assert.Equal(t, existingPriv, *metaRepo.Meta.SwPrivateKey)
}

// 明示的な JSON null で既存鍵をクリアしつつ enable=true を送ってきた
// 場合も auto-generate を発火させる (= null も "" と同じく empty 扱い)。
func TestUpdateMeta_VAPIDAutoGenerateOnNullClear(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	existingPub := "old_pub"
	existingPriv := "old_priv"
	metaRepo.Meta.EnableServiceWorker = true
	metaRepo.Meta.SwPublicKey = &existingPub
	metaRepo.Meta.SwPrivateKey = &existingPriv

	rec := doPost(h.UpdateMeta, `{"enableServiceWorker":true,"swPublicKey":null,"swPrivateKey":null}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, metaRepo.Meta.SwPublicKey)
	require.NotNil(t, metaRepo.Meta.SwPrivateKey)
	pub, priv := *metaRepo.Meta.SwPublicKey, *metaRepo.Meta.SwPrivateKey
	assert.NotEqual(t, "old_pub", pub)
	assert.NotEqual(t, "old_priv", priv)
	assert.GreaterOrEqual(t, len(pub), 80)
}

// SW 無効のまま (enable=false) で keys が空でも何も生成しない
// (= 不要な鍵をぶら下げない)。
func TestUpdateMeta_VAPIDSkipWhenSWDisabled(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"enableServiceWorker":false}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Nil(t, metaRepo.Meta.SwPublicKey)
	assert.Nil(t, metaRepo.Meta.SwPrivateKey)
}

func TestAccountsCreate_EmptyUsername(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.AccountsCreate, `{"username":"","password":"pass"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAccountsCreate_WhitespaceOnlyUsername(t *testing.T) {
	// Bindはusernameがemptyかチェックするが、空白のみはbindを通過する。
	// Signup側でTrimSpace後にemptyになり、ErrInvalidUsernameが返る。
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.AccountsCreate, `{"username":"   ","password":"pass"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAccountsCreate_PreservedUsername(t *testing.T) {
	// rootUser 済み + admin ユーザーがリクエストしている前提 (初回セットアップ
	// ではないので preservedUsernames チェックが有効)。
	h, userRepo, metaRepo, _ := newTestHandler(t)
	rootID := "root1"
	userRepo.Users[rootID] = &model.User{ID: rootID, Username: "root", UsernameLower: "root"}
	metaRepo.Meta = &model.Meta{
		ID:                 "x",
		RootUserID:         &rootID,
		PreservedUsernames: []string{"admin", "support"},
	}

	rec := doPost(h.AccountsCreate, `{"username":"admin","password":"pass"}`, &model.User{ID: rootID})
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "USED_USERNAME", errObj["code"])
}

func TestAccountsCreate_SetupPassword_ConfigSet_Matches(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetConfigSetupPassword("mysecret")
	rec := doPost(h.AccountsCreate, `{"username":"admin","password":"pass","setupPassword":"mysecret"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAccountsCreate_SetupPassword_ConfigSet_Mismatch(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetConfigSetupPassword("mysecret")
	rec := doPost(h.AccountsCreate, `{"username":"admin","password":"pass","setupPassword":"wrong"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INCORRECT_INITIAL_PASSWORD")
}

// 前方一致・後方一致では通らないことを固定する。比較を定数時間の
// subtle.ConstantTimeCompare にしてあるが、prefix 比較に書き換えても
// 「wrong」を使う上のテストは通ってしまう。
func TestAccountsCreate_SetupPassword_ConfigSet_PartialMatchRejected(t *testing.T) {
	for name, sent := range map[string]string{
		"prefix": "mysec",
		"suffix": "secret",
		"longer": "mysecretx",
		"empty":  "",
	} {
		t.Run(name, func(t *testing.T) {
			h, _, _, _ := newTestHandler(t)
			h.SetConfigSetupPassword("mysecret")
			rec := doPost(h.AccountsCreate,
				`{"username":"admin","password":"pass","setupPassword":"`+sent+`"}`, nil)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "INCORRECT_INITIAL_PASSWORD")
		})
	}
}

func TestAccountsCreate_SetupPassword_ConfigNotSet_ClientSendsNonEmpty(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// configにsetupPasswordなし、クライアントが非空値を送信 → 拒否
	rec := doPost(h.AccountsCreate, `{"username":"admin","password":"pass","setupPassword":"unexpected"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INCORRECT_INITIAL_PASSWORD")
}

func TestAccountsCreate_SetupPassword_ConfigNotSet_ClientSendsEmpty(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// configにsetupPasswordなし、クライアントもnull → OK
	rec := doPost(h.AccountsCreate, `{"username":"admin","password":"pass"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestShowUsers_InvalidJSON(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.ShowUsers, `invalid`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUnsuspendUser_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.UnsuspendUser, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Failing repo tests ---

type failingUpdateUserRepo struct {
	*testutil.MockUserRepository
}

func (f *failingUpdateUserRepo) UpdateUser(_ string, _ map[string]any) error { return assert.AnError }

type failingListUsersRepo struct {
	*testutil.MockUserRepository
}

func (f *failingListUsersRepo) ListUsers(_ model.UserListFilter) ([]*model.User, error) {
	return nil, assert.AnError
}

type failingUpdateMetaRepo struct {
	*testutil.MockMetaRepository
}

func (f *failingUpdateMetaRepo) Update(_ map[string]any) error { return assert.AnError }

func TestSuspendUser_UpdateError(t *testing.T) {
	repo := &failingUpdateUserRepo{testutil.NewMockUserRepository()}
	repo.Users["u1"] = &model.User{ID: "u1"}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	h := apiadmin.NewHandler(signup.NewService(repo, metaRepo, idGen), nil, metaRepo, repo, idGen)
	rec := doPost(h.SuspendUser, `{"userId":"u1"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestUnsuspendUser_UpdateError(t *testing.T) {
	repo := &failingUpdateUserRepo{testutil.NewMockUserRepository()}
	repo.Users["u1"] = &model.User{ID: "u1"}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	h := apiadmin.NewHandler(signup.NewService(repo, metaRepo, idGen), nil, metaRepo, repo, idGen)
	rec := doPost(h.UnsuspendUser, `{"userId":"u1"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestShowUsers_ListError(t *testing.T) {
	repo := &failingListUsersRepo{testutil.NewMockUserRepository()}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	h := apiadmin.NewHandler(signup.NewService(repo, metaRepo, idGen), nil, metaRepo, repo, idGen)
	rec := doPost(h.ShowUsers, `{"limit":10}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestUpdateMeta_UpdateError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	metaRepo := &failingUpdateMetaRepo{testutil.NewMockMetaRepository()}
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	h := apiadmin.NewHandler(signup.NewService(userRepo, metaRepo, idGen), nil, metaRepo, userRepo, idGen)
	rec := doPost(h.UpdateMeta, `{"name":"test"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestAccountsCreate_SignupInternalError(t *testing.T) {
	// User作成で失敗するrepoを使ってINTERNAL_ERRORパスをテスト
	repo := &failingUpdateUserRepo{testutil.NewMockUserRepository()}
	// Create もオーバーライド
	failCreateRepo := &struct {
		*failingUpdateUserRepo
	}{repo}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	// signupServiceのuserRepo.Createが失敗するようにする
	failRepo := &failingCreateUserRepoForAdmin{testutil.NewMockUserRepository()}
	h := apiadmin.NewHandler(signup.NewService(failRepo, metaRepo, idGen), nil, metaRepo, failRepo, idGen)
	_ = failCreateRepo // suppress unused
	rec := doPost(h.AccountsCreate, `{"username":"newuser","password":"pass"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingCreateUserRepoForAdmin struct {
	*testutil.MockUserRepository
}

func (f *failingCreateUserRepoForAdmin) Create(_ *model.User) error { return assert.AnError }

func TestUpdateMeta_InvalidJSON(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `invalid`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Roles endpoints ---

// fullRoleCreatePayload は upstream Misskey TS が paramDef で required と
// する 13 field を満たした最小 payload (#889)。**PR #1102 以降**は全 field
// が DB に persist される (旧版は color / iconUrl / target / condFormula /
// canEditMembersByModerator / policies を /dev/null に流していた)。
const fullRoleCreatePayload = `{
	"name": "Admin",
	"description": "",
	"color": null,
	"iconUrl": null,
	"target": "manual",
	"condFormula": {},
	"isPublic": true,
	"isModerator": false,
	"isAdministrator": true,
	"asBadge": false,
	"canEditMembersByModerator": false,
	"displayOrder": 0,
	"policies": {}
}`

func TestRolesCreate_Success(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	rec := doPost(h.RolesCreate, fullRoleCreatePayload, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, roleRepo.Roles, 1)
}

// PR #1102 regression guard: RolesCreate が policies / color / target /
// condFormula 等を実際に persist することを assert。旧版は受け取りつつ
// /dev/null に流していたため admin UI で設定したロール設定が反映されなかった。
func TestRolesCreate_PersistsAllFields(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	color := "#ff0000"
	icon := "https://example.com/i.png"
	payload := `{
		"name": "Cap",
		"description": "limited role",
		"color": "` + color + `",
		"iconUrl": "` + icon + `",
		"target": "conditional",
		"condFormula": {"type":"isLocal"},
		"isPublic": true,
		"isModerator": false,
		"isAdministrator": false,
		"isExplorable": true,
		"asBadge": true,
		"canEditMembersByModerator": true,
		"displayOrder": 7,
		"policies": {"canPublicNote": false, "mentionLimit": 5}
	}`
	rec := doPost(h.RolesCreate, payload, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, roleRepo.Roles, 1)
	var r *model.Role
	for _, v := range roleRepo.Roles {
		r = v
	}
	assert.Equal(t, "Cap", r.Name)
	assert.Equal(t, "limited role", r.Description)
	require.NotNil(t, r.Color)
	assert.Equal(t, color, *r.Color)
	require.NotNil(t, r.IconURL)
	assert.Equal(t, icon, *r.IconURL)
	assert.Equal(t, model.RoleTargetConditional, r.Target)
	assert.Equal(t, true, r.IsExplorable)
	assert.Equal(t, true, r.AsBadge)
	assert.Equal(t, true, r.CanEditMembersByModerator)
	assert.Equal(t, 7, r.DisplayOrder)
	// CondFormula / Policies は JSON bytes として保存される。
	assert.JSONEq(t, `{"type":"isLocal"}`, string(r.CondFormula))
	assert.JSONEq(t, `{"canPublicNote":false,"mentionLimit":5}`, string(r.Policies))
}

func TestRolesCreate_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// 空 payload → 13 required field 不足で 400 (#889)
	rec := doPost(h.RolesCreate, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// upstream paramDef で required な field が一部欠けると 400 (#889)。
// description だけ欠けたケースを代表として検証する。
func TestRolesCreate_PartialPayloadRejected(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// description を抜いた payload
	rec := doPost(h.RolesCreate, `{
		"name": "X",
		"color": null,
		"iconUrl": null,
		"target": "manual",
		"condFormula": {},
		"isPublic": true,
		"isModerator": false,
		"isAdministrator": false,
		"asBadge": false,
		"canEditMembersByModerator": false,
		"displayOrder": 0,
		"policies": {}
	}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRolesShow_Success(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Test"}
	rec := doPost(h.RolesShow, `{"roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// admin の Role response に usersCount / createdAt / policies(default-fill) /
// target / preserveAssignmentOnMoveAccount が含まれること (旧実装は raw model で
// 欠落していた)。
func TestRolesShow_IncludesPackedFields(t *testing.T) {
	h, _, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Test", Target: model.RoleTargetManual}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}))

	rec := doPost(h.RolesShow, `{"roleId":"r1"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, float64(1), resp["usersCount"])
	assert.Contains(t, resp, "createdAt")
	assert.Contains(t, resp, "preserveAssignmentOnMoveAccount")
	assert.Equal(t, "manual", resp["target"])
	policies, ok := resp["policies"].(map[string]any)
	require.True(t, ok)
	assert.NotEmpty(t, policies, "policies は default-fill されて非空")
}

func TestRolesShow_NotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.RolesShow, `{"roleId":"ghost"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, adminNoSuchRoleJSON["show"], rec.Body.String())
}

func TestRolesShow_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.RolesShow, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRolesList_Success(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A"}
	rec := doPost(h.RolesList, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRolesUpdate_Success(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Old"}
	rec := doPost(h.RolesUpdate, `{"roleId":"r1","name":"New"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// TestRolesUpdate_BasicFieldsCompat: 旧来 5 field (name/description/
// isModerator/isAdministrator/isPublic) のみ送る payload が backward
// compatible で通ること。新規 10 field の persistence は別 test 群
// (TestRolesUpdate_Persists*) で網羅する。
func TestRolesUpdate_BasicFieldsCompat(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Old"}
	rec := doPost(h.RolesUpdate, `{"roleId":"r1","name":"New","description":"desc","isModerator":true,"isAdministrator":true,"isPublic":true}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	got := roleRepo.Roles["r1"]
	assert.Equal(t, "New", got.Name)
	assert.Equal(t, "desc", got.Description)
	assert.True(t, got.IsModerator)
	assert.True(t, got.IsAdministrator)
	assert.True(t, got.IsPublic)
}

// PR #1102 regression guard: RolesUpdate が policies を実際に persist する
// ことを assert (user 報告経路、canPublicNote が UI 上で反映されない bug)。
func TestRolesUpdate_PersistsPolicies(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Limited"}
	rec := doPost(h.RolesUpdate,
		`{"roleId":"r1","policies":{"canPublicNote":false,"ltlAvailable":false}}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	got := roleRepo.Roles["r1"]
	require.NotNil(t, got)
	assert.JSONEq(t, `{"canPublicNote":false,"ltlAvailable":false}`, string(got.Policies))
}

// PR #1102 regression guard: 追加 field (color / iconUrl / target /
// condFormula / asBadge / isExplorable / displayOrder / canEditMembersBy
// Moderator / preserveAssignmentOnMoveAccount) が全部 persist されること。
func TestRolesUpdate_PersistsAllUpstreamFields(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Old"}
	payload := `{
		"roleId":"r1",
		"color":"#abcdef",
		"iconUrl":"https://example.com/i.png",
		"target":"conditional",
		"condFormula":{"type":"isLocal"},
		"isExplorable":true,
		"asBadge":true,
		"canEditMembersByModerator":true,
		"preserveAssignmentOnMoveAccount":true,
		"displayOrder":42
	}`
	rec := doPost(h.RolesUpdate, payload, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	got := roleRepo.Roles["r1"]
	require.NotNil(t, got)
	require.NotNil(t, got.Color)
	assert.Equal(t, "#abcdef", *got.Color)
	require.NotNil(t, got.IconURL)
	assert.Equal(t, "https://example.com/i.png", *got.IconURL)
	assert.Equal(t, model.RoleTargetConditional, got.Target)
	assert.JSONEq(t, `{"type":"isLocal"}`, string(got.CondFormula))
	assert.Equal(t, true, got.IsExplorable)
	assert.Equal(t, true, got.AsBadge)
	assert.Equal(t, true, got.CanEditMembersByModerator)
	assert.Equal(t, true, got.PreserveAssignmentOnMoveAccount)
	assert.Equal(t, 42, got.DisplayOrder)
}

// upstream nullable な color / iconUrl を空文字で送ると null クリアされる。
func TestRolesUpdate_NullableColorClear(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	c := "#abcdef"
	icon := "https://example.com/i.png"
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "X", Color: &c, IconURL: &icon}
	rec := doPost(h.RolesUpdate, `{"roleId":"r1","color":"","iconUrl":""}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	got := roleRepo.Roles["r1"]
	assert.Nil(t, got.Color)
	assert.Nil(t, got.IconURL)
}

// target 不正値 ("weird" 等) は upstream Misskey TS と同じく 400 で reject。
// 旧版 (PR #1102 first commit) は silent に manual に倒していたが、frontend
// の typo で conditional role が意図せず manual に書き換わる silent
// corruption の方が深刻なので、enum validation を upstream に揃える。
func TestRolesUpdate_InvalidTargetReturns400(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "X", Target: model.RoleTargetConditional}
	rec := doPost(h.RolesUpdate, `{"roleId":"r1","target":"weird"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// gate で弾かれているので Target は元のまま (silent corruption しない)
	assert.Equal(t, model.RoleTargetConditional, roleRepo.Roles["r1"].Target)
}

// condFormula が JSON object でないと bind 段階で 400 (= request struct の
// *map[string]any に string をマップできない)。これは Go の json binding が
// 担保するので、handler 内部の json.Marshal error path は実質到達しないが、
// payload validation の shape を契約として guard する。
func TestRolesUpdate_CondFormulaNonObjectRejected(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "X"}
	rec := doPost(h.RolesUpdate, `{"roleId":"r1","condFormula":"not-an-object"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// Create 経路でも target 不正値は 400 で reject (Update 経路と shape を揃える)。
func TestRolesCreate_InvalidTargetReturns400(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	payload := `{
		"name": "Cap",
		"description": "",
		"color": null,
		"iconUrl": null,
		"target": "weird",
		"condFormula": {},
		"isPublic": false,
		"isModerator": false,
		"isAdministrator": false,
		"asBadge": false,
		"canEditMembersByModerator": false,
		"displayOrder": 0,
		"policies": {}
	}`
	rec := doPost(h.RolesCreate, payload, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// 不正 payload で role は作られない
	assert.Empty(t, roleRepo.Roles)
}

// Create でも preserveAssignmentOnMoveAccount が persist されることを assert
// (TestRolesCreate_PersistsAllFields の payload に含めていなかったため別 case で補完)。
func TestRolesCreate_PersistsPreserveAssignmentOnMoveAccount(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	payload := `{
		"name": "Sticky",
		"description": "",
		"color": null,
		"iconUrl": null,
		"target": "manual",
		"condFormula": {},
		"isPublic": false,
		"isModerator": false,
		"isAdministrator": false,
		"asBadge": false,
		"canEditMembersByModerator": false,
		"preserveAssignmentOnMoveAccount": true,
		"displayOrder": 0,
		"policies": {}
	}`
	rec := doPost(h.RolesCreate, payload, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var r *model.Role
	for _, v := range roleRepo.Roles {
		r = v
	}
	require.NotNil(t, r)
	assert.True(t, r.PreserveAssignmentOnMoveAccount)
}

func TestRolesUpdate_NotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.RolesUpdate, `{"roleId":"ghost"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, adminNoSuchRoleJSON["update"], rec.Body.String())
}

func TestRolesUpdate_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.RolesUpdate, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRolesDelete_Success(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	rec := doPost(h.RolesDelete, `{"roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestRolesDelete_NotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.RolesDelete, `{"roleId":"ghost"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, adminNoSuchRoleJSON["delete"], rec.Body.String())
}

func TestRolesDelete_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.RolesDelete, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRolesAssign_Success(t *testing.T) {
	h, userRepo, _, roleRepo := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	// canEditMembersByModerator=true の role は moderator (viewer 不問) でも
	// 付け外しできる (#1542)。happy path はこの前提で seed する。
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: true}
	rec := doPost(h.RolesAssign, `{"userId":"u1","roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// upstream assign.ts paramDef は expiresAt: type:'integer' (epoch ms)。number
// を送って 400 にならず Assign まで到達することを確認する (#1542 regression)。
func TestRolesAssign_WithExpiry(t *testing.T) {
	h, userRepo, _, roleRepo := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: true}
	// 4099-01-01 (= far future) を epoch ms で送る。
	future := time.Date(4099, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	body := fmt.Sprintf(`{"userId":"u1","roleId":"r1","expiresAt":%d}`, future)
	rec := doPost(h.RolesAssign, body, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// 過去 (現在以下) の expiresAt は upstream assign.ts:83-85 同様 no-op で 204 を
// 返し、実際の assignment は作られない (#1542)。
func TestRolesAssign_PastExpiry_NoOp(t *testing.T) {
	h, userRepo, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: true}
	past := time.Now().Add(-time.Hour).UnixMilli()
	body := fmt.Sprintf(`{"userId":"u1","roleId":"r1","expiresAt":%d}`, past)
	rec := doPost(h.RolesAssign, body, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	exists, _ := assignRepo.Exists("u1", "r1")
	assert.False(t, exists, "past expiresAt must not create an assignment")
}

func TestRolesAssign_NotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.RolesAssign, `{"userId":"u1","roleId":"ghost"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, adminNoSuchRoleJSON["assign"], rec.Body.String())
}

func TestRolesAssign_AlreadyAssigned(t *testing.T) {
	h, userRepo, _, roleRepo := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: true}
	doPost(h.RolesAssign, `{"userId":"u1","roleId":"r1"}`, nil) // first assign
	rec := doPost(h.RolesAssign, `{"userId":"u1","roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestRolesAssign_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.RolesAssign, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// upstream assign.ts:77-81: role は存在し canEdit を通るが対象 user が不在なら
// NO_SUCH_USER (#1542)。
func TestRolesAssign_NoSuchUser(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t) // userRepo は空 (u1 等 seed しない)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: true}
	rec := doPost(h.RolesAssign, `{"userId":"ghostuser","roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj, _ := resp["error"].(map[string]any)
	require.NotNil(t, errObj)
	assert.Equal(t, "NO_SUCH_USER", errObj["code"])
	assert.Equal(t, "558ea170-f653-4700-94d0-5a818371d0df", errObj["id"])
}

func TestRolesUnassign_NoSuchUser(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: true}
	rec := doPost(h.RolesUnassign, `{"userId":"ghostuser","roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj, _ := resp["error"].(map[string]any)
	require.NotNil(t, errObj)
	assert.Equal(t, "NO_SUCH_USER", errObj["code"])
	assert.Equal(t, "2b730f78-1179-461b-88ad-d24c9af1a5ce", errObj["id"])
}

func TestRolesUnassign_Success(t *testing.T) {
	h, userRepo, _, roleRepo := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: true}
	doPost(h.RolesAssign, `{"userId":"u1","roleId":"r1"}`, nil)
	rec := doPost(h.RolesUnassign, `{"userId":"u1","roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// role が存在し canEditMembersByModerator=true だが assignment が無い場合は
// NOT_ASSIGNED (404)。
func TestRolesUnassign_NotAssigned(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: true}
	rec := doPost(h.RolesUnassign, `{"userId":"u1","roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// role 自体が無ければ assign 同様 NO_SUCH_ROLE (404) を先に返す (#1542)。
func TestRolesUnassign_NoSuchRole(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.RolesUnassign, `{"userId":"u1","roleId":"ghost"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj, _ := resp["error"].(map[string]any)
	require.NotNil(t, errObj)
	assert.Equal(t, "NO_SUCH_ROLE", errObj["code"])
}

func TestRolesUnassign_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.RolesUnassign, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- canEditMembersByModerator gate (#1542) ---
//
// upstream assign.ts:74-76 / unassign.ts:76-78: moderator (非 administrator)
// は canEditMembersByModerator=false の role を assign/unassign できない。
// administrator (および root) は常に可。viewer 不明は fail-closed で拒否。

// adminViewerFixture wires a handler with an administrator viewer that owns an
// IsAdministrator role assignment.
func adminViewerFixture(t *testing.T) (*apiadmin.Handler, *testutil.MockRoleRepository, *model.User) {
	t.Helper()
	h, userRepo, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	userRepo.Users["adm"] = &model.User{ID: "adm"}
	// assign 対象の fixture user (#1542: NO_SUCH_USER 検証を通すため)。
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	roleRepo.Roles["admrole"] = &model.Role{ID: "admrole", IsAdministrator: true}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "aa1", UserID: "adm", RoleID: "admrole"}))
	return h, roleRepo, &model.User{ID: "adm"}
}

func TestRolesAssign_ModeratorGate_Denied(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	// canEditMembersByModerator=false かつ viewer が administrator でない
	// (= nil viewer) → ACCESS_DENIED。
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: false}
	rec := doPost(h.RolesAssign, `{"userId":"u1","roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj, _ := resp["error"].(map[string]any)
	require.NotNil(t, errObj)
	assert.Equal(t, "ACCESS_DENIED", errObj["code"])
	assert.Equal(t, "25b5bc31-dc79-4ebd-9bd2-c84978fd052c", errObj["id"])
}

func TestRolesUnassign_ModeratorGate_Denied(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: false}
	rec := doPost(h.RolesUnassign, `{"userId":"u1","roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj, _ := resp["error"].(map[string]any)
	require.NotNil(t, errObj)
	assert.Equal(t, "ACCESS_DENIED", errObj["code"])
	assert.Equal(t, "24636eee-e8c1-493e-94b2-e16ad401e262", errObj["id"])
}

// administrator は canEditMembersByModerator=false の role でも assign できる。
func TestRolesAssign_Administrator_BypassesGate(t *testing.T) {
	h, roleRepo, adm := adminViewerFixture(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: false}
	rec := doPost(h.RolesAssign, `{"userId":"u1","roleId":"r1"}`, adm)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// administrator は canEditMembersByModerator=false の role でも unassign できる。
func TestRolesUnassign_Administrator_BypassesGate(t *testing.T) {
	h, roleRepo, adm := adminViewerFixture(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: false}
	doPost(h.RolesAssign, `{"userId":"u1","roleId":"r1"}`, adm)
	rec := doPost(h.RolesUnassign, `{"userId":"u1","roleId":"r1"}`, adm)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// rolesUsersFixture wires user / role / assignment 用の handler を組み立てる。
// 個別 test で Service.ListByRole の戻りに手を入れたいので、Mock 系を直接
// 受け渡せる関数として切り出している (newTestHandler は roleRepo しか返さない)。
func rolesUsersFixture(t *testing.T) (
	*apiadmin.Handler,
	*testutil.MockUserRepository,
	*testutil.MockRoleRepository,
	*testutil.MockRoleAssignmentRepository,
) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	// MockRoleAssignmentRepository は UserRepo を持っていれば
	// ListByRole の戻りに User を埋めてくれる (handler が a.User を見るため)。
	assignRepo.UserRepo = userRepo
	idGen, _ := id.NewGenerator("aidx")
	signupSvc := signup.NewService(userRepo, metaRepo, idGen)
	roleSvc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	h := apiadmin.NewHandler(signupSvc, roleSvc, metaRepo, userRepo, idGen)
	return h, userRepo, roleRepo, assignRepo
}

func TestRolesUsers_Success_ReturnsAssignmentEnvelope(t *testing.T) {
	h, userRepo, roleRepo, assignRepo := rolesUsersFixture(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	require.NoError(t, userRepo.Create(&model.User{ID: "u1", Username: "alice"}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "9c2bw9q5fa0000000000000000", UserID: "u1", RoleID: "r1"}))

	rec := doPost(h.RolesUsers, `{"roleId":"r1","limit":10}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "9c2bw9q5fa0000000000000000", resp[0]["id"])
	assert.NotEmpty(t, resp[0]["createdAt"])
	user, _ := resp[0]["user"].(map[string]any)
	require.NotNil(t, user)
	assert.Equal(t, "u1", user["id"])
	assert.Equal(t, "alice", user["username"])
	// upstream users.ts は expiresAt を含める。期限なしは null (#1542)。
	require.Contains(t, resp[0], "expiresAt")
	assert.Nil(t, resp[0]["expiresAt"])
}

// #1822: admin/roles/users は UserDetailed で pack し、email など includeSecrets
// 限定 field を read:admin:roles scope に漏らさない。
func TestRolesUsers_DoesNotLeakSecrets(t *testing.T) {
	h, userRepo, roleRepo, assignRepo := rolesUsersFixture(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	require.NoError(t, userRepo.Create(&model.User{ID: "u1", Username: "alice"}))
	email := "alice@example.com"
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Email: &email, EmailVerified: true}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "9c2bw9q5fa0000000000000099", UserID: "u1", RoleID: "r1"}))

	rec := doPost(h.RolesUsers, `{"roleId":"r1","limit":10}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	user, _ := resp[0]["user"].(map[string]any)
	require.NotNil(t, user)
	assert.Equal(t, "u1", user["id"])
	_, hasEmail := user["email"]
	assert.False(t, hasEmail, "email を read:admin:roles に漏らさない")
	_, hasEmailVerified := user["emailVerified"]
	assert.False(t, hasEmailVerified, "emailVerified を漏らさない")
	_, hasSecurityKeysList := user["securityKeysList"]
	assert.False(t, hasSecurityKeysList, "securityKeysList を漏らさない")
}

// upstream users.ts:101 は expiresAt: assign.expiresAt?.toISOString() を含める (#1542)。
func TestRolesUsers_IncludesExpiresAt(t *testing.T) {
	h, userRepo, roleRepo, assignRepo := rolesUsersFixture(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	require.NoError(t, userRepo.Create(&model.User{ID: "u1", Username: "alice"}))
	exp := time.Date(4099, 1, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "9c2bw9q5fa0000000000000001", UserID: "u1", RoleID: "r1", ExpiresAt: &exp}))

	rec := doPost(h.RolesUsers, `{"roleId":"r1","limit":10}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "4099-01-02T03:04:05.000Z", resp[0]["expiresAt"])
}

func TestRolesUsers_Success_Empty(t *testing.T) {
	h, _, roleRepo, _ := rolesUsersFixture(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	rec := doPost(h.RolesUsers, `{"roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

func TestRolesUsers_LimitValidation(t *testing.T) {
	h, userRepo, roleRepo, assignRepo := rolesUsersFixture(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	// limit=0 → default 10、limit>100 → 100 にクランプされていることを 3 件 seed で確認
	for i := 0; i < 3; i++ {
		uid := fmt.Sprintf("user%d", i)
		require.NoError(t, userRepo.Create(&model.User{ID: uid}))
		// ID は ULID 風文字列。順序を保つために i を後ろに付ける
		aid := fmt.Sprintf("9c2bw9q5fa%016d", i)
		require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: aid, UserID: uid, RoleID: "r1"}))
	}
	// limit=2 で 2 件のみ。Mock 経由で repo に渡された limit も検証 (#598 review item 2)。
	rec := doPost(h.RolesUsers, `{"roleId":"r1","limit":2}`, nil)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 2)
	assert.Equal(t, 2, assignRepo.LastListByRoleLimit, "limit=2 がそのまま repo に伝わる")

	// limit=999 は範囲外なので 400 (upstream の ajv maximum)。repo は呼ばれない。
	rec = doPost(h.RolesUsers, `{"roleId":"r1","limit":999}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 2, assignRepo.LastListByRoleLimit, "拒否された request は repo に届かない")

	// limit=0 (未指定) → default 10
	rec = doPost(h.RolesUsers, `{"roleId":"r1"}`, nil)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 10, assignRepo.LastListByRoleLimit, "limit 未指定は default 10")
}

// dangling assignment (a.User == nil) は結果から落とされる + 警告ログが出る。
// ログ出力の有無は slog テスト用 handler を差し替えて捕捉する (#598 review item 1)。
func TestRolesUsers_DanglingAssignmentSkipped(t *testing.T) {
	h, userRepo, roleRepo, assignRepo := rolesUsersFixture(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	// alive user + dangling assignment (UserID は存在しない user を指す)
	require.NoError(t, userRepo.Create(&model.User{ID: "alive", Username: "alive"}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "9c2bw9q5fa0000000000000001", UserID: "alive", RoleID: "r1"}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "9c2bw9q5fa0000000000000002", UserID: "ghost", RoleID: "r1"}))

	// Test 用に slog handler を差し替えて Warn が出るかを観測。
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	rec := doPost(h.RolesUsers, `{"roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1, "dangling assignment は結果から除外される")
	user, _ := resp[0]["user"].(map[string]any)
	assert.Equal(t, "alive", user["id"])

	// dangling 検知の警告ログが出ている
	logged := buf.String()
	assert.Contains(t, logged, "dangling role assignment")
	assert.Contains(t, logged, "userId=ghost")
}

func TestRolesUsers_NotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.RolesUsers, `{"roleId":"ghost"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, adminNoSuchRoleJSON["users"], rec.Body.String())
}

func TestRolesUsers_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.RolesUsers, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingListByRoleRepo は ListByRole で error を返す stub。Service が
// repo error をそのまま伝播して handler が 500 を返す経路をカバーする。
type failingListByRoleRepo struct {
	*testutil.MockRoleAssignmentRepository
}

func (f *failingListByRoleRepo) ListByRole(_, _, _ string, _ int) ([]*model.RoleAssignment, error) {
	return nil, assert.AnError
}

func TestRolesUsers_ListError(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	assignRepo := &failingListByRoleRepo{testutil.NewMockRoleAssignmentRepository(roleRepo)}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	userRepo := testutil.NewMockUserRepository()
	idGen, _ := id.NewGenerator("aidx")
	roleSvc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	h := apiadmin.NewHandler(signup.NewService(userRepo, metaRepo, idGen), roleSvc, metaRepo, userRepo, idGen)
	rec := doPost(h.RolesUsers, `{"roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRolesUpdateDefaultPolicies_Success(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.RolesUpdateDefaultPolicies, `{"policies":{"driveCapacityMb":500}}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// upstream update-default-policies.ts は updateServerSettings moderation log を
// 記録する (#1542)。
func TestRolesUpdateDefaultPolicies_WritesModerationLog(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta = &model.Meta{ID: "x"}
	repo := attachModLog(t, h)

	rec := doPost(h.RolesUpdateDefaultPolicies, `{"policies":{"driveCapacityMb":500}}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "updateServerSettings", repo.Snapshot()[0].Type)
}

func TestRolesUpdateDefaultPolicies_UpdateError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	metaRepo := &failingUpdateMetaRepo{testutil.NewMockMetaRepository()}
	metaRepo.Meta = &model.Meta{ID: "x"}
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	idGen, _ := id.NewGenerator("aidx")
	roleSvc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	h := apiadmin.NewHandler(signup.NewService(userRepo, metaRepo, idGen), roleSvc, metaRepo, userRepo, idGen)
	rec := doPost(h.RolesUpdateDefaultPolicies, `{"policies":{"x":1}}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRolesCreate_ErrorFromService(t *testing.T) {
	// Createがエラーになるケースをテスト — failing roleRepoが必要
	// ここではfailingリポジトリでHandler直接作成
	failRepo := &failingCreateRoleRepo{testutil.NewMockRoleRepository()}
	assignRepo := testutil.NewMockRoleAssignmentRepository(failRepo.MockRoleRepository)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	roleSvc := role.NewService(failRepo, assignRepo, metaRepo, idGen)
	userRepo := testutil.NewMockUserRepository()
	h := apiadmin.NewHandler(signup.NewService(userRepo, metaRepo, idGen), roleSvc, metaRepo, userRepo, idGen)
	// #889 fullRoleCreatePayload で 13 required field を満たして 500 path
	// (= service error) を test する。
	rec := doPost(h.RolesCreate, fullRoleCreatePayload, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingCreateRoleRepo struct {
	*testutil.MockRoleRepository
}

func (f *failingCreateRoleRepo) Create(_ *model.Role) error { return assert.AnError }

func TestRolesAssign_InternalError(t *testing.T) {
	// Exists がエラーになるケースをテスト
	h, userRepo, _, roleRepo := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: true}
	// 1回目のassignは成功
	doPost(h.RolesAssign, `{"userId":"u1","roleId":"r1"}`, nil)
	// 2回目はALREADY_ASSIGNED → 409 (既にテスト済みだが念のため)
	rec := doPost(h.RolesAssign, `{"userId":"u1","roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestRolesUnassign_InternalError(t *testing.T) {
	h, userRepo, _, roleRepo := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: true}
	// 存在しないassignmentのunassign → NOT_ASSIGNED
	rec := doPost(h.RolesUnassign, `{"userId":"u1","roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

type failingListRoleRepo struct {
	*testutil.MockRoleRepository
}

func (f *failingListRoleRepo) List() ([]*model.Role, error)           { return nil, assert.AnError }
func (f *failingListRoleRepo) ListByLastUsed() ([]*model.Role, error) { return nil, assert.AnError }

func TestRolesList_Error(t *testing.T) {
	failRepo := &failingListRoleRepo{testutil.NewMockRoleRepository()}
	assignRepo := testutil.NewMockRoleAssignmentRepository(failRepo.MockRoleRepository)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	userRepo := testutil.NewMockUserRepository()
	idGen, _ := id.NewGenerator("aidx")
	roleSvc := role.NewService(failRepo, assignRepo, metaRepo, idGen)
	h := apiadmin.NewHandler(signup.NewService(userRepo, metaRepo, idGen), roleSvc, metaRepo, userRepo, idGen)
	rec := doPost(h.RolesList, `{}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingAssignExistsRepo struct {
	*testutil.MockRoleAssignmentRepository
}

func (f *failingAssignExistsRepo) Exists(_ string, _ string) (bool, error) {
	return false, assert.AnError
}

func TestRolesAssign_ExistsError(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: true}
	assignRepo := &failingAssignExistsRepo{testutil.NewMockRoleAssignmentRepository(roleRepo)}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	idGen, _ := id.NewGenerator("aidx")
	roleSvc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	h := apiadmin.NewHandler(signup.NewService(userRepo, metaRepo, idGen), roleSvc, metaRepo, userRepo, idGen)
	rec := doPost(h.RolesAssign, `{"userId":"u1","roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRolesUnassign_ExistsError(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	// gate を通すため canEditMembersByModerator=true の role を seed する
	// (role 不在だと NO_SUCH_ROLE が先に返り Exists error 経路に到達しない)。
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: true}
	assignRepo := &failingAssignExistsRepo{testutil.NewMockRoleAssignmentRepository(roleRepo)}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	idGen, _ := id.NewGenerator("aidx")
	roleSvc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	h := apiadmin.NewHandler(signup.NewService(userRepo, metaRepo, idGen), roleSvc, metaRepo, userRepo, idGen)
	rec := doPost(h.RolesUnassign, `{"userId":"u1","roleId":"r1"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRolesUpdateDefaultPolicies_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.RolesUpdateDefaultPolicies, `invalid`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Abuse Report / Moderation Log endpoints ---

func TestAbuseReports_Empty(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// abuseRepo=nil → 空配列
	rec := doPost(h.AbuseReports, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAbuseReports_WithRepo(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{ID: "r1", Comment: "spam"}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
}

// stubSystemWebhookDispatcher records DispatchSystemExcluding calls for the
// abuseReportResolved webhook tests (#1723).
type stubSystemWebhookDispatcher struct {
	calls []struct {
		eventType string
		body      any
		excludes  []string
	}
	testCalls []struct {
		webhookID      string
		eventType      string
		body           any
		overrideURL    string
		overrideSecret string
	}
}

func (s *stubSystemWebhookDispatcher) DispatchSystemExcluding(eventType string, body any, excludes []string) {
	s.calls = append(s.calls, struct {
		eventType string
		body      any
		excludes  []string
	}{eventType, body, excludes})
}

func (s *stubSystemWebhookDispatcher) DispatchSystemTest(webhookID, eventType string, body any, overrideURL, overrideSecret string) {
	s.testCalls = append(s.testCalls, struct {
		webhookID      string
		eventType      string
		body           any
		overrideURL    string
		overrideSecret string
	}{webhookID, eventType, body, overrideURL, overrideSecret})
}

// resolve は abuseReportResolved system webhook を発火し、inactive な
// notification recipient (method=webhook) の systemWebhookId を excludes に渡す
// (#1723, upstream notifySystemWebhook)。
func TestResolveAbuseReport_FiresResolvedWebhook(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	reporterHost := "remote.example"
	assigneeID := "mod1"
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{
		ID: "r1", ReporterID: "rep1", TargetUserID: "tgt1", ReporterHost: &reporterHost,
		Reporter:   &model.User{ID: "rep1", Username: "rep", Host: &reporterHost, FollowersCount: 3, Emojis: []string{"blob"}},
		AssigneeID: &assigneeID, Assignee: &model.User{ID: assigneeID, Username: "mod"},
	}
	h.SetAbuseRepo(abuseRepo)
	instanceRepo := testutil.NewMockInstanceRepository()
	instanceName := "Remote"
	instanceRepo.Instances[reporterHost] = &model.Instance{Host: reporterHost, Name: &instanceName}
	h.SetInstanceRepo(instanceRepo)
	emojiRepo := testutil.NewMockEmojiRepository()
	emojiRepo.Emojis["e1"] = &model.Emoji{ID: "e1", Name: "blob", Host: &reporterHost, PublicURL: "https://remote.example/blob.png"}
	h.SetEmojiRepo(emojiRepo)

	recipientRepo := testutil.NewMockAbuseReportNotificationRecipientRepository()
	inactiveID := "wh_inactive"
	activeID := "wh_active"
	emailInactiveID := "wh_email"
	require.NoError(t, recipientRepo.Create(&model.AbuseReportNotificationRecipient{
		ID: "rc1", Method: "webhook", IsActive: false, SystemWebhookID: &inactiveID,
	}))
	require.NoError(t, recipientRepo.Create(&model.AbuseReportNotificationRecipient{
		ID: "rc2", Method: "webhook", IsActive: true, SystemWebhookID: &activeID,
	}))
	require.NoError(t, recipientRepo.Create(&model.AbuseReportNotificationRecipient{
		ID: "rc3", Method: "email", IsActive: false, SystemWebhookID: &emailInactiveID,
	}))
	h.SetRecipientRepo(recipientRepo)

	disp := &stubSystemWebhookDispatcher{}
	h.SetSystemWebhookDispatcher(disp)

	rec := doPost(h.ResolveAbuseReport, `{"reportId":"r1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, disp.calls, 1)
	call := disp.calls[0]
	assert.Equal(t, "abuseReportResolved", call.eventType)
	// excludes は inactive な webhook recipient のみ (active webhook / email は除外しない)。
	assert.Equal(t, []string{inactiveID}, call.excludes)
	// body は packedAbuseReport。wire shape (JSON) で id / resolved を検証する
	// (packedAbuseReport は unexported のため JSON 経由で assert)。
	raw, err := json.Marshal(call.body)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.Equal(t, "r1", body["id"])
	assert.Equal(t, true, body["resolved"])
	// 管理画面の API の形ではなく、本家の Webhook の形で送る (#3260)。
	assert.Equal(t, "remote.example", body["reporterHost"])
	assert.Contains(t, body, "targetUserHost")
	reporter, ok := body["reporter"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "rep", reporter["username"])
	assert.NotContains(t, reporter, "followersCount", "利用者は UserLite で載せる")
	instance, ok := reporter["instance"].(map[string]any)
	require.True(t, ok, "リモートの通報者に instance を付ける")
	assert.Equal(t, "Remote", instance["name"])
	assert.Equal(t, map[string]any{"blob": "https://remote.example/blob.png"}, reporter["emojis"], "絵文字の URL を解決する")
	assignee, ok := body["assignee"].(map[string]any)
	require.True(t, ok, "担当者も載せる")
	assert.Equal(t, "mod", assignee["username"])
}

// dispatcher 未配線時は webhook を発火しないが resolve 自体は成功する (#1723)。
func TestResolveAbuseReport_NoDispatcherStillResolves(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{ID: "r1"}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.ResolveAbuseReport, `{"reportId":"r1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, abuseRepo.Reports["r1"].Resolved)
}

func TestResolveAbuseReport_WithRepo(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{ID: "r1"}
	h.SetAbuseRepo(abuseRepo)

	// resolvedAs 未送出 → null クリア (upstream `cw ?? text ?? ''` 同様の
	// nullable enum 挙動、PR #1108 で対応)。
	rec := doPost(h.ResolveAbuseReport, `{"reportId":"r1"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, abuseRepo.Reports["r1"].Resolved)
	assert.Nil(t, abuseRepo.Reports["r1"].ResolvedAs)
}

// resolve は assigneeId を moderator (= 呼出ユーザー) の ID に設定する
// (upstream AbuseReportService.resolve)。
func TestResolveAbuseReport_SetsAssignee(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{ID: "r1"}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.ResolveAbuseReport, `{"reportId":"r1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, abuseRepo.Reports["r1"].AssigneeID)
	assert.Equal(t, adminUser.ID, *abuseRepo.Reports["r1"].AssigneeID)
}

// PR #1108 regression guard: resolvedAs='reject' を受け付ける (旧版は
// `"accept"` を hard-code していて reject 判定が記録されなかった)。
func TestResolveAbuseReport_AcceptsReject(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{ID: "r1"}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.ResolveAbuseReport, `{"reportId":"r1","resolvedAs":"reject"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, abuseRepo.Reports["r1"].ResolvedAs)
	assert.Equal(t, "reject", *abuseRepo.Reports["r1"].ResolvedAs)
}

// resolvedAs='accept' を明示送出した場合も正しく保存される。
func TestResolveAbuseReport_AcceptsAccept(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{ID: "r1"}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.ResolveAbuseReport, `{"reportId":"r1","resolvedAs":"accept"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, abuseRepo.Reports["r1"].ResolvedAs)
	assert.Equal(t, "accept", *abuseRepo.Reports["r1"].ResolvedAs)
}

// 不正値は upstream enum check と同じく 400 reject。
func TestResolveAbuseReport_InvalidResolvedAsReturns400(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{ID: "r1"}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.ResolveAbuseReport, `{"reportId":"r1","resolvedAs":"maybe"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// 不正値で row は変更されない
	assert.False(t, abuseRepo.Reports["r1"].Resolved)
}

// 存在しない ID は 404。モックの UpdateFields は本物と同じく該当行が無くても
// エラーを返さないので、存在の確認を UpdateFields に任せると 204 になる (#3259)。
func TestResolveAbuseReport_NotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	h.SetAbuseRepo(abuseRepo)
	disp := &stubSystemWebhookDispatcher{}
	h.SetSystemWebhookDispatcher(disp)
	logs := attachModLog(t, h)

	rec := doPost(h.ResolveAbuseReport, `{"reportId":"ghost"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_ABUSE_REPORT")
	assert.Empty(t, disp.calls, "存在しない通報で abuseReportResolved を出さない")
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, logs.Snapshot(), "存在しない通報でモデレーションログを残さない")
}

// 本家 AbuseReportService.resolve と同じく resolveAbuseReport を残し、
// report には更新前の行を載せる (#3259)。
func TestResolveAbuseReport_WritesModerationLog(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{ID: "r1", ReporterID: "rep1", TargetUserID: "tgt1"}
	h.SetAbuseRepo(abuseRepo)
	logs := attachModLog(t, h)

	rec := doPost(h.ResolveAbuseReport, `{"reportId":"r1","resolvedAs":"reject"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(logs.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	log := logs.Snapshot()[0]
	assert.Equal(t, "resolveAbuseReport", log.Type)
	assert.Equal(t, adminUser.ID, log.UserID)
	var info map[string]any
	require.NoError(t, json.Unmarshal(log.Info, &info))
	assert.Equal(t, "r1", info["reportId"])
	assert.Equal(t, "reject", info["resolvedAs"])
	report, ok := info["report"].(map[string]any)
	require.True(t, ok, "report must be the report row: %v", info["report"])
	assert.Equal(t, "tgt1", report["targetUserId"])
	assert.Equal(t, false, report["resolved"], "report は更新前の行")
}

// resolvedAs の空文字と省略は、ログでも null になる (列と同じ値を残す)。
func TestResolveAbuseReport_ModerationLogResolvedAsNull(t *testing.T) {
	for _, body := range []string{`{"reportId":"r1","resolvedAs":""}`, `{"reportId":"r1"}`} {
		t.Run(body, func(t *testing.T) {
			h, _, _, _ := newTestHandler(t)
			abuseRepo := testutil.NewMockAbuseReportRepository()
			abuseRepo.Reports["r1"] = &model.AbuseUserReport{ID: "r1"}
			h.SetAbuseRepo(abuseRepo)
			logs := attachModLog(t, h)

			rec := doPost(h.ResolveAbuseReport, body, adminUser)
			require.Equal(t, http.StatusNoContent, rec.Code)
			require.Eventually(t, func() bool { return len(logs.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
			var info map[string]any
			require.NoError(t, json.Unmarshal(logs.Snapshot()[0].Info, &info))
			v, ok := info["resolvedAs"]
			require.True(t, ok, "resolvedAs のキーは残す")
			assert.Nil(t, v)
		})
	}
}

// 引いた後の更新の失敗は DB の障害なので 500 にする (#2792)。
func TestResolveAbuseReport_UpdateFailureIs500(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	inner := testutil.NewMockAbuseReportRepository()
	inner.Reports["r1"] = &model.AbuseUserReport{ID: "r1"}
	h.SetAbuseRepo(&failingAbuseUpdateRepo{MockAbuseReportRepository: inner})
	disp := &stubSystemWebhookDispatcher{}
	h.SetSystemWebhookDispatcher(disp)

	rec := doPost(h.ResolveAbuseReport, `{"reportId":"r1"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, disp.calls)
}

// failingAbuseUpdateRepo finds reports but fails every update.
type failingAbuseUpdateRepo struct {
	*testutil.MockAbuseReportRepository
}

func (r *failingAbuseUpdateRepo) UpdateFields(string, map[string]any) error {
	return errors.New("db down")
}

func TestShowModerationLogs_WithRepo(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	modLogRepo := testutil.NewMockModerationLogRepository()
	require.NoError(t, modLogRepo.Create(&model.ModerationLog{ID: "l1", Type: "suspend"}))
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetModLogService(moderationlog.New(modLogRepo, gen))

	rec := doPost(h.ShowModerationLogs, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
}

func TestAbuseReports_InvalidJSON(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.AbuseReports, `invalid`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestResolveAbuseReport_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.ResolveAbuseReport, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestResolveAbuseReport_NilRepo(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.ResolveAbuseReport, `{"reportId":"r1"}`, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestShowModerationLogs_Empty(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.ShowModerationLogs, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1539: user は生 model.User でなく packed (UserDetailed) で返す。
func TestShowModerationLogs_PacksUser(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	modLogRepo := testutil.NewMockModerationLogRepository()
	require.NoError(t, modLogRepo.Create(&model.ModerationLog{
		ID: "l1", UserID: "mod1", Type: "suspend", User: &model.User{ID: "mod1", Username: "modu"},
	}))
	gen, _ := id.NewGenerator("aidx")
	h.SetModLogService(moderationlog.New(modLogRepo, gen))
	userRepo.Profiles["mod1"] = &model.UserProfile{UserID: "mod1"}

	rec := doPost(h.ShowModerationLogs, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	user, ok := resp[0]["user"].(map[string]any)
	require.True(t, ok, "user must be a packed object, not raw model.User")
	assert.Equal(t, "mod1", user["id"])
	assert.Contains(t, user, "avatarUrl", "packed UserDetailed exposes avatarUrl")
}

// #1539: type / userId / search の絞り込み。
func TestShowModerationLogs_Filters(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	modLogRepo := testutil.NewMockModerationLogRepository()
	require.NoError(t, modLogRepo.Create(&model.ModerationLog{ID: "l1", UserID: "a", Type: "suspend", Info: []byte(`{"x":"findme"}`)}))
	require.NoError(t, modLogRepo.Create(&model.ModerationLog{ID: "l2", UserID: "b", Type: "deleteNote", Info: []byte(`{}`)}))
	gen, _ := id.NewGenerator("aidx")
	h.SetModLogService(moderationlog.New(modLogRepo, gen))

	listIDs := func(body string) []string {
		rec := doPost(h.ShowModerationLogs, body, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		ids := make([]string, 0, len(resp))
		for _, m := range resp {
			ids = append(ids, m["id"].(string))
		}
		return ids
	}
	assert.Equal(t, []string{"l1"}, listIDs(`{"type":"suspend"}`))
	assert.Equal(t, []string{"l2"}, listIDs(`{"userId":"b"}`))
	assert.Equal(t, []string{"l1"}, listIDs(`{"search":"findme"}`))
}

type failingAbuseListRepo struct {
	*testutil.MockAbuseReportRepository
}

func (f *failingAbuseListRepo) List(_ *bool, _, _, _, _ string, _ int) ([]*model.AbuseUserReport, error) {
	return nil, assert.AnError
}

func TestAbuseReports_ListError(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetAbuseRepo(&failingAbuseListRepo{testutil.NewMockAbuseReportRepository()})
	rec := doPost(h.AbuseReports, `{}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// PR for #1114 regression guard: untilId cursor を渡すと、それより小さい
// id (= 古い report) のみが返ることを確認する。旧 mk-go は untilId を無視
// していたため、frontend MkPagination が末尾検知できず無限ロードする
// 直接の root cause だった。
func TestAbuseReports_CursorUntilId_ExcludesNewerReports(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	// id DESC 順なので "r3" → "r2" → "r1" の順で返る。
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{ID: "r1"}
	abuseRepo.Reports["r2"] = &model.AbuseUserReport{ID: "r2"}
	abuseRepo.Reports["r3"] = &model.AbuseUserReport{ID: "r3"}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{"untilId":"r2"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// untilId=r2 → id < r2 のみ → r1 だけ
	require.Len(t, resp, 1)
	assert.Equal(t, "r1", resp[0]["id"])
}

// PR for #1114: sinceId cursor も同様に動く (id > sinceId)。
func TestAbuseReports_CursorSinceId_ExcludesOlderReports(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{ID: "r1"}
	abuseRepo.Reports["r2"] = &model.AbuseUserReport{ID: "r2"}
	abuseRepo.Reports["r3"] = &model.AbuseUserReport{ID: "r3"}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{"sinceId":"r2"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// sinceId=r2 → id > r2 のみ → r3 だけ
	require.Len(t, resp, 1)
	assert.Equal(t, "r3", resp[0]["id"])
}

// PR for #1114: state='unresolved' で resolved=false の report のみ返る
// (frontend 既定値、= upstream paramDef 互換)。
func TestAbuseReports_StateUnresolved_FiltersByResolved(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["unr"] = &model.AbuseUserReport{ID: "unr", Resolved: false}
	abuseRepo.Reports["res"] = &model.AbuseUserReport{ID: "res", Resolved: true}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{"state":"unresolved"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "unr", resp[0]["id"])
}

// state='resolved' で resolved=true の report のみ返る。
func TestAbuseReports_StateResolved_FiltersByResolved(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["unr"] = &model.AbuseUserReport{ID: "unr", Resolved: false}
	abuseRepo.Reports["res"] = &model.AbuseUserReport{ID: "res", Resolved: true}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{"state":"resolved"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "res", resp[0]["id"])
}

// state と legacy resolved boolean の両方が送られた場合、state を優先する。
// GoDoc に「state 優先」と明記してある契約の regression guard。
// (例: 古い admin UI が `resolved:false` をデフォルト送出しつつ、新 UI が
// `state:"resolved"` を選択した場合、state 側が勝つべき。)
func TestAbuseReports_StateOverridesLegacyResolved(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["unr"] = &model.AbuseUserReport{ID: "unr", Resolved: false}
	abuseRepo.Reports["res"] = &model.AbuseUserReport{ID: "res", Resolved: true}
	h.SetAbuseRepo(abuseRepo)

	// state='resolved' を送りつつ resolved=false も送る → state 優先で
	// resolved=true の report のみ返るべき。
	rec := doPost(h.AbuseReports, `{"state":"resolved","resolved":false}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "res", resp[0]["id"])
}

// state='all' / 空文字列は legacy `resolved` boolean に fallback。
// 互換維持の guard (= 旧 client / 既存 e2e が boolean を送ってくるケース)。
func TestAbuseReports_LegacyResolvedBool_StillRespected(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["unr"] = &model.AbuseUserReport{ID: "unr", Resolved: false}
	abuseRepo.Reports["res"] = &model.AbuseUserReport{ID: "res", Resolved: true}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{"resolved":true}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "res", resp[0]["id"])
}

// 不正 state 値は 400 で reject。silent fallback (= 全件返す) を許すと
// 「`state` の typo に気付かず全部表示される drop-in regression」が
// 静かに起きるため、PR #1102 / #1108 と同方針で明示 reject。
func TestAbuseReports_InvalidState_Rejected(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetAbuseRepo(testutil.NewMockAbuseReportRepository())
	rec := doPost(h.AbuseReports, `{"state":"bogus"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// reporterOrigin / targetUserOrigin の不正値も 400 で reject。
func TestAbuseReports_InvalidOrigin_Rejected(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetAbuseRepo(testutil.NewMockAbuseReportRepository())

	rec := doPost(h.AbuseReports, `{"reporterOrigin":"bogus"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec = doPost(h.AbuseReports, `{"targetUserOrigin":"bogus"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// PR for #1116 regression guard: response に aidx ID から派生した
// createdAt が ISO 8601 形式で含まれる。frontend MkAbuseReport.vue が
// <MkTime :time="report.createdAt"/> を直接読むため、注入が漏れると
// 「日時の解析が失敗しました。」表示になる。
//
// aidx ParseTime は stateless (= ID 先頭 8 文字を decode するだけ) なので
// test 側で生成した generator と handler 内 generator が別物でも parse 可。
func TestAbuseReports_InjectsCreatedAtFromAidx(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	aidxID := gen.Generate(time.Now())
	abuseRepo.Reports[aidxID] = &model.AbuseUserReport{ID: aidxID}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	// createdAt field が存在し ISO 8601 (= "2006-01-02T15:04:05.000Z")
	// 形式に parse 可能であること。
	ca, ok := resp[0]["createdAt"].(string)
	require.True(t, ok, "createdAt must be a string, got %T", resp[0]["createdAt"])
	_, perr := time.Parse("2006-01-02T15:04:05.000Z", ca)
	assert.NoError(t, perr, "createdAt must parse as Misskey-standard ISO 8601: %q", ca)
}

// PR for #1116: aidx 形式でない legacy ID (= 古い report 移行データ等) で
// は createdAt 派生を skip して omitempty で field 省略する。panic せず
// 他 report の表示は維持される (= defensive、ShowModerationLogs と同方針)。
func TestAbuseReports_NonAidxID_OmitsCreatedAt(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	// "legacy_xyz" は aidx 形式ではないので ParseTime が失敗する。
	abuseRepo.Reports["legacy_xyz"] = &model.AbuseUserReport{ID: "legacy_xyz"}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	// omitempty で field 自体が消える (= frontend MkTime が undefined を
	// 受けて no-render 経路に入る、「Invalid Date」回避)。
	_, has := resp[0]["createdAt"]
	assert.False(t, has, "non-aidx ID should omit createdAt entirely (omitempty)")
}

// PR for #1116: GORM Preload で nested に bind される *User
// (TargetUser / Reporter / Assignee) も packedAbuseReport の embedded
// inline で正しく JSON 出力されることを確認する。frontend MkAbuseReport.vue
// は `report.targetUser.avatarUrl` / `report.reporter.username` 等を直接
// 描画するため、embedded marshalling 経路で nested object が落ちると
// アバター / 名前等が一斉に表示されなくなる。
func TestAbuseReports_EmbeddedFieldsPreserved_NestedUsers(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{
		ID:           "r1",
		TargetUserID: "u_t",
		ReporterID:   "u_r",
		TargetUser:   &model.User{ID: "u_t", Username: "victim"},
		Reporter:     &model.User{ID: "u_r", Username: "accuser"},
	}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)

	// targetUser / reporter が UserDetailed nested object として返ること
	tu, ok := resp[0]["targetUser"].(map[string]any)
	require.True(t, ok, "targetUser must be a nested object, got %T", resp[0]["targetUser"])
	assert.Equal(t, "u_t", tu["id"])
	assert.Equal(t, "victim", tu["username"])
	// UserDetailed shape なので生 model.User の内部 field (usernameLower 等) は漏れない。
	_, leaksUsernameLower := tu["usernameLower"]
	assert.False(t, leaksUsernameLower, "raw model.User の内部 field を漏らさない")

	rep, ok := resp[0]["reporter"].(map[string]any)
	require.True(t, ok, "reporter must be a nested object, got %T", resp[0]["reporter"])
	assert.Equal(t, "u_r", rep["id"])
	assert.Equal(t, "accuser", rep["username"])

	// Assignee は未設定でも key は常に存在し null (upstream optional:false nullable:true)。
	assignee, hasAssignee := resp[0]["assignee"]
	assert.True(t, hasAssignee, "assignee key は常に存在する")
	assert.Nil(t, assignee, "未割当時は null")
}

// reporter/targetUser/assignee が UserDetailed として pack され、profile も
// 反映され、内部 host field (targetUserHost/reporterHost) を漏らさないこと。
func TestAbuseReports_PacksUsersAsUserDetailed(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	host := "remote.example"
	assignID := "u_a"
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{
		ID: "r1", TargetUserID: "u_t", ReporterID: "u_r", AssigneeID: &assignID,
		TargetUser:     &model.User{ID: "u_t", Username: "victim", AvatarDecorations: []byte("[]")},
		Reporter:       &model.User{ID: "u_r", Username: "accuser", AvatarDecorations: []byte("[]")},
		Assignee:       &model.User{ID: "u_a", Username: "mod", AvatarDecorations: []byte("[]")},
		TargetUserHost: &host,
		ReporterHost:   &host,
	}
	// targetUser の profile を seed して batch 解決 + 反映を検証。
	userRepo.Profiles["u_t"] = &model.UserProfile{
		UserID: "u_t", Description: metaStrPtr("victim bio"),
		MutedWords: []byte("[]"), HardMutedWords: []byte("[]"), MutedInstances: []byte("[]"),
	}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)

	// assignee 設定済 → UserDetailed nested object。
	assignee, ok := resp[0]["assignee"].(map[string]any)
	require.True(t, ok, "assignee must be a nested object, got %T", resp[0]["assignee"])
	assert.Equal(t, "u_a", assignee["id"])
	assert.Equal(t, "mod", assignee["username"])

	// profile (description) が targetUser に反映される (batch 解決経路)。
	tu := resp[0]["targetUser"].(map[string]any)
	assert.Equal(t, "victim bio", tu["description"])

	// 内部 host field は漏れない。
	_, hasTH := resp[0]["targetUserHost"]
	assert.False(t, hasTH, "targetUserHost は露出しない")
	_, hasRH := resp[0]["reporterHost"]
	assert.False(t, hasRH, "reporterHost は露出しない")
}

// PR for #1116: existing fields (id / resolved / targetUser 等) が
// packedAbuseReport の embedded inline で温存されることを確認する。
// embedded struct を使った JSON marshalling regression guard。
func TestAbuseReports_EmbeddedFieldsPreserved(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{
		ID:           "r1",
		TargetUserID: "u_target",
		ReporterID:   "u_reporter",
		Resolved:     true,
		Comment:      "spam content",
	}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	r := resp[0]
	assert.Equal(t, "r1", r["id"])
	assert.Equal(t, "u_target", r["targetUserId"])
	assert.Equal(t, "u_reporter", r["reporterId"])
	assert.Equal(t, true, r["resolved"])
	assert.Equal(t, "spam content", r["comment"])
}

// reporterOrigin='local' は reporterHost IS NULL のみを通す。
// handler レイヤでは mock repo にフィルタ責務を委ねるので、ここでは
// handler の origin field 受け渡しと repo 側の filter 連動を確認する。
func TestAbuseReports_OriginFilter_RoundTrip(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	remote := "remote.example.com"
	abuseRepo.Reports["loc"] = &model.AbuseUserReport{ID: "loc"}
	abuseRepo.Reports["rem"] = &model.AbuseUserReport{
		ID:             "rem",
		ReporterHost:   &remote,
		TargetUserHost: &remote,
	}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{"reporterOrigin":"local"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "loc", resp[0]["id"])

	rec = doPost(h.AbuseReports, `{"reporterOrigin":"remote"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "rem", resp[0]["id"])
}

type failingModLogListRepo struct {
	*testutil.MockModerationLogRepository
}

func (f *failingModLogListRepo) List(_ model.ModerationLogFilter) ([]*model.ModerationLog, error) {
	return nil, assert.AnError
}

func TestShowModerationLogs_ListError(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetModLogService(moderationlog.New(
		&failingModLogListRepo{testutil.NewMockModerationLogRepository()},
		gen,
	))
	rec := doPost(h.ShowModerationLogs, `{}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Emoji Admin endpoints ---

func TestEmojiAdd_Success(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	emojiRepo := testutil.NewMockEmojiRepository()
	h.SetEmojiRepo(emojiRepo)
	rec := doPost(h.EmojiAdd, `{"name":"smile","url":"https://example.com/smile.png"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestEmojiAdd_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.EmojiAdd, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestEmojiAdd_NilRepo(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.EmojiAdd, `{"name":"x","url":"u"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// captureBroadcast records PublishBroadcast calls (#2046)。
type captureBroadcast struct {
	events []string
}

func (c *captureBroadcast) PublishBroadcast(eventType string, _ any) {
	c.events = append(c.events, eventType)
}

// #2046: emoji の add/update/delete が broadcast stream へ emojiAdded/Updated/Deleted を流す。
// name 変更 update は emojiDeleted+emojiAdded。
func TestEmoji_BroadcastEvents(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	emojiRepo := testutil.NewMockEmojiRepository()
	h.SetEmojiRepo(emojiRepo)
	bc := &captureBroadcast{}
	h.SetBroadcastPublisher(bc)

	// add → emojiAdded。
	rec := doPost(h.EmojiAdd, `{"name":"smile","url":"https://example.com/s.png"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"emojiAdded"}, bc.events)

	// non-name update → emojiUpdated。
	bc.events = nil
	emojiRepo.Emojis["test@"] = &model.Emoji{ID: "e1", Name: "test"}
	rec = doPost(h.EmojiUpdate, `{"id":"e1","category":"face"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"emojiUpdated"}, bc.events)

	// name 変更 update → emojiDeleted + emojiAdded。
	bc.events = nil
	emojiRepo.Emojis["ren@"] = &model.Emoji{ID: "e2", Name: "ren"}
	rec = doPost(h.EmojiUpdate, `{"id":"e2","name":"renamed"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"emojiDeleted", "emojiAdded"}, bc.events)

	// delete → emojiDeleted。
	bc.events = nil
	emojiRepo.Emojis["del@"] = &model.Emoji{ID: "e3", Name: "del"}
	rec = doPost(h.EmojiDelete, `{"id":"e3"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"emojiDeleted"}, bc.events)
}

// #2046: bulk 操作 (set-category-bulk / delete-bulk / copy) も broadcast を流す。
func TestEmoji_BroadcastEvents_Bulk(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	emojiRepo := testutil.NewMockEmojiRepository()
	h.SetEmojiRepo(emojiRepo)
	bc := &captureBroadcast{}
	h.SetBroadcastPublisher(bc)
	emojiRepo.Emojis["a@"] = &model.Emoji{ID: "ba1", Name: "a"}
	emojiRepo.Emojis["b@"] = &model.Emoji{ID: "ba2", Name: "b"}

	// set-category-bulk → emojiUpdated (1 件にまとめる)。
	bc.events = nil
	rec := doPost(h.EmojiSetCategoryBulk, `{"ids":["ba1","ba2"],"category":"face"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"emojiUpdated"}, bc.events)

	// delete-bulk → emojiDeleted。
	bc.events = nil
	rec = doPost(h.EmojiDeleteBulk, `{"ids":["ba1","ba2"]}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"emojiDeleted"}, bc.events)

	// copy (remote → local) → emojiAdded。
	bc.events = nil
	host := "remote.example"
	emojiRepo.Emojis["srcemoji@remote.example"] = &model.Emoji{ID: "src1", Name: "srcemoji", Host: &host}
	rec = doPost(h.EmojiCopy, `{"emojiId":"src1"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"emojiAdded"}, bc.events)
}

func TestEmojiUpdate_Success(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	emojiRepo := testutil.NewMockEmojiRepository()
	emojiRepo.Emojis["test@"] = &model.Emoji{ID: "e1", Name: "test"}
	h.SetEmojiRepo(emojiRepo)
	rec := doPost(h.EmojiUpdate, `{"id":"e1","name":"updated"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestEmojiUpdate_WithAliases(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	emojiRepo := testutil.NewMockEmojiRepository()
	emojiRepo.Emojis["test@"] = &model.Emoji{ID: "e1", Name: "test"}
	h.SetEmojiRepo(emojiRepo)
	rec := doPost(h.EmojiUpdate, `{"id":"e1","name":"new","category":"faces","aliases":["smile"]}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestEmojiUpdate_NotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	emojiRepo := testutil.NewMockEmojiRepository()
	h.SetEmojiRepo(emojiRepo)
	rec := doPost(h.EmojiUpdate, `{"id":"ghost","name":"x"}`, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestEmojiUpdate_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.EmojiUpdate, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestEmojiUpdate_NilRepo(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.EmojiUpdate, `{"id":"e1"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestEmojiDelete_Success(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	emojiRepo := testutil.NewMockEmojiRepository()
	emojiRepo.Emojis["test@"] = &model.Emoji{ID: "e1", Name: "test"}
	h.SetEmojiRepo(emojiRepo)
	rec := doPost(h.EmojiDelete, `{"id":"e1"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestEmojiDelete_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.EmojiDelete, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestEmojiDelete_NilRepo(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.EmojiDelete, `{"id":"e1"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestEmojiList_Success(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	emojiRepo := testutil.NewMockEmojiRepository()
	emojiRepo.Emojis["smile@"] = &model.Emoji{ID: "e1", Name: "smile", PublicURL: "https://example.com/smile.png"}
	h.SetEmojiRepo(emojiRepo)
	rec := doPost(h.EmojiList, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "https://example.com/smile.png", rows[0]["url"])
}

func TestEmojiList_URLFallbackToOriginalUrl(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	emojiRepo := testutil.NewMockEmojiRepository()
	// publicUrl空 → originalUrlにフォールバック
	emojiRepo.Emojis["wave@"] = &model.Emoji{ID: "e2", Name: "wave", PublicURL: "", OriginalURL: "https://example.com/wave-orig.png"}
	h.SetEmojiRepo(emojiRepo)
	rec := doPost(h.EmojiList, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "https://example.com/wave-orig.png", rows[0]["url"])
}

func TestEmojiList_NilRepo(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.EmojiList, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

type failingCreateEmojiRepo struct {
	*testutil.MockEmojiRepository
}

func (f *failingCreateEmojiRepo) Create(_ *model.Emoji) error { return assert.AnError }

func TestEmojiAdd_CreateError(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetEmojiRepo(&failingCreateEmojiRepo{testutil.NewMockEmojiRepository()})
	rec := doPost(h.EmojiAdd, `{"name":"x","url":"u"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingListEmojiRepo struct {
	*testutil.MockEmojiRepository
}

func (f *failingListEmojiRepo) ListWithFilter(_, _ string, _ bool, _, _ string, _, _ int) ([]*model.Emoji, error) {
	return nil, assert.AnError
}

func TestEmojiList_Error(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetEmojiRepo(&failingListEmojiRepo{testutil.NewMockEmojiRepository()})
	rec := doPost(h.EmojiList, `{}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingDeleteEmojiRepo struct {
	*testutil.MockEmojiRepository
}

func (f *failingDeleteEmojiRepo) Delete(_ string) error { return assert.AnError }

func TestEmojiDelete_Error(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetEmojiRepo(&failingDeleteEmojiRepo{testutil.NewMockEmojiRepository()})
	rec := doPost(h.EmojiDelete, `{"id":"e1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

type failingUpdateEmojiRepo struct {
	*testutil.MockEmojiRepository
}

func (f *failingUpdateEmojiRepo) UpdateFields(_ string, _ map[string]any) error {
	return assert.AnError
}

// UpdateFields が DB 障害を返したら 500 (#2792)。**絵文字を seed するのが要点** —
// 入れずに書くと FindByID の時点で 404 になり、UpdateFields の枝に入らないまま
// 緑になる (#3014 でここを分けるまで、実際にそうなっていた)。
func TestEmojiUpdate_Error(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockEmojiRepository()
	require.NoError(t, repo.Create(&model.Emoji{ID: "e1", Name: "happy"}))
	h.SetEmojiRepo(&failingUpdateEmojiRepo{repo})
	rec := doPost(h.EmojiUpdate, `{"id":"e1","name":"x"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestEmojiList_InvalidJSON(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.EmojiList, `invalid`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShowModerationLogs_InvalidJSON(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.ShowModerationLogs, `invalid`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestShowModerationLogs_IncludesCreatedAt は frontend modlog.ModLog.vue
// が `log.createdAt` を直接読んで MkTime に渡すため、handler が aidx ID
// から派生した createdAt 文字列を必ず response に含めることを guard する。
func TestShowModerationLogs_IncludesCreatedAt(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	modLogRepo := testutil.NewMockModerationLogRepository()
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	// 既知の固定時刻で aidx ID を生成 → response の createdAt と照合
	fixedTime := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	aidxID := gen.Generate(fixedTime)
	require.NoError(t, modLogRepo.Create(&model.ModerationLog{ID: aidxID, UserID: "u1", Type: "suspend"}))
	h.SetModLogService(moderationlog.New(modLogRepo, gen))

	rec := doPost(h.ShowModerationLogs, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)

	createdAt, ok := resp[0]["createdAt"].(string)
	require.True(t, ok, "createdAt must be present and be a string")

	// Misskey の標準 format で parse 可能であること
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", createdAt)
	require.NoError(t, err, "createdAt must be parseable as Misskey-format ISO string")
	// aidx の解像度は ms なので fixedTime と完全一致するはず
	assert.WithinDuration(t, fixedTime, parsed, time.Millisecond)
}

// TestShowModerationLogs_NonAidxIDOmitsCreatedAt は aidx として parse でき
// ない legacy ID が紛れ込んだ場合に handler が createdAt を埋めずに response
// から省略する (= frontend 側で「Invalid Date」を表示しない) ことを guard
// する。
func TestShowModerationLogs_NonAidxIDOmitsCreatedAt(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	modLogRepo := testutil.NewMockModerationLogRepository()
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	// aidx の base36 で扱えない文字 ("!") を混ぜて parse 失敗を強制する
	require.NoError(t, modLogRepo.Create(&model.ModerationLog{ID: "!!!notaidx", UserID: "u1", Type: "suspend"}))
	h.SetModLogService(moderationlog.New(modLogRepo, gen))

	rec := doPost(h.ShowModerationLogs, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	_, has := resp[0]["createdAt"]
	assert.False(t, has, "createdAt must be omitted when ID cannot be parsed as aidx")
}

// TestShowModerationLogs_PreservesInfoJSON は datatypes.JSON で persist された
// info が map[string]any 経由 marshal を通っても (key 順以外) 中身を保つこと
// を guard する。frontend modlog detail dialog は info の structure を直接
// 触るため、handler 側で誤って string 化したり再 escape するような変換を
// 入れないようにする。
func TestShowModerationLogs_PreservesInfoJSON(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	modLogRepo := testutil.NewMockModerationLogRepository()
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	infoJSON := []byte(`{"userId":"target1","userUsername":"alice","reason":"spam"}`)
	require.NoError(t, modLogRepo.Create(&model.ModerationLog{
		ID:     gen.Generate(time.Now()),
		UserID: "admin1",
		Type:   "suspend",
		Info:   infoJSON,
	}))
	h.SetModLogService(moderationlog.New(modLogRepo, gen))

	rec := doPost(h.ShowModerationLogs, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	info, ok := resp[0]["info"].(map[string]any)
	require.True(t, ok, "info must be a JSON object after round-trip")
	assert.Equal(t, "target1", info["userId"])
	assert.Equal(t, "alice", info["userUsername"])
	assert.Equal(t, "spam", info["reason"])
}

// --- moderation log assertions for user moderation handlers ---

func TestSuspendUser_WritesModerationLog(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	repo := attachModLog(t, h)

	rec := doPost(h.SuspendUser, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "admin1", logs[0].UserID)
	assert.Equal(t, "suspend", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.Equal(t, "u1", info["userId"])
	assert.Equal(t, "alice", info["userUsername"])
}

func TestUnsuspendUser_WritesModerationLog(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	repo := attachModLog(t, h)

	rec := doPost(h.UnsuspendUser, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "unsuspend", repo.Snapshot()[0].Type)
}

// --- moderation log assertions for role handlers ---

func TestRolesCreate_WritesModerationLog(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := attachModLog(t, h)

	// #889 fullRoleCreatePayload で 13 required field を満たす。
	rec := doPost(h.RolesCreate, fullRoleCreatePayload, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "createRole", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.NotEmpty(t, info["roleId"])
	assert.NotNil(t, info["role"])
}

func TestRolesUpdate_WritesModerationLog(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Old"}
	repo := attachModLog(t, h)

	rec := doPost(h.RolesUpdate, `{"roleId":"r1","name":"New"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	// DB 更新も走る (#651 PR-A bonus fix)
	assert.Equal(t, "New", roleRepo.Roles["r1"].Name)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "updateRole", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.Equal(t, "r1", info["roleId"])
	require.NotNil(t, info["before"])
	require.NotNil(t, info["after"])
}

func TestRolesDelete_WritesModerationLog(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Mod"}
	repo := attachModLog(t, h)

	rec := doPost(h.RolesDelete, `{"roleId":"r1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "deleteRole", logs[0].Type)
}

func TestRolesAssign_WritesModerationLog(t *testing.T) {
	h, userRepo, _, roleRepo := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Mod", CanEditMembersByModerator: true}
	repo := attachModLog(t, h)

	rec := doPost(h.RolesAssign, `{"userId":"u1","roleId":"r1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "assignRole", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.Equal(t, "u1", info["userId"])
	assert.Equal(t, "alice", info["userUsername"])
	assert.Equal(t, "r1", info["roleId"])
	assert.Equal(t, "Mod", info["roleName"])
}

func TestUpdateMeta_WritesModerationLog(t *testing.T) {
	// #664: updateServerSettings log が before/after 込みで書かれる。
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta = &model.Meta{ID: "x"}
	repo := attachModLog(t, h)

	rec := doPost(h.UpdateMeta, `{"name":"new"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "updateServerSettings", repo.Snapshot()[0].Type)
}

func TestRolesUpdate_NoFieldsReturnsNoContentWithoutLog(t *testing.T) {
	// 全 optional pointer が nil のリクエストは log を書かずに 204 で帰る
	// (#668 review minor #2)。
	h, _, _, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Stay"}
	repo := attachModLog(t, h)

	rec := doPost(h.RolesUpdate, `{"roleId":"r1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Never(t, func() bool {
		return len(repo.Snapshot()) > 0
	}, 100*time.Millisecond, 10*time.Millisecond, "empty fields → log must not be written")
}

func TestRolesUnassign_WritesModerationLog(t *testing.T) {
	h, userRepo, _, roleRepo := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Mod", CanEditMembersByModerator: true}
	// assign を先に行ってから modlog spy を取り付け、unassign 1 件だけ観測する
	doPost(h.RolesAssign, `{"userId":"u1","roleId":"r1"}`, adminUser)
	repo := attachModLog(t, h)

	rec := doPost(h.RolesUnassign, `{"userId":"u1","roleId":"r1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "unassignRole", repo.Snapshot()[0].Type)
}

// --- #1174 H-PR7: admin/meta が model.Meta の値を返す + update-meta 正規化 ---

func metaStrPtr(s string) *string { return &s }

// AdminMeta が以前リテラル固定していた field を model.Meta から読むこと。
func TestAdminMeta_ReadsModelFields(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	h.SetServerURL("https://meta.example")
	metaRepo.Meta = &model.Meta{
		ID:                               "x",
		SingleUserMode:                   true,
		AllowExternalApRedirect:          false,
		UgcVisibilityForVisitor:          "all",
		App192IconURL:                    metaStrPtr("https://meta.example/192.png"),
		MascotImageURL:                   metaStrPtr("https://meta.example/ai.png"),
		InquiryURL:                       metaStrPtr("https://meta.example/inquiry"),
		DeeplAuthKey:                     metaStrPtr("dk"),
		DeeplIsPro:                       true,
		NotesPerOneAd:                    5,
		ManifestJSONOverride:             `{"k":1}`,
		URLPreviewEnabled:                false,
		URLPreviewTimeout:                12345,
		URLPreviewSummaryProxyURL:        metaStrPtr("https://proxy.example"),
		PerLocalUserUserTimelineCacheMax: 111,
		RemoteNotesCleaningExpiryDaysForEachNotes: 42,
		GoogleAnalyticsMeasurementID:              metaStrPtr("G-XYZ"),
	}
	rec := doPost(h.AdminMeta, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, "https://meta.example", resp["uri"])
	assert.Equal(t, true, resp["singleUserMode"])
	assert.Equal(t, false, resp["allowExternalApRedirect"])
	assert.Equal(t, "all", resp["ugcVisibilityForVisitor"])
	assert.Equal(t, "https://meta.example/192.png", resp["app192IconUrl"])
	assert.Equal(t, "https://meta.example/ai.png", resp["mascotImageUrl"])
	assert.Equal(t, "https://meta.example/inquiry", resp["inquiryUrl"])
	assert.Equal(t, "dk", resp["deeplAuthKey"])
	assert.Equal(t, true, resp["deeplIsPro"])
	assert.Equal(t, true, resp["translatorAvailable"], "deeplAuthKey != null で true")
	assert.Equal(t, float64(5), resp["notesPerOneAd"])
	assert.Equal(t, `{"k":1}`, resp["manifestJsonOverride"])
	assert.Equal(t, false, resp["urlPreviewEnabled"])
	assert.Equal(t, float64(12345), resp["urlPreviewTimeout"])
	assert.Equal(t, float64(111), resp["perLocalUserUserTimelineCacheMax"])
	assert.Equal(t, float64(42), resp["remoteNotesCleaningExpiryDaysForEachNotes"])
	assert.Equal(t, "G-XYZ", resp["googleAnalyticsMeasurementId"])
	// summalyProxy は urlPreviewSummaryProxyUrl の別名で同値。
	assert.Equal(t, "https://proxy.example", resp["urlPreviewSummaryProxyUrl"])
	assert.Equal(t, "https://proxy.example", resp["summalyProxy"])
}

// translatorAvailable は deeplAuthKey が nil なら false。
func TestAdminMeta_TranslatorAvailableFalseWhenNoKey(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta = &model.Meta{ID: "x"}
	rec := doPost(h.AdminMeta, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["translatorAvailable"])
	assert.Nil(t, resp["deeplAuthKey"])
}

// policies は DEFAULT_POLICIES と instance.policies のマージで返る。
func TestAdminMeta_PoliciesMergedWithDefaults(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta = &model.Meta{ID: "x", Policies: []byte(`{"canInvite":true,"mentionLimit":7}`)}
	rec := doPost(h.AdminMeta, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	policies, ok := resp["policies"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, policies["canInvite"], "override 反映")
	assert.Equal(t, true, policies["ltlAvailable"], "未設定 key は default")
	// 数値 policy は coerce 後 JSON 化され float64(7) として返る。
	assert.Equal(t, float64(7), policies["mentionLimit"])
}

// clientOptions / deliverSuspendedSoftware は jsonb をパースして返す。
func TestAdminMeta_JSONColumnsParsed(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta = &model.Meta{
		ID:                       "x",
		ClientOptions:            datatypes.JSON([]byte(`{"foo":"bar"}`)),
		DeliverSuspendedSoftware: datatypes.JSON([]byte(`[{"software":"x","versionRange":">=1"}]`)),
	}
	rec := doPost(h.AdminMeta, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	co := resp["clientOptions"].(map[string]any)
	assert.Equal(t, "bar", co["foo"])
	dss := resp["deliverSuspendedSoftware"].([]any)
	require.Len(t, dss, 1)
	assert.Equal(t, "x", dss[0].(map[string]any)["software"])
}

// update-meta: mcaptchaSiteKey (capital K) は DB 列 mcaptchaSitekey に alias される。
func TestUpdateMeta_McaptchaSiteKeyAlias(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"mcaptchaSiteKey":"SITEKEY"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, metaRepo.Meta.McaptchaSiteKey)
	assert.Equal(t, "SITEKEY", *metaRepo.Meta.McaptchaSiteKey)
}

// blockedHosts / federationHosts は filter(Boolean) + lowercase される。
func TestUpdateMeta_HostsLowercasedAndFiltered(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta,
		`{"blockedHosts":["BAD.Example","",  "Dup.Example"],"federation":"specified","federationHosts":["Allowed.Example"]}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"bad.example", "dup.example"}, []string(metaRepo.Meta.BlockedHosts))
	assert.Equal(t, []string{"allowed.example"}, []string(metaRepo.Meta.FederationHosts))
}

// silencedHosts は sort + dedup + 空除外 + 同一リクエストの blockedHosts 除外。
func TestUpdateMeta_SilencedHostsSortDedupExcludeBlocked(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta,
		`{"blockedHosts":["b.example"],"silencedHosts":["z.example","a.example","a.example","b.example",""]}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	// sort→[a,a,b,z(+空)]; dedup→[a,b,z]; b は blocked 除外→[a,z]。
	assert.Equal(t, []string{"a.example", "z.example"}, []string(metaRepo.Meta.SilencedHosts))
}

// 空文字列の string field は null に変換して保存される。
func TestUpdateMeta_EmptyStringBecomesNull(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta.DeeplAuthKey = metaStrPtr("existing")
	rec := doPost(h.UpdateMeta, `{"deeplAuthKey":""}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Nil(t, metaRepo.Meta.DeeplAuthKey)
}

// repositoryUrl は妥当な URL でなければ null。
func TestUpdateMeta_RepositoryUrlValidation(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"repositoryUrl":"not a url"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Nil(t, metaRepo.Meta.RepositoryURL)

	rec = doPost(h.UpdateMeta, `{"repositoryUrl":"https://github.com/x/y"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, metaRepo.Meta.RepositoryURL)
	assert.Equal(t, "https://github.com/x/y", *metaRepo.Meta.RepositoryURL)
}

// urlPreviewUserAgent は trim して空なら null。
func TestUpdateMeta_UrlPreviewUserAgentTrimEmptyToNull(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta.URLPreviewUserAgent = metaStrPtr("old")
	rec := doPost(h.UpdateMeta, `{"urlPreviewUserAgent":"   "}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Nil(t, metaRepo.Meta.URLPreviewUserAgent)
}

// summalyProxy は urlPreviewSummaryProxyUrl の別名で、trim して保存される。
func TestUpdateMeta_SummalyProxyAliasTrimmed(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"summalyProxy":"  https://p.example  "}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, metaRepo.Meta.URLPreviewSummaryProxyURL)
	assert.Equal(t, "https://p.example", *metaRepo.Meta.URLPreviewSummaryProxyURL)
}

// langs も filter(Boolean) で空文字要素を除去する (upstream update-meta.ts:447)。
func TestUpdateMeta_LangsFilteredOfEmpties(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"langs":["en","","ja"]}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"en", "ja"}, []string(metaRepo.Meta.Langs))
}

// mediaSilencedHosts も silencedHosts と同じ sort/dedup/blocked 除外を受ける。
func TestUpdateMeta_MediaSilencedHostsSortDedupExcludeBlocked(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta,
		`{"blockedHosts":["b.example"],"mediaSilencedHosts":["z.example","a.example","a.example","b.example",""]}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"a.example", "z.example"}, []string(metaRepo.Meta.MediaSilencedHosts))
}

// urlPreviewUserAgent は非空のとき trim せず原文を保存する (upstream は trim 結果で
// null 判定しつつ ps.urlPreviewUserAgent をそのまま格納)。summalyProxy の trim 保存
// との挙動差を固定する。
func TestUpdateMeta_UrlPreviewUserAgentKeepsNonEmptyUntrimmed(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"urlPreviewUserAgent":"  MyAgent  "}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, metaRepo.Meta.URLPreviewUserAgent)
	assert.Equal(t, "  MyAgent  ", *metaRepo.Meta.URLPreviewUserAgent, "非空は untrimmed 原文保存")
}

// 混合型 array (string 以外混入) は normalize でスキップされ、coerce/real repo の
// 型エラー経路で 500 になる (silent に握り潰さない)。
func TestUpdateMeta_MixedTypeArrayRejected(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"pinnedUsers":["a",1]}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// admin/meta は nil の画像/テーマ field を null で返し、public meta の
// /assets/ai.png フォールバックを誤って適用しない (parity 監査の警戒点)。
func TestAdminMeta_NilImageFieldsAreNullNotFallback(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta = &model.Meta{ID: "x"} // 全 *string nil
	rec := doPost(h.AdminMeta, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	for _, key := range []string{"mascotImageUrl", "serverErrorImageUrl", "notFoundImageUrl", "infoImageUrl", "inquiryUrl", "app192IconUrl", "app512IconUrl", "defaultLightTheme", "defaultDarkTheme"} {
		assert.Nil(t, resp[key], key+" は nil (fallback しない)")
	}
}

// --- #H-PR8a: show-user moderation fields + admin guard / show-users username ---

func adminStrPtr(s string) *string { return &s }

// show-user は profile の followedMessage / moderationNote /
// notificationRecieveConfig と user の isHibernated を実データで返す。
func TestShowUser_ModerationFieldsFromProfile(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	uid := "moduser"
	userRepo.Users[uid] = &model.User{ID: uid, Username: "mod", AvatarDecorations: []byte("[]"), IsHibernated: true}
	userRepo.Profiles[uid] = &model.UserProfile{
		UserID: uid, MutedWords: []byte("[]"), HardMutedWords: []byte("[]"), MutedInstances: []byte("[]"),
		FollowedMessage:           adminStrPtr("welcome"),
		ModerationNote:            adminStrPtr("watch this user"),
		NotificationRecieveConfig: []byte(`{"mention":{"type":"following"}}`),
	}
	rec := doPost(h.ShowUser, `{"userId":"`+uid+`"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "welcome", resp["followedMessage"])
	assert.Equal(t, "watch this user", resp["moderationNote"])
	assert.Equal(t, true, resp["isHibernated"])
	nrc, ok := resp["notificationRecieveConfig"].(map[string]any)
	require.True(t, ok)
	mention := nrc["mention"].(map[string]any)
	assert.Equal(t, "following", mention["type"])
}

// moderationNote は profile が nil/未設定でも "" を返す (TS の ?? ”)。
func TestShowUser_ModerationNoteEmptyWhenNilProfile(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	uid := "noprof"
	userRepo.Users[uid] = &model.User{ID: uid, Username: "np", AvatarDecorations: []byte("[]")}
	rec := doPost(h.ShowUser, `{"userId":"`+uid+`"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "", resp["moderationNote"])
	assert.Nil(t, resp["followedMessage"])
}

// 非 administrator (moderator) が administrator の情報を取得しようとすると 403。
func TestShowUser_AdminGuardBlocksNonAdmin(t *testing.T) {
	h, userRepo, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	target := "adminuser"
	userRepo.Users[target] = &model.User{ID: target, Username: "admin", AvatarDecorations: []byte("[]")}
	roleRepo.Roles["adminrole"] = &model.Role{ID: "adminrole", Name: "Admin", IsAdministrator: true}
	assignRepo.Assignments[target+":adminrole"] = &model.RoleAssignment{ID: "aa1", UserID: target, RoleID: "adminrole"}

	// 呼び出し元 (me) は非 admin。
	me := &model.User{ID: "modviewer", Username: "modviewer"}
	rec := doPost(h.ShowUser, `{"userId":"`+target+`"}`, me)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
}

// administrator 同士なら閲覧できる。
func TestShowUser_AdminCanViewAdmin(t *testing.T) {
	h, userRepo, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	target := "adminuser2"
	userRepo.Users[target] = &model.User{ID: target, Username: "admin2", AvatarDecorations: []byte("[]")}
	roleRepo.Roles["adminrole"] = &model.Role{ID: "adminrole", Name: "Admin", IsAdministrator: true}
	assignRepo.Assignments[target+":adminrole"] = &model.RoleAssignment{ID: "aa2", UserID: target, RoleID: "adminrole"}
	// me も admin。
	me := &model.User{ID: "adminviewer", Username: "adminviewer"}
	userRepo.Users[me.ID] = me
	assignRepo.Assignments[me.ID+":adminrole"] = &model.RoleAssignment{ID: "aa3", UserID: me.ID, RoleID: "adminrole"}

	rec := doPost(h.ShowUser, `{"userId":"`+target+`"}`, me)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// show-users の username param が usernameLower prefix フィルタとして渡る。
func TestShowUsers_UsernameFilter(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["ua"] = &model.User{ID: "ua", Username: "alice", UsernameLower: "alice"}
	userRepo.Users["ub"] = &model.User{ID: "ub", Username: "bob", UsernameLower: "bob"}
	rec := doPost(h.ShowUsers, `{"limit":100,"username":"al"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "ua", resp[0].(map[string]any)["id"])
}

// negative-control: 非admin viewer が非admin target を見るのは 200。guard の
// target 側 clause (isAdmin(target)) が live であることを固定する (F1)。
func TestShowUser_NonAdminCanViewNonAdmin(t *testing.T) {
	h, userRepo, _, _, _ := newTestHandlerWithAssign(t)
	target := "plainuser"
	userRepo.Users[target] = &model.User{ID: target, Username: "plain", AvatarDecorations: []byte("[]")}
	me := &model.User{ID: "modviewer2", Username: "modviewer2"}
	rec := doPost(h.ShowUser, `{"userId":"`+target+`"}`, me)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// root user は administrator 扱いなので非admin は閲覧不可 (meta.RootUserID 経由)。
func TestShowUser_AdminGuardBlocksViewingRoot(t *testing.T) {
	h, userRepo, metaRepo, _, _ := newTestHandlerWithAssign(t)
	root := "rootuser"
	userRepo.Users[root] = &model.User{ID: root, Username: "root", AvatarDecorations: []byte("[]")}
	metaRepo.Meta.RootUserID = &root
	me := &model.User{ID: "modviewer3", Username: "modviewer3"}
	rec := doPost(h.ShowUser, `{"userId":"`+root+`"}`, me)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// notificationRecieveConfig は profile があっても jsonb が空/未設定なら {} に fallback。
func TestShowUser_NotificationRecieveConfigEmptyFallback(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	uid := "nrcuser"
	userRepo.Users[uid] = &model.User{ID: uid, Username: "nrc", AvatarDecorations: []byte("[]")}
	userRepo.Profiles[uid] = &model.UserProfile{
		UserID: uid, MutedWords: []byte("[]"), HardMutedWords: []byte("[]"), MutedInstances: []byte("[]"),
		// NotificationRecieveConfig は未設定 (nil bytes)。
	}
	rec := doPost(h.ShowUser, `{"userId":"`+uid+`"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	nrc, ok := resp["notificationRecieveConfig"].(map[string]any)
	require.True(t, ok)
	assert.Empty(t, nrc)
}

// show-users は UserDetailed shape を返し、email/signins/roleAssigns/
// notificationRecieveConfig といった admin 専用 field を漏らさない (GUARD-1)。
func TestShowUsers_DoesNotLeakAdminOnlyFields(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	uid := "leaky"
	email := "secret@example.test"
	userRepo.Users[uid] = &model.User{ID: uid, Username: "leaky", UsernameLower: "leaky", AvatarDecorations: []byte("[]")}
	userRepo.Profiles[uid] = &model.UserProfile{
		UserID: uid, Email: &email, MutedWords: []byte("[]"), HardMutedWords: []byte("[]"), MutedInstances: []byte("[]"),
		NotificationRecieveConfig: []byte(`{"mention":{"type":"all"}}`),
	}
	rec := doPost(h.ShowUsers, `{"limit":10}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	for _, k := range []string{"email", "signins", "roleAssigns", "notificationRecieveConfig"} {
		_, present := resp[0][k]
		assert.False(t, present, k+" は show-users で露出しない")
	}
	// UserDetailed の基本 field は出る。
	assert.Equal(t, "leaky", resp[0]["username"])
}

// #2106 S1: update-meta は identity / 専用 endpoint 管轄の column (rootUserId / id /
// proxyAccountId) を書き込めない (admin→root 昇格防止)。他 meta field は従来通り更新可。
func TestUpdateMeta_IdentityColumnsCarvedOut(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	orig := "root-original"
	metaRepo.Meta.RootUserID = &orig
	rec := doPost(h.UpdateMeta, `{"rootUserId":"attacker","id":"evil","tosUrl":"https://example.test/tos"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, metaRepo.Meta.RootUserID)
	assert.Equal(t, "root-original", *metaRepo.Meta.RootUserID, "rootUserId must NOT be writable via update-meta")
	// 他 field (tosUrl→termsOfServiceUrl) は従来通り更新される。
	require.NotNil(t, metaRepo.Meta.TermsOfServiceURL)
	assert.Equal(t, "https://example.test/tos", *metaRepo.Meta.TermsOfServiceURL)
}

// #2106 S2: admin/accounts/create は app/OAuth access token (IsApp) を root でも拒否する
// (upstream の inline `token !== null` gate)。native login token のみ許可。
func TestAccountsCreate_AppTokenDenied(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rootID := "root1"
	metaRepo.Meta.RootUserID = &rootID
	rootUser := &model.User{ID: "root1", Username: "root"}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"username":"user2","password":"pass"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), rootUser)
	c.Set(string(middleware.AuthScopeContextKey), &middleware.AuthScope{IsApp: true})

	_ = h.AccountsCreate(c)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "app/OAuth token must be denied even for root")
}

// upstream admin/show-user (show-user.ts:233-261) が返す key 集合と完全に
// 一致することを固定する (#2287)。旧実装は PackUserDetailed をベースに
// UserLite / UserDetailed / MeDetailed を丸ごと返しており、upstream に無い
// id をはじめ 40 以上の余剰 field を含んでいた。
func TestShowUser_ResponseKeysMatchUpstream(t *testing.T) {
	want := []string{
		"alwaysMarkNsfw", "autoAcceptFollowed", "autoSensitive", "carefulBot",
		"email", "emailVerified", "followedMessage", "injectFeaturedNote",
		"isHibernated", "isModerator", "isSilenced", "isSuspended",
		"lastActiveDate", "moderationNote", "mutedInstances", "mutedWords",
		"noCrawle", "notificationRecieveConfig", "policies", "preventAiLearning",
		"receiveAnnouncementEmail", "roleAssigns", "roles", "signins",
	}

	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "target"}

	rec := doPost(h.ShowUser, `{"userId":"u1"}`, &model.User{ID: "admin1"})
	require.Equal(t, http.StatusOK, rec.Code)

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	gotKeys := make([]string, 0, len(got))
	for k := range got {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	sort.Strings(want)
	assert.Equal(t, want, gotKeys, "upstream に無い field を返してはいけない")
}

// #2313: 分割アップロード設定の範囲検証。chunkSize は S3 の最小パートサイズと
// リバースプロキシの上限に挟まれるので、下限・上限の両方を弾く必要がある。
// core/drive 側でも clamp するが、admin UI から入れた値が黙って別の値になるのは
// 分かりにくいので入口で明示的に拒否する。
func TestUpdateMeta_ChunkedUploadRanges(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)

	tooSmall := fmt.Sprintf(`{"chunkedUploadChunkSizeMb":%d}`, coredrive.MinChunkSizeMb-1)
	assert.Equal(t, http.StatusBadRequest, doPost(h.UpdateMeta, tooSmall, nil).Code)

	tooBig := fmt.Sprintf(`{"chunkedUploadChunkSizeMb":%d}`, coredrive.MaxChunkSizeMb+1)
	assert.Equal(t, http.StatusBadRequest, doPost(h.UpdateMeta, tooBig, nil).Code)

	assert.Equal(t, http.StatusBadRequest, doPost(h.UpdateMeta, `{"chunkedUploadChunkSizeMb":10.5}`, nil).Code, "non-integer must be rejected")
	assert.Equal(t, http.StatusBadRequest, doPost(h.UpdateMeta, `{"chunkedUploadChunkSizeMb":null}`, nil).Code, "null must be rejected")

	ttlTooSmall := fmt.Sprintf(`{"chunkedUploadSessionTtlMinutes":%d}`, coredrive.MinSessionTTLMinutes-1)
	assert.Equal(t, http.StatusBadRequest, doPost(h.UpdateMeta, ttlTooSmall, nil).Code)
	ttlTooBig := fmt.Sprintf(`{"chunkedUploadSessionTtlMinutes":%d}`, coredrive.MaxSessionTTLMinutes+1)
	assert.Equal(t, http.StatusBadRequest, doPost(h.UpdateMeta, ttlTooBig, nil).Code)

	// サーバー cap は 1 以上。
	assert.Equal(t, http.StatusBadRequest, doPost(h.UpdateMeta, `{"chunkedUploadMaxSessionsPerUser":0}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, doPost(h.UpdateMeta, `{"chunkedUploadMaxPendingMbPerUser":0}`, nil).Code)

	body := `{"chunkedUploadEnabled":true,"chunkedUploadChunkSizeMb":16,"chunkedUploadSessionTtlMinutes":120,` +
		`"chunkedUploadMaxSessionsPerUser":3,"chunkedUploadMaxPendingMbPerUser":512}`
	require.Equal(t, http.StatusNoContent, doPost(h.UpdateMeta, body, nil).Code)
	assert.True(t, metaRepo.Meta.ChunkedUploadEnabled)
	assert.Equal(t, 16, metaRepo.Meta.ChunkedUploadChunkSizeMb)
	assert.Equal(t, 120, metaRepo.Meta.ChunkedUploadSessionTTLMinutes)
	assert.Equal(t, 3, metaRepo.Meta.ChunkedUploadMaxSessionsPerUser)
	assert.Equal(t, 512, metaRepo.Meta.ChunkedUploadMaxPendingMbPerUser)
}

// admin/meta は分割アップロード設定を返す。管理画面 (オブジェクトストレージ)
// がここから値を読む。
func TestAdminMeta_ChunkedUploadFields(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta.ChunkedUploadEnabled = true
	metaRepo.Meta.ChunkedUploadChunkSizeMb = 10
	metaRepo.Meta.ChunkedUploadSessionTTLMinutes = 60
	metaRepo.Meta.ChunkedUploadMaxSessionsPerUser = 8
	metaRepo.Meta.ChunkedUploadMaxPendingMbPerUser = 2048

	rec := doPost(h.AdminMeta, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, true, resp["chunkedUploadEnabled"])
	assert.EqualValues(t, 10, resp["chunkedUploadChunkSizeMb"])
	assert.EqualValues(t, 60, resp["chunkedUploadSessionTtlMinutes"])
	assert.EqualValues(t, 8, resp["chunkedUploadMaxSessionsPerUser"])
	assert.EqualValues(t, 2048, resp["chunkedUploadMaxPendingMbPerUser"])
}

// 承認制のフラグは管理画面のトグルが読むので admin/meta に出す (#2554)。
// update-meta は generic passthrough なので書き込み側の配線は不要。
func TestAdminMeta_ApprovalRequiredForSignup(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta = &model.Meta{ID: "x", ApprovalRequiredForSignup: true}

	rec := doPost(h.AdminMeta, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"approvalRequiredForSignup":true`)
}

// 登録可否の組み合わせ (#2565 / #2803)。
//
// 承認制それ自体が /api/signup を 403 で塞ぐので、登録開放は安全性の条件ではなく
// 表示上の整合にすぎない。**「先に開放してから承認制を入れる」手順を強制すると、
// その間に素通しで登録される窓ができる**ため、承認制を入れる更新では同じ更新で
// 開放する。
//
// **外す更新では逆に閉じる (#2803)。** 開放は承認制がゲートとして立っていることが
// 前提の整合なので、ゲートが消える更新で維持すると、招待制 → 承認制 ON → 承認制 OFF
// の 3 操作で無警告の全開状態が残る。閉じる側は明示指定を尊重し (開ける側は #2565 の
// 整合の強制として上書きする)、承認制の状態が変わらない更新では触らない。
func TestUpdateMeta_SignupConditions(t *testing.T) {
	tests := []struct {
		name    string
		current *model.Meta
		body    string
		// 400 を期待するときだけ設定する。レスポンスに含まれるべき文言。
		wantRejectedMessage string
		// 更新後に期待する各値。nil なら検証しない。
		wantDisableRegistration *bool
		wantEmailRequired       *bool
		wantApprovalRequired    *bool
	}{
		{
			name:                    "招待制のまま承認制を入れると同じ更新で開放される",
			current:                 &model.Meta{ID: "x", DisableRegistration: true},
			body:                    `{"approvalRequiredForSignup":true}`,
			wantDisableRegistration: boolPtr(false),
		},
		{
			name:                    "既に開放済みなら触らない",
			current:                 &model.Meta{ID: "x", DisableRegistration: false},
			body:                    `{"approvalRequiredForSignup":true}`,
			wantDisableRegistration: boolPtr(false),
		},
		{
			// **無条件に開けると、管理者が閉じた操作を黙って巻き戻すことになる。**
			name:                    "承認制が元から有効なだけの更新では開放しない",
			current:                 &model.Meta{ID: "x", ApprovalRequiredForSignup: true, DisableRegistration: false},
			body:                    `{"disableRegistration":true}`,
			wantDisableRegistration: boolPtr(true),
		},
		{
			name:                    "承認制を切る更新では開放しない",
			current:                 &model.Meta{ID: "x", ApprovalRequiredForSignup: true, DisableRegistration: true},
			body:                    `{"approvalRequiredForSignup":false}`,
			wantDisableRegistration: boolPtr(true),
		},
		{
			// #2803 の本体。承認制で開いた登録は、承認制を外す更新で閉じ直す。
			// 維持すると、招待制 → 承認制 ON → 承認制 OFF でゲートが 1 つも
			// 無い全開状態になる。
			name:                    "承認制を切る更新では登録も閉じる",
			current:                 &model.Meta{ID: "x", ApprovalRequiredForSignup: true, DisableRegistration: false},
			body:                    `{"approvalRequiredForSignup":false}`,
			wantApprovalRequired:    boolPtr(false),
			wantDisableRegistration: boolPtr(true),
		},
		{
			// 明示指定があれば尊重する。両方送るクライアント (モデレーション
			// 画面) が「開けたままにする」を選べなくなると意味が無い。
			name:                    "承認制を切る更新で開放を明示すれば尊重する",
			current:                 &model.Meta{ID: "x", ApprovalRequiredForSignup: true, DisableRegistration: false},
			body:                    `{"approvalRequiredForSignup":false,"disableRegistration":false}`,
			wantApprovalRequired:    boolPtr(false),
			wantDisableRegistration: boolPtr(false),
		},
		{
			name:                    "承認制を切る更新で閉鎖を明示すれば尊重する",
			current:                 &model.Meta{ID: "x", ApprovalRequiredForSignup: true, DisableRegistration: false},
			body:                    `{"approvalRequiredForSignup":false,"disableRegistration":true}`,
			wantDisableRegistration: boolPtr(true),
		},
		{
			// 元から閉じているサーバーで開放を明示するのも尊重する。既定の
			// 補完が「閉じ直す」向きなので、こちらは上書きされうる形。
			name:                    "元が招待制でも承認制を切る更新の開放明示は尊重する",
			current:                 &model.Meta{ID: "x", ApprovalRequiredForSignup: true, DisableRegistration: true},
			body:                    `{"approvalRequiredForSignup":false,"disableRegistration":false}`,
			wantDisableRegistration: boolPtr(false),
		},
		{
			// **型を推測しない (#2803)。** GORM は map の値をそのまま driver へ渡す
			// ので、string でも PostgreSQL の boolean 入力構文に当たれば列は更新
			// される。正規化は bool しか見ないため、弾かないと「承認制は外れたのに
			// 登録は全開のまま」が作れる。
			//
			// `disableRegistration` を bool で添えるのは、**弾いたのに書いている
			// 回帰をこのケース単独でも検出するため**。mock の Update は bool 以外を
			// 無視するので、string の列だけでは書き込みが起きても差が出ない。
			name:                "承認制の値が bool でなければ弾く",
			current:             &model.Meta{ID: "x", ApprovalRequiredForSignup: true, DisableRegistration: false},
			body:                `{"approvalRequiredForSignup":"false","disableRegistration":true}`,
			wantRejectedMessage: "approvalRequiredForSignup must be a boolean",
		},
		{
			// 逆向き。string で送れると #2565 が防ぐ「承認制 + 招待制」が作れる。
			name:                "disableRegistration の値が bool でなければ弾く",
			current:             &model.Meta{ID: "x", ApprovalRequiredForSignup: false, DisableRegistration: false},
			body:                `{"approvalRequiredForSignup":true,"disableRegistration":"true"}`,
			wantRejectedMessage: "disableRegistration must be a boolean",
		},
		{
			// **null は弾かず落とす。** upstream の paramDef は `nullable: true` で、
			// 実装も `typeof === 'boolean'` でしか読まない (= 無指定と同じ)。
			// misskey-js の生成型も `boolean | null` なので、400 にすると型どおりに
			// 送るクライアントを壊す。無指定なので閉じる既定が効く。
			name:                    "disableRegistration が null なら無指定として扱う",
			current:                 &model.Meta{ID: "x", ApprovalRequiredForSignup: true, DisableRegistration: false},
			body:                    `{"approvalRequiredForSignup":false,"disableRegistration":null}`,
			wantApprovalRequired:    boolPtr(false),
			wantDisableRegistration: boolPtr(true),
		},
		{
			// 承認制側の null も同じ。無指定なので遷移が無く、何も触らない。
			name:                    "approvalRequiredForSignup が null なら何も触らない",
			current:                 &model.Meta{ID: "x", ApprovalRequiredForSignup: true, DisableRegistration: false},
			body:                    `{"approvalRequiredForSignup":null}`,
			wantApprovalRequired:    boolPtr(true),
			wantDisableRegistration: boolPtr(false),
		},
		{
			// **開ける側は明示より整合を優先する (#2565)。** 承認制と招待制を
			// 重ねると登録手段がゼロになるので、同じ更新で両方来ても開く。
			// 閉じる側と非対称なのは意図的。
			name:                    "承認制を入れる更新では閉鎖の明示より整合を優先する",
			current:                 &model.Meta{ID: "x", ApprovalRequiredForSignup: false, DisableRegistration: false},
			body:                    `{"approvalRequiredForSignup":true,"disableRegistration":true}`,
			wantApprovalRequired:    boolPtr(true),
			wantDisableRegistration: boolPtr(false),
		},
		{
			// **承認制が元から無効な更新で閉じてはいけない。** 管理者が開いた
			// 操作を黙って巻き戻すことになる。
			name:                    "承認制が元から無効なまま false を送っても閉じない",
			current:                 &model.Meta{ID: "x", ApprovalRequiredForSignup: false, DisableRegistration: false},
			body:                    `{"approvalRequiredForSignup":false}`,
			wantApprovalRequired:    boolPtr(false),
			wantDisableRegistration: boolPtr(false),
		},
		{
			// 逆向きも同じ。承認制が元から有効なだけの更新では開放しない。
			name:                    "承認制が元から有効なまま true を送っても開放しない",
			current:                 &model.Meta{ID: "x", ApprovalRequiredForSignup: true, DisableRegistration: true},
			body:                    `{"approvalRequiredForSignup":true}`,
			wantApprovalRequired:    boolPtr(true),
			wantDisableRegistration: boolPtr(true),
		},
		{
			// 排他は撤去した (#2571)。承認済みからの登録も確認メールの経路を
			// 通るようになり、設定と実態が食い違わなくなったため。
			name:                    "承認制が有効なままメール必須を入れる",
			current:                 &model.Meta{ID: "x", ApprovalRequiredForSignup: true},
			body:                    `{"emailRequiredForSignup":true}`,
			wantEmailRequired:       boolPtr(true),
			wantApprovalRequired:    boolPtr(true),
			wantDisableRegistration: boolPtr(false),
		},
		{
			name:                 "メール必須が有効なまま承認制を入れる",
			current:              &model.Meta{ID: "x", EmailRequiredForSignup: true},
			body:                 `{"approvalRequiredForSignup":true}`,
			wantEmailRequired:    boolPtr(true),
			wantApprovalRequired: boolPtr(true),
		},
		{
			name:                 "承認制とメール必須を同時に入れる",
			current:              &model.Meta{ID: "x", DisableRegistration: true},
			body:                 `{"approvalRequiredForSignup":true,"emailRequiredForSignup":true}`,
			wantEmailRequired:    boolPtr(true),
			wantApprovalRequired: boolPtr(true),
			// 承認制を入れる更新なので登録も開く (表示の整合)。
			wantDisableRegistration: boolPtr(false),
		},
		{
			name:    "承認制が無効なら招待制とメール必須は自由",
			current: &model.Meta{ID: "x"},
			body:    `{"disableRegistration":true,"emailRequiredForSignup":true}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, metaRepo, _ := newTestHandler(t)
			metaRepo.Meta = tt.current
			// **呼び出し前に値でコピーを取る。** mock は metaRepo.Meta の
			// フィールドを書き換えるので、tt.current と比べても同じポインタを
			// 両辺に置くだけになり、書き込みが起きても絶対に落ちない。
			var before model.Meta
			if tt.current != nil {
				before = *tt.current
			}

			rec := doPost(h.UpdateMeta, tt.body, adminUser)
			if tt.wantRejectedMessage != "" {
				assert.Equal(t, http.StatusBadRequest, rec.Code)
				assert.Contains(t, rec.Body.String(), tt.wantRejectedMessage)
				// **弾いたなら列も動いていないこと。** 400 を返しつつ書き込んで
				// いたら、型検査を入れた意味が無い。
				if tt.current != nil {
					assert.Equal(t, before.DisableRegistration, metaRepo.Meta.DisableRegistration)
					assert.Equal(t, before.ApprovalRequiredForSignup, metaRepo.Meta.ApprovalRequiredForSignup)
				}
				return
			}
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			if tt.wantDisableRegistration != nil {
				assert.Equal(t, *tt.wantDisableRegistration, metaRepo.Meta.DisableRegistration)
			}
			if tt.wantEmailRequired != nil {
				assert.Equal(t, *tt.wantEmailRequired, metaRepo.Meta.EmailRequiredForSignup)
			}
			if tt.wantApprovalRequired != nil {
				assert.Equal(t, *tt.wantApprovalRequired, metaRepo.Meta.ApprovalRequiredForSignup)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

// meta が引けないときは何も書かない (#2803)。
//
// normalizeSignupConditions は current == nil だと遷移を判定できず、承認制を切る
// 更新でも登録を閉じる補正を掛けられない。**それが fail-open にならないのは、
// 後段の maybeAutoGenerateVAPID が同じ Fetch の error を返して Update まで
// 到達しないから。** その前提をここで固定しておかないと、VAPID 側を meta 欠損に
// 寛容にした瞬間に「承認制は外れたのに登録は開いたまま」が書き込まれる。
func TestUpdateMeta_MetaUnavailable(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta = nil

	rec := doPost(h.UpdateMeta, `{"approvalRequiredForSignup":false}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	// Update が呼ばれていれば mock が Meta を作る。作られていない = 書いていない。
	assert.Nil(t, metaRepo.Meta)
}

// 申請フォームの定義を検証する (#2570)。
//
// **上限を置かないと管理者が自分で壊せる。** 保存してから壊れているのに気づく形に
// しないよう、update-meta で弾く。
func TestUpdateMeta_SignupApplicationForm(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantRejected bool
	}{
		{
			name: "valid definition",
			body: `{"signupApplicationForm":[{"label":"動機","type":"textarea","required":true,"maxLength":500}]}`,
		},
		{name: "empty definition", body: `{"signupApplicationForm":[]}`},
		{
			name:         "unknown type",
			body:         `{"signupApplicationForm":[{"label":"x","type":"password"}]}`,
			wantRejected: true,
		},
		{
			name:         "empty label",
			body:         `{"signupApplicationForm":[{"label":"  ","type":"text"}]}`,
			wantRejected: true,
		},
		{
			name:         "max length over the ceiling",
			body:         `{"signupApplicationForm":[{"label":"x","type":"text","maxLength":999999}]}`,
			wantRejected: true,
		},
		{
			name:         "not an array",
			body:         `{"signupApplicationForm":{"label":"x"}}`,
			wantRejected: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, metaRepo, _ := newTestHandler(t)
			metaRepo.Meta = &model.Meta{ID: "x"}

			rec := doPost(h.UpdateMeta, tt.body, adminUser)
			if tt.wantRejected {
				assert.Equal(t, http.StatusBadRequest, rec.Code)
				return
			}
			assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		})
	}
}

// 送られてこなければ触らない (部分更新)。
func TestUpdateMeta_SignupApplicationForm_Absent(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta = &model.Meta{ID: "x"}

	existing := []byte(`[{"label":"動機","type":"textarea","required":true}]`)
	metaRepo.Meta.SignupApplicationForm = existing

	rec := doPost(h.UpdateMeta, `{"name":"x"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// **触れていないことまで見る。** 204 だけ見ていると、未送出でも null を
	// 書き込む実装に退行したときに素通りする (実際 `if !ok { return nil }` を
	// 外してもこのテストは通っていた)。
	assert.JSONEq(t, string(existing), string(metaRepo.Meta.SignupApplicationForm))
}

// **未知のキーを jsonb に残さない。** 残すと後で読み手を混乱させる。
func TestUpdateMeta_SignupApplicationForm_Normalizes(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta = &model.Meta{ID: "x"}

	rec := doPost(h.UpdateMeta,
		`{"signupApplicationForm":[{"label":"動機","type":"text","bogus":"nope"}]}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.NotContains(t, string(metaRepo.Meta.SignupApplicationForm), "bogus")
}

// 明示的な null は空配列に寄せる。**jsonb null を入れると他の jsonb 列と
// 中身が揃わない** (他は coerceMetaJSONBFields が `[]` にする)。
func TestUpdateMeta_SignupApplicationForm_Null(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta = &model.Meta{ID: "x"}

	rec := doPost(h.UpdateMeta, `{"signupApplicationForm":null}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.JSONEq(t, `[]`, string(metaRepo.Meta.SignupApplicationForm))
}

// --- #2973: 凍結の由来 ---

// stubOriginRepo is an in-memory UserSuspensionOriginRepository.
type stubOriginRepo struct {
	origins map[string]string
	setErr  error
}

func newStubOriginRepo() *stubOriginRepo {
	return &stubOriginRepo{origins: map[string]string{}}
}

func (s *stubOriginRepo) Origin(userID string) (string, error) { return s.origins[userID], nil }

func (s *stubOriginRepo) Set(userID, origin string) error {
	if s.setErr != nil {
		return s.setErr
	}
	s.origins[userID] = origin
	return nil
}

func (s *stubOriginRepo) Clear(userID string) error { delete(s.origins, userID); return nil }

// **モデレーターの凍結は local として刻む。** 刻まないと、リモート actor が
// `toot:suspended` を下ろした時点で凍結が解除される (#2973)。
func TestSuspendUser_RecordsLocalOrigin(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	origins := newStubOriginRepo()
	h.SetSuspensionOriginRepo(origins)

	rec := doPost(h.SuspendUser, `{"userId":"u1"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, model.SuspensionOriginLocal, origins.origins["u1"],
		"凍結の由来を刻んでいない。発信元が suspended を下ろすと解除される")
}

// **モデレーターの解除も local として刻む。** 刻まないと、発信元が立て続けて
// いる限り次の actor refresh で無言で戻る (この issue が塞ぎに来た状態そのもの)。
func TestUnsuspendUser_RecordsLocalOrigin(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", IsSuspended: true}
	origins := newStubOriginRepo()
	origins.origins["u1"] = model.SuspensionOriginRemote // 発信元由来で凍結されていた
	h.SetSuspensionOriginRepo(origins)

	rec := doPost(h.UnsuspendUser, `{"userId":"u1"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equalf(t, model.SuspensionOriginLocal, origins.origins["u1"],
		"解除の由来を刻んでいない。remote のまま残ると次の refresh で再凍結される")
}

// **解除で由来を記録できなかったら 500 を返す。** 記録できないまま解除すると
// 「解除したはずが戻っている」状態になるので、成功として返さない。
func TestUnsuspendUser_OriginWriteFailureIs500(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", IsSuspended: true}
	origins := newStubOriginRepo()
	origins.setErr = errors.New("connection refused")
	h.SetSuspensionOriginRepo(origins)

	rec := doPost(h.UnsuspendUser, `{"userId":"u1"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"由来を記録できないのに解除を成功として返している")
}

// **アカウント削除も local として刻む。** `isSuspended` と `isDeleted` を同時に
// 立てるので、remote 由来の記録が残っていると発信元が tombstone の凍結を外せる。
// inbound の gate は `isSuspended` しか見ないため、削除済みアカウントからの
// activity が再び通ることになる。
func TestAccountsDelete_RecordsLocalOrigin(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	host := "remote.example"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Host: &host}
	origins := newStubOriginRepo()
	origins.origins["u1"] = model.SuspensionOriginRemote
	h.SetSuspensionOriginRepo(origins)

	rec := doPost(h.AccountsDelete, `{"userId":"u1"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equalf(t, model.SuspensionOriginLocal, origins.origins["u1"],
		"削除で由来を刻んでいない。発信元が tombstone の凍結を外せる")
}

// #3015: 最小文字数の範囲検証。上限は `localUsernamePattern` の 20 と揃える。
//
// **21 以上を書けると登録が全滅する** — どの username も format 検証で先に
// 落ちるので、管理画面から自分のインスタンスを壊せてしまう。service 側にも
// clamp があるが、admin が入れた値が黙って別の値になるのは分かりにくいので
// 入口で拒否する (分割アップロードの範囲検証と同じ判断)。
func TestUpdateMeta_MinimumUsernameLengthRange(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)

	tooSmall := fmt.Sprintf(`{"minimumUsernameLength":%d}`, signup.MinUsernameLength-1)
	assert.Equal(t, http.StatusBadRequest, doPost(h.UpdateMeta, tooSmall, nil).Code)
	tooBig := fmt.Sprintf(`{"minimumUsernameLength":%d}`, signup.MaxUsernameLength+1)
	assert.Equal(t, http.StatusBadRequest, doPost(h.UpdateMeta, tooBig, nil).Code)
	assert.Equal(t, http.StatusBadRequest, doPost(h.UpdateMeta, `{"minimumUsernameLength":5.5}`, nil).Code, "non-integer must be rejected")

	// 境界は通り、実際に保存されること。**弾きすぎていないことを見ないと
	// 「常に 400」でも緑になる。**
	for _, v := range []int{signup.MinUsernameLength, signup.MaxUsernameLength} {
		body := fmt.Sprintf(`{"minimumUsernameLength":%d}`, v)
		require.Equal(t, http.StatusNoContent, doPost(h.UpdateMeta, body, nil).Code, body)
		assert.Equal(t, v, metaRepo.Meta.MinimumUsernameLength)
	}
}

// #3015: admin/meta は**列の生値**を返す。
//
// 公開 meta (`/api/meta`) が返すのは clamp 後の「実際に効く値」だが、管理画面は
// 入力欄の初期値に使うので、丸めた値を出すと**開いて保存しただけで設定が
// 書き換わる**。
func TestAdminMeta_ExposesMinimumUsernameLength(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta.MinimumUsernameLength = 5

	rec := doPost(h.AdminMeta, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, float64(5), resp["minimumUsernameLength"])
}

// #3015: `admin/accounts/create` は最小文字数を受けない。
//
// 運営が公式アカウントに短い ID を配れるようにするため
// (`signup.UsernamePolicyOperator`)。**位置では区別が付かない** — admin も
// `isInitialSetup == false` で `Signup` を呼ぶので、handler が policy を選ぶ。
func TestAccountsCreate_IgnoresMinimumUsernameLength(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rootID := "root1"
	metaRepo.Meta.RootUserID = &rootID
	metaRepo.Meta.MinimumUsernameLength = 10

	rootUser := &model.User{ID: "root1", Username: "root"}
	rec := doPost(h.AccountsCreate, `{"username":"ops","password":"pass1234"}`, rootUser)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// **予約は引き続き効く。** 除外したのは最小長だけなので、
	// 「policy を渡したら全部素通り」になっていないことを見る。
	metaRepo.Meta.PreservedUsernames = model.StringArray{"reserved"}
	rec = doPost(h.AccountsCreate, `{"username":"reserved","password":"pass1234"}`, rootUser)
	assert.NotEqual(t, http.StatusOK, rec.Code, "予約 username が admin 経路で通っている")
}
