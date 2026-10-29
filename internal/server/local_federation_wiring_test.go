package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/config"
	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	corelocalfederation "github.com/elythia-network/elythia/internal/core/localfederation"
	coresystemaccount "github.com/elythia-network/elythia/internal/core/systemaccount"
	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type wiringRateLimitStore struct {
	calls []string
}

func (s *wiringRateLimitStore) Check(_ context.Context, key string, _ time.Duration, max int) (middleware.LimitInfo, error) {
	s.calls = append(s.calls, key)
	return middleware.LimitInfo{Remaining: max - 1}, nil
}

// The real repositories and buildHandlers wiring must make an OFF local actor
// unavailable at the AP boundary and expose its state only on the self API.
func TestBuiltHandlersWireLocalFederationService(t *testing.T) {
	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	testutil.ApplyMigrations(db)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	userID := idGen.Generate(time.Now())
	user := &model.User{
		ID: userID, Username: "lfwireuser", UsernameLower: "lfwireuser",
		IsExplorable: true, AvatarDecorations: []byte("[]"),
		FederationStatus: model.FederationStatusOff,
	}
	nativeToken := "nativefederation" // PostgreSQL user.token is char(16).
	user.Token = &nativeToken
	require.NoError(t, db.Create(user).Error)
	require.NoError(t, db.Create(&model.UserProfile{UserID: userID}).Error)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, userID)
		db.Exec(`DELETE FROM "user" WHERE id = ?`, userID)
	})

	userCache := repository.NewCachedUserRepository(repository.NewUserRepository(db))
	userRepo := repository.UserRepository(userCache)
	noteRepo := repository.NewNoteRepository(db)
	localFederation := corelocalfederation.NewService(repository.NewCachedLocalFederationRepository(
		repository.NewLocalFederationRepository(db), userCache,
	), false)
	keypairRepo := repository.NewUserKeypairRepository(db)
	systemAccounts := coresystemaccount.NewService(userRepo, keypairRepo, repository.NewSystemAccountRepository(db), idGen)
	metaRepo := repository.NewMetaRepository(db)
	meta, err := metaRepo.Fetch()
	require.NoError(t, err)
	previousFederation := meta.Federation
	require.NoError(t, metaRepo.Update(map[string]any{"federation": "all"}))
	t.Cleanup(func() { require.NoError(t, metaRepo.Update(map[string]any{"federation": previousFederation})) })
	instanceService := coreinstance.NewService(repository.NewInstanceRepository(db), metaRepo, idGen)
	userService := coreuser.NewService(userRepo, noteRepo, repository.NewUserNotePiningRepository(db), idGen, nil)
	urls := activitypub.NewURLBuilder("https://example.test")
	renderer := activitypub.NewRenderer(urls)
	authUsers := testutil.NewMockUserRepository()
	authUsers.Tokens[nativeToken] = user
	appWriteUser := *user
	appReadUser := *user
	tokens := testutil.NewMockAccessTokenRepository()
	tokens.Tokens["write-token"] = &model.AccessToken{
		ID: "write-app-token", Token: "write-token", Permission: model.StringArray{"write:account"}, User: &appWriteUser,
	}
	tokens.Tokens["read-token"] = &model.AccessToken{
		ID: "read-app-token", Token: "read-token", Permission: model.StringArray{"read:account"}, User: &appReadUser,
	}
	authMiddleware := middleware.NewAuthMiddleware(authUsers, tokens)
	srv := &Server{
		config: &config.Config{URL: "https://example.test", Host: "example.test", Hostname: "example.test"},
		auth:   authMiddleware,
	}
	handlers := srv.buildHandlers(
		&wiring{repos: repos{user: userRepo, note: noteRepo, keypair: keypairRepo}},
		core{localFederation: localFederation, sysAcct: systemAccounts, user: userService, instance: instanceService},
		fed{apRenderer: renderer, apURLs: urls}, idGen, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)

	// Exercise the registered self API route with tokens resolved by the same
	// authentication middleware as the router. A scoped app token must not
	// perform the irreversible federation transition, while the owner's native
	// session can. The read endpoint retains its read:account scope contract.
	e := echo.New()
	e.Use(authMiddleware.Authenticate())
	api := apiRoutes{e.Group("/api")}
	api.Use(middleware.HideAppUserWithoutRead())
	limitStore := &wiringRateLimitStore{}
	rateLimiter := middleware.NewRateLimiter(limitStore, false, map[string]*middleware.EndpointLimit{})
	api.Use(rateLimiter.Middleware())
	registerIRoutes(api, handlers.i, nil, nil, nil, nil, nil, nil, rateLimiter)

	request := func(token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/i/update-federation", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}
	appResponse := request("write-token", `{"federationStatus":"on"}`)
	require.Equal(t, http.StatusForbidden, appResponse.Code)
	require.Contains(t, appResponse.Body.String(), "ACCESS_DENIED")
	assert.Empty(t, limitStore.calls, "RequireSecure must reject app tokens before charging the native-user bucket")
	var persisted model.User
	require.NoError(t, db.Select("federationStatus", "federationEnabledAt").Where("id = ?", userID).Take(&persisted).Error)
	assert.Equal(t, model.FederationStatusOff, persisted.FederationStatus, "denied app token must not mutate federation state")
	assert.Nil(t, persisted.FederationEnabledAt)
	var keypairCount int64
	require.NoError(t, db.Model(&model.UserKeypair{}).Where(`"userId" = ?`, userID).Count(&keypairCount).Error)
	assert.Zero(t, keypairCount, "denied app token must not create federation keys")

	readReq := httptest.NewRequest(http.MethodPost, "/api/i/federation", strings.NewReader(`{}`))
	readReq.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	readReq.Header.Set(echo.HeaderAuthorization, "Bearer read-token")
	readRec := httptest.NewRecorder()
	e.ServeHTTP(readRec, readReq)
	require.Equal(t, http.StatusOK, readRec.Code)
	assert.Contains(t, readRec.Body.String(), `"federationStatus":"off"`)

	// i/federation is backed by the service installed by buildHandlers.
	// The same built handler must reject an ActivityPub actor request while
	// the account remains OFF.
	cached, err := userRepo.FindByID(userID)
	require.NoError(t, err)
	require.Equal(t, model.FederationStatusOff, cached.FederationStatus)
	req := httptest.NewRequest(http.MethodGet, "/users/"+userID, nil)
	req.Header.Set(echo.HeaderAccept, "application/activity+json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/users/:id")
	c.SetParamNames("id")
	c.SetParamValues(userID)
	require.NoError(t, handlers.ap.User(c))
	require.Equal(t, http.StatusNotFound, rec.Code)

	// Enabling through the authenticated self API invalidates the warmed user
	// cache, so the existing AP handler immediately serves the actor again.
	ownerResponse := request(nativeToken, `{"federationStatus":"on"}`)
	require.Equal(t, http.StatusOK, ownerResponse.Code)
	assert.Contains(t, ownerResponse.Body.String(), `"federationStatus":"on"`)
	assert.Equal(t, []string{userID + ":i/update-federation"}, limitStore.calls,
		"the production route applies its scoped limiter once, after secure auth")
	req = httptest.NewRequest(http.MethodGet, "/users/"+userID, nil)
	req.Header.Set(echo.HeaderAccept, "application/activity+json")
	rec = httptest.NewRecorder()
	c = e.NewContext(req, rec)
	c.SetPath("/users/:id")
	c.SetParamNames("id")
	c.SetParamValues(userID)
	require.NoError(t, handlers.ap.User(c))
	require.Equal(t, http.StatusOK, rec.Code)
	cached, err = userRepo.FindByID(userID)
	require.NoError(t, err)
	require.Equal(t, model.FederationStatusOn, cached.FederationStatus)
	require.NotNil(t, cached.FirstExternalReferenceAt)
	require.NotNil(t, cached.FirstExternalReferenceReason)
	require.Equal(t, corelocalfederation.ExternalReferenceReasonActor, *cached.FirstExternalReferenceReason)
	firstExternalReferenceAt := *cached.FirstExternalReferenceAt

	// WebFinger observes the already-known actor reference. It must preserve the
	// first-observation timestamp and reason in the cached user.
	req = httptest.NewRequest(http.MethodGet, "/.well-known/webfinger?resource=acct:lfwireuser@example.test", nil)
	rec = httptest.NewRecorder()
	c = e.NewContext(req, rec)
	require.NoError(t, handlers.wellknown.Webfinger(c))
	require.Equal(t, http.StatusOK, rec.Code)
	cached, err = userRepo.FindByID(userID)
	require.NoError(t, err)
	require.Equal(t, model.FederationStatusOn, cached.FederationStatus)
	require.NotNil(t, cached.FirstExternalReferenceAt)
	require.NotNil(t, cached.FirstExternalReferenceReason)
	assert.True(t, firstExternalReferenceAt.Equal(*cached.FirstExternalReferenceAt))
	assert.Equal(t, corelocalfederation.ExternalReferenceReasonActor, *cached.FirstExternalReferenceReason)

	// A system actor starts externally available and already referenced. Its
	// normal AP and WebFinger routes must remain available.
	kind := "lfw" + userID[len(userID)-6:]
	systemUser, err := systemAccounts.Fetch(kind)
	require.NoError(t, err)
	require.Equal(t, model.FederationStatusOn, systemUser.FederationStatus)
	require.NotNil(t, systemUser.FirstExternalReferenceReason)
	require.Equal(t, "system-account", *systemUser.FirstExternalReferenceReason)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "system_account" WHERE type = ?`, kind)
		db.Exec(`DELETE FROM "user_keypair_extra" WHERE "userId" = ?`, systemUser.ID)
		db.Exec(`DELETE FROM "user_keypair" WHERE "userId" = ?`, systemUser.ID)
		db.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, systemUser.ID)
		db.Exec(`DELETE FROM "user" WHERE id = ?`, systemUser.ID)
	})

	req = httptest.NewRequest(http.MethodGet, "/users/"+systemUser.ID, nil)
	req.Header.Set(echo.HeaderAccept, "application/activity+json")
	rec = httptest.NewRecorder()
	c = e.NewContext(req, rec)
	c.SetPath("/users/:id")
	c.SetParamNames("id")
	c.SetParamValues(systemUser.ID)
	require.NoError(t, handlers.ap.User(c))
	assert.Equal(t, http.StatusOK, rec.Code, "system actor remains available to AP clients")

	req = httptest.NewRequest(http.MethodGet, "/.well-known/webfinger?resource=acct:"+systemUser.Username+"@example.test", nil)
	rec = httptest.NewRecorder()
	c = e.NewContext(req, rec)
	require.NoError(t, handlers.wellknown.Webfinger(c))
	assert.Equal(t, http.StatusOK, rec.Code, "system account remains discoverable through WebFinger")
}
