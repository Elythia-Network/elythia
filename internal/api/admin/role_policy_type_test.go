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

// **`admin/update-meta` も同じ `meta.policies` 列を書く (#3037 レビュー 2 周目)。**
//
// 1 周目で `roles/create` / `roles/update` / `roles/update-default-policies` の
// 3 経路に型検査を入れたが、**4 本目のこれを見ていなかった**。`policies` は
// 保護列ではなく `dropUnknownMetaFields` も通すので、endpoint を直に 1 回叩く
// だけで数値の policy に文字列を入れられ、**全利用者でその上限が消える**。
func TestUpdateMeta_RejectsWrongPolicyValueType(t *testing.T) {
	h, _, _, _, _ := newTestHandlerWithAssign(t)

	rec := doPost(h.UpdateMeta, `{"policies":{"driveCapacityMb":"100"}}`, adminUser)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "型の違う既定 policy を受け取っている: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "driveCapacityMb")
}

// **`policies` がそもそも object でない形も弾く。**
func TestUpdateMeta_RejectsNonObjectPolicies(t *testing.T) {
	h, _, _, _, _ := newTestHandlerWithAssign(t)

	rec := doPost(h.UpdateMeta, `{"policies":"everything"}`, adminUser)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "object でない policies を受け取っている: %s", rec.Body.String())
}

// **型が合っていれば通る。** これが無いと「policies を常に拒否する」実装でも
// 上の 2 つが通る。
func TestUpdateMeta_AcceptsCorrectPolicyValueType(t *testing.T) {
	h, _, metaRepo, _, _ := newTestHandlerWithAssign(t)
	require.NotNil(t, metaRepo)

	rec := doPost(h.UpdateMeta, `{"policies":{"driveCapacityMb":100,"canInvite":true,"chatAvailability":"readonly"}}`, adminUser)

	assert.Less(t, rec.Code, 300, "正しい値を拒否している: %s", rec.Body.String())
}

// **`policies: null` は従来どおり列のリセットとして受ける
// (#3037 レビュー 3 周目)。** `coerceMetaJSONBFields` が nil を `{}` に倒す。
// 他の jsonb 列も null を受けるので、ここだけ 400 にすると非対称になる。
func TestUpdateMeta_AcceptsNullPolicies(t *testing.T) {
	h, _, _, _, _ := newTestHandlerWithAssign(t)

	rec := doPost(h.UpdateMeta, `{"policies":null}`, adminUser)

	assert.Less(t, rec.Code, 300, "null の policies を拒否している: %s", rec.Body.String())
}
