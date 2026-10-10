package role_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func newTestService(t *testing.T) (*role.Service, *testutil.MockRoleRepository, *testutil.MockRoleAssignmentRepository, *testutil.MockMetaRepository) {
	t.Helper()
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	return svc, roleRepo, assignRepo, metaRepo
}

type roleLookupResult struct {
	role *model.Role
	err  error
}

type sequenceRoleRepository struct {
	repository.RoleRepository
	results []roleLookupResult
	calls   int
}

func (r *sequenceRoleRepository) FindByID(string) (*model.Role, error) {
	result := r.results[min(r.calls, len(r.results)-1)]
	r.calls++
	return result.role, result.err
}

func newServiceWithRoleRepository(t *testing.T, roleRepo repository.RoleRepository, base *testutil.MockRoleRepository) *role.Service {
	t.Helper()
	assignRepo := testutil.NewMockRoleAssignmentRepository(base)
	metaRepo := testutil.NewMockMetaRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return role.NewService(roleRepo, assignRepo, metaRepo, idGen)
}

func roleLookupCalls() []struct {
	name string
	call func(*role.Service) error
} {
	return []struct {
		name string
		call func(*role.Service) error
	}{
		{"assign", func(s *role.Service) error { return s.Assign("user", "role", nil) }},
		{"show", func(s *role.Service) error { _, err := s.Show("role"); return err }},
		{"list by role", func(s *role.Service) error { _, err := s.ListByRole("role", "", "", 10); return err }},
		{"update fields", func(s *role.Service) error {
			_, err := s.UpdateFields("role", map[string]any{"name": "new"})
			return err
		}},
		{"delete", func(s *role.Service) error { return s.Delete("role") }},
		{"find role", func(s *role.Service) error { _, err := s.FindRole("role"); return err }},
	}
}

func TestRoleLookupsPreservePersistenceErrors(t *testing.T) {
	wantErr := errors.New("role SELECT failed")
	for _, tt := range roleLookupCalls() {
		t.Run(tt.name, func(t *testing.T) {
			base := testutil.NewMockRoleRepository()
			repo := &sequenceRoleRepository{RoleRepository: base, results: []roleLookupResult{{err: wantErr}}}
			err := tt.call(newServiceWithRoleRepository(t, repo, base))
			assert.ErrorIs(t, err, wantErr)
			assert.NotErrorIs(t, err, role.ErrRoleNotFound)
		})
	}
}

func TestRoleLookupsClassifyWrappedNotFound(t *testing.T) {
	for _, tt := range roleLookupCalls() {
		t.Run(tt.name, func(t *testing.T) {
			base := testutil.NewMockRoleRepository()
			repo := &sequenceRoleRepository{
				RoleRepository: base,
				results:        []roleLookupResult{{err: fmt.Errorf("lookup role: %w", gorm.ErrRecordNotFound)}},
			}
			err := tt.call(newServiceWithRoleRepository(t, repo, base))
			assert.ErrorIs(t, err, role.ErrRoleNotFound)
			assert.NotErrorIs(t, err, gorm.ErrRecordNotFound)
		})
	}
}

func TestFindRoleEmptyIDSkipsRepository(t *testing.T) {
	base := testutil.NewMockRoleRepository()
	repo := &sequenceRoleRepository{RoleRepository: base, results: []roleLookupResult{{err: errors.New("must not run")}}}
	svc := newServiceWithRoleRepository(t, repo, base)
	_, err := svc.FindRole("")
	assert.ErrorIs(t, err, role.ErrRoleNotFound)
	assert.Zero(t, repo.calls)
}

func TestUpdateFieldsPreservesReadbackFailures(t *testing.T) {
	wantErr := errors.New("role readback failed")
	for _, fields := range []map[string]any{nil, {"name": "new"}} {
		base := testutil.NewMockRoleRepository()
		base.Roles["role"] = &model.Role{ID: "role", Name: "old"}
		repo := &sequenceRoleRepository{
			RoleRepository: base,
			results: []roleLookupResult{
				{role: base.Roles["role"]},
				{err: wantErr},
			},
		}
		_, err := newServiceWithRoleRepository(t, repo, base).UpdateFields("role", fields)
		assert.ErrorIs(t, err, wantErr)
		assert.NotErrorIs(t, err, role.ErrRoleNotFound)
		assert.Equal(t, 2, repo.calls)
	}
}

func TestUpdateFieldsClassifiesReadbackNotFound(t *testing.T) {
	for _, fields := range []map[string]any{nil, {"name": "new"}} {
		base := testutil.NewMockRoleRepository()
		base.Roles["role"] = &model.Role{ID: "role", Name: "old"}
		repo := &sequenceRoleRepository{
			RoleRepository: base,
			results: []roleLookupResult{
				{role: base.Roles["role"]},
				{err: fmt.Errorf("read back role: %w", gorm.ErrRecordNotFound)},
			},
		}
		_, err := newServiceWithRoleRepository(t, repo, base).UpdateFields("role", fields)
		assert.ErrorIs(t, err, role.ErrRoleNotFound)
		assert.NotErrorIs(t, err, gorm.ErrRecordNotFound)
		assert.Equal(t, 2, repo.calls)
	}
}

// FilterExistingRoleIDs は existing に含まれる id だけを順序保持で返し、
// 戻り値は常に非 nil (#1543)。
func TestFilterExistingRoleIDs(t *testing.T) {
	existing := map[string]bool{"r1": true, "r3": true}
	got := role.FilterExistingRoleIDs([]string{"r1", "r2", "r3"}, existing)
	assert.Equal(t, []string{"r1", "r3"}, got, "r2 (非存在) は除去")

	// 全て非存在 → 空でも非 nil ([])。
	got = role.FilterExistingRoleIDs([]string{"gone"}, existing)
	assert.NotNil(t, got)
	assert.Empty(t, got)

	// nil 入力でも非 nil の空 slice。
	got = role.FilterExistingRoleIDs(nil, existing)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

// ExistingRoleIDSet は全ロールの id 集合を返す (#1543)。
func TestExistingRoleIDSet(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["a"] = &model.Role{ID: "a", Name: "A"}
	roleRepo.Roles["b"] = &model.Role{ID: "b", Name: "B"}

	set, err := svc.ExistingRoleIDSet()
	require.NoError(t, err)
	assert.True(t, set["a"])
	assert.True(t, set["b"])
	assert.False(t, set["ghost"])
}

func TestGetUserRoles_NoRoles(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	roles, err := svc.GetUserRoles("user1")
	require.NoError(t, err)
	assert.Empty(t, roles)
}

func TestService_IsPublicExplorable(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Public", IsPublic: true, IsExplorable: true}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "Hidden", IsPublic: true, IsExplorable: false}
	roleRepo.Roles["r3"] = &model.Role{ID: "r3", Name: "Private", IsPublic: false, IsExplorable: true}
	assert.True(t, svc.IsPublicExplorable("r1"))
	assert.False(t, svc.IsPublicExplorable("r2"), "non-explorable role must not stream")
	assert.False(t, svc.IsPublicExplorable("r3"), "explorable but non-public role must not stream")
	assert.False(t, svc.IsPublicExplorable("missing"), "unknown role must be fail-closed")
}

// PublicRoleIDSet は isPublic なロールの id だけを返す (#17987)。
func TestPublicRoleIDSet(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["pub"] = &model.Role{ID: "pub", Name: "Pub", IsPublic: true}
	roleRepo.Roles["priv"] = &model.Role{ID: "priv", Name: "Priv", IsPublic: false}

	set, err := svc.PublicRoleIDSet()
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"pub": true}, set)
}

func TestGetUserRoles_WithRoles(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	adminRole := &model.Role{ID: "r1", Name: "Admin", IsAdministrator: true}
	roleRepo.Roles["r1"] = adminRole
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}

	roles, err := svc.GetUserRoles("user1")
	require.NoError(t, err)
	assert.Len(t, roles, 1)
	assert.Equal(t, "Admin", roles[0].Name)
}

func TestGetUserRoles_ExpiredRoleExcluded(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Temp"}
	expired := time.Now().Add(-1 * time.Hour)
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1", ExpiresAt: &expired,
	}

	roles, err := svc.GetUserRoles("user1")
	require.NoError(t, err)
	assert.Empty(t, roles)
}

func TestGetUserAssigns_EmptyUserID(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	assigns, err := svc.GetUserAssigns("")
	require.NoError(t, err)
	assert.Empty(t, assigns)
}

func TestGetUserAssigns_ActiveOnly(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Active"}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "Expired"}
	future := time.Now().Add(1 * time.Hour)
	expired := time.Now().Add(-1 * time.Hour)
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1", ExpiresAt: &future,
	}
	assignRepo.Assignments["user1:r2"] = &model.RoleAssignment{
		ID: "a2", UserID: "user1", RoleID: "r2", ExpiresAt: &expired,
	}

	// upstream getUserAssigns と同じく期限切れ assignment は除外される。
	assigns, err := svc.GetUserAssigns("user1")
	require.NoError(t, err)
	require.Len(t, assigns, 1)
	assert.Equal(t, "r1", assigns[0].RoleID)
}

func TestIsAdministrator_RootUser(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	rootID := "root1"
	metaRepo.Meta = &model.Meta{ID: "x", RootUserID: &rootID}
	assert.True(t, svc.IsAdministrator("root1"))
}

// drop-in 互換 (#785): TS DB を引き継いだ場合は meta.rootUserId が nil の
// まま user.isRoot=true で root が記録されるので、SetUserRepo 経由で
// fallback できる必要がある。
func TestIsAdministrator_DropInRoot_UserIsRootFallback(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	metaRepo.Meta = &model.Meta{ID: "x"} // RootUserID は nil
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "alice", Username: "alice", IsRoot: true}))
	svc.SetUserRepo(userRepo)

	assert.True(t, svc.IsAdministrator("alice"))
}

// user.isRoot=false のユーザーを fallback で誤って root 判定しないこと。
func TestIsAdministrator_DropIn_NonRootUser_NotPromoted(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	metaRepo.Meta = &model.Meta{ID: "x"}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "bob", Username: "bob", IsRoot: false}))
	svc.SetUserRepo(userRepo)

	assert.False(t, svc.IsAdministrator("bob"))
}

// SetUserRepo 未配線時は従来どおり meta.rootUserId のみで判定する
// (後方互換性 regression guard)。
func TestIsAdministrator_NoUserRepoWired_BehavesAsBefore(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	rootID := "root1"
	metaRepo.Meta = &model.Meta{ID: "x", RootUserID: &rootID}
	// SetUserRepo は呼ばない

	assert.True(t, svc.IsAdministrator("root1"))
	assert.False(t, svc.IsAdministrator("other"))
}

// userRepo.FindByID が transient error (RecordNotFound 以外) を返した場合は
// silent に false 扱い (admin 昇格に倒さない方が安全)。slog.Warn は出すが
// 現状 logger を inject しないので副作用のみ確認。
func TestIsAdministrator_UserRepoTransientError_FallsThrough(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	metaRepo.Meta = &model.Meta{ID: "x"}
	svc.SetUserRepo(&errorUserRepo{
		MockUserRepository: testutil.NewMockUserRepository(),
		err:                assert.AnError,
	})

	assert.False(t, svc.IsAdministrator("alice"))
}

func TestIsAdministrator_AdminRole(t *testing.T) {
	svc, roleRepo, assignRepo, metaRepo := newTestService(t)
	metaRepo.Meta = &model.Meta{ID: "x"}
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", IsAdministrator: true}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}
	assert.True(t, svc.IsAdministrator("user1"))
}

func TestIsAdministrator_NoRole(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	metaRepo.Meta = &model.Meta{ID: "x"}
	assert.False(t, svc.IsAdministrator("user1"))
}

func TestIsModerator_ModeratorRole(t *testing.T) {
	svc, roleRepo, assignRepo, metaRepo := newTestService(t)
	metaRepo.Meta = &model.Meta{ID: "x"}
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", IsModerator: true}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}
	assert.True(t, svc.IsModerator("user1"))
}

func TestIsModerator_AdminRoleAlsoCounts(t *testing.T) {
	svc, roleRepo, assignRepo, metaRepo := newTestService(t)
	metaRepo.Meta = &model.Meta{ID: "x"}
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", IsAdministrator: true}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}
	assert.True(t, svc.IsModerator("user1"))
}

func TestIsModerator_NoRole(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	metaRepo.Meta = &model.Meta{ID: "x"}
	assert.False(t, svc.IsModerator("user1"))
}

func TestIsAdministrator_MetaFetchError(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	metaRepo.FetchErr = errors.New("meta unavailable")
	assert.False(t, svc.IsAdministrator("user1"))
}

func TestGetUserPolicies_Default(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	policies := svc.GetUserPolicies("user1")
	assert.Equal(t, true, policies["gtlAvailable"])
	assert.Equal(t, 100, policies["driveCapacityMb"])
}

