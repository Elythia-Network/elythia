package i

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/localfederation"
	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestUpdateFederationIgnoresRequestedOtherUserID(t *testing.T) {
	db, err := testutil.OpenTestDB()
	if err != nil {
		t.Skipf("PostgreSQL test DB unavailable: %v", err)
	}
	testutil.ApplyMigrations(db)

	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	userRepo := repository.NewUserRepository(db)
	keypairRepo := repository.NewUserKeypairRepository(db)
	keypairExtraRepo := repository.NewUserKeypairExtraRepository(db)
	signupService := signup.NewService(userRepo, metaRepo, idGen, db, nil, nil)
	signupService.SetKeypairRepo(keypairRepo)
	signupService.SetKeypairExtraRepo(keypairExtraRepo)
	prefix := "itif" + strings.ToLower(idGen.Generate(time.Now()))[:8]
	adminResult, err := signupService.SignupWithFederationStatus(prefix+"admin", "pass1234", false, signup.UsernamePolicyOperator, model.FederationStatusPending)
	require.NoError(t, err)
	targetResult, err := signupService.SignupWithFederationStatus(prefix+"target", "pass1234", false, signup.UsernamePolicyOperator, model.FederationStatusOn)
	require.NoError(t, err)
	adminResult.User.IsRoot = true
	require.NoError(t, userRepo.UpdateUser(adminResult.User.ID, map[string]any{"isRoot": true}))
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "user_profile" WHERE "userId" IN (?, ?)`, adminResult.User.ID, targetResult.User.ID)
		db.Exec(`DELETE FROM "user_keypair" WHERE "userId" IN (?, ?)`, adminResult.User.ID, targetResult.User.ID)
		db.Exec(`DELETE FROM "user_keypair_extra" WHERE "userId" IN (?, ?)`, adminResult.User.ID, targetResult.User.ID)
		db.Exec(`DELETE FROM "user" WHERE id IN (?, ?)`, adminResult.User.ID, targetResult.User.ID)
		db.Exec(`DELETE FROM "used_username" WHERE username IN (?, ?)`, prefix+"admin", prefix+"target")
	})

	h := NewHandler(nil, idGen, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h.SetLocalFederationService(localfederation.NewService(repository.NewLocalFederationRepository(db), false))
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i/update-federation", strings.NewReader(`{"userId":"`+targetResult.User.ID+`","federationStatus":"off"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), adminResult.User)

	require.NoError(t, h.UpdateFederation(c))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var admin, target model.User
	require.NoError(t, db.First(&admin, "id = ?", adminResult.User.ID).Error)
	require.NoError(t, db.First(&target, "id = ?", targetResult.User.ID).Error)
	require.Equal(t, model.FederationStatusOff, admin.FederationStatus)
	require.Equal(t, model.FederationStatusOn, target.FederationStatus,
		"even an administrator request can only change the authenticated account")
}
