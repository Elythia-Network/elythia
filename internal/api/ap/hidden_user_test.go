package ap

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
)

type moderatorSet map[string]bool

func (m moderatorSet) IsModerator(userID string) bool { return m[userID] }

func postJSONAs(handler func(*echo.Context) error, viewer *model.User, body string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if viewer != nil {
		c.Set(string(middleware.UserContextKey), viewer)
	}
	_ = handler(c)
	return rec
}

func TestAPIShow_HidesSuspendedAndDeletedLocalUsers(t *testing.T) {
	cases := []struct {
		name string
		user *model.User
	}{
		{"suspended", &model.User{ID: "u1", Username: "bob", IsSuspended: true}},
		{"deleted", &model.User{ID: "u1", Username: "bob", IsDeleted: true}},
	}
	viewer := &model.User{ID: "viewer", Username: "carol"}
	moderator := &model.User{ID: "mod", Username: "mod"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, userRepo, _, _ := newHandler(t)
			h.SetModeratorChecker(moderatorSet{"mod": true})
			userRepo.Users["u1"] = tc.user

			rec := postJSONAs(h.APIShow, viewer, `{"uri":"https://example.com/users/u1"}`)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "NO_SUCH_OBJECT")
			assert.NotContains(t, rec.Body.String(), "bob")

			rec = postJSONAs(h.APIShow, moderator, `{"uri":"https://example.com/users/u1"}`)
			assert.Equal(t, http.StatusOK, rec.Code, "moderators still see the user, as in users/show")
			assert.Contains(t, rec.Body.String(), "bob")
		})
	}
}

func TestAPIShow_HidesSuspendedUserWhenModeratorCheckIsUnwired(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "bob", IsSuspended: true}
	rec := postJSONAs(h.APIShow, &model.User{ID: "viewer"}, `{"uri":"https://example.com/users/u1"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.NotContains(t, rec.Body.String(), "bob")
}

func TestAPIShow_ShowsActiveLocalUserToNonModerator(t *testing.T) {
	h, userRepo, _, _ := newHandler(t)
	h.SetModeratorChecker(moderatorSet{})
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "bob"}
	rec := postJSONAs(h.APIShow, &model.User{ID: "viewer"}, `{"uri":"https://example.com/users/u1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "bob")
}