func TestGetUserPolicies_WithRoleOverride(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	// ロールが driveCapacityMb を 500 にオーバーライド
	roleRepo.Roles["r1"] = &model.Role{
		ID:   "r1",
		Name: "Pro",
		Policies: datatypes.JSON([]byte(`{
			"driveCapacityMb": {"useDefault": false, "priority": 0, "value": 500}
		}`)),
	}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}

	policies := svc.GetUserPolicies("user1")
	// #1020 で aggregator が base 型 (int) を維持するように。旧 impl の素朴な
	// `base[key] = p.Value` は JSON unmarshal の float64 をそのまま代入していた
	// ため float64(500) を返していたが、それは consumer 側 type assertion が
	// 壊れる原因にもなっていた。新 impl は upstream TS の Math.max 同等を
	// base 型で実装する (= 数値は int / int64 / float64 各々を維持)。
	assert.Equal(t, 500, policies["driveCapacityMb"])
}

// policies jsonb に想定外の形の entry が混ざっていても、他の key の override は
// 生き残ること。key ごとに decode しないと 1 件の異物で role の override が
// 丸ごと落ち、「サイレンス用ロールを割り当てたのに効かない」等の silent failure
// になる (upstream は JS の object lookup なので余計なキーは無害)。
func TestGetUserPolicies_MalformedEntryDoesNotDropOthers(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{
		ID:   "r1",
		Name: "Silenced",
		Policies: datatypes.JSON([]byte(`{
			"0": ["alwaysMarkNsfw", {"priority": 0, "useDefault": true, "value": false}],
			"canPublicNote": {"useDefault": false, "priority": 0, "value": false}
		}`)),
	}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}

	assert.Equal(t, false, svc.GetUserPolicies("user1")["canPublicNote"])
	assert.True(t, svc.IsSilenced("user1"))
}

// 1MB 未満の maxFileSizeMb を設定できること。int に丸めると 0 になり、
// drive 側の gate が「上限なし」として素通ししてしまう。
func TestGetUserPolicies_KeepsFractionalOverride(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{
		ID:   "r1",
		Name: "Tiny",
		Policies: datatypes.JSON([]byte(`{
			"maxFileSizeMb": {"useDefault": false, "priority": 1, "value": 9.5367431640625e-06}
		}`)),
	}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "user1", RoleID: "r1"}

	got := svc.GetUserPolicies("user1")["maxFileSizeMb"]
	assert.InDelta(t, 9.5367431640625e-06, got, 1e-12)
}

// 整数値の override は int のまま返す (既存 consumer の type assertion 維持)。
func TestGetUserPolicies_IntegerOverrideStaysInt(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{
		ID:       "r1",
		Name:     "Pro",
		Policies: datatypes.JSON([]byte(`{"maxFileSizeMb": {"useDefault": false, "priority": 1, "value": 500}}`)),
	}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "user1", RoleID: "r1"}

	assert.Equal(t, 500, svc.GetUserPolicies("user1")["maxFileSizeMb"])
}

func TestGetUserPolicies_UseDefaultTrue_NotOverridden(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{
		ID:       "r1",
		Policies: datatypes.JSON([]byte(`{"driveCapacityMb": {"useDefault": true, "priority": 0, "value": 999}}`)),
	}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}

	policies := svc.GetUserPolicies("user1")
	assert.Equal(t, 100, policies["driveCapacityMb"]) // デフォルト値が維持される
}

func TestGetUserPolicies_InvalidJSON(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{
		ID:       "r1",
		Policies: datatypes.JSON([]byte(`invalid`)),
	}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}

	policies := svc.GetUserPolicies("user1")
	// 不正なJSONの場合はデフォルト値がそのまま返る
	assert.Equal(t, 100, policies["driveCapacityMb"])
}

// --- #1020: 3 種類の role (manual / conditional) + base policies (meta.policies)
// + priority aggregation を覆う追加テスト群 ---

// Conditional role が isBot=true の user に自動付与され、policy が反映される。
func TestGetUserRoles_ConditionalRole_Matched(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "bot1", Username: "bot", IsBot: true}))
	svc.SetUserRepo(userRepo)

	// target=conditional の role で「bot 用 canSearchNotes=true」
	roleRepo.Roles["r-bot"] = &model.Role{
		ID:          "r-bot",
		Name:        "BotRole",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
		Policies:    datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":true}}`)),
	}

	roles, err := svc.GetUserRoles("bot1")
	require.NoError(t, err)
	require.Len(t, roles, 1, "bot user は conditional role でマッチして付与される")
	assert.Equal(t, "BotRole", roles[0].Name)

	// HasRolePolicy も反映される (本 PR の middleware gate と一直線)。
	assert.True(t, svc.HasRolePolicy("bot1", role.PolicyCanSearchNotes))
}

// Conditional role の formula が false に評価される user には付与されない。
func TestGetUserRoles_ConditionalRole_NotMatched(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u1", Username: "alice", IsBot: false}))
	svc.SetUserRepo(userRepo)

	roleRepo.Roles["r-bot"] = &model.Role{
		ID:          "r-bot",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
		Policies:    datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":true}}`)),
	}

	roles, err := svc.GetUserRoles("u1")
	require.NoError(t, err)
	assert.Empty(t, roles, "isBot=false の user は formula 不一致で 0 件")
	// canSearchNotes は default false のまま
	assert.False(t, svc.HasRolePolicy("u1", role.PolicyCanSearchNotes))
}

// userRepo 未配線 → conditional 評価 skip (= 旧挙動と同等の fail-safe)。
func TestGetUserRoles_ConditionalSkippedWhenUserRepoNotWired(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["r-bot"] = &model.Role{
		ID:          "r-bot",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
	}
	roles, err := svc.GetUserRoles("u1")
	require.NoError(t, err)
	assert.Empty(t, roles, "userRepo 未配線では conditional 評価しない")
}

// meta.policies の base override が適用される (= 全 user に効く"ベースロール"
// 相当)。Role assignment 無しでも反映される。
func TestGetUserPolicies_MetaBasePolicyOverridesDefault(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	// meta.policies で canSearchNotes を全員 true に上書き
	metaRepo.Meta = &model.Meta{
		ID:       "x",
		Policies: datatypes.JSON([]byte(`{"canSearchNotes":true,"driveCapacityMb":200}`)),
	}
	policies := svc.GetUserPolicies("u1")
	assert.Equal(t, true, policies["canSearchNotes"], "meta.policies の base override が反映")
	// #1020 review: meta.policies 経由の数値も base の型 (int) に丸められて
	// 入る。これにより consumer 側の type assert (`policies[key].(int)`) が
	// role override / meta override どちらの経路でも安全に動く。
	assert.Equal(t, 200, policies["driveCapacityMb"])
	// 未上書きの key は default が維持される
	assert.Equal(t, 20, policies["mentionLimit"])
}

// 複数 role の bool policy は OR で集約される (canSearchNotes が 1 role でも
// true なら true)。
func TestGetUserPolicies_BoolPolicyORAcrossRoles(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1",
		Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":false}}`))}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2",
		Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":true}}`))}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}
	assignRepo.Assignments["u1:r2"] = &model.RoleAssignment{ID: "a2", UserID: "u1", RoleID: "r2"}

	policies := svc.GetUserPolicies("u1")
	assert.Equal(t, true, policies["canSearchNotes"], "r1=false でも r2=true なら OR で true")
}

// 数値 policy は max で集約される。
func TestGetUserPolicies_NumericPolicyMaxAcrossRoles(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1",
		Policies: datatypes.JSON([]byte(`{"driveCapacityMb":{"useDefault":false,"priority":0,"value":200}}`))}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2",
		Policies: datatypes.JSON([]byte(`{"driveCapacityMb":{"useDefault":false,"priority":0,"value":500}}`))}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}
	assignRepo.Assignments["u1:r2"] = &model.RoleAssignment{ID: "a2", UserID: "u1", RoleID: "r2"}

	policies := svc.GetUserPolicies("u1")
	assert.Equal(t, 500, policies["driveCapacityMb"], "r1=200, r2=500 → max=500")
}

// priority 2 は priority 1 を勝つ。priority 2 でだけ override されている
// 場合、priority 1 / 0 の値は無視される。
func TestGetUserPolicies_PriorityCascade(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r-high"] = &model.Role{ID: "r-high",
		Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":2,"value":false}}`))}
	roleRepo.Roles["r-low"] = &model.Role{ID: "r-low",
		Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":1,"value":true}}`))}
	assignRepo.Assignments["u1:r-high"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r-high"}
	assignRepo.Assignments["u1:r-low"] = &model.RoleAssignment{ID: "a2", UserID: "u1", RoleID: "r-low"}

	policies := svc.GetUserPolicies("u1")
	// priority=2 の false が勝つ (priority=1 の true は集約対象外)。
	assert.Equal(t, false, policies["canSearchNotes"])
}

// chatAvailability は available > readonly > unavailable で集約される。
func TestGetUserPolicies_ChatAvailabilityRanking(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1",
		Policies: datatypes.JSON([]byte(`{"chatAvailability":{"useDefault":false,"priority":0,"value":"readonly"}}`))}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2",
		Policies: datatypes.JSON([]byte(`{"chatAvailability":{"useDefault":false,"priority":0,"value":"unavailable"}}`))}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}
	assignRepo.Assignments["u1:r2"] = &model.RoleAssignment{ID: "a2", UserID: "u1", RoleID: "r2"}

	policies := svc.GetUserPolicies("u1")
	// readonly が available より弱いので、readonly + unavailable → readonly
	assert.Equal(t, "readonly", policies["chatAvailability"])
}

// 空 userID は base policies (default + meta.policies merge 後) のみを返す。
func TestGetUserPolicies_EmptyUserIDReturnsBaseOnly(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	metaRepo.Meta = &model.Meta{ID: "x",
		Policies: datatypes.JSON([]byte(`{"canSearchNotes":true}`))}

	policies := svc.GetUserPolicies("")
	assert.Equal(t, true, policies["canSearchNotes"], "userID=\"\" でも meta.policies は反映")
	assert.Equal(t, 20, policies["mentionLimit"], "default はそのまま")
}

// meta.policies が空 / 不正 JSON / fetch エラーのときは default のままで silently
// 続行する (fail-soft、upstream TS 互換)。
func TestGetUserPolicies_MetaPoliciesFallbacks(t *testing.T) {
	t.Run("invalid JSON falls back to default", func(t *testing.T) {
		svc, _, _, metaRepo := newTestService(t)
		metaRepo.Meta = &model.Meta{ID: "x", Policies: datatypes.JSON([]byte(`not-json`))}
		policies := svc.GetUserPolicies("")
		assert.Equal(t, false, policies["canSearchNotes"], "不正 JSON は無視されて default")
	})
	t.Run("empty policies leaves default", func(t *testing.T) {
		svc, _, _, metaRepo := newTestService(t)
		metaRepo.Meta = &model.Meta{ID: "x", Policies: datatypes.JSON([]byte(``))}
		policies := svc.GetUserPolicies("")
		assert.Equal(t, false, policies["canSearchNotes"])
	})
}

// #1034: uploadableFileTypes は set union で集約される。role の override は
// JSON unmarshal で []any 型として渡るが、aggregator がそれを取り出して
// set union 化する。
func TestGetUserPolicies_UploadableFileTypesSingleRoleOverride(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1",
		Policies: datatypes.JSON([]byte(`{"uploadableFileTypes":{"useDefault":false,"priority":0,"value":["image/jpeg"]}}`))}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}

	policies := svc.GetUserPolicies("u1")
	got, ok := policies["uploadableFileTypes"].([]string)
	require.True(t, ok, "set union 結果は []string 型で返る")
	assert.Equal(t, []string{"image/jpeg"}, got)
}

// #1036 review: meta.policies で uploadableFileTypes を override しても、
// JSON unmarshal 由来の []any が coerceToBaseType で []string に丸まり、
// 後段の aggregator が case []string にマッチして set union 集約が動く。
// 旧 (#1034 だけだった) 実装では base 型が []any のまま残り、aggregator
// の switch が default fall through して base 維持 (= 集約 bypass) する
// regression があった。
func TestGetUserPolicies_UploadableFileTypesMetaOverrideAggregated(t *testing.T) {
	svc, roleRepo, assignRepo, metaRepo := newTestService(t)
	// meta.policies で base allowlist を ["text/*"] に絞る
	metaRepo.Meta = &model.Meta{ID: "x",
		Policies: datatypes.JSON([]byte(`{"uploadableFileTypes":["text/*"]}`))}
	// role override で image/* を追加
	roleRepo.Roles["r1"] = &model.Role{ID: "r1",
		Policies: datatypes.JSON([]byte(`{"uploadableFileTypes":{"useDefault":false,"priority":0,"value":["image/*"]}}`))}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}

	policies := svc.GetUserPolicies("u1")
	got, ok := policies["uploadableFileTypes"].([]string)
	require.True(t, ok, "meta override 経由でも []string 型で返る")
	// 本 test の seal point は **meta override が []any のまま aggregator を
	// bypass しない** こと。挙動: base override (text/*) は set union 集約に
	// 参加せず、role override (= useDefault:false の値) のみが flatten される
	// (upstream Misskey TS の calc も同 logic)。結果は role の image/* のみ。
	assert.Equal(t, []string{"image/*"}, got)
}

