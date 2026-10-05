package admin_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSecurityKeyRepo is a minimal in-memory UserSecurityKeyRepository for
// exercising the admin/unset-mfa passkey bulk deletion path.
type fakeSecurityKeyRepo struct {
	keys map[string]*model.UserSecurityKey
	err  error
}

func newFakeSecurityKeyRepo() *fakeSecurityKeyRepo {
	return &fakeSecurityKeyRepo{keys: map[string]*model.UserSecurityKey{}}
}

func (f *fakeSecurityKeyRepo) Create(key *model.UserSecurityKey) error {
	f.keys[key.ID] = key
	return nil
}
func (f *fakeSecurityKeyRepo) FindByID(id string) (*model.UserSecurityKey, error) {
	return f.keys[id], nil
}
func (f *fakeSecurityKeyRepo) ListByUser(userID string) ([]*model.UserSecurityKey, error) {
	var out []*model.UserSecurityKey
	for _, k := range f.keys {
		if k.UserID == userID {
			out = append(out, k)
		}
	}
	return out, nil
}
func (f *fakeSecurityKeyRepo) UpdateName(_, _, _ string) error       { return nil }
func (f *fakeSecurityKeyRepo) UpdateCounter(_ string, _ int64) error { return nil }
func (f *fakeSecurityKeyRepo) Delete(id, _ string) error             { delete(f.keys, id); return nil }
func (f *fakeSecurityKeyRepo) DeleteByUser(userID string) error {
	if f.err != nil {
		return f.err
	}
	for id, k := range f.keys {
		if k.UserID == userID {
			delete(f.keys, id)
		}
	}
	return nil
}
func (f *fakeSecurityKeyRepo) CountByUser(userID string) (int64, error) {
	n := int64(0)
	for _, k := range f.keys {
		if k.UserID == userID {
			n++
		}
	}
	return n, nil
}

func TestUnsetMfa_Success(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	secret := "totp-secret"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	userRepo.Profiles["u1"] = &model.UserProfile{
		UserID:                "u1",
		TwoFactorEnabled:      true,
		TwoFactorSecret:       &secret,
		TwoFactorBackupSecret: model.StringArray{"b1", "b2"},
		UsePasswordLessLogin:  true,
		SecurityKeysAvailable: true,
	}
	skRepo := newFakeSecurityKeyRepo()
	skRepo.keys["k1"] = &model.UserSecurityKey{ID: "k1", UserID: "u1"}
	skRepo.keys["k2"] = &model.UserSecurityKey{ID: "k2", UserID: "other"}
	h.SetSecurityKeyRepo(skRepo)
	modlog := attachModLog(t, h)

	rec := doPost(h.UnsetMfa, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	p := userRepo.Profiles["u1"]
	assert.False(t, p.TwoFactorEnabled)
	assert.Nil(t, p.TwoFactorSecret)
	assert.Empty(t, p.TwoFactorBackupSecret)
	assert.False(t, p.UsePasswordLessLogin)
	assert.False(t, p.SecurityKeysAvailable, "鍵を全削除したので securityKeysAvailable も落とす")
	assert.NotContains(t, skRepo.keys, "k1", "target user's passkeys are removed")
	assert.Contains(t, skRepo.keys, "k2", "other users' passkeys are untouched")

	require.Eventually(t, func() bool { return len(modlog.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := modlog.Snapshot()
	assert.Equal(t, "unsetMfa", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.Equal(t, "u1", info["userId"])
	assert.Equal(t, "alice", info["userUsername"])
}

func TestUnsetMfa_MissingParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.UnsetMfa, `{}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUnsetMfa_NoSuchUser(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.UnsetMfa, `{"userId":"ghost"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ccafc7fe-5074-4edd-9dc0-8ef9ef6a701d")
}

// administrator を他人が unset-mfa できない (upstream fork merge の認可)。
func TestUnsetMfa_AdministratorTargetRejected(t *testing.T) {
	h, userRepo, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	userRepo.Users["adm1"] = &model.User{ID: "adm1", Username: "adm"}
	roleRepo.Roles["admrole"] = &model.Role{ID: "admrole", Name: "Admin", IsAdministrator: true}
	assignRepo.Assignments["adm1:admrole"] = &model.RoleAssignment{ID: "a1", UserID: "adm1", RoleID: "admrole"}

	rec := doPost(h.UnsetMfa, `{"userId":"adm1"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "cda8f8ce-89a6-4f92-8055-33bbe0c1464d")
}

// administrator 本人によるセルフ unset-mfa は許可。
func TestUnsetMfa_AdminSelfAllowed(t *testing.T) {
	h, userRepo, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	userRepo.Users["adm1"] = &model.User{ID: "adm1", Username: "adm"}
	userRepo.Profiles["adm1"] = &model.UserProfile{UserID: "adm1", TwoFactorEnabled: true}
	roleRepo.Roles["admrole"] = &model.Role{ID: "admrole", Name: "Admin", IsAdministrator: true}
	assignRepo.Assignments["adm1:admrole"] = &model.RoleAssignment{ID: "a1", UserID: "adm1", RoleID: "admrole"}
	h.SetSecurityKeyRepo(newFakeSecurityKeyRepo())

	rec := doPost(h.UnsetMfa, `{"userId":"adm1"}`, userRepo.Users["adm1"])
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.False(t, userRepo.Profiles["adm1"].TwoFactorEnabled)
}

// securityKeyRepo 未配線は成功扱いにしない。パスキーを消さずに 204 を返すと
// 「2FA 無効 + key 行残存」が残り、後の TOTP 再有効化で攻撃者のパスキーが
// 復活する。
func TestUnsetMfa_NoSecurityKeyRepo(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", TwoFactorEnabled: true}

	rec := doPost(h.UnsetMfa, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.True(t, userRepo.Profiles["u1"].TwoFactorEnabled, "2FA は無効化されないまま")
}

// パスキー削除に失敗したら profile を触らず 500 (部分失敗で危険な状態を作らない)。
func TestUnsetMfa_SecurityKeyDeleteFails(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", TwoFactorEnabled: true}
	skRepo := newFakeSecurityKeyRepo()
	skRepo.err = assertError{}
	h.SetSecurityKeyRepo(skRepo)

	rec := doPost(h.UnsetMfa, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.True(t, userRepo.Profiles["u1"].TwoFactorEnabled, "key 削除に失敗したら 2FA は維持")
}
