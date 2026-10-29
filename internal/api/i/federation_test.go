package i

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/localfederation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type federationAPIRepository struct{ user model.User }

func (r *federationAPIRepository) FindUser(string) (*model.User, error) { return &r.user, nil }
func (*federationAPIRepository) IsSystemAccount(string) (bool, error)   { return false, nil }
func (r *federationAPIRepository) ObserveExternalReference(string, string, string, time.Time) (*model.User, bool, error) {
	return &r.user, false, nil
}
func (r *federationAPIRepository) ChangeStatus(_ string, next model.FederationStatus, at time.Time, _ *model.UserKeypair, _ *model.UserKeypairExtra) (*model.User, bool, error) {
	changed := r.user.FederationStatus != next
	r.user.FederationStatus = next
	if next == model.FederationStatusOn {
		r.user.FederationEnabledAt = &at
	}
	return &r.user, changed, nil
}
func (*federationAPIRepository) WithLockedUser(string, func(tx *gorm.DB, user *model.User) error) error {
	return nil
}

func TestUpdateFederationInvalidatesUserCache(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	invalidator := &stubTokenInvalidator{}
	publisher := &stubIMainStreamPublisher{}
	h := NewHandler(nil, idGen, nil, nil, nil, nil, nil, invalidator, nil, nil, nil, nil)
	h.SetMainStreamPublisher(publisher)
	h.SetLocalFederationService(localfederation.NewService(&federationAPIRepository{
		user: model.User{ID: "u1", Username: "alice", FederationStatus: model.FederationStatusOn},
	}, false))

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i/update-federation", strings.NewReader(`{"federationStatus":"off"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), &model.User{ID: "u1", Username: "alice", FederationStatus: model.FederationStatusOn})

	require.NoError(t, h.UpdateFederation(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"u1"}, invalidator.userCalls, "all cached tokens must reload the updated user state")
	require.Len(t, publisher.calls, 1)
	assert.Equal(t, "u1", publisher.calls[0].userID)
	assert.Equal(t, "meUpdated", publisher.calls[0].eventType)
	assert.Equal(t, map[string]any{"federationStatus": model.FederationStatusOff}, publisher.calls[0].body,
		"partial update must not overwrite unread state or other MeDetailed fields")
}

var _ repository.LocalFederationRepository = (*federationAPIRepository)(nil)