// 全 role が useDefault の場合は base 値 (meta.policies 経由含む) が反映される
// regression guard。aggregator は useDefault entry の場合 base 値を candidate
// として使うので、結果は set union 後 base そのもの。本 test は base override
// が「集約に参加しない」のではなく「useDefault 経路では base が候補として
// 使われる」挙動を seal する。
func TestGetUserPolicies_UploadableFileTypesAllUseDefaultUsesBase(t *testing.T) {
	svc, roleRepo, assignRepo, metaRepo := newTestService(t)
	// meta.policies で base allowlist を ["text/*", "audio/*"] に
	metaRepo.Meta = &model.Meta{ID: "x",
		Policies: datatypes.JSON([]byte(`{"uploadableFileTypes":["text/*","audio/*"]}`))}
	// role は当該 policy で useDefault=true (= base 値を採用)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1",
		Policies: datatypes.JSON([]byte(`{"uploadableFileTypes":{"useDefault":true,"priority":0,"value":["IGNORED"]}}`))}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}

	policies := svc.GetUserPolicies("u1")
	got, ok := policies["uploadableFileTypes"].([]string)
	require.True(t, ok)
	// base = ["text/*", "audio/*"] が candidate になり、set union 後 sort 済。
	assert.Equal(t, []string{"audio/*", "text/*"}, got)
}

// 複数 role の uploadableFileTypes が set union で merge される (= 「より
// 寛容な方向への集約」、upstream Misskey TS の calc('uploadableFileTypes',
// set union) と等価)。
func TestGetUserPolicies_UploadableFileTypesMultiRoleUnion(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1",
		Policies: datatypes.JSON([]byte(`{"uploadableFileTypes":{"useDefault":false,"priority":0,"value":["image/png","image/jpeg"]}}`))}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2",
		Policies: datatypes.JSON([]byte(`{"uploadableFileTypes":{"useDefault":false,"priority":0,"value":["image/png","video/mp4"]}}`))}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}
	assignRepo.Assignments["u1:r2"] = &model.RoleAssignment{ID: "a2", UserID: "u1", RoleID: "r2"}

	policies := svc.GetUserPolicies("u1")
	got, ok := policies["uploadableFileTypes"].([]string)
	require.True(t, ok)
	// r1 + r2 の和集合、sort 順 (image/jpeg, image/png, video/mp4)
	assert.Equal(t, []string{"image/jpeg", "image/png", "video/mp4"}, got)
}

// priority=0 fallback: 全 role が default を選んでいても、最終 aggregator は
// base を返す (regression: priority-2/1 が一件も無い時に空集合で集約しない)。
func TestGetUserPolicies_PriorityZeroFallback(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1",
		Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":true,"priority":0,"value":false}}`))}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}

	policies := svc.GetUserPolicies("u1")
	// useDefault=true の role しか無い場合は base (= default false) のまま。
	assert.Equal(t, false, policies["canSearchNotes"])
}

// 複数 role × 全 useDefault: 全 role が当該 policy で default を選んでいる
// 場合、bool OR aggregator も base に倒れる。
// 一方で 1 つでも override true を持つ role があれば、別 role が
// useDefault=true (base=false) であっても OR で true に上がる cross-test。
func TestGetUserPolicies_MultipleRolesUseDefaultCross(t *testing.T) {
	t.Run("all use default → base のまま false", func(t *testing.T) {
		svc, roleRepo, assignRepo, _ := newTestService(t)
		roleRepo.Roles["r1"] = &model.Role{ID: "r1",
			Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":true,"priority":0,"value":true}}`))}
		roleRepo.Roles["r2"] = &model.Role{ID: "r2",
			Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":true,"priority":0,"value":true}}`))}
		assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}
		assignRepo.Assignments["u1:r2"] = &model.RoleAssignment{ID: "a2", UserID: "u1", RoleID: "r2"}

		policies := svc.GetUserPolicies("u1")
		assert.Equal(t, false, policies["canSearchNotes"], "全 role useDefault なら base false")
	})

	t.Run("1 つだけ override=true なら全体 true", func(t *testing.T) {
		svc, roleRepo, assignRepo, _ := newTestService(t)
		roleRepo.Roles["r1"] = &model.Role{ID: "r1",
			Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":true,"priority":0,"value":false}}`))}
		roleRepo.Roles["r2"] = &model.Role{ID: "r2",
			Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":true}}`))}
		assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}
		assignRepo.Assignments["u1:r2"] = &model.RoleAssignment{ID: "a2", UserID: "u1", RoleID: "r2"}

		policies := svc.GetUserPolicies("u1")
		assert.Equal(t, true, policies["canSearchNotes"], "1 role でも override true なら OR で true")
	})

	t.Run("role に当該 policy entry が無い (= 別 policy のみ override)", func(t *testing.T) {
		svc, roleRepo, assignRepo, _ := newTestService(t)
		// r1 は canInvite のみ持って canSearchNotes は entry 無し (= 暗黙 useDefault)。
		roleRepo.Roles["r1"] = &model.Role{ID: "r1",
			Policies: datatypes.JSON([]byte(`{"canInvite":{"useDefault":false,"priority":0,"value":true}}`))}
		assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}

		policies := svc.GetUserPolicies("u1")
		assert.Equal(t, false, policies["canSearchNotes"], "当該 policy entry 不在 = useDefault 扱いで base")
		assert.Equal(t, true, policies["canInvite"], "別 policy の override は影響しない")
	})
}

// meta.policies 経由の数値が base の型 (int) に丸められることを cross-check。
// role override (maxNumberAsInt 経由) と meta override (coerceToBaseType 経由)
// の両方で同じ Go 型が返ることを保証する (= consumer の type assert 安全性)。
func TestGetUserPolicies_NumericCoercion(t *testing.T) {
	t.Run("meta.policies が int に丸まる", func(t *testing.T) {
		svc, _, _, metaRepo := newTestService(t)
		metaRepo.Meta = &model.Meta{ID: "x",
			Policies: datatypes.JSON([]byte(`{"pinLimit":7}`))}

		policies := svc.GetUserPolicies("u1")
		v, ok := policies["pinLimit"].(int)
		require.True(t, ok, "meta override 経由でも int を返す")
		assert.Equal(t, 7, v)
	})
	t.Run("role override も同じ int 型", func(t *testing.T) {
		svc, roleRepo, assignRepo, _ := newTestService(t)
		roleRepo.Roles["r1"] = &model.Role{ID: "r1",
			Policies: datatypes.JSON([]byte(`{"pinLimit":{"useDefault":false,"priority":0,"value":12}}`))}
		assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}

		policies := svc.GetUserPolicies("u1")
		v, ok := policies["pinLimit"].(int)
		require.True(t, ok, "role override 経由でも int を返す")
		assert.Equal(t, 12, v)
	})
}

func TestAssign_Success(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Admin"}

	err := svc.Assign("user1", "r1", nil)
	require.NoError(t, err)
	assert.Len(t, assignRepo.Assignments, 1)
}

func TestAssign_RoleNotFound(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	err := svc.Assign("user1", "ghost", nil)
	assert.ErrorIs(t, err, role.ErrRoleNotFound)
}

func TestAssign_AlreadyAssigned(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}

	err := svc.Assign("user1", "r1", nil)
	assert.ErrorIs(t, err, role.ErrAlreadyAssigned)
}

func TestAssign_WithExpiry(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	exp := time.Now().Add(24 * time.Hour)
	err := svc.Assign("user1", "r1", &exp)
	require.NoError(t, err)
}

// recordingRoleAssignNotifier captures OnRoleAssigned calls (#1559)。
type recordingRoleAssignNotifier struct{ calls [][2]string }

func (n *recordingRoleAssignNotifier) OnRoleAssigned(userID, roleID string) {
	n.calls = append(n.calls, [2]string{userID, roleID})
}

// #1559 [MEDIUM] public role 割当時に roleAssigned 通知が発火する。
func TestAssign_FiresRoleAssignedForPublicRole(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "VIP", IsPublic: true}
	notifier := &recordingRoleAssignNotifier{}
	svc.SetRoleAssignNotifier(notifier)

	require.NoError(t, svc.Assign("user1", "r1", nil))
	require.Equal(t, [][2]string{{"user1", "r1"}}, notifier.calls)
}

// 非 public role の割当では通知しない (upstream の role.isPublic ガード)。
func TestAssign_NoNotificationForNonPublicRole(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Hidden", IsPublic: false}
	notifier := &recordingRoleAssignNotifier{}
	svc.SetRoleAssignNotifier(notifier)

	require.NoError(t, svc.Assign("user1", "r1", nil))
	assert.Empty(t, notifier.calls, "non-public role は通知しない")
}

func TestUnassign_Success(t *testing.T) {
	svc, _, assignRepo, _ := newTestService(t)
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}

	err := svc.Unassign("user1", "r1")
	require.NoError(t, err)
	assert.Empty(t, assignRepo.Assignments)
}

func TestUnassign_NotAssigned(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	err := svc.Unassign("user1", "r1")
	assert.ErrorIs(t, err, role.ErrNotAssigned)
}

func TestCreate_Success(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	r, err := svc.Create("Admin", "Administrator role", role.CreateOptions{
		IsAdministrator: true,
		IsPublic:        true,
	})
	require.NoError(t, err)
	assert.Equal(t, "Admin", r.Name)
	assert.True(t, r.IsAdministrator)
	assert.Len(t, roleRepo.Roles, 1)
}

func TestShow_Found(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Test"}
	r, err := svc.Show("r1")
	require.NoError(t, err)
	assert.Equal(t, "Test", r.Name)
}

func TestShow_NotFound(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	_, err := svc.Show("ghost")
	assert.ErrorIs(t, err, role.ErrRoleNotFound)
}

func TestList(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A"}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "B"}
	roles, err := svc.List()
	require.NoError(t, err)
	assert.Len(t, roles, 2)
}

func TestDelete_Success(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	err := svc.Delete("r1")
	require.NoError(t, err)
	assert.Empty(t, roleRepo.Roles)
}

func TestDelete_NotFound(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	err := svc.Delete("ghost")
	assert.ErrorIs(t, err, role.ErrRoleNotFound)
}

// --- Error path tests ---

func TestGetUserRoles_RepoError(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	// assignRepo は空なのでエラーにならないが、nil role は除外される
	roles, err := svc.GetUserRoles("user1")
	require.NoError(t, err)
	assert.Empty(t, roles)
}

func TestIsAdministrator_GetRolesError(t *testing.T) {
	// metaRepo にMeta設定してrootUser判定をスキップ → GetUserRoles を呼ぶ
	svc, _, _, metaRepo := newTestService(t)
	metaRepo.Meta = &model.Meta{ID: "x"}
	// assignRepo が空なので GetUserRoles は空を返す → false
	assert.False(t, svc.IsAdministrator("user1"))
}

func TestIsModerator_RootUser(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	rootID := "root1"
	metaRepo.Meta = &model.Meta{ID: "x", RootUserID: &rootID}
	assert.True(t, svc.IsModerator("root1"))
}

func TestIsModerator_MetaFetchError(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	metaRepo.FetchErr = errors.New("meta unavailable")
	assert.False(t, svc.IsModerator("user1"))
}

func TestAssign_ExistsError(t *testing.T) {
	// Exists がエラーを返すケースは mock では発生しにくいが、
	// role が存在して assignment が空の場合は正常に Assign できるので
	// Create のエラーパスをテスト
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	err := svc.Assign("user1", "r1", nil)
	require.NoError(t, err)
}

func TestUnassign_DeleteError(t *testing.T) {
	// Unassign で Exists true → Delete → mock では常に成功
	svc, _, assignRepo, _ := newTestService(t)
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}
	require.NoError(t, svc.Unassign("user1", "r1"))
}

func TestCreate_RepoError(t *testing.T) {
	// mock の Create は常に成功するのでエラーパスは到達しにくい
	// 正常パスの補強として CreateOptions の全フィールドを検証
	svc, _, _, _ := newTestService(t)
	r, err := svc.Create("Mod", "Moderator", role.CreateOptions{
		IsModerator:  true,
		AsBadge:      true,
		IsExplorable: true,
		DisplayOrder: 10,
	})
	require.NoError(t, err)
	assert.True(t, r.IsModerator)
	assert.Equal(t, 10, r.DisplayOrder)
}

func TestApplyRolePolicies_EmptyPolicies(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{
		ID:       "r1",
		Policies: datatypes.JSON([]byte(`{}`)),
	}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}
	policies := svc.GetUserPolicies("user1")
	assert.Equal(t, 100, policies["driveCapacityMb"]) // デフォルト維持
}

// --- Failing repository tests ---

type failingAssignRepo struct {
	*testutil.MockRoleAssignmentRepository
}

func (f *failingAssignRepo) ListByUser(_ string) ([]*model.RoleAssignment, error) {
	return nil, assert.AnError
}

func (f *failingAssignRepo) Exists(_ string, _ string) (bool, error) {
	return false, assert.AnError
}

func (f *failingAssignRepo) CountActiveByRole(_ string) (int, error) {
	return 0, assert.AnError
}

// CountAssignedUsers は active assignment 数を返す。
func TestCountAssignedUsers_Counts(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "R"}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}
	assignRepo.Assignments["u2:r1"] = &model.RoleAssignment{ID: "a2", UserID: "u2", RoleID: "r1"}
	assert.Equal(t, 2, svc.CountAssignedUsers("r1"))
}

// repo error 時は fail-open で 0 を返す (pack 全体を失敗させない)。
func TestCountAssignedUsers_FailOpen(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := &failingAssignRepo{testutil.NewMockRoleAssignmentRepository(roleRepo)}
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	assert.Equal(t, 0, svc.CountAssignedUsers("r1"))
}

