package roles_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/api/roles"
	"github.com/elythia-network/elythia/internal/api/userrelation"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const publicInternalErrorJSON = `{"error":{"message":"Internal error.","code":"INTERNAL_ERROR","id":"5d37dbcb-891e-41ca-a3d6-e690c97775ac","kind":"server"}}`
const showNoSuchRoleJSON = `{"error":{"message":"No such role.","code":"NO_SUCH_ROLE","id":"de5502bf-009a-4639-86c1-fec349e46dcb","kind":"client"}}`
const usersNoSuchRoleJSON = `{"error":{"message":"No such role.","code":"NO_SUCH_ROLE","id":"30aaaee3-4792-48dc-ab0d-cf501a575ac5","kind":"client"}}`
const notesNoSuchRoleJSON = `{"error":{"message":"No such role.","code":"NO_SUCH_ROLE","id":"eb70323a-df61-4dd4-ad90-89c83c7cf26e","kind":"client"}}`

type publicRoleLookupResult struct {
	role *model.Role
	err  error
}

type publicSequenceRoleRepository struct {
	repository.RoleRepository
	results []publicRoleLookupResult
	calls   int
}

func (r *publicSequenceRoleRepository) FindByID(string) (*model.Role, error) {
	result := r.results[min(r.calls, len(r.results)-1)]
	r.calls++
	return result.role, result.err
}

func publicHandlerWithRepository(t *testing.T, roleRepo repository.RoleRepository, base *testutil.MockRoleRepository) *roles.Handler {
	t.Helper()
	assignRepo := testutil.NewMockRoleAssignmentRepository(base)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return roles.NewHandler(corerole.NewService(roleRepo, assignRepo, metaRepo, idGen), idGen)
}

func publicRoleEndpoints() []struct {
	name         string
	call         func(*roles.Handler, string) *httptest.ResponseRecorder
	notFoundJSON string
} {
	return []struct {
		name         string
		call         func(*roles.Handler, string) *httptest.ResponseRecorder
		notFoundJSON string
	}{
		{"show", func(h *roles.Handler, body string) *httptest.ResponseRecorder { return doPost(h.Show, body) }, showNoSuchRoleJSON},
		{"users", func(h *roles.Handler, body string) *httptest.ResponseRecorder { return doPost(h.Users, body) }, usersNoSuchRoleJSON},
		{"notes", func(h *roles.Handler, body string) *httptest.ResponseRecorder { return doPost(h.Notes, body) }, notesNoSuchRoleJSON},
	}
}

func TestPublicRolePersistenceFailures(t *testing.T) {
	for _, tt := range publicRoleEndpoints() {
		t.Run(tt.name, func(t *testing.T) {
			wantErr := errors.New("role SELECT failed")
			base := testutil.NewMockRoleRepository()
			repo := &publicSequenceRoleRepository{
				RoleRepository: base,
				results:        []publicRoleLookupResult{{err: wantErr}},
			}
			rec := tt.call(publicHandlerWithRepository(t, repo, base), `{"roleId":"secret-role-id"}`)
			require.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.JSONEq(t, publicInternalErrorJSON, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), wantErr.Error())
			assert.NotContains(t, rec.Body.String(), "secret-role-id")
		})
	}
}

func TestPublicRoleWrappedNotFound(t *testing.T) {
	for _, tt := range publicRoleEndpoints() {
		t.Run(tt.name, func(t *testing.T) {
			base := testutil.NewMockRoleRepository()
			repo := &publicSequenceRoleRepository{
				RoleRepository: base,
				results:        []publicRoleLookupResult{{err: fmt.Errorf("classified role: %w", corerole.ErrRoleNotFound)}},
			}
			rec := tt.call(publicHandlerWithRepository(t, repo, base), `{"roleId":"secret-role-id"}`)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.JSONEq(t, tt.notFoundJSON, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "secret-role-id")
		})
	}
}

