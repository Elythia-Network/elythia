package signup_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisignup "github.com/elythia-network/elythia/internal/api/signup"
	coresignup "github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newFederationIntegrationHandler(t *testing.T) (*apisignup.Handler, *gorm.DB, string) {
	t.Helper()
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
	svc := coresignup.NewService(userRepo, metaRepo, idGen, db, nil, nil)
	svc.SetKeypairRepo(repository.NewUserKeypairRepository(db))
	svc.SetKeypairExtraRepo(repository.NewUserKeypairExtraRepository(db))
	h := apisignup.NewHandler(svc, metaRepo, idGen, nil, nil, nil)

	prefix := "if" + strings.ToLower(idGen.Generate(time.Now()))[:8]
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "user_profile" WHERE "userId" IN (SELECT id FROM "user" WHERE "usernameLower" LIKE ?)`, prefix+"%")
		db.Exec(`DELETE FROM "user_keypair" WHERE "userId" IN (SELECT id FROM "user" WHERE "usernameLower" LIKE ?)`, prefix+"%")
		db.Exec(`DELETE FROM "user_keypair_extra" WHERE "userId" IN (SELECT id FROM "user" WHERE "usernameLower" LIKE ?)`, prefix+"%")
		db.Exec(`DELETE FROM "user" WHERE "usernameLower" LIKE ?`, prefix+"%")
		db.Exec(`DELETE FROM "used_username" WHERE username LIKE ?`, prefix+"%")
	})
	return h, db, prefix
}

func postFederationSignup(t *testing.T, h *apisignup.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/signup", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Signup(e.NewContext(req, rec)))
	return rec
}

func TestSignupFederationStatusControlsStoredStateAndKeys(t *testing.T) {
	h, db, prefix := newFederationIntegrationHandler(t)
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
			username := prefix + tc.usernameSuffix
			resp := postFederationSignup(t, h, fmt.Sprintf(`{"username":%q,"password":"pass1234"%s}`, username, tc.field))
			assert.Equal(t, http.StatusOK, resp.Code, resp.Body.String())

			var user model.User
			require.NoError(t, db.Where(`"usernameLower" = ?`, username).First(&user).Error)
			assert.Equal(t, tc.wantStatus, user.FederationStatus)
			var rsaCount, edCount int64
			require.NoError(t, db.Model(&model.UserKeypair{}).Where(`"userId" = ?`, user.ID).Count(&rsaCount).Error)
			require.NoError(t, db.Model(&model.UserKeypairExtra{}).Where(`"userId" = ?`, user.ID).Count(&edCount).Error)
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