type failingRoleRepo struct {
	*testutil.MockRoleRepository
}

func (f *failingRoleRepo) Create(_ *model.Role) error { return assert.AnError }

func TestGetUserRoles_ListByUserError(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := &failingAssignRepo{testutil.NewMockRoleAssignmentRepository(roleRepo)}
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)

	_, err := svc.GetUserRoles("user1")
	assert.Error(t, err)
}

func TestIsAdministrator_GetRolesError_ReturnsFalse(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := &failingAssignRepo{testutil.NewMockRoleAssignmentRepository(roleRepo)}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)

	assert.False(t, svc.IsAdministrator("user1"))
}

func TestIsModerator_GetRolesError_ReturnsFalse(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := &failingAssignRepo{testutil.NewMockRoleAssignmentRepository(roleRepo)}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)

	assert.False(t, svc.IsModerator("user1"))
}

func TestAssign_ExistsRepoError(t *testing.T) {
	rr := testutil.NewMockRoleRepository()
	rr.Roles["r1"] = &model.Role{ID: "r1"}
	assignRepo := &failingAssignRepo{testutil.NewMockRoleAssignmentRepository(rr)}
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(rr, assignRepo, metaRepo, idGen)

	err := svc.Assign("user1", "r1", nil)
	assert.Error(t, err)
}

func TestUnassign_ExistsRepoError(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := &failingAssignRepo{testutil.NewMockRoleAssignmentRepository(roleRepo)}
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)

	err := svc.Unassign("user1", "r1")
	assert.Error(t, err)
}

func TestCreate_RepoCreateError(t *testing.T) {
	rr := &failingRoleRepo{testutil.NewMockRoleRepository()}
	assignRepo := testutil.NewMockRoleAssignmentRepository(rr.MockRoleRepository)
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(rr, assignRepo, metaRepo, idGen)

	_, err := svc.Create("Test", "desc", role.CreateOptions{})
	assert.Error(t, err)
}

func TestApplyRolePolicies_NilPolicies(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Policies: nil}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}
	policies := svc.GetUserPolicies("user1")
	assert.Equal(t, 100, policies["driveCapacityMb"])
}

func TestDefaultPolicies(t *testing.T) {
	p := role.DefaultPolicies()
	assert.Equal(t, true, p["gtlAvailable"])
	assert.Equal(t, 100, p["driveCapacityMb"])
	assert.Equal(t, 5, p["pinLimit"])
}

// #1377: DefaultPolicies は共有 cache を返すため、DefaultPoliciesClone は
// 独立した copy でなければならない。clone を mutate しても共有 default が
// 壊れないことを保証する (壊れると全リクエストの policy が汚染される)。
func TestDefaultPoliciesClone_Isolation(t *testing.T) {
	origDrive := role.DefaultPolicies()["driveCapacityMb"]

	clone := role.DefaultPoliciesClone()
	assert.Equal(t, origDrive, clone["driveCapacityMb"], "clone は同値で始まる")

	clone["driveCapacityMb"] = 999999
	clone["injected"] = true

	assert.Equal(t, origDrive, role.DefaultPolicies()["driveCapacityMb"],
		"clone の mutation が共有 default を破壊している")
	_, leaked := role.DefaultPolicies()["injected"]
	assert.False(t, leaked, "clone への key 追加が共有 default に漏れている")
}

func TestGetUserPolicies_NativeSliceMutationIsIsolated(t *testing.T) {
	want := []string{"text/*", "application/json", "image/*", "video/*", "audio/*"}
	shared := role.DefaultPolicies()["uploadableFileTypes"].([]string)
	original := append([]string(nil), shared...)
	defer copy(shared, original)

	svc, _, _, _ := newTestService(t)
	policies := svc.GetUserPolicies("u1")
	policies["uploadableFileTypes"].([]string)[0] = "application/x-corrupt"

	assert.Equal(t, want, role.DefaultPolicies()["uploadableFileTypes"])
	assert.Equal(t, want, svc.GetUserPolicies("u1")["uploadableFileTypes"])
	assert.Equal(t, want, role.DefaultPoliciesClone()["uploadableFileTypes"])
}

// #1377: GetUserPolicies は applyMetaBasePolicies で base を mutate するため
// clone を使う。meta override が共有 DefaultPolicies cache を汚染しないことを
// 保証する (= 本最適化の安全性の要)。clone を使わないと本テストは fail する。
func TestGetUserPolicies_MetaOverrideDoesNotCorruptSharedDefault(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	metaRepo.Meta = &model.Meta{
		ID:       "x",
		Policies: datatypes.JSON([]byte(`{"driveCapacityMb": 5000}`)),
	}
	origDrive := role.DefaultPolicies()["driveCapacityMb"] // = 100

	got := svc.GetUserPolicies("") // userID="" → base + meta override のみ
	assert.Equal(t, 5000, got["driveCapacityMb"], "meta override が反映される")

	assert.Equal(t, origDrive, role.DefaultPolicies()["driveCapacityMb"],
		"GetUserPolicies の meta override が共有 default を破壊している (#1377)")
}

func TestIsSilenced_DefaultIsFalse(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	// 誰もロールを持っていない → DefaultPolicies は canPublicNote=true
	assert.False(t, svc.IsSilenced("user1"))
}

func TestIsSilenced_CanPublicNoteFalseMeansSilenced(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	// policies JSON で canPublicNote=false を上書きした role を持たせる
	roleRepo.Roles["r1"] = &model.Role{
		ID: "r1",
		Policies: datatypes.JSON([]byte(
			`{"canPublicNote":{"useDefault":false,"priority":1,"value":false}}`,
		)),
	}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}
	assert.True(t, svc.IsSilenced("user1"))
}

// --- per-user role cache (#300 3-5) -----------------------------------------

// countingAssignmentRepo wraps the mock to count ListByUser calls so we can
// verify the role cache hits.
type countingAssignmentRepo struct {
	*testutil.MockRoleAssignmentRepository
	listByUserCalls int
}

func (c *countingAssignmentRepo) ListByUser(userID string) ([]*model.RoleAssignment, error) {
	c.listByUserCalls++
	return c.MockRoleAssignmentRepository.ListByUser(userID)
}

func newServiceWithCountingAssign(t *testing.T) (*role.Service, *testutil.MockRoleRepository, *countingAssignmentRepo) {
	t.Helper()
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := &countingAssignmentRepo{
		MockRoleAssignmentRepository: testutil.NewMockRoleAssignmentRepository(roleRepo),
	}
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	return svc, roleRepo, assignRepo
}

// 同一 user に対する 10 連続 GetUserRoles で assignmentRepo.ListByUser が
// 1 回しか呼ばれないことを担保 (#300 3-5)。
func TestGetUserRoles_CachedHitsRepoOnce(t *testing.T) {
	svc, roleRepo, assignRepo := newServiceWithCountingAssign(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Mod", IsModerator: true}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}

	for i := 0; i < 10; i++ {
		roles, err := svc.GetUserRoles("user1")
		require.NoError(t, err)
		require.Len(t, roles, 1)
	}
	assert.Equal(t, 1, assignRepo.listByUserCalls,
		"per-user role list should be cached; only the first call hits the repo")
}

func TestGetUserRoles_ReturnsIndependentDeepClones(t *testing.T) {
	svc, roleRepo, assignRepo := newServiceWithCountingAssign(t)
	manualColor := "red"
	manualIcon := "https://example.com/manual.png"
	manual := &model.Role{
		ID:          "manual",
		Name:        "Manual",
		Color:       &manualColor,
		IconURL:     &manualIcon,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
		Policies:    datatypes.JSON([]byte(`{"canSearchNotes":{"value":true}}`)),
	}
	conditionalColor := "blue"
	conditionalIcon := "https://example.com/conditional.png"
	conditional := &model.Role{
		ID:          "conditional",
		Name:        "Conditional",
		Color:       &conditionalColor,
		IconURL:     &conditionalIcon,
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
		Policies:    datatypes.JSON([]byte(`{"canSearchUsers":{"value":true}}`)),
	}
	roleRepo.Roles[manual.ID] = manual
	roleRepo.Roles[conditional.ID] = conditional
	assignRepo.Assignments["u1:manual"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: manual.ID}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u1", Username: "bot", IsBot: true}))
	svc.SetUserRepo(userRepo)

	first, err := svc.GetUserRoles("u1")
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.NotSame(t, manual, first[0])
	require.NotSame(t, conditional, first[1])

	first[0].Name = "changed"
	*first[0].Color = "green"
	*first[0].IconURL = "https://example.com/changed.png"
	first[0].CondFormula[0] = '['
	first[0].Policies[0] = '['
	first[1] = nil

	second, err := svc.GetUserRoles("u1")
	require.NoError(t, err)
	require.Len(t, second, 2)
	assert.Equal(t, "Manual", second[0].Name)
	assert.Equal(t, "red", *second[0].Color)
	assert.Equal(t, "https://example.com/manual.png", *second[0].IconURL)
	assert.JSONEq(t, `{"type":"isBot"}`, string(second[0].CondFormula))
	assert.JSONEq(t, `{"canSearchNotes":{"value":true}}`, string(second[0].Policies))
	assert.Equal(t, "Conditional", second[1].Name)
	require.NotSame(t, first[0], second[0])
	require.NotSame(t, manual.Color, second[0].Color)
	require.NotSame(t, manual.IconURL, second[0].IconURL)
	assert.Equal(t, 1, assignRepo.listByUserCalls)

	second[0].Policies[0] = '['
	third, err := svc.GetUserRoles("u1")
	require.NoError(t, err)
	assert.JSONEq(t, `{"canSearchNotes":{"value":true}}`, string(third[0].Policies))
}

func TestGetUserRoles_PreservesNilAndEmptyMutableFields(t *testing.T) {
	svc, roleRepo, assignRepo := newServiceWithCountingAssign(t)
	roleRepo.Roles["r1"] = &model.Role{
		ID:          "r1",
		CondFormula: datatypes.JSON{},
		Policies:    nil,
	}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}

	roles, err := svc.GetUserRoles("u1")
	require.NoError(t, err)
	require.Len(t, roles, 1)
	assert.Nil(t, roles[0].Color)
	assert.Nil(t, roles[0].IconURL)
	assert.NotNil(t, roles[0].CondFormula)
	assert.Empty(t, roles[0].CondFormula)
	assert.Nil(t, roles[0].Policies)
}

func TestGetUserRoles_CacheSnapshotIgnoresRepositoryMutationUntilInvalidated(t *testing.T) {
	svc, roleRepo, assignRepo := newServiceWithCountingAssign(t)
	oldColor := "red"
	oldRole := &model.Role{
		ID:       "r1",
		Name:     "Old",
		Color:    &oldColor,
		Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"value":true}}`)),
	}
	roleRepo.Roles["r1"] = oldRole
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}

	warmed, err := svc.GetUserRoles("u1")
	require.NoError(t, err)
	require.Len(t, warmed, 1)
	oldRole.Name = "Mutated"
	*oldRole.Color = "green"
	oldRole.Policies[0] = '['
	newColor := "blue"
	replacement := &model.Role{
		ID:       "r1",
		Name:     "New",
		Color:    &newColor,
		Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"value":false}}`)),
	}
	roleRepo.Roles["r1"] = replacement

	cached, err := svc.GetUserRoles("u1")
	require.NoError(t, err)
	assert.Equal(t, "Old", cached[0].Name)
	assert.Equal(t, "red", *cached[0].Color)
	assert.JSONEq(t, `{"canSearchNotes":{"value":true}}`, string(cached[0].Policies))

	require.NoError(t, svc.InvalidateUser(context.Background(), "u1"))
	refreshed, err := svc.GetUserRoles("u1")
	require.NoError(t, err)
	assert.Equal(t, "New", refreshed[0].Name)
	require.NotSame(t, replacement, refreshed[0])
	require.NotSame(t, replacement.Color, refreshed[0].Color)
	assert.JSONEq(t, `{"canSearchNotes":{"value":false}}`, string(refreshed[0].Policies))
}

func TestGetUserRoles_ConditionalCacheSnapshotIgnoresRepositoryMutationUntilInvalidated(t *testing.T) {
	svc, roleRepo, _ := newServiceWithCountingRoleRepo(t)
	oldRole := &model.Role{
		ID:          "r1",
		Name:        "Old",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
		Policies:    datatypes.JSON([]byte(`{"canSearchNotes":{"value":true}}`)),
	}
	roleRepo.Roles["r1"] = oldRole
	userRepo := testutil.NewMockUserRepository()
	for _, userID := range []string{"u1", "u2", "u3"} {
		require.NoError(t, userRepo.Create(&model.User{ID: userID, Username: userID, IsBot: true}))
	}
	svc.SetUserRepo(userRepo)

	warmed, err := svc.GetUserRoles("u1")
	require.NoError(t, err)
	require.Len(t, warmed, 1)
	oldRole.Name = "Mutated"
	oldRole.CondFormula[0] = '['
	oldRole.Policies[0] = '['
	replacement := &model.Role{
		ID:          "r1",
		Name:        "New",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
		Policies:    datatypes.JSON([]byte(`{"canSearchNotes":{"value":false}}`)),
	}
	roleRepo.Roles["r1"] = replacement

	cached, err := svc.GetUserRoles("u2")
	require.NoError(t, err)
	require.Len(t, cached, 1)
	assert.Equal(t, "Old", cached[0].Name)
	assert.JSONEq(t, `{"canSearchNotes":{"value":true}}`, string(cached[0].Policies))
	assert.Equal(t, 1, roleRepo.listCalls)

	require.NoError(t, svc.InvalidateRolePolicies(context.Background(), "r1"))
	refreshed, err := svc.GetUserRoles("u3")
	require.NoError(t, err)
	require.Len(t, refreshed, 1)
	assert.Equal(t, "New", refreshed[0].Name)
	require.NotSame(t, replacement, refreshed[0])
	assert.Equal(t, 2, roleRepo.listCalls)
}

