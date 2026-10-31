package admin_test

import (
	"net/http"
	"testing"
	"time"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRoleIntegrationHandler returns a handler whose role service writes to the
// real test database, with one manual role seeded.
func newRoleIntegrationHandler(t *testing.T) (*apiadmin.Handler, *model.Role) {
	t.Helper()
	if integrationDB == nil {
		t.Skip("integrationDB unavailable (set TEST_DB_HOST or run with Docker daemon for testcontainer)")
	}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	userRepo := testutil.NewMockUserRepository()
	roleSvc := role.NewService(repository.NewRoleRepository(integrationDB), repository.NewRoleAssignmentRepository(integrationDB), metaRepo, idGen)
	h := apiadmin.NewHandler(signup.NewService(userRepo, metaRepo, idGen, nil, nil, nil), roleSvc, metaRepo, userRepo, idGen, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	color, icon := "#ff0000", "https://remote.example/icon.png"
	r := &model.Role{
		ID:          "roleupd_" + idGen.Generate(time.Now()),
		Name:        "r",
		Target:      model.RoleTargetManual,
		Color:       &color,
		IconURL:     &icon,
		CondFormula: []byte(`{}`),
		Policies:    []byte(`{}`),
		UpdatedAt:   time.Now(),
	}
	require.NoError(t, integrationDB.Create(r).Error)
	t.Cleanup(func() { integrationDB.Exec(`DELETE FROM "role" WHERE id = ?`, r.ID) })
	return h, r
}

func loadRole(t *testing.T, roleID string) model.Role {
	t.Helper()
	var got model.Role
	require.NoError(t, integrationDB.First(&got, "id = ?", roleID).Error)
	return got
}

// condFormula と policies を、テストの DB (simple protocol) でも保存できる (#3369)。
func TestRolesUpdate_StoresCondFormulaAndPoliciesInDB(t *testing.T) {
	h, r := newRoleIntegrationHandler(t)
	body := `{"roleId":"` + r.ID + `","condFormula":{"id":"c1","type":"isLocal"},` +
		`"policies":{"canInvite":{"useDefault":false,"priority":0,"value":true}}}`
	rec := doPost(h.RolesUpdate, body, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	got := loadRole(t, r.ID)
	assert.JSONEq(t, `{"id":"c1","type":"isLocal"}`, string(got.CondFormula))
	assert.JSONEq(t, `{"canInvite":{"useDefault":false,"priority":0,"value":true}}`, string(got.Policies))
}

// 本家と同じく、color・iconUrl の null は列を消し、欠けは変えない (#3369)。
func TestRolesUpdate_NullClearsColorAndIconURL(t *testing.T) {
	h, r := newRoleIntegrationHandler(t)

	rec := doPost(h.RolesUpdate, `{"roleId":"`+r.ID+`","name":"renamed"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	got := loadRole(t, r.ID)
	require.NotNil(t, got.Color, "an absent color must not change")
	require.NotNil(t, got.IconURL, "an absent iconUrl must not change")

	rec = doPost(h.RolesUpdate, `{"roleId":"`+r.ID+`","color":null}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	got = loadRole(t, r.ID)
	assert.Nil(t, got.Color)
	assert.NotNil(t, got.IconURL)

	rec = doPost(h.RolesUpdate, `{"roleId":"`+r.ID+`","iconUrl":null}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	got = loadRole(t, r.ID)
	assert.Nil(t, got.IconURL)

	rec = doPost(h.RolesUpdate, `{"roleId":"`+r.ID+`","color":"#00ff00","iconUrl":"https://remote.example/b.png"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	got = loadRole(t, r.ID)
	require.NotNil(t, got.Color)
	assert.Equal(t, "#00ff00", *got.Color)
	require.NotNil(t, got.IconURL)
	assert.Equal(t, "https://remote.example/b.png", *got.IconURL)
}
