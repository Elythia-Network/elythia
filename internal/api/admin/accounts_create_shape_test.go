package admin_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upstream admin/accounts/create は `pack(account, account, {schema: 'MeDetailed',
// includeSecrets: true})` を返す。viewer は作ったアカウント自身 (#4121)。

func TestAccountsCreate_InitialSetupEmitsModerationNoteAsEmptyString(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.AccountsCreate, `{"username":"admin","password":"pass123"}`, nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, true, resp["isModerator"], "the initial-setup account is root and so a moderator")
	v, ok := resp["moderationNote"]
	require.True(t, ok, "moderationNote must be present when the viewer is a moderator")
	assert.Equal(t, "", v, "moderationNote must be '' (upstream `?? ''`), never null")
}

func TestAccountsCreate_NonModeratorAccountOmitsModerationNote(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rootID := "root1"
	metaRepo.Meta.RootUserID = &rootID
	rec := doPost(h.AccountsCreate, `{"username":"user2","password":"pass"}`, &model.User{ID: "root1", Username: "root"})
	require.Equal(t, 200, rec.Code, rec.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, false, resp["isModerator"])
	_, ok := resp["moderationNote"]
	assert.False(t, ok, "moderationNote must be omitted when the viewer (the new account) is not a moderator")
}

// MeDetailedOnly (2026.10.1) の必須項目が揃っていること。以前は手で組んだ map で、
// followApproval* / mutedWords / mutedInstances / hardMutedWords / email が欠けていた。
func TestAccountsCreate_ResponseHasMeDetailedRequiredKeys(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.AccountsCreate, `{"username":"admin","password":"pass123"}`, nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	for _, k := range []string{
		"followApprovalLocalSeconds", "followApprovalRemoteSeconds",
		"mutedWords", "mutedInstances", "hardMutedWords",
		"email", "emailVerified", "securityKeysList",
		"policies", "token", "createdAt", "pinnedPageId", "pinnedPage",
	} {
		_, ok := resp[k]
		assert.True(t, ok, "missing MeDetailed key %q", k)
	}
	assert.Nil(t, resp["followApprovalLocalSeconds"])
	assert.Nil(t, resp["followApprovalRemoteSeconds"])
	shapetest.Assert(t, "MeDetailedOnly", resp)
}

// 実 DB で作ったアカウントでは、DB の既定値 (fields '[]' / visibility 'public') が
// レスポンスに乗ること。in-memory の profile を渡すと zero 値のまま出てしまう。
func TestAccountsCreate_DBBackedResponseReflectsProfileDefaults(t *testing.T) {
	if integrationDB == nil {
		t.Skip("integrationDB unavailable (set TEST_DB_HOST or run with Docker daemon for testcontainer)")
	}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	adminID := idGen.Generate(time.Now())
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", RootUserID: &adminID}
	userRepo := repository.NewUserRepository(integrationDB)
	adminUser := &model.User{ID: adminID, Username: "root", UsernameLower: "root", IsRoot: true}
	require.NoError(t, integrationDB.Create(adminUser).Error)

	username := "itshape" + strings.ToLower(idGen.Generate(time.Now()))[:8]
	t.Cleanup(func() {
		integrationDB.Exec(`DELETE FROM "user_profile" WHERE "userId" IN (SELECT id FROM "user" WHERE "usernameLower" = ?)`, username)
		integrationDB.Exec(`DELETE FROM "user" WHERE "usernameLower" = ?`, username)
		integrationDB.Exec(`DELETE FROM "used_username" WHERE username = ?`, username)
		integrationDB.Exec(`DELETE FROM "user" WHERE id = ?`, adminID)
	})

	svc := signup.NewService(userRepo, metaRepo, idGen, integrationDB, nil, nil)
	h := apiadmin.NewHandler(svc, nil, metaRepo, userRepo, idGen, nil, entity.NewPacker(nil, entity.PackerDeps{}), nil, nil, nil, nil, nil, nil, nil)
	rec := doPost(h.AccountsCreate, fmt.Sprintf(`{"username":%q,"password":"pass1234","federationStatus":"off"}`, username), adminUser)
	require.Equal(t, 200, rec.Code, rec.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "public", resp["followersVisibility"])
	assert.Equal(t, "public", resp["followingVisibility"])
	assert.Equal(t, []any{}, resp["fields"])
	assert.Equal(t, []any{}, resp["verifiedLinks"])
	_, hasLocal := resp["followApprovalLocalSeconds"]
	assert.True(t, hasLocal)
	_, hasNote := resp["moderationNote"]
	assert.False(t, hasNote)
}