// Assign で当該ユーザーの cache が invalidate されることを担保。
func TestAssign_InvalidatesUserRoleCache(t *testing.T) {
	svc, roleRepo, assignRepo := newServiceWithCountingAssign(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Mod"}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "Trial"}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}

	// warm cache
	_, _ = svc.GetUserRoles("user1")
	assert.Equal(t, 1, assignRepo.listByUserCalls)

	require.NoError(t, svc.Assign("user1", "r2", nil))

	// next read should miss cache and go to DB
	_, _ = svc.GetUserRoles("user1")
	assert.Equal(t, 2, assignRepo.listByUserCalls,
		"Assign must invalidate the user's role cache so next read is fresh")
}

// Unassign で当該ユーザーの cache が invalidate されることを担保。
func TestUnassign_InvalidatesUserRoleCache(t *testing.T) {
	svc, roleRepo, assignRepo := newServiceWithCountingAssign(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Mod"}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}

	_, _ = svc.GetUserRoles("user1")
	assert.Equal(t, 1, assignRepo.listByUserCalls)

	require.NoError(t, svc.Unassign("user1", "r1"))

	_, _ = svc.GetUserRoles("user1")
	assert.Equal(t, 2, assignRepo.listByUserCalls,
		"Unassign must invalidate the user's role cache")
}

// Service.Delete (role 削除) で全 user の cache が flush されること。
func TestDelete_InvalidatesAllRoleCaches(t *testing.T) {
	svc, roleRepo, assignRepo := newServiceWithCountingAssign(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Mod"}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "Trial"}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}
	assignRepo.Assignments["user2:r1"] = &model.RoleAssignment{
		ID: "a2", UserID: "user2", RoleID: "r1",
	}

	// warm cache for both users
	_, _ = svc.GetUserRoles("user1")
	_, _ = svc.GetUserRoles("user2")
	assert.Equal(t, 2, assignRepo.listByUserCalls)

	require.NoError(t, svc.Delete("r2")) // 別 role の delete でも全 flush

	_, _ = svc.GetUserRoles("user1")
	_, _ = svc.GetUserRoles("user2")
	assert.Equal(t, 4, assignRepo.listByUserCalls,
		"Delete should flush every cached entry (we don't know which users were assigned)")
}

func TestGetUserRoles_EmptyUserIDDoesNotCache(t *testing.T) {
	svc, _, assignRepo := newServiceWithCountingAssign(t)
	roles, err := svc.GetUserRoles("")
	require.NoError(t, err)
	assert.Empty(t, roles)
	assert.Equal(t, 0, assignRepo.listByUserCalls,
		"empty userID must not hit the repo at all")
}

// --- #1030: roleRepo.List() の TTL cache ---

type countingRoleRepo struct {
	*testutil.MockRoleRepository
	listCalls int
}

func (c *countingRoleRepo) List() ([]*model.Role, error) {
	c.listCalls++
	return c.MockRoleRepository.List()
}

func newServiceWithCountingRoleRepo(t *testing.T) (*role.Service, *countingRoleRepo, *testutil.MockRoleAssignmentRepository) {
	t.Helper()
	mockRoleRepo := testutil.NewMockRoleRepository()
	roleRepo := &countingRoleRepo{MockRoleRepository: mockRoleRepo}
	assignRepo := testutil.NewMockRoleAssignmentRepository(mockRoleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	return svc, roleRepo, assignRepo
}

// evaluateConditionalRoles 経由で複数 user が連続して GetUserRoles を呼んでも、
// roleRepo.List() は cache hit で 1 回しか発火しないことを保証する (#1030)。
// userRoleCache は user 単位なので別 user の cache miss でも fire するが、
// rolesListCache は global なので「1 回呼んだら 5 min 共有」になる。
func TestListRolesCached_HitsRepoOnceAcrossUsers(t *testing.T) {
	svc, roleRepo, _ := newServiceWithCountingRoleRepo(t)
	// conditional role を 1 件用意して evaluateConditionalRoles 経路を起動
	roleRepo.Roles["r-bot"] = &model.Role{
		ID:          "r-bot",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
	}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u1", Username: "u1", IsBot: true}))
	require.NoError(t, userRepo.Create(&model.User{ID: "u2", Username: "u2", IsBot: false}))
	svc.SetUserRepo(userRepo)

	// 別 user の連続呼び出しで user-level cache は別 entry を持つが、roleRepo
	// 側は global TTL cache に乗るので呼び出し回数は 1 回のままになる。
	_, _ = svc.GetUserRoles("u1")
	_, _ = svc.GetUserRoles("u2")
	_, _ = svc.GetUserRoles("u1")
	assert.Equal(t, 1, roleRepo.listCalls,
		"rolesListCache は global なので user 跨ぎでも 1 回しか repo を叩かない")
}

// Create で role list cache が invalidate されることを担保。
func TestCreate_InvalidatesRoleListCache(t *testing.T) {
	svc, roleRepo, _ := newServiceWithCountingRoleRepo(t)
	roleRepo.Roles["r-bot"] = &model.Role{
		ID:          "r-bot",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
	}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u1", Username: "u1", IsBot: true}))
	svc.SetUserRepo(userRepo)

	// warm cache
	_, _ = svc.GetUserRoles("u1")
	assert.Equal(t, 1, roleRepo.listCalls)

	// 新規 role を作ると role list cache が flush される
	_, err := svc.Create("NewRole", "desc", role.CreateOptions{})
	require.NoError(t, err)

	// 同 user の userRoleCache はまだ live なので GetUserRoles 自体は repo を
	// 叩かないが、cache を bust するため別 user で確認する。
	require.NoError(t, userRepo.Create(&model.User{ID: "u2", Username: "u2", IsBot: true}))
	_, _ = svc.GetUserRoles("u2")
	assert.Equal(t, 2, roleRepo.listCalls,
		"Create で role list cache が invalidate されて、次回 evaluation で repo を再 fetch")
}

// UpdateFields で role list cache が invalidate されることを担保。
func TestUpdateFields_InvalidatesRoleListCache(t *testing.T) {
	svc, roleRepo, _ := newServiceWithCountingRoleRepo(t)
	roleRepo.Roles["r-bot"] = &model.Role{
		ID:          "r-bot",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
	}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "Other"}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u1", Username: "u1", IsBot: true}))
	require.NoError(t, userRepo.Create(&model.User{ID: "u2", Username: "u2", IsBot: true}))
	svc.SetUserRepo(userRepo)

	_, _ = svc.GetUserRoles("u1")
	assert.Equal(t, 1, roleRepo.listCalls)

	// 別 role の condFormula を変更しても全 role list cache は flush される
	_, err := svc.UpdateFields("r2", map[string]any{"name": "Updated"})
	require.NoError(t, err)

	_, _ = svc.GetUserRoles("u2") // 別 user で確認
	assert.Equal(t, 2, roleRepo.listCalls)
}

// PR #1102 regression guard: UpdateFields は **userRoleCache** も flush する。
// 旧版は rolesListCache だけ flush していて、既に assigned 済 user の policy
// 評価は per-user TTL (5min) の間 stale policies を返し続け、admin で policy
// を変えても効かない現象になっていた。InvalidateAllRoleCaches を呼ぶよう
// 修正したので、UpdateFields 直後に同 user の GetUserRoles を呼ぶと
// assignmentRepo.ListByUser が再 fire する (cache が live なら叩かない)。
func TestUpdateFields_InvalidatesUserRoleCache(t *testing.T) {
	svc, roleRepo, assignRepo := newServiceWithCountingAssign(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Old"}
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "u1", RoleID: "r1",
	}

	// warm cache
	_, _ = svc.GetUserRoles("u1")
	require.Equal(t, 1, assignRepo.listByUserCalls)
	// 2 度目は cache hit
	_, _ = svc.GetUserRoles("u1")
	require.Equal(t, 1, assignRepo.listByUserCalls)

	// admin が policy を update する経路を模擬
	_, err := svc.UpdateFields("r1", map[string]any{"name": "New"})
	require.NoError(t, err)

	// userRoleCache["u1"] が flush されているので、GetUserRoles は再 listing
	_, _ = svc.GetUserRoles("u1")
	assert.Equal(t, 2, assignRepo.listByUserCalls,
		"UpdateFields must flush userRoleCache so admin policy changes take effect immediately")
}

// 並行 cache miss でも roleRepo.List() は single-flight 相当で 1 回しか
// 発火しないことを担保する (#1035 review: RWMutex 化に伴う double-check
// pattern の regression guard、-race で並行性も検証する)。
func TestListRolesCached_ConcurrentMissSingleFlight(t *testing.T) {
	svc, roleRepo, _ := newServiceWithCountingRoleRepo(t)
	roleRepo.Roles["r-bot"] = &model.Role{
		ID:          "r-bot",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
	}
	userRepo := testutil.NewMockUserRepository()
	// 複数 user を作って evaluateConditionalRoles を並行に起動できるようにする
	for i := 0; i < 20; i++ {
		uid := "u" + string(rune('a'+i))
		require.NoError(t, userRepo.Create(&model.User{ID: uid, Username: uid, IsBot: true}))
	}
	svc.SetUserRepo(userRepo)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		uid := "u" + string(rune('a'+i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.GetUserRoles(uid)
		}()
	}
	wg.Wait()
	// 全 20 goroutine が cache miss から開始しても、double-check pattern で
	// roleRepo.List() は 1 回しか呼ばれない (= single-flight 相当)。
	assert.Equal(t, 1, roleRepo.listCalls,
		"concurrent cache miss should de-dup to single roleRepo.List() call")
}

// Delete (= InvalidateAllRoleCaches 経由) で role list cache も flush される。
func TestDelete_InvalidatesRoleListCache(t *testing.T) {
	svc, roleRepo, _ := newServiceWithCountingRoleRepo(t)
	roleRepo.Roles["r-bot"] = &model.Role{
		ID:          "r-bot",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
	}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "Trial"}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u1", Username: "u1", IsBot: true}))
	require.NoError(t, userRepo.Create(&model.User{ID: "u2", Username: "u2", IsBot: true}))
	svc.SetUserRepo(userRepo)

	_, _ = svc.GetUserRoles("u1")
	assert.Equal(t, 1, roleRepo.listCalls)

	require.NoError(t, svc.Delete("r2"))

	_, _ = svc.GetUserRoles("u2")
	assert.Equal(t, 2, roleRepo.listCalls)
}

// 失敗した Assign / Unassign は invalidate しない (DB 状態が変わらないため)。
func TestAssign_FailedDoesNotInvalidate(t *testing.T) {
	svc, roleRepo, assignRepo := newServiceWithCountingAssign(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Mod"}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1",
	}

	_, _ = svc.GetUserRoles("user1") // warm

	// 同じ role を再度 Assign → ErrAlreadyAssigned で失敗
	err := svc.Assign("user1", "r1", nil)
	require.Error(t, err)

	_, _ = svc.GetUserRoles("user1")
	assert.Equal(t, 1, assignRepo.listByUserCalls,
		"failed Assign must not invalidate cache")
}

// --- ListByRole ---

func TestListByRole_RoleNotFound(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	_, err := svc.ListByRole("ghost", "", "", 10)
	assert.ErrorIs(t, err, role.ErrRoleNotFound)
}

func TestListByRole_PreloadsUser(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	// MockRoleAssignmentRepository は UserRepo がセットされていれば
	// ListByRole の戻りに User を埋める。
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u1", Username: "alice"}))
	assignRepo.UserRepo = userRepo
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}

	got, err := svc.ListByRole("r1", "", "", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].User)
	assert.Equal(t, "alice", got[0].User.Username)
}

func TestListByRole_RepoError(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	assignRepo := &failingListByRoleAssignRepo{testutil.NewMockRoleAssignmentRepository(roleRepo)}
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)

	_, err := svc.ListByRole("r1", "", "", 10)
	assert.Error(t, err)
}

// errorUserRepo は FindByID で任意の error を返す UserRepository stub。
// ErrRecordNotFound 以外の transient な error 経路 (#785 fallback の
// silent failure 動作) を検証するために MockUserRepository を embed する
// (他のメソッドが将来呼ばれても nil deref しないよう必ず初期化済みの mock
// をセットすること)。
type errorUserRepo struct {
	*testutil.MockUserRepository
	err error
}

func (e *errorUserRepo) FindByID(_ string) (*model.User, error) {
	return nil, e.err
}

// failingListByRoleAssignRepo は ListByRole で error を返す stub。
type failingListByRoleAssignRepo struct {
	*testutil.MockRoleAssignmentRepository
}

func (f *failingListByRoleAssignRepo) ListByRole(_, _, _ string, _ int) ([]*model.RoleAssignment, error) {
	return nil, assert.AnError
}

