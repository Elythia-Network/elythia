package entity_test

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/datatypes"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPackRole(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	roleID := idGen.Generate(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))

	color := "#fff"
	r := &model.Role{
		ID:                              roleID,
		Name:                            "Trusted",
		Description:                     "trusted users",
		Color:                           &color,
		Target:                          model.RoleTargetManual,
		CondFormula:                     datatypes.JSON([]byte(`{"type":"isLocal"}`)),
		IsPublic:                        true,
		IsExplorable:                    true,
		PreserveAssignmentOnMoveAccount: true,
		CanEditMembersByModerator:       true,
		DisplayOrder:                    3,
		// role 固有 override: canSearchNotes のみ value=true で上書き。
		Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":1,"value":true}}`)),
	}
	defaultPolicies := map[string]any{
		"canSearchNotes": false,
		"canPublicNote":  true,
		"mentionLimit":   20,
	}

	out := entity.PackRole(r, 7, idGen, defaultPolicies)

	assert.Equal(t, roleID, out["id"])
	assert.Equal(t, 7, out["usersCount"])
	assert.Equal(t, "Trusted", out["name"])
	assert.Equal(t, model.RoleTargetManual, out["target"])
	assert.Equal(t, true, out["preserveAssignmentOnMoveAccount"])
	assert.Equal(t, true, out["canEditMembersByModerator"])
	// createdAt は aidx 由来の ISO 文字列。
	createdAt, _ := out["createdAt"].(string)
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}T.*Z$`, createdAt)
	// condFormula は object として返る。
	cf, ok := out["condFormula"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "isLocal", cf["type"])

	// policies: override は維持、未設定 key は default-fill。
	policies, ok := out["policies"].(map[string]any)
	require.True(t, ok)
	// 上書き済み canSearchNotes はそのまま (useDefault:false)。
	override, ok := policies["canSearchNotes"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, override["useDefault"])
	assert.Equal(t, true, override["value"])
	// 未設定 canPublicNote / mentionLimit は default-fill (useDefault:true)。
	filled, ok := policies["canPublicNote"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, filled["useDefault"])
	assert.Equal(t, 0, filled["priority"])
	assert.Equal(t, true, filled["value"])
	ment, ok := policies["mentionLimit"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, 20, ment["value"])
}

// explicit null override: policies に明示的 null が入っている key も
// (upstream `policies[k] == null` と同じく) default-fill 対象になる。
func TestPackRole_ExplicitNullOverrideDefaultFilled(t *testing.T) {
	r := &model.Role{
		ID:        "r",
		UpdatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Policies:  datatypes.JSON([]byte(`{"canPublicNote":null}`)),
	}
	out := entity.PackRole(r, 0, nil, map[string]any{"canPublicNote": true})
	policies := out["policies"].(map[string]any)
	filled, ok := policies["canPublicNote"].(map[string]any)
	require.True(t, ok, "explicit null は default-fill される")
	assert.Equal(t, true, filled["useDefault"])
	assert.Equal(t, true, filled["value"])
}

// malformed jsonb は fail-soft: panic せず全 default-fill / condFormula は {}。
func TestPackRole_MalformedJSONFailsSoft(t *testing.T) {
	r := &model.Role{
		ID:          "r",
		UpdatedAt:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Policies:    datatypes.JSON([]byte(`{bad`)),
		CondFormula: datatypes.JSON([]byte(`{bad`)),
	}
	out := entity.PackRole(r, 0, nil, map[string]any{"canPublicNote": true})
	policies := out["policies"].(map[string]any)
	filled, ok := policies["canPublicNote"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, filled["useDefault"])
	cf, ok := out["condFormula"].(map[string]any)
	require.True(t, ok)
	assert.Empty(t, cf)
}

// 非 object な condFormula (配列/スカラー) は object 契約のため {} に正規化。
func TestPackRole_CondFormulaNonObjectNormalized(t *testing.T) {
	for _, raw := range []string{`[1,2,3]`, `42`, `"x"`} {
		r := &model.Role{ID: "r", UpdatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), CondFormula: datatypes.JSON([]byte(raw))}
		out := entity.PackRole(r, 0, nil, map[string]any{})
		cf, ok := out["condFormula"].(map[string]any)
		require.True(t, ok, "condFormula=%s は object に正規化される", raw)
		assert.Empty(t, cf)
	}
}

// createdAt fallback: idGen 無し / 非 aidx ID は updatedAt にフォールバック。
func TestPackRole_CreatedAtFallback(t *testing.T) {
	r := &model.Role{ID: "not-aidx", Name: "x", UpdatedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	out := entity.PackRole(r, 0, nil, map[string]any{})
	createdAt, _ := out["createdAt"].(string)
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}T.*Z$`, createdAt)
	// policies は空 default でも map (nil でない)。
	_, ok := out["policies"].(map[string]any)
	assert.True(t, ok)
	// condFormula は空 jsonb のとき {}。
	cf, ok := out["condFormula"].(map[string]any)
	require.True(t, ok)
	assert.Empty(t, cf)
}