func TestUsersSecondLookupErrors(t *testing.T) {
	tests := []struct {
		name      string
		secondErr error
		status    int
		response  string
	}{
		{"repository not found", testutil.ErrNotFound, http.StatusBadRequest, usersNoSuchRoleJSON},
		{"wrapped role not found", fmt.Errorf("classified role: %w", corerole.ErrRoleNotFound), http.StatusBadRequest, usersNoSuchRoleJSON},
		{"persistence", errors.New("assignment role lookup failed"), http.StatusInternalServerError, publicInternalErrorJSON},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := testutil.NewMockRoleRepository()
			repo := &publicSequenceRoleRepository{
				RoleRepository: base,
				results: []publicRoleLookupResult{
					{role: &model.Role{ID: "secret-role-id", IsPublic: true, IsExplorable: true}},
					{err: tt.secondErr},
				},
			}
			rec := doPost(publicHandlerWithRepository(t, repo, base).Users, `{"roleId":"secret-role-id"}`)
			require.Equal(t, tt.status, rec.Code)
			assert.JSONEq(t, tt.response, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "secret-role-id")
			if tt.status == http.StatusInternalServerError {
				assert.NotContains(t, rec.Body.String(), tt.secondErr.Error())
			}
		})
	}
}

// mockRoleNotesQuery is a test double for roles.RoleNotesQuery.
type mockRoleNotesQuery struct {
	Notes []*model.Note
	Err   error
}

func (m *mockRoleNotesQuery) ListByRole(roleID string, limit int, sinceID, untilID string) ([]*model.Note, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	result := m.Notes
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func newTestHandler(t *testing.T) (*roles.Handler, *testutil.MockRoleRepository) {
	t.Helper()
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	svc := corerole.NewService(roleRepo, assignRepo, metaRepo, idGen)
	h := roles.NewHandler(svc, idGen)
	return h, roleRepo
}

func doPost(h func(echo.Context) error, body string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = h(c)
	return rec
}

// viewerCtx bundles an echo.Context that carries an authenticated viewer with
// its response recorder so mute/block/channel filter tests can drive Notes and
// inspect the result.
type viewerCtx struct {
	ctx echo.Context
	rec *httptest.ResponseRecorder
}

func newCtxWithViewer(body, viewerID string) viewerCtx {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), &model.User{ID: viewerID})
	return viewerCtx{ctx: c, rec: rec}
}