func TestUpdateFields_Success(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Old", Description: "x"}

	after, err := svc.UpdateFields("r1", map[string]any{"name": "New"})
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, "New", after.Name)
	assert.Equal(t, "x", after.Description, "untouched fields stay intact")
}

func TestUpdateFields_NotFound(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	_, err := svc.UpdateFields("ghost", map[string]any{"name": "x"})
	assert.ErrorIs(t, err, role.ErrRoleNotFound)
}

func TestUpdateFields_EmptyFieldsReturnsCurrent(t *testing.T) {
	// 空 map の場合は UpdateFields を呼ばずに現在値を返す。
	// admin/roles/update が optional pointer 全部 nil で来たケース。
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Stay"}

	after, err := svc.UpdateFields("r1", map[string]any{})
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, "Stay", after.Name)
}

// failingUpdateFieldsRoleRepo は UpdateFields で error を返す stub。
type failingUpdateFieldsRoleRepo struct {
	*testutil.MockRoleRepository
}

func (f *failingUpdateFieldsRoleRepo) UpdateFields(_ string, _ map[string]any) error {
	return assert.AnError
}

func TestUpdateFields_RepoError(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Old"}
	wrapped := &failingUpdateFieldsRoleRepo{roleRepo}
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(wrapped, assignRepo, metaRepo, idGen)

	_, err := svc.UpdateFields("r1", map[string]any{"name": "New"})
	assert.Error(t, err)
}

// upstream Misskey #17121 (= 2026.5.1 fix / triage #1012): HasRolePolicy は
// channels/create 等の requiredRolePolicy gate で使う policy check。default
// policies + role override の merge を見て、root / admin / policy true の
// いずれかなら true を返す。
func TestHasRolePolicy(t *testing.T) {
	t.Run("default_canCreateChannel_true_for_plain_user", func(t *testing.T) {
		svc, _, _, _ := newTestService(t)
		// 何も role 設定なし → DefaultPolicies の canCreateChannel=true がそのまま
		assert.True(t, svc.HasRolePolicy("alice", "canCreateChannel"))
	})

	t.Run("default_canHideAds_false_for_plain_user", func(t *testing.T) {
		svc, _, _, _ := newTestService(t)
		// DefaultPolicies の canHideAds=false がそのまま deny される
		assert.False(t, svc.HasRolePolicy("alice", "canHideAds"))
	})

	t.Run("root_bypasses_policy_false", func(t *testing.T) {
		svc, _, _, metaRepo := newTestService(t)
		rootID := "root1"
		metaRepo.Meta = &model.Meta{ID: "x", RootUserID: &rootID}
		// canHideAds は default false だが root は無条件で true
		assert.True(t, svc.HasRolePolicy("root1", "canHideAds"))
	})

	t.Run("admin_role_bypasses_policy_false", func(t *testing.T) {
		svc, roleRepo, assignRepo, _ := newTestService(t)
		roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Admin", IsAdministrator: true}
		assignRepo.Assignments["alice:r1"] = &model.RoleAssignment{
			ID: "a1", UserID: "alice", RoleID: "r1",
		}
		// admin role assignment → policy 値に関係なく true
		assert.True(t, svc.HasRolePolicy("alice", "canHideAds"))
	})

	t.Run("role_override_can_deny_canCreateChannel", func(t *testing.T) {
		svc, roleRepo, assignRepo, _ := newTestService(t)
		// canCreateChannel=false を override する role を作成
		roleRepo.Roles["r_no_channel"] = &model.Role{
			ID:       "r_no_channel",
			Name:     "NoChannel",
			Policies: datatypes.JSON([]byte(`{"canCreateChannel": {"useDefault": false, "priority": 0, "value": false}}`)),
		}
		assignRepo.Assignments["bob:r_no_channel"] = &model.RoleAssignment{
			ID: "a2", UserID: "bob", RoleID: "r_no_channel",
		}
		assert.False(t, svc.HasRolePolicy("bob", "canCreateChannel"),
			"role override で canCreateChannel=false → deny")
	})

	t.Run("unknown_policy_key_denies", func(t *testing.T) {
		svc, _, _, _ := newTestService(t)
		// 未知 key は fail-closed
		assert.False(t, svc.HasRolePolicy("alice", "thisDoesNotExist"))
	})
}

// MergeMetaPolicies overlays the instance's meta.policies JSON override onto the
// default policies (upstream { ...DEFAULT_POLICIES, ...instance.policies }) and
// coerces numeric overrides to the default's Go type.
func TestMergeMetaPolicies(t *testing.T) {
	t.Run("nil override returns defaults", func(t *testing.T) {
		merged := role.MergeMetaPolicies(nil)
		assert.Equal(t, role.DefaultPolicies()["gtlAvailable"], merged["gtlAvailable"])
		assert.Equal(t, role.DefaultPolicies()["mentionLimit"], merged["mentionLimit"])
	})

	t.Run("override replaces defaults and coerces numeric type", func(t *testing.T) {
		// JSON は数値を float64 に decode するが、default で int の key は
		// int へ coerce される必要がある (consumer の type assert 保護)。
		merged := role.MergeMetaPolicies([]byte(`{"gtlAvailable":false,"mentionLimit":99}`))
		assert.Equal(t, false, merged["gtlAvailable"])
		assert.Equal(t, 99, merged["mentionLimit"], "int policy は int で coerce される")
		// 未指定 key は default のまま。
		assert.Equal(t, true, merged["ltlAvailable"])
	})

	// `/api/meta` や frontend に埋め込む meta の policies も、size の 2 つの key の
	// 小数を残す (#3489)。frontend はこの値でアップロードを手前で止める。
	t.Run("fractional size policies are kept", func(t *testing.T) {
		merged := role.MergeMetaPolicies([]byte(`{"maxFileSizeMb":0.5,"driveCapacityMb":1.5,"pinLimit":2.7}`))
		assert.Equal(t, 0.5, merged["maxFileSizeMb"])
		assert.Equal(t, 1.5, merged["driveCapacityMb"])
		assert.Equal(t, 2, merged["pinLimit"], "other int keys are still rounded")
	})
	t.Run("invalid JSON falls back to defaults", func(t *testing.T) {
		merged := role.MergeMetaPolicies([]byte(`{not json`))
		assert.Equal(t, role.DefaultPolicies()["ltlAvailable"], merged["ltlAvailable"])
	})

	t.Run("does not mutate the shared default map", func(t *testing.T) {
		_ = role.MergeMetaPolicies([]byte(`{"gtlAvailable":false}`))
		assert.Equal(t, true, role.DefaultPolicies()["gtlAvailable"], "default cache は不変")
	})

	t.Run("slice policy override is coerced to []string", func(t *testing.T) {
		merged := role.MergeMetaPolicies([]byte(`{"uploadableFileTypes":["text/*","image/*"]}`))
		assert.Equal(t, []string{"text/*", "image/*"}, merged["uploadableFileTypes"])
	})

	t.Run("empty slice override falls back to default", func(t *testing.T) {
		// coerceToBaseType の slice 経路は normalize 後 0 件で default に巻き戻す
		// (aggregation helper 由来の意味論)。空配列にする運用は degenerate なので
		// この挙動を仕様として固定する。
		merged := role.MergeMetaPolicies([]byte(`{"uploadableFileTypes":[]}`))
		assert.Equal(t, role.DefaultPolicies()["uploadableFileTypes"], merged["uploadableFileTypes"])
	})

	t.Run("explicit JSON null override returns defaults", func(t *testing.T) {
		merged := role.MergeMetaPolicies([]byte(`null`))
		assert.Equal(t, role.DefaultPolicies()["ltlAvailable"], merged["ltlAvailable"])
	})

	t.Run("unknown key passes through", func(t *testing.T) {
		merged := role.MergeMetaPolicies([]byte(`{"someUnknownPolicy":42}`))
		assert.Equal(t, float64(42), merged["someUnknownPolicy"], "未知 key は coerce 対象外で素通し")
	})
}

func TestService_GetModerators(t *testing.T) {
	svc, roleRepo, assignRepo, metaRepo := newTestService(t)
	userRepo := testutil.NewMockUserRepository()
	svc.SetUserRepo(userRepo)

	// moderator role + 通常 role を用意。
	roleRepo.Roles["modrole"] = &model.Role{ID: "modrole", IsModerator: true}
	roleRepo.Roles["normalrole"] = &model.Role{ID: "normalrole"}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "ga1", UserID: "mod1", RoleID: "modrole"}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "ga2", UserID: "normal1", RoleID: "normalrole"}))
	userRepo.Users["mod1"] = &model.User{ID: "mod1"}
	userRepo.Users["root1"] = &model.User{ID: "root1"}
	// root user を meta に設定 (includeRoot)。
	rootID := "root1"
	metaRepo.Meta = &model.Meta{RootUserID: &rootID}

	mods, err := svc.GetModerators()
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, u := range mods {
		ids[u.ID] = true
	}
	assert.True(t, ids["mod1"], "moderator role を持つ user が含まれる")
	assert.True(t, ids["root1"], "root user が含まれる")
	assert.False(t, ids["normal1"], "通常 user は含まれない")
}

func TestService_GetModerators_NilUserRepo(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	// userRepo 未配線 (drop-in optional) は nil を返す。
	mods, err := svc.GetModerators()
	require.NoError(t, err)
	assert.Nil(t, mods)
}

// #2061: Assign / Unassign は role.lastUsedAt を更新する (upstream RoleService)。
func TestAssignUnassign_UpdatesLastUsedAt(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Admin", LastUsedAt: old}

	require.NoError(t, svc.Assign("user1", "r1", nil))
	assert.True(t, roleRepo.Roles["r1"].LastUsedAt.After(old), "Assign が lastUsedAt を更新 (#2061)")

	roleRepo.Roles["r1"].LastUsedAt = old // reset
	require.NoError(t, svc.Unassign("user1", "r1"))
	assert.True(t, roleRepo.Roles["r1"].LastUsedAt.After(old), "Unassign が lastUsedAt を更新 (#2061)")
}

// #2061: ListByLastUsed は lastUsedAt DESC で返す。
func TestService_ListByLastUsed(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["a"] = &model.Role{ID: "a", LastUsedAt: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)}
	roleRepo.Roles["b"] = &model.Role{ID: "b", LastUsedAt: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)}
	roleRepo.Roles["c"] = &model.Role{ID: "c", LastUsedAt: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)}

	roles, err := svc.ListByLastUsed()
	require.NoError(t, err)
	require.Len(t, roles, 3)
	assert.Equal(t, []string{"b", "c", "a"}, []string{roles[0].ID, roles[1].ID, roles[2].ID}, "lastUsedAt DESC (#2061)")
}

// #2069 (upstream #17389): maxFileSizeMb は server 全体上限 (SetServerMaxFileSizeMb)
// で cap される。role が server 上限超の値を持っても cap 後の値になり、base-only
// (role 無し) ユーザーでも cap が効く。
func TestGetUserPolicies_MaxFileSizeServerCap(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	svc.SetServerMaxFileSizeMb(100) // server 全体 100MB

	// role が maxFileSizeMb=500 を要求 → server cap 100 に丸められる。
	roleRepo.Roles["r1"] = &model.Role{
		ID: "r1", Name: "Big",
		Policies: datatypes.JSON([]byte(`{"maxFileSizeMb": {"useDefault": false, "priority": 0, "value": 500}}`)),
	}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "user1", RoleID: "r1"}
	assert.Equal(t, 100, svc.GetUserPolicies("user1")["maxFileSizeMb"], "role 値 500 は server cap 100 に丸められる (#2069)")

	// role 無し (base-only) でも cap が効く: default 30 < cap 100 なので default 維持。
	assert.Equal(t, 30, svc.GetUserPolicies("user2")["maxFileSizeMb"], "base default 30 は cap 100 未満で維持")

	// cap が default 未満なら default も cap される (base-role-only bug fix)。
	svc.SetServerMaxFileSizeMb(10)
	assert.Equal(t, 10, svc.GetUserPolicies("user2")["maxFileSizeMb"], "cap 10 < default 30 → base-only でも 10 に cap (#2069)")
}

// #2313: 分割アップロードの policy はインスタンス設定 (meta) で cap される。
// これが無いと admin が role に大きい値を入れるだけで、未完了マルチパート
// アップロードでオブジェクトストレージを埋められる。
func TestGetUserPolicies_ChunkedUploadServerCap(t *testing.T) {
	svc, roleRepo, assignRepo, metaRepo := newTestService(t)
	metaRepo.Meta = &model.Meta{
		ID:                               "x",
		ChunkedUploadMaxSessionsPerUser:  2,
		ChunkedUploadMaxPendingMbPerUser: 64,
	}

	roleRepo.Roles["r1"] = &model.Role{
		ID: "r1", Name: "Big",
		Policies: datatypes.JSON([]byte(`{
			"chunkedUploadMaxConcurrentSessions": {"useDefault": false, "priority": 0, "value": 100},
			"chunkedUploadMaxPendingMb": {"useDefault": false, "priority": 0, "value": 100000}
		}`)),
	}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "user1", RoleID: "r1"}

	p := svc.GetUserPolicies("user1")
	assert.Equal(t, 2, p[role.PolicyChunkedUploadMaxConcurrentSessions], "role 値はインスタンス設定に丸められる")
	assert.Equal(t, 64, p[role.PolicyChunkedUploadMaxPendingMb])

	// role 無し (base-only) でも cap が効く。
	base := svc.GetUserPolicies("user2")
	assert.Equal(t, 2, base[role.PolicyChunkedUploadMaxConcurrentSessions])
	assert.Equal(t, 64, base[role.PolicyChunkedUploadMaxPendingMb])
}

