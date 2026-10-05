package admin_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/misc"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/password"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// attachModLog wires a fresh moderation log service backed by the
// shared race-safe testutil mock and returns the mock so the test can
// assert on Snapshot() without touching internals.
func attachModLog(t *testing.T, h modLogServiceSetter) *testutil.MockModerationLogRepository {
	t.Helper()
	repo := testutil.NewMockModerationLogRepository()
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetModLogService(moderationlog.New(repo, gen))
	return repo
}

// modLogServiceSetter narrows *apiadmin.Handler to just the setter we
// need so the helper stays compatible with any future handler that
// adopts the same wiring pattern.
type modLogServiceSetter interface {
	SetModLogService(*moderationlog.Service)
}

func TestResetPasswordAdmin_Empty(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, doPost(h.ResetPassword, `{}`, adminUser).Code)
}

// upstream reset-password.ts は常に secureRndstr(8) で 8 文字の英数字パスワードを
// その場で再設定して {password} を返す (res schema は min/maxLength=8)。mk-go も
// shape を揃える (#1825)。
func TestResetPasswordAdmin_Success(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "testuser"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}
	rec := doPost(h.ResetPassword, `{"userId":"u1"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Password string `json:"password"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Password, 8, "upstream secureRndstr(8) は 8 文字")
	for _, c := range resp.Password {
		assert.Contains(t, misc.AlphanumericChars, string(c),
			"password chars must be alphanumeric (LU_CHARS)")
	}
	profile := userRepo.Profiles["u1"]
	require.NotNil(t, profile.Password)
	assert.True(t, strings.HasPrefix(*profile.Password, "$2"))
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(*profile.Password), []byte(resp.Password)))
	cost, err := bcrypt.Cost([]byte(*profile.Password))
	require.NoError(t, err)
	assert.Equal(t, password.Cost(), cost)
}

// password 永続化に失敗したら 500 を返し、ハッシュ未保存のまま password を
// 返してしまわないことを保証する (#1825 で error を握り潰さなくした)。
func TestResetPasswordAdmin_UpdateProfileError(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "testuser"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}
	userRepo.UpdateProfileErr = errors.New("db down")

	rec := doPost(h.ResetPassword, `{"userId":"u1"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "password")
}

// upstream 2026.7.0 (fork merge): administrator のパスワードは他人からリセット
// できない。root は IsAdministrator に含まれるため引き続き保護される。旧
// CANNOT_RESET_PASSWORD_OF_ROOT_USER (400) は廃止され ACCESS_DENIED (403)。
func TestResetPasswordAdmin_RootRejected(t *testing.T) {
	h, userRepo, metaRepo, _ := newTestHandler(t)
	userRepo.Users["root1"] = &model.User{ID: "root1", Username: "admin", IsRoot: true}
	rootID := "root1"
	metaRepo.Meta.RootUserID = &rootID
	rec := doPost(h.ResetPassword, `{"userId":"root1"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
	assert.Contains(t, rec.Body.String(), "cda8f8ce-89a6-4f92-8055-33bbe0c1464d")
}

// 非 root の administrator も他人からはリセット不可 (upstream 2026.7.0)。
func TestResetPasswordAdmin_AdministratorTargetRejected(t *testing.T) {
	h, userRepo, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	userRepo.Users["adm1"] = &model.User{ID: "adm1", Username: "adm"}
	roleRepo.Roles["admrole"] = &model.Role{ID: "admrole", Name: "Admin", IsAdministrator: true}
	assignRepo.Assignments["adm1:admrole"] = &model.RoleAssignment{ID: "a1", UserID: "adm1", RoleID: "admrole"}
	rec := doPost(h.ResetPassword, `{"userId":"adm1"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "cda8f8ce-89a6-4f92-8055-33bbe0c1464d")
}

// administrator 本人によるセルフリセットは許可 (upstream: me.id !== user.id の
// 条件により self は素通り)。
func TestResetPasswordAdmin_AdminSelfResetAllowed(t *testing.T) {
	h, userRepo, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	userRepo.Users["adm1"] = &model.User{ID: "adm1", Username: "adm"}
	userRepo.Profiles["adm1"] = &model.UserProfile{UserID: "adm1"}
	roleRepo.Roles["admrole"] = &model.Role{ID: "admrole", Name: "Admin", IsAdministrator: true}
	assignRepo.Assignments["adm1:admrole"] = &model.RoleAssignment{ID: "a1", UserID: "adm1", RoleID: "admrole"}
	rec := doPost(h.ResetPassword, `{"userId":"adm1"}`, userRepo.Users["adm1"])
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "password")
}

func TestResetPasswordAdmin_WritesModerationLog(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}

	repo := attachModLog(t, h)

	rec := doPost(h.ResetPassword, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "password")

	require.Eventually(t, func() bool {
		return len(repo.Snapshot()) == 1
	}, 500*time.Millisecond, 5*time.Millisecond, "moderation log should be written")

	logs := repo.Snapshot()
	assert.Equal(t, "admin1", logs[0].UserID)
	assert.Equal(t, "resetPassword", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.Equal(t, "u1", info["userId"])
	assert.Equal(t, "alice", info["userUsername"])
}

func TestResetPasswordAdmin_NoLogWhenServiceUnwired(t *testing.T) {
	// service が未配線 (production で誤って setter を忘れた等) でも
	// API 自体は機能し続けることを保証する。fire-and-forget の趣旨は
	// 「audit 失敗で本処理を止めない」なので、未配線も同じ扱い。
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}

	rec := doPost(h.ResetPassword, `{"userId":"u1"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestResetPasswordAdmin_TargetMissing(t *testing.T) {
	// 存在しない userId への reset-password は upstream の 'user not found' に倣い
	// NO_SUCH_USER を返し、password を再設定せず moderation log も残さない (#1862)。
	h, _, _, _ := newTestHandler(t)
	// u1 を userRepo に登録しない → FindByID は error 返す
	repo := attachModLog(t, h)

	rec := doPost(h.ResetPassword, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
	assert.NotContains(t, rec.Body.String(), "password")

	require.Never(t, func() bool {
		return len(repo.Snapshot()) > 0
	}, 100*time.Millisecond, 10*time.Millisecond, "target missing → log must not be written")
}

func TestResetPasswordAdmin_NoLogWhenActorMissing(t *testing.T) {
	// RequireModerator middleware が actor を context に載せ忘れる
	// (= 配線ミス) ケース。logUserAction の actor==nil branch を経由して
	// log は書かれず、本処理 (パスワード reset) は通常通り完了する。
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}

	repo := attachModLog(t, h)

	rec := doPost(h.ResetPassword, `{"userId":"u1"}`, nil) // actor 未配線
	assert.Equal(t, http.StatusOK, rec.Code)

	require.Never(t, func() bool {
		return len(repo.Snapshot()) > 0
	}, 100*time.Millisecond, 10*time.Millisecond, "actor missing → log must not be written")
}