func TestList_PublicOnly(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	// upstream は isPublic AND isExplorable。public+explorable のみ list に出る。
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Public", IsPublic: true, IsExplorable: true}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "Private", IsPublic: false}
	roleRepo.Roles["r3"] = &model.Role{ID: "r3", Name: "PublicNonExplorable", IsPublic: true, IsExplorable: false}
	rec := doPost(h.List, `{}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1, "isPublic かつ isExplorable のみ (r1)")
}

func TestList_Empty(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.List, `{}`)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1249: misskey_dart の RolesListResponse.fromJson が非null必須とする
// createdAt (String) / updatedAt (String) / canEditMembersByModerator (bool) /
// usersCount (num) が含まれること。欠落で roles 一覧が cast crash していた。
func TestList_FullShape(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	roleID := idGen.Generate(time.Now())
	roleRepo.Roles[roleID] = &model.Role{
		ID: roleID, Name: "Public", IsPublic: true, IsExplorable: true,
		UpdatedAt:                 time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		CanEditMembersByModerator: true,
	}
	rec := doPost(h.List, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	r := resp[0]
	createdAt, ok := r["createdAt"].(string)
	assert.True(t, ok, "createdAt must be a non-null string")
	assert.NotEmpty(t, createdAt)
	assert.Equal(t, "2026-05-01T00:00:00.000Z", r["updatedAt"])
	assert.Equal(t, true, r["canEditMembersByModerator"])
	assert.Equal(t, float64(0), r["usersCount"])
	shapetest.Assert(t, "RoleLite", r) // L3 (#1286)
}

func TestShow_Success(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Pub", IsPublic: true}
	rec := doPost(h.Show, `{"roleId":"r1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// 公開 role の pack に target / condFormula / policies(default-fill) /
// preserveAssignmentOnMoveAccount / 実 usersCount が含まれること (旧実装は
// これらを欠き usersCount=0 固定だった)。
func TestShow_IncludesPoliciesTargetAndUsersCount(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	svc := corerole.NewService(roleRepo, assignRepo, metaRepo, idGen)
	h := roles.NewHandler(svc, idGen)

	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Pub", IsPublic: true, Target: model.RoleTargetManual}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "a2", UserID: "u2", RoleID: "r1"}))

	rec := doPost(h.Show, `{"roleId":"r1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, float64(2), resp["usersCount"], "usersCount は active assignment 数")
	assert.Equal(t, "manual", resp["target"])
	assert.Contains(t, resp, "condFormula")
	assert.Contains(t, resp, "preserveAssignmentOnMoveAccount")
	policies, ok := resp["policies"].(map[string]any)
	require.True(t, ok, "policies が含まれること")
	// default-fill された任意の policy key が {useDefault:true,...} 形式。
	if cp, ok := policies["canPublicNote"].(map[string]any); ok {
		assert.Equal(t, true, cp["useDefault"])
	}
}

// #1249: role ID が aidx でなく ParseTime が失敗しても createdAt は空文字に
// ならず updatedAt にフォールバックすること (misskey_dart の DateTimeConverter
// は空文字を FormatException にするため)。
func TestList_CreatedAtFallsBackToUpdatedAt(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["non-aidx-id"] = &model.Role{
		ID: "non-aidx-id", Name: "Seeded", IsPublic: true, IsExplorable: true,
		UpdatedAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
	}
	rec := doPost(h.List, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "2026-05-01T00:00:00.000Z", resp[0]["createdAt"], "non-aidx ID は updatedAt にフォールバック")
}

func TestShow_NotPublic(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Priv", IsPublic: false}
	rec := doPost(h.Show, `{"roleId":"r1"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Show, `{"roleId":"ghost"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, showNoSuchRoleJSON, rec.Body.String())
}

func TestShow_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Show, `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUsers_Success(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	// upstream は isPublic かつ isExplorable な role のみ対象 (#1544)。
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", IsPublic: true, IsExplorable: true}
	rec := doPost(h.Users, `{"roleId":"r1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// newUsersTestHandler は roleRepo / assignRepo (UserRepo 紐付け済) / userRepo を
// 露出した handler を返す (roles/users の割当ユーザー一覧テスト用)。
func newUsersTestHandler(t *testing.T) (*roles.Handler, *testutil.MockRoleRepository, *testutil.MockRoleAssignmentRepository, *testutil.MockUserRepository) {
	t.Helper()
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	userRepo := testutil.NewMockUserRepository()
	assignRepo.UserRepo = userRepo
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	svc := corerole.NewService(roleRepo, assignRepo, metaRepo, idGen)
	h := roles.NewHandler(svc, idGen)
	h.SetUserRepo(userRepo)
	return h, roleRepo, assignRepo, userRepo
}

// upstream users.ts: 割り当てユーザーを [{id, user:UserDetailed}] で返す (#1544)。
func TestUsers_ReturnsAssignedUsers(t *testing.T) {
	h, roleRepo, assignRepo, userRepo := newUsersTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", IsPublic: true, IsExplorable: true}
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "ra1", RoleID: "r1", UserID: "alice"}))

	rec := doPost(h.Users, `{"roleId":"r1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "ra1", resp[0]["id"])
	user, ok := resp[0]["user"].(map[string]any)
	require.True(t, ok, "user は UserDetailed object")
	assert.Equal(t, "alice", user["id"])
}

// #1973: roles/users の embed user に viewer 視点の relation block が乗る。
// viewer が assigned user を follow していれば isFollowing=true、匿名なら省略。
func TestUsers_EmbedsViewerRelation(t *testing.T) {
	h, roleRepo, assignRepo, userRepo := newUsersTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", IsPublic: true, IsExplorable: true}
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "ra1", RoleID: "r1", UserID: "alice"}))
	followingRepo := testutil.NewMockFollowingRepository()
	followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "viewer1", FolloweeID: "alice"}
	h.SetListPacker(userpack.New(userpack.Lookups{Relations: userrelation.Repos{Following: followingRepo}}, nil))

	// 認証 viewer: isFollowing=true。
	vc := newCtxWithViewer(`{"roleId":"r1"}`, "viewer1")
	require.NoError(t, h.Users(vc.ctx))
	require.Equal(t, http.StatusOK, vc.rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(vc.rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	user := resp[0]["user"].(map[string]any)
	assert.Equal(t, true, user["isFollowing"], "viewer の embed user に isFollowing=true (#1973)")

	// 匿名: relation 省略。
	rec := doPost(h.Users, `{"roleId":"r1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var anon []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &anon))
	require.Len(t, anon, 1)
	anonUser := anon[0]["user"].(map[string]any)
	_, has := anonUser["isFollowing"]
	assert.False(t, has, "匿名には relation を出さない (#1973)")
}

// 期限切れの割当 (expiresAt <= now) はユーザー一覧に現れない (#1544)。
func TestUsers_ExpiredAssignmentExcluded(t *testing.T) {
	h, roleRepo, assignRepo, userRepo := newUsersTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", IsPublic: true, IsExplorable: true}
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	past := time.Now().Add(-time.Hour)
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "ra1", RoleID: "r1", UserID: "alice", ExpiresAt: &past}))

	rec := doPost(h.Users, `{"roleId":"r1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp, "期限切れ割当は除外される")
}

// userRepo 未配線でも user は packed される (profile なし)。
func TestUsers_NilUserRepoPacksWithoutProfile(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	userRepo := testutil.NewMockUserRepository()
	assignRepo.UserRepo = userRepo // assignment.User の preload 用
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	svc := corerole.NewService(roleRepo, assignRepo, metaRepo, idGen)
	h := roles.NewHandler(svc, idGen) // SetUserRepo は呼ばない (profile lookup 無効)

	roleRepo.Roles["r1"] = &model.Role{ID: "r1", IsPublic: true, IsExplorable: true}
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "ra1", RoleID: "r1", UserID: "alice"}))

	rec := doPost(h.Users, `{"roleId":"r1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "alice", resp[0]["user"].(map[string]any)["id"])
}

// assignment.User が nil (preload 失敗) の割当は skip され一覧に出ない。
func TestUsers_NilUserSkipped(t *testing.T) {
	h, roleRepo, assignRepo, _ := newUsersTestHandler(t)
	// assignRepo.UserRepo に対応 user を入れない → a.User が nil のまま。
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", IsPublic: true, IsExplorable: true}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "ra1", RoleID: "r1", UserID: "ghostuser"}))

	rec := doPost(h.Users, `{"roleId":"r1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp, "User preload できなかった割当は skip")
}

// isPublic でも !isExplorable なら NO_SUCH_ROLE (#1544)。
func TestUsers_NotExplorableIsNotFound(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", IsPublic: true, IsExplorable: false}
	rec := doPost(h.Users, `{"roleId":"r1"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUsers_NotPublicIsNotFound(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", IsPublic: false, IsExplorable: true}
	rec := doPost(h.Users, `{"roleId":"r1"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUsers_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Users, `{"roleId":"ghost"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, usersNoSuchRoleJSON, rec.Body.String())
}

func TestUsers_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Users, `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestList_Error(t *testing.T) {
	roleRepo := &failingListRepo{testutil.NewMockRoleRepository()}
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo.MockRoleRepository)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	svc := corerole.NewService(roleRepo, assignRepo, metaRepo, idGen)
	h := roles.NewHandler(svc, idGen)
	rec := doPost(h.List, `{}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingListRepo struct {
	*testutil.MockRoleRepository
}

func (f *failingListRepo) List() ([]*model.Role, error)           { return nil, assert.AnError }
func (f *failingListRepo) ListByLastUsed() ([]*model.Role, error) { return nil, assert.AnError }

// --- Notes ---

func TestNotes_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Notes, `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestNotes_RoleNotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Notes, `{"roleId":"ghost"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, notesNoSuchRoleJSON, rec.Body.String())
}

func TestNotes_NotPublic(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Private", IsPublic: false}
	rec := doPost(h.Notes, `{"roleId":"r1"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestNotes_NilQuery(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Public", IsPublic: true, IsExplorable: true}
	rec := doPost(h.Notes, `{"roleId":"r1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var arr []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &arr))
	assert.Empty(t, arr)
}

func TestNotes_Success(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Public", IsPublic: true, IsExplorable: true}
	mock := &mockRoleNotesQuery{
		Notes: []*model.Note{
			{ID: "n1", UserID: "u1", Text: strPtr("hello"), Visibility: "public"},
		},
	}
	h.SetNotesQuery(mock)
	rec := doPost(h.Notes, `{"roleId":"r1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var arr []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &arr))
	assert.Len(t, arr, 1)
}

// #1544: role が public でも isExplorable=false なら空配列を返す (notesQuery を
// 呼ばずに早期 return する)。upstream notes.ts のガード。
func TestNotes_NonExplorableReturnsEmpty(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Public", IsPublic: true, IsExplorable: false}
	mock := &mockRoleNotesQuery{
		Notes: []*model.Note{
			{ID: "n1", UserID: "u1", Text: strPtr("hello"), Visibility: "public"},
		},
	}
	h.SetNotesQuery(mock)
	rec := doPost(h.Notes, `{"roleId":"r1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var arr []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &arr))
	assert.Empty(t, arr, "isExplorable=false role must return an empty note list")
}

// #1544: viewer が mute した user / viewer を block した user の note と、
// viewer が mute した channel の note が roles/notes から除外されること。
func TestNotes_MuteBlockChannelFiltered(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Public", IsPublic: true, IsExplorable: true}

	muting := testutil.NewMockMutingRepository()
	require.NoError(t, muting.Create(&model.Muting{ID: "m1", MuterID: "viewer", MuteeID: "muted"}))
	blocking := testutil.NewMockBlockingRepository()
	require.NoError(t, blocking.Create(&model.Blocking{ID: "b1", BlockerID: "blocker", BlockeeID: "viewer"}))
	channelMuting := testutil.NewMockChannelMutingRepository()
	require.NoError(t, channelMuting.Create(&model.ChannelMuting{UserID: "viewer", ChannelID: "ch-muted"}))
	h.SetMuteBlockRepos(muting, blocking, channelMuting)

	mutedCh := "ch-muted"
	visibleCh := "ch-ok"
	mock := &mockRoleNotesQuery{
		Notes: []*model.Note{
			{ID: "keep", UserID: "author", Text: strPtr("ok"), Visibility: "public"},
			{ID: "muted-author", UserID: "muted", Text: strPtr("x"), Visibility: "public"},
			{ID: "blocked", UserID: "blocker", Text: strPtr("x"), Visibility: "public"},
			{ID: "muted-channel", UserID: "author", Text: strPtr("x"), Visibility: "public", ChannelID: &mutedCh},
			{ID: "ok-channel", UserID: "author", Text: strPtr("x"), Visibility: "public", ChannelID: &visibleCh},
		},
	}
	h.SetNotesQuery(mock)

	c := newCtxWithViewer(`{"roleId":"r1"}`, "viewer")
	require.NoError(t, h.Notes(c.ctx))
	body := c.rec.Body.String()
	assert.Contains(t, body, "keep")
	assert.Contains(t, body, "ok-channel")
	assert.NotContains(t, body, "muted-author", "note authored by a muted user must be dropped")
	assert.NotContains(t, body, "blocked", "note authored by a user who blocked the viewer must be dropped")
	assert.NotContains(t, body, "muted-channel", "note in a muted channel must be dropped")
}

// #1544 fail-closed: mute/block/channel set のロードでリポジトリエラーが出たら
// silently note を漏らさず 500 を返すこと。
func TestNotes_MuteBlockLoadError(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Public", IsPublic: true, IsExplorable: true}

	blocking := testutil.NewMockBlockingRepository()
	blocking.ExistsErr = assert.AnError // ListBlockerIDs もこのエラーで失敗する
	h.SetMuteBlockRepos(testutil.NewMockMutingRepository(), blocking, testutil.NewMockChannelMutingRepository())

	mock := &mockRoleNotesQuery{
		Notes: []*model.Note{
			{ID: "n1", UserID: "author", Text: strPtr("hello"), Visibility: "public"},
		},
	}
	h.SetNotesQuery(mock)

	c := newCtxWithViewer(`{"roleId":"r1"}`, "viewer")
	require.NoError(t, h.Notes(c.ctx))
	assert.Equal(t, http.StatusInternalServerError, c.rec.Code)
}

func TestNotes_QueryError(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Public", IsPublic: true, IsExplorable: true}
	mock := &mockRoleNotesQuery{Err: assert.AnError}
	h.SetNotesQuery(mock)
	rec := doPost(h.Notes, `{"roleId":"r1"}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestNotes_DefaultLimit(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Public", IsPublic: true, IsExplorable: true}
	mock := &mockRoleNotesQuery{}
	h.SetNotesQuery(mock)
	rec := doPost(h.Notes, `{"roleId":"r1","limit":0}`)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestNotes_LimitClamped(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Public", IsPublic: true, IsExplorable: true}
	mock := &mockRoleNotesQuery{}
	h.SetNotesQuery(mock)
	rec := doPost(h.Notes, `{"roleId":"r1","limit":999}`)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func strPtr(s string) *string { return &s }

// stubBufferedReactions implements entity.BufferedReactionsReader. 戻り値が
// 空マップでも reactionReader() が non-nil を返す経路を踏ませる。
type stubBufferedReactions struct{}

func (stubBufferedReactions) GetBufferedMany(_ context.Context, _ []string) (map[string]map[string]int64, error) {
	return map[string]map[string]int64{}, nil
}

// SetInstanceRepo / SetEmojiRepo / SetReactionReader / SetNoteFieldResolver
// を wire した状態で Notes を呼び、各 setter が field を設定し lookup の
// non-nil 分岐 (instanceLookup / emojiLookup) を踏むことを確認する。これら
// setter は他 handler でも同じ pattern なので回帰検知の意味も兼ねる (#739)。
func TestSettersWireOptionalDeps(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Public", IsPublic: true, IsExplorable: true}

	instanceRepo := testutil.NewMockInstanceRepository()
	h.SetInstanceRepo(instanceRepo)
	emojiRepo := testutil.NewMockEmojiRepository()
	h.SetEmojiRepo(emojiRepo)
	h.SetReactionReader(stubBufferedReactions{})
	h.SetNoteFieldResolver(nil) // Apply は r==nil で no-op

	mock := &mockRoleNotesQuery{
		Notes: []*model.Note{
			{ID: "n1", UserID: "u1", Text: strPtr("hello"), Visibility: "public"},
		},
	}
	h.SetNotesQuery(mock)
	rec := doPost(h.Notes, `{"roleId":"r1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
}
