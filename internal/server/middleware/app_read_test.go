package middleware

import (
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAppReadCtx builds a context as Authenticate leaves it for a resolved
// token: the user and the scope view are both set.
func newAppReadCtx(scope *AuthScope) (echo.Context, *model.User) {
	c, _ := newScopeCtx(scope)
	tok := "native-token"
	u := &model.User{ID: "u1", Token: &tok}
	c.Set(string(UserContextKey), u)
	if scope != nil && !scope.IsApp {
		c.Set(string(TokenContextKey), tok)
	} else {
		c.Set(string(TokenContextKey), "app-token")
	}
	return c, u
}

// runChain runs HideAppUserWithoutRead, then the given route middlewares, and
// reports the user the handler saw (nil when it was not reached) and the
// response status.
func runChain(t *testing.T, c echo.Context, mws ...echo.MiddlewareFunc) (seen *model.User, reached bool) {
	t.Helper()
	h := func(c echo.Context) error {
		reached = true
		seen = GetUser(c)
		return c.NoContent(http.StatusOK)
	}
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	require.NoError(t, HideAppUserWithoutRead()(h)(c))
	return seen, reached
}

func TestHideAppUserWithoutRead_KindlessEndpointSeesAnonymous(t *testing.T) {
	for _, tt := range []struct {
		name     string
		scope    *AuthScope
		wantUser bool
	}{
		{"app token without read:account is anonymous", &AuthScope{IsApp: true, Scopes: []string{"write:notes"}}, false},
		{"app token with no permission is anonymous", &AuthScope{IsApp: true, Scopes: []string{}}, false},
		{"app token with read:account keeps its user", &AuthScope{IsApp: true, Scopes: []string{"write:notes", AppReadKind}}, true},
		{"native token keeps its user", &AuthScope{IsApp: false}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, u := newAppReadCtx(tt.scope)
			seen, reached := runChain(t, c)
			require.True(t, reached)
			if tt.wantUser {
				assert.Same(t, u, seen)
			} else {
				assert.Nil(t, seen)
			}
		})
	}
}

func TestHideAppUserWithoutRead_AnonymousRequestUntouched(t *testing.T) {
	c, _ := newScopeCtx(nil)
	seen, reached := runChain(t, c)
	assert.True(t, reached)
	assert.Nil(t, seen)
}

// The gates that check the app token against the endpoint give the user back,
// so endpoints with a kind keep working for a narrow token that holds it.
func TestHideAppUserWithoutRead_GatesAdmitTheUser(t *testing.T) {
	policy := newStubPolicyChecker()
	policy.set("u1", "canX", true)
	for _, tt := range []struct {
		name string
		mws  []echo.MiddlewareFunc
	}{
		{"RequireAuth", []echo.MiddlewareFunc{RequireAuth()}},
		{"RequireScope with the held kind", []echo.MiddlewareFunc{RequireScope("write:notes")}},
		{"RequireAuth then RequireScope", []echo.MiddlewareFunc{RequireAuth(), RequireScope("write:notes")}},
		{"RequireAdmin", []echo.MiddlewareFunc{RequireAdmin(&stubRoleChecker{admin: true})}},
		{"RequireModerator", []echo.MiddlewareFunc{RequireModerator(&stubRoleChecker{moderator: true})}},
		{"RequireRolePolicy", []echo.MiddlewareFunc{RequireRolePolicy(policy, "canX")}},
		{"AdmitAppUser", []echo.MiddlewareFunc{AdmitAppUser()}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, u := newAppReadCtx(&AuthScope{IsApp: true, Scopes: []string{"write:notes"}})
			seen, reached := runChain(t, c, tt.mws...)
			require.True(t, reached)
			assert.Same(t, u, seen)
		})
	}
}

// RequireScope admits only after the kind matched; a token lacking it is
// still rejected with PERMISSION_DENIED.
func TestHideAppUserWithoutRead_RequireScopeStillRejectsMissingKind(t *testing.T) {
	c, _ := newAppReadCtx(&AuthScope{IsApp: true, Scopes: []string{"write:notes"}})
	_, reached := runChain(t, c, RequireScope("write:reactions"))
	assert.False(t, reached)
	assert.Equal(t, http.StatusForbidden, c.Response().Status)
}

// RequireSecure must see the user to answer 403 ACCESS_DENIED (an app token
// on a secure endpoint) rather than 401 CREDENTIAL_REQUIRED.
func TestHideAppUserWithoutRead_RequireSecureKeepsAccessDenied(t *testing.T) {
	c, _ := newAppReadCtx(&AuthScope{IsApp: true, Scopes: []string{"write:notes"}})
	_, reached := runChain(t, c, RequireSecure())
	assert.False(t, reached)
	assert.Equal(t, http.StatusForbidden, c.Response().Status)
}

// A second admit is a no-op once the user is back.
func TestAdmitHeldAppUser_NothingHeld(t *testing.T) {
	c, u := newAppReadCtx(&AuthScope{IsApp: false})
	admitHeldAppUser(c)
	assert.Same(t, u, GetUser(c))
}
