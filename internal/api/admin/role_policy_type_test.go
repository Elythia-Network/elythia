package admin_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// **数値の policy に文字列を入れさせない (#3037)。**
//
// consumer は `if limit, ok := role.PolicyNumber(v); ok { ...gate... }` の形で
// 読むので、型が違うと **上限違反で弾かれるのではなく上限そのものが消える**
// (#2611 と同じ壊れ方)。管理画面は型どおりの値しか送らないが、endpoint を
// 直に 1 回叩けばその状態を作れた。
func TestRolesCreate_RejectsWrongPolicyValueType(t *testing.T) {
	h, _, _, _, _ := newTestHandlerWithAssign(t)

	body := `{"name":"r","description":"","target":"manual","condFormula":{},` +
		`"isPublic":false,"isModerator":false,"isAdministrator":false,"asBadge":false,` +
		`"isExplorable":false,"canEditMembersByModerator":false,"displayOrder":0,` +
		`"policies":{"maxFileSizeMb":{"useDefault":false,"priority":0,"value":"30"}}}`
	rec := doPost(h.RolesCreate, body, adminUser)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "型の違う policy 値を受け取っている")
	assert.Contains(t, rec.Body.String(), "maxFileSizeMb")
}

// **型が合っていれば通る。** これが無いと「policies を常に拒否する」実装でも
// 上のテストが通る。
func TestRolesCreate_AcceptsCorrectPolicyValueType(t *testing.T) {
	h, _, _, _, _ := newTestHandlerWithAssign(t)

	body := `{"name":"r","description":"","target":"manual","condFormula":{},` +
		`"isPublic":false,"isModerator":false,"isAdministrator":false,"asBadge":false,` +
		`"isExplorable":false,"canEditMembersByModerator":false,"displayOrder":0,` +
		`"policies":{"maxFileSizeMb":{"useDefault":false,"priority":0,"value":30},` +
		`"canInvite":{"useDefault":false,"priority":0,"value":true},` +
		`"chatAvailability":{"useDefault":false,"priority":0,"value":"readonly"},` +
		`"unknownFuturePolicy":{"useDefault":false,"priority":0,"value":"whatever"},` +
		// **`useDefault` の entry は値を読まない。** 管理画面は既定へ戻した
		// あとも古い値を残したまま送るので、ここを見ると正当な設定が落ちる。
		`"driveCapacityMb":{"useDefault":true,"priority":0,"value":"ignored"}}}`
	rec := doPost(h.RolesCreate, body, adminUser)

	assert.Equal(t, http.StatusOK, rec.Code, "正しい値を拒否している: %s", rec.Body.String())
}

func TestRolesUpdate_RejectsWrongPolicyValueType(t *testing.T) {
	h, _, _, roles, _ := newTestHandlerWithAssign(t)
	roles.Roles["r"] = &model.Role{ID: "r", Target: model.RoleTargetManual}

	body := `{"roleId":"r","policies":{"driveCapacityMb":{"useDefault":false,"priority":0,"value":[1,2]}}}`
	rec := doPost(h.RolesUpdate, body, adminUser)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "driveCapacityMb")
}

// **既定 policy はさらに効きが広い。** ここが壊れると全利用者でその上限が消える。
func TestRolesUpdateDefaultPolicies_RejectsWrongValueType(t *testing.T) {
	h, _, metaRepo, _, _ := newTestHandlerWithAssign(t)
	require.NotNil(t, metaRepo)

	rec := doPost(h.RolesUpdateDefaultPolicies, `{"policies":{"maxFileSizeMb":"30"}}`, adminUser)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "maxFileSizeMb")
}

func TestRolesUpdateDefaultPolicies_AcceptsCorrectValueType(t *testing.T) {
	h, _, _, _, _ := newTestHandlerWithAssign(t)

	rec := doPost(h.RolesUpdateDefaultPolicies, `{"policies":{"maxFileSizeMb":30,"canInvite":true}}`, adminUser)

	assert.Equal(t, http.StatusNoContent, rec.Code, "正しい値を拒否している: %s", rec.Body.String())
}

// admin/update-meta は policies を受けない (#3888)。既定のポリシーを変えるのは
// admin/roles/update-default-policies だけにする。本家の paramDef に無い項目で、
// 本家は知らない項目として無視する。以前は 4 本目の書き込みの経路で、型の検査
// (#3037) を重ねていた。型が正しくても誤っていても、null でも、列に届かない。
func TestUpdateMeta_IgnoresPolicies(t *testing.T) {
	for _, body := range []string{
		`{"policies":{"driveCapacityMb":100,"canInvite":true}}`,
		`{"policies":{"driveCapacityMb":"100"}}`,
		`{"policies":"everything"}`,
		`{"policies":null}`,
		`{"policies":{"driveCapacityMb":100},"name":"x"}`,
	} {
		t.Run(body, func(t *testing.T) {
			h, _, metaRepo, _, _ := newTestHandlerWithAssign(t)
			metaRepo.Meta.Policies = []byte(`{"canInvite":true}`)
			rec := doPost(h.UpdateMeta, body, adminUser)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			assert.NotContains(t, metaRepo.LastUpdateFields, "policies")
			assert.JSONEq(t, `{"canInvite":true}`, string(metaRepo.Meta.Policies))
		})
	}
}
