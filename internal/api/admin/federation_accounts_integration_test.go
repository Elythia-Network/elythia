package admin_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountsCreateFederationStatusControlsStoredStateAndKeys(t *testing.T) {
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

	accountPrefix := "itaf" + strings.ToLower(idGen.Generate(time.Now()))[:8]
	t.Cleanup(func() {
		integrationDB.Exec(`DELETE FROM "user_profile" WHERE "userId" IN (SELECT id FROM "user" WHERE "usernameLower" LIKE ?)`, accountPrefix+"%")
		integrationDB.Exec(`DELETE FROM "user_keypair" WHERE "userId" IN (SELECT id FROM "user" WHERE "usernameLower" LIKE ?)`, accountPrefix+"%")
		integrationDB.Exec(`DELETE FROM "user_keypair_extra" WHERE "userId" IN (SELECT id FROM "user" WHERE "usernameLower" LIKE ?)`, accountPrefix+"%")
		integrationDB.Exec(`DELETE FROM "user" WHERE "usernameLower" LIKE ?`, accountPrefix+"%")
		integrationDB.Exec(`DELETE FROM "used_username" WHERE username LIKE ?`, accountPrefix+"%")
		integrationDB.Exec(`DELETE FROM "user" WHERE id = ?`, adminID)
	})

	svc := signup.NewService(userRepo, metaRepo, idGen, integrationDB, nil, nil)
	svc.SetKeypairRepo(repository.NewUserKeypairRepository(integrationDB))
	svc.SetKeypairExtraRepo(repository.NewUserKeypairExtraRepository(integrationDB))
	h := apiadmin.NewHandler(svc, nil, metaRepo, userRepo, idGen, nil, entity.NewPacker(nil, entity.PackerDeps{}), nil, nil, nil, nil, nil, nil, nil)

	for _, tc := range []struct {
		name           string
		usernameSuffix string
		field          string
		wantStatus     model.FederationStatus
		wantKeys       bool
	}{
		{name: "omitted defaults on", usernameSuffix: "default", wantStatus: model.FederationStatusOn, wantKeys: true},
		{name: "explicit on", usernameSuffix: "on", field: `,"federationStatus":"on"`, wantStatus: model.FederationStatusOn, wantKeys: true},
		{name: "off", usernameSuffix: "off", field: `,"federationStatus":"off"`, wantStatus: model.FederationStatusOff},
		{name: "pending", usernameSuffix: "pending", field: `,"federationStatus":"pending"`, wantStatus: model.FederationStatusPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			username := accountPrefix + tc.usernameSuffix
			body := fmt.Sprintf(`{"username":%q,"password":"pass1234"%s}`, username, tc.field)
			rec := doPost(h.AccountsCreate, body, adminUser)
			require.Equal(t, 200, rec.Code, rec.Body.String())

			var user model.User
			require.NoError(t, integrationDB.Where(`"usernameLower" = ?`, username).First(&user).Error)
			assert.Equal(t, tc.wantStatus, user.FederationStatus)
			var rsaCount, edCount int64
			require.NoError(t, integrationDB.Model(&model.UserKeypair{}).Where(`"userId" = ?`, user.ID).Count(&rsaCount).Error)
			require.NoError(t, integrationDB.Model(&model.UserKeypairExtra{}).Where(`"userId" = ?`, user.ID).Count(&edCount).Error)
			if tc.wantKeys {
				assert.EqualValues(t, 1, rsaCount)
				assert.EqualValues(t, 1, edCount)
			} else {
				assert.Zero(t, rsaCount)
				assert.Zero(t, edCount)
			}
		})
	}
}
