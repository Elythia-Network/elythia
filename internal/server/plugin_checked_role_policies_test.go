package server

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/plugin"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

type checkedPolicyResolverStub struct {
	policies map[string]any
	err      error
	userID   string
	keys     []string
}

func (s *checkedPolicyResolverStub) GetUserPoliciesCheckedForKeys(userID string, keys ...string) (map[string]any, error) {
	s.userID = userID
	s.keys = append([]string(nil), keys...)
	return s.policies, s.err
}

func TestPluginPolicyAdaptersReturnOnlyRequestedKeys(t *testing.T) {
	all := map[string]any{"policyA": true, "privatePolicy": "hidden"}
	resolver := &checkedPolicyResolverStub{policies: all}
	requestContext := echo.New().NewContext(httptest.NewRequest("POST", "/", nil), httptest.NewRecorder())
	requestContext.Set(string(middleware.UserContextKey), &model.User{ID: "viewer"})

	gotRequest, err := (&pluginRequest{c: requestContext, policyResolver: resolver}).CheckedRolePolicies("policyA", "missingPolicy")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"policyA": true}, gotRequest)
	require.Equal(t, "viewer", resolver.userID)
	require.Equal(t, []string{"policyA", "missingPolicy"}, resolver.keys)

	resolver.userID, resolver.keys = "", nil
	gotContext, err := (&pluginContext{policyResolver: resolver}).CheckedUserRolePolicies("queued-actor", "privatePolicy")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"privatePolicy": "hidden"}, gotContext)
	require.Equal(t, "queued-actor", resolver.userID)
	require.Equal(t, []string{"privatePolicy"}, resolver.keys)
	require.Equal(t, "hidden", all["privatePolicy"], "projection must not mutate resolver output")
}

func TestPluginPolicyAdaptersPropagateResolverErrorsAndRequireIdentity(t *testing.T) {
	lookupErr := errors.New("role policy lookup failed")
	resolver := &checkedPolicyResolverStub{err: lookupErr}
	requestContext := echo.New().NewContext(httptest.NewRequest("POST", "/", nil), httptest.NewRecorder())
	requestContext.Set(string(middleware.UserContextKey), &model.User{ID: "viewer"})

	_, err := (&pluginRequest{c: requestContext, policyResolver: resolver}).CheckedRolePolicies("policyA")
	require.ErrorIs(t, err, lookupErr)
	_, err = (&pluginContext{policyResolver: resolver}).CheckedUserRolePolicies("queued-actor", "policyA")
	require.ErrorIs(t, err, lookupErr)

	_, err = (&pluginRequest{c: echo.New().NewContext(httptest.NewRequest("POST", "/", nil), httptest.NewRecorder()), policyResolver: resolver}).CheckedRolePolicies("policyA")
	require.Error(t, err, "an unauthenticated request cannot make a checked policy decision")
	_, err = (&pluginContext{policyResolver: resolver}).CheckedUserRolePolicies("", "policyA")
	require.Error(t, err, "a background check must identify the user whose roles are being resolved")
}

func TestWrapPluginHandlerCarriesCheckedPolicyResolver(t *testing.T) {
	resolver := &checkedPolicyResolverStub{policies: map[string]any{"policyA": true}}
	e := echo.New()
	request := httptest.NewRequest("POST", "/check", nil)
	recorder := httptest.NewRecorder()
	ctx := e.NewContext(request, recorder)
	ctx.Set(string(middleware.UserContextKey), &model.User{ID: "viewer"})
	handler := wrapPluginHandler(func(req plugin.Request) (any, error) {
		return req.(plugin.CheckedRolePolicyRequest).CheckedRolePolicies("policyA")
	}, nil, resolver)

	require.NoError(t, handler(ctx))
	require.Equal(t, "viewer", resolver.userID)
	require.Equal(t, []string{"policyA"}, resolver.keys)
	require.JSONEq(t, `{"policyA":true}`, recorder.Body.String())
}