func TestGetUserPolicies_NativeNegativeUnlimitedCannotBypassInstanceCaps(t *testing.T) {
	svc, roleRepo, assignRepo, metaRepo := newTestService(t)
	svc.SetServerMaxFileSizeMb(100)
	metaRepo.Meta = &model.Meta{
		ID:                               "x",
		ChunkedUploadMaxSessionsPerUser:  2,
		ChunkedUploadMaxPendingMbPerUser: 64,
	}
	roleRepo.Roles["r1"] = &model.Role{
		ID: "r1", Name: "Unlimited",
		Policies: datatypes.JSON([]byte(`{
			"maxFileSizeMb": {"useDefault": false, "priority": 2, "value": -1},
			"chunkedUploadMaxConcurrentSessions": {"useDefault": false, "priority": 2, "value": -1},
			"chunkedUploadMaxPendingMb": {"useDefault": false, "priority": 2, "value": -1}
		}`)),
	}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "user1", RoleID: "r1"}

	policies := svc.GetUserPolicies("user1")
	// maxFileSizeMb の 0 以下は「保存できない」で、server の上限に持ち上げない
	// (本家 `Math.min(serverMaxFileSizeMb, Math.max(...vs))`)。分割アップロードの
	// 独自 policy は 0 以下を上限なしと読むので、インスタンスの上限に丸める。
	assert.Equal(t, -1, policies["maxFileSizeMb"])
	assert.Equal(t, 2, policies[role.PolicyChunkedUploadMaxConcurrentSessions])
	assert.Equal(t, 64, policies[role.PolicyChunkedUploadMaxPendingMb])
}

func TestGetUserPolicies_PositiveFractionalMaxFileSizeBelowCapRemainsUnchanged(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	svc.SetServerMaxFileSizeMb(100)
	roleRepo.Roles["r1"] = &model.Role{
		ID: "r1", Name: "Small",
		Policies: datatypes.JSON([]byte(`{
			"maxFileSizeMb": {"useDefault": false, "priority": 2, "value": 0.5}
		}`)),
	}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "user1", RoleID: "r1"}

	assert.Equal(t, 0.5, svc.GetUserPolicies("user1")["maxFileSizeMb"])
}

// 基本ポリシー (meta) の小数は、size の 2 つの key だけ切り捨てずに残す
// (#3489)。切り捨てると 0.5 が 0 になり、drive で「保存できない」に変わる
// (本家は 0.5MB まで通す)。他の int の key は従来どおり整数に丸める。
func TestGetUserPolicies_FractionalBaseSizePoliciesAreKept(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	svc.SetServerMaxFileSizeMb(100)
	metaRepo.Meta = &model.Meta{ID: "x", Policies: datatypes.JSON([]byte(`{"maxFileSizeMb": 0.5, "driveCapacityMb": 1.5, "pinLimit": 2.7}`))}
	p := svc.GetUserPolicies("user1")
	assert.Equal(t, 0.5, p["maxFileSizeMb"])
	assert.Equal(t, 1.5, p["driveCapacityMb"])
	assert.Equal(t, 2, p["pinLimit"], "other int keys are still rounded")
}

// 基本が小数でも、ロールで付けた値が集約で勝つ (#3489)。base が float64 のとき
// int の分岐に入らず、ロールの値を捨てていた形 (#3484 の作業中に出た回帰)。
func TestGetUserPolicies_FractionalBaseDoesNotHideRoleValue(t *testing.T) {
	svc, roleRepo, assignRepo, metaRepo := newTestService(t)
	svc.SetServerMaxFileSizeMb(250)
	metaRepo.Meta = &model.Meta{ID: "x", Policies: datatypes.JSON([]byte(`{"maxFileSizeMb": 0.5, "driveCapacityMb": 1.5}`))}
	roleRepo.Roles["r1"] = &model.Role{
		ID: "r1", Name: "Big",
		Policies: datatypes.JSON([]byte(`{
			"maxFileSizeMb": {"useDefault": false, "priority": 2, "value": 200},
			"driveCapacityMb": {"useDefault": false, "priority": 2, "value": 5000}
		}`)),
	}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "user1", RoleID: "r1"}
	p := svc.GetUserPolicies("user1")
	assert.Equal(t, 200, p["maxFileSizeMb"])
	assert.Equal(t, 5000, p["driveCapacityMb"])
	// ロールの無い利用者は基本の小数のまま。
	assert.Equal(t, 0.5, svc.GetUserPolicies("nobody")["maxFileSizeMb"])
}

// server の上限を超える小数は上限の値に下がる (policyAboveCap の float64 の枝)。
func TestGetUserPolicies_FractionalMaxFileSizeAboveCapIsLowered(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	svc.SetServerMaxFileSizeMb(100)
	roleRepo.Roles["r1"] = &model.Role{
		ID: "r1", Name: "Big",
		Policies: datatypes.JSON([]byte(`{
			"maxFileSizeMb": {"useDefault": false, "priority": 2, "value": 150.5}
		}`)),
	}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "user1", RoleID: "r1"}
	assert.Equal(t, 100, svc.GetUserPolicies("user1")["maxFileSizeMb"])
}

// cap が既定値より大きい場合は既定値のまま。cap は上限であって強制値ではない。
func TestGetUserPolicies_ChunkedUploadCapAboveDefaultKeepsDefault(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	metaRepo.Meta = &model.Meta{
		ID:                               "x",
		ChunkedUploadMaxSessionsPerUser:  99,
		ChunkedUploadMaxPendingMbPerUser: 99999,
	}
	p := svc.GetUserPolicies("user1")
	assert.Equal(t, 4, p[role.PolicyChunkedUploadMaxConcurrentSessions], "default 4 は cap 99 未満なので維持")
	assert.Equal(t, 1024, p[role.PolicyChunkedUploadMaxPendingMb])
}

// meta が引けない / 値が 0 の構成では cap しない。ここで fail-closed に倒すと
// transient DB error で全ユーザーの分割アップロードが止まる。
func TestGetUserPolicies_ChunkedUploadCapDisabled(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)

	// meta 取得失敗 (Meta == nil → ErrNotFound)。
	p := svc.GetUserPolicies("user1")
	assert.Equal(t, 4, p[role.PolicyChunkedUploadMaxConcurrentSessions])
	assert.Equal(t, 1024, p[role.PolicyChunkedUploadMaxPendingMb])

	// 0 は「cap 無効」の意味 (capServerMaxFileSize と同じ規約)。
	metaRepo.Meta = &model.Meta{ID: "x"}
	p = svc.GetUserPolicies("user1")
	assert.Equal(t, true, p[role.PolicyCanUseChunkedUpload])
	assert.Equal(t, 4, p[role.PolicyChunkedUploadMaxConcurrentSessions])
	assert.Equal(t, 1024, p[role.PolicyChunkedUploadMaxPendingMb])
}

// bool policy は OR 集約される。片方の role が許可していれば使える。
func TestGetUserPolicies_CanUseChunkedUploadAggregation(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["deny"] = &model.Role{
		ID: "deny", Name: "Deny",
		Policies: datatypes.JSON([]byte(`{"canUseChunkedUpload": {"useDefault": false, "priority": 0, "value": false}}`)),
	}
	assignRepo.Assignments["user1:deny"] = &model.RoleAssignment{ID: "a1", UserID: "user1", RoleID: "deny"}
	assert.Equal(t, false, svc.GetUserPolicies("user1")[role.PolicyCanUseChunkedUpload])

	roleRepo.Roles["allow"] = &model.Role{
		ID: "allow", Name: "Allow",
		Policies: datatypes.JSON([]byte(`{"canUseChunkedUpload": {"useDefault": false, "priority": 0, "value": true}}`)),
	}
	assignRepo.Assignments["user1:allow"] = &model.RoleAssignment{ID: "a2", UserID: "user1", RoleID: "allow"}
	svc.InvalidateAllRoleCaches()
	assert.Equal(t, true, svc.GetUserPolicies("user1")[role.PolicyCanUseChunkedUpload])
}

// **ロールに入れた値が実際に届くこと (#2958)。**
//
// `resolvePolicies` は `effectivepolicy.Defaults()` のキーだけで出力 map を
// 作るので、**defaults からキーが落ちると、管理画面には設定が入って見えるのに
// 上限が一切効かない**。しかも frontend のキー一覧を見る gate は期待値を
// defaults から導出するため、削除すると gate 自身が黙って弱くなる。
// service 側のテストは stub の map を返すのでこの経路を通らない。
func TestGetUserPolicies_EmojiApplicationQuotaAggregation(t *testing.T) {
	for _, key := range []string{
		role.PolicyEmojiApplicationMaxPerDay,
		role.PolicyEmojiApplicationMaxPerWeek,
		role.PolicyEmojiApplicationMaxPerMonth,
		role.PolicyEmojiApplicationMaxPending,
	} {
		t.Run(key, func(t *testing.T) {
			svc, roleRepo, assignRepo, _ := newTestService(t)
			// 既定は 0 = その期間の上限なし。
			assert.Equal(t, 0, svc.GetUserPolicies("user1")[key])

			roleRepo.Roles["tight"] = &model.Role{
				ID: "tight", Name: "Tight",
				Policies: datatypes.JSON([]byte(
					`{"` + key + `": {"useDefault": false, "priority": 0, "value": 3}}`)),
			}
			assignRepo.Assignments["user1:tight"] = &model.RoleAssignment{ID: "a1", UserID: "user1", RoleID: "tight"}
			svc.InvalidateAllRoleCaches()
			assert.Equal(t, 3, svc.GetUserPolicies("user1")[key])

			// **集約は max。** 緩い方に倒れるので、上限としては弱い方が勝つ。
			// `docs/divergence.md` が「0 は無制限を表現できない」と書いている
			// のはこの向きによる。
			roleRepo.Roles["loose"] = &model.Role{
				ID: "loose", Name: "Loose",
				Policies: datatypes.JSON([]byte(
					`{"` + key + `": {"useDefault": false, "priority": 0, "value": 10}}`)),
			}
			assignRepo.Assignments["user1:loose"] = &model.RoleAssignment{ID: "a2", UserID: "user1", RoleID: "loose"}
			svc.InvalidateAllRoleCaches()
			assert.Equal(t, 10, svc.GetUserPolicies("user1")[key])
		})
	}
}

// **`rootUserId` を設定したら、それが唯一の答え (#3037)。**
//
// 以前は「どちらかが一致すれば root」だったので、`user.isRoot` が立った
// 利用者を降ろす手段がどこにも無かった (mk-go には `isRoot` を書く経路しか
// 無く、admin API にも false へ戻す口が無い)。DB を直接触るしかないうえ、
// 管理画面にも出ないので「元の運営者が永久に管理者のまま」に気付けない。
func TestIsAdministrator_RootUserIDOverridesStaleIsRoot(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	newRoot := "carol"
	metaRepo.Meta = &model.Meta{ID: "x", RootUserID: &newRoot}
	userRepo := testutil.NewMockUserRepository()
	// 引き継ぎ前の運営者。フラグは立ったまま。
	require.NoError(t, userRepo.Create(&model.User{ID: "alice", Username: "alice", IsRoot: true}))
	require.NoError(t, userRepo.Create(&model.User{ID: "carol", Username: "carol"}))
	svc.SetUserRepo(userRepo)

	assert.False(t, svc.IsAdministrator("alice"), "降ろしたはずの root が管理者のまま")
	assert.True(t, svc.IsAdministrator("carol"), "設定した root が管理者になっていない")
}

// **空文字は「未設定」として扱う。** 列は nullable だが、空文字が入った DB を
// 「root は誰もいない」ではなく「誰とも一致しない」と読むと、drop-in の
// fallback が効かなくなる。
func TestIsAdministrator_EmptyRootUserIDFallsBackToIsRoot(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	empty := ""
	metaRepo.Meta = &model.Meta{ID: "x", RootUserID: &empty}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "alice", Username: "alice", IsRoot: true}))
	svc.SetUserRepo(userRepo)

	assert.True(t, svc.IsAdministrator("alice"))
}

// **meta を読めないときは従来どおり `isRoot` を見る。** 「設定されているか」が
// 分からない状態で無視すると、DB の瞬断のあいだ本物の root が管理画面から
// 締め出される。
func TestIsAdministrator_MetaUnavailableFallsBackToIsRoot(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	metaRepo.FetchErr = errors.New("meta unavailable")
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "alice", Username: "alice", IsRoot: true}))
	svc.SetUserRepo(userRepo)

	assert.True(t, svc.IsAdministrator("alice"))
}

// **自己付与できる管理者ロールを作らせない (#3037)。**
func TestCreate_RejectsSelfGrantableConditionalPrivilege(t *testing.T) {
	svc, _, _, _ := newTestService(t)

	for _, tt := range []struct {
		name    string
		opts    role.CreateOptions
		wantErr bool
	}{
		{
			name: "条件つき + 管理者 + isCat",
			opts: role.CreateOptions{
				Target: model.RoleTargetConditional, IsAdministrator: true,
				CondFormula: datatypes.JSON(`{"type":"isCat"}`),
			},
			wantErr: true,
		},
		{
			name: "条件つき + モデレーター + 入れ子の isBot",
			opts: role.CreateOptions{
				Target: model.RoleTargetConditional, IsModerator: true,
				CondFormula: datatypes.JSON(`{"type":"and","values":[{"type":"isLocal"},{"type":"isBot"}]}`),
			},
			wantErr: true,
		},
		{
			// **読めない式は判定できない。** ここで通すと壊れた JSON を
			// 送るだけで検査を迂回できる。
			name: "条件つき + 管理者 + 壊れた式",
			opts: role.CreateOptions{
				Target: model.RoleTargetConditional, IsAdministrator: true,
				CondFormula: datatypes.JSON(`{`),
			},
			wantErr: true,
		},
		{
			// **アカウントの年齢は barrier にならない (#3045)。** 攻撃者は
			// いくらでも待てるし、ロールを作った時点で条件を満たす既存
			// アカウントが全員その場で管理者になる。
			name: "条件つき + 管理者 + createdMoreThan 1年",
			opts: role.CreateOptions{
				Target: model.RoleTargetConditional, IsAdministrator: true,
				CondFormula: datatypes.JSON(`{"type":"and","values":[{"type":"isLocal"},{"type":"createdMoreThan","sec":31536000}]}`),
			},
			wantErr: true,
		},
		{
			// **`not(createdLessThan)` は「`sec` 以上前に作られた」(#3045)。**
			// #3044 は否定側に判定を当てておらず、「1 秒より前に
			// 作られた全アカウントが管理者」がそのまま通っていた。
			name: "条件つき + 管理者 + not(createdLessThan 1秒)",
			opts: role.CreateOptions{
				Target: model.RoleTargetConditional, IsAdministrator: true,
				CondFormula: datatypes.JSON(`{"type":"not","value":{"type":"createdLessThan","sec":1}}`),
			},
			wantErr: true,
		},
		{
			// 手動ロールを参照する形は残る。誰が配れるかは
			// `RoleGrantsPrivilegeIndirectly` が別に見る。
			name: "条件つき + 管理者 + roleAssignedTo",
			opts: role.CreateOptions{
				Target: model.RoleTargetConditional, IsAdministrator: true,
				CondFormula: datatypes.JSON(`{"type":"and","values":[{"type":"isLocal"},{"type":"roleAssignedTo","roleId":"staff"}]}`),
			},
		},
		{
			// 手動ロールは条件で配らないので対象外。
			name: "手動 + 管理者 + isCat",
			opts: role.CreateOptions{
				Target: model.RoleTargetManual, IsAdministrator: true,
				CondFormula: datatypes.JSON(`{"type":"isCat"}`),
			},
		},
		{
			// 権限を持たない条件つきロールは従来どおり。
			name: "条件つき + 権限なし + isCat",
			opts: role.CreateOptions{
				Target:      model.RoleTargetConditional,
				CondFormula: datatypes.JSON(`{"type":"isCat"}`),
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.Create(tt.name, "", tt.opts)
			if tt.wantErr {
				assert.ErrorIs(t, err, role.ErrSelfGrantablePrivilege)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// **更新後の姿で判定する。** 「条件つきに変える」「管理者を立てる」
// 「条件を差し替える」のどれ 1 つを送っても組み合わせは作れる。
func TestUpdateFields_RejectsSelfGrantableConditionalPrivilege(t *testing.T) {
	for _, tt := range []struct {
		name    string
		stored  *model.Role
		fields  map[string]any
		wantErr bool
	}{
		{
			name:    "管理者だけを立てる",
			stored:  &model.Role{ID: "r", Target: model.RoleTargetConditional, CondFormula: datatypes.JSON(`{"type":"isCat"}`)},
			fields:  map[string]any{"isAdministrator": true},
			wantErr: true,
		},
		{
			name:    "条件つきに変える",
			stored:  &model.Role{ID: "r", Target: model.RoleTargetManual, IsAdministrator: true, CondFormula: datatypes.JSON(`{"type":"isCat"}`)},
			fields:  map[string]any{"target": string(model.RoleTargetConditional)},
			wantErr: true,
		},
		{
			name:    "条件を差し替える",
			stored:  &model.Role{ID: "r", Target: model.RoleTargetConditional, IsModerator: true, CondFormula: datatypes.JSON(`{"type":"isLocal"}`)},
			fields:  map[string]any{"condFormula": datatypes.JSON(`{"type":"isBot"}`)},
			wantErr: true,
		},
		{
			name:   "権限を下ろせば通る",
			stored: &model.Role{ID: "r", Target: model.RoleTargetConditional, IsAdministrator: true, CondFormula: datatypes.JSON(`{"type":"isCat"}`)},
			fields: map[string]any{"isAdministrator": false},
		},
		{
			name:   "無関係な更新",
			stored: &model.Role{ID: "r", Target: model.RoleTargetConditional, CondFormula: datatypes.JSON(`{"type":"isCat"}`)},
			fields: map[string]any{"name": "new"},
		},
		{
			// **既存のロールは動き続けるが、編集はできなくなる (#3045)。**
			// 評価側に guard は無いので黙って降格させない一方、更新は
			// **更新後の姿**で判定するため名前だけの変更も弾かれる。
			// upstream にこの検査は無いので、TS から引き継いだ DB には
			// 「古参はモデレーター」のようなロールが現実に存在しうる。
			// 逃げ道は権限を下ろす / 条件を差し替える / 手動に変える。
			name: "特権つき条件ロールは無関係な更新も弾く",
			stored: &model.Role{ID: "r", Target: model.RoleTargetConditional, IsModerator: true,
				CondFormula: datatypes.JSON(`{"type":"and","values":[{"type":"isLocal"},{"type":"createdMoreThan","sec":31536000}]}`)},
			fields:  map[string]any{"name": "new"},
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, roleRepo, _, _ := newTestService(t)
			roleRepo.Roles["r"] = tt.stored

			_, err := svc.UpdateFields("r", tt.fields)
			if tt.wantErr {
				assert.ErrorIs(t, err, role.ErrSelfGrantablePrivilege)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// **`isAdministrator` / `isModerator` だけでは足りない (#3037 レビュー)。**
//
// `canManageCustomEmojis` / `canManageAvatarDecorations` は `RequireRolePolicy`
// だけで `/admin/*` を開ける (emoji は 20 route、avatar-decorations は 4 route で、
// どちらも `RequireModerator` を併用していない)。管理者フラグを塞いだことで
// 「システムが守ってくれる」と読まれるぶん、こちらが無警告で通るのはより悪い。
func TestCreate_RejectsSelfGrantablePrivilegedPolicy(t *testing.T) {
	for _, tt := range []struct {
		name     string
		policies string
		wantErr  bool
	}{
		{
			name:     "canManageCustomEmojis を配る",
			policies: `{"canManageCustomEmojis":{"useDefault":false,"priority":2,"value":true}}`,
			wantErr:  true,
		},
		{
			name:     "canManageAvatarDecorations を配る",
			policies: `{"canManageAvatarDecorations":{"useDefault":false,"priority":0,"value":true}}`,
			wantErr:  true,
		},
		{
			// **読めない policies は判定できないので拒否側へ。**
			name:     "壊れた policies",
			policies: `{`,
			wantErr:  true,
		},
		// 既定へ戻す / 明示的に与えない設定まで弾くと正当なロールが作れない。
		{
			name:     "useDefault の entry",
			policies: `{"canManageCustomEmojis":{"useDefault":true,"priority":0,"value":true}}`,
		},
		{
			name:     "false を明示",
			policies: `{"canManageCustomEmojis":{"useDefault":false,"priority":0,"value":false}}`,
		},
		{
			name:     "特権でない policy",
			policies: `{"canCreateChannel":{"useDefault":false,"priority":0,"value":true}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _, _ := newTestService(t)
			_, err := svc.Create(tt.name, "", role.CreateOptions{
				Target:      model.RoleTargetConditional,
				CondFormula: datatypes.JSON(`{"type":"isCat"}`),
				Policies:    datatypes.JSON(tt.policies),
			})
			if tt.wantErr {
				assert.ErrorIs(t, err, role.ErrSelfGrantablePrivilege)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// **handler が実際に入れる型で判定できること (#3037 レビュー)。**
//
// `admin/roles/update` は `fields["target"]` に `model.RoleTarget` を入れる。
// `case string:` しか踏まないテストだと、`case model.RoleTarget:` を消しても
// 緑のまま通り、production では「既存の管理者ロールを条件つきに変える」経路で
// guard が黙って無効になる。
func TestUpdateFields_MergedShapeHandlesHandlerTypes(t *testing.T) {
	for _, tt := range []struct {
		name   string
		fields map[string]any
	}{
		{"target が model.RoleTarget", map[string]any{"target": model.RoleTargetConditional}},
		{"target が string", map[string]any{"target": string(model.RoleTargetConditional)}},
		{"policies が datatypes.JSON", map[string]any{
			"target":   model.RoleTargetConditional,
			"policies": datatypes.JSON(`{"canManageCustomEmojis":{"useDefault":false,"priority":0,"value":true}}`),
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, roleRepo, _, _ := newTestService(t)
			roleRepo.Roles["r"] = &model.Role{
				ID: "r", Target: model.RoleTargetManual, IsAdministrator: true,
				CondFormula: datatypes.JSON(`{"type":"isCat"}`),
			}
			if _, ok := tt.fields["policies"]; ok {
				roleRepo.Roles["r"].IsAdministrator = false
			}

			_, err := svc.UpdateFields("r", tt.fields)
			assert.ErrorIs(t, err, role.ErrSelfGrantablePrivilege)
		})
	}
}

// `canSearchIpHistory` (#3104) は**既定で管理者のみ**。
//
// **この既定が崩れると、upstream の `admin/get-user-ips` (`requireAdmin: true`)
// と同じ機密情報に、モデレーター全員が通れる新しい経路ができる。** 既定値は
// `internal/effectivepolicy` にあり、そこを `true` に書き換えても build も
// `perm-check` も通る (`perm-check` は upstream にある endpoint しか見ない)。
func TestHasRolePolicy_CanSearchIpHistory(t *testing.T) {
	t.Run("既定では一般利用者に開かない", func(t *testing.T) {
		svc, _, _, _ := newTestService(t)
		assert.False(t, svc.HasRolePolicy("alice", role.PolicyCanSearchIPHistory))
	})

	// **モデレーターは短絡しない。** `HasRolePolicy` が見るのは管理者だけなので、
	// route 側の `RequireModerator` を通っても policy が false なら 403 になる。
	t.Run("モデレーターでも既定では開かない", func(t *testing.T) {
		svc, roleRepo, assignRepo, _ := newTestService(t)
		roleRepo.Roles["r_mod"] = &model.Role{ID: "r_mod", Name: "Mod", IsModerator: true}
		assignRepo.Assignments["mod1:r_mod"] = &model.RoleAssignment{
			ID: "a1", UserID: "mod1", RoleID: "r_mod",
		}
		assert.False(t, svc.HasRolePolicy("mod1", role.PolicyCanSearchIPHistory),
			"既定でモデレーターに開いている (upstream は requireAdmin)")
	})

	t.Run("管理者は既定でも通る", func(t *testing.T) {
		svc, roleRepo, assignRepo, _ := newTestService(t)
		roleRepo.Roles["r_admin"] = &model.Role{ID: "r_admin", Name: "Admin", IsAdministrator: true}
		assignRepo.Assignments["admin1:r_admin"] = &model.RoleAssignment{
			ID: "a1", UserID: "admin1", RoleID: "r_admin",
		}
		assert.True(t, svc.HasRolePolicy("admin1", role.PolicyCanSearchIPHistory))
	})

	// 運営者がロールで開ける = 「モデレーターに許可するかを設定で決められる」。
	t.Run("ロールで開ける", func(t *testing.T) {
		svc, roleRepo, assignRepo, _ := newTestService(t)
		roleRepo.Roles["r_ip"] = &model.Role{
			ID: "r_ip", Name: "IPSearch",
			Policies: datatypes.JSON([]byte(
				`{"canSearchIpHistory":{"useDefault":false,"priority":0,"value":true}}`)),
		}
		assignRepo.Assignments["mod1:r_ip"] = &model.RoleAssignment{
			ID: "a1", UserID: "mod1", RoleID: "r_ip",
		}
		assert.True(t, svc.HasRolePolicy("mod1", role.PolicyCanSearchIPHistory))
	})
}

// 既定値そのものも固定する。**`DefaultPolicies` に key が無いと `HasRolePolicy`
// は fail-closed で false を返す**ので、上の「既定では開かない」は key を丸ごと
// 消しても通る。ロール編集画面へ出すには key が要るので、存在も併せて見る。
func TestDefaultPolicies_CanSearchIpHistoryIsFalse(t *testing.T) {
	v, ok := role.DefaultPolicies()[role.PolicyCanSearchIPHistory]
	require.True(t, ok, "既定値が無いとロール編集画面に項目が出ない (#3104)")
	assert.Equal(t, false, v, "既定は管理者のみ (upstream の requireAdmin に合わせる)")
}
