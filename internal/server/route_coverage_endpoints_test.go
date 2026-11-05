package server

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/routecoverage"
)

// TestRouteCoverageEndpointsMatchRoutes checks that the denominator of the
// API coverage (routecoverage.Endpoints, #4037) is the set of /api routes of
// the built server: every route is an endpoint there, and every endpoint is
// routed. The catch-all "*" and the plugin namespaces are not endpoints;
// reset-db is routed in test mode only.
//
// 分母はparamDefのgoldenから静的に作るので、宣言の無いルートを足したり消したり
// したときに、ここで食い違いを落とす。
func TestRouteCoverageEndpointsMatchRoutes(t *testing.T) {
	if serverIntegrationDB == nil {
		// CIではTestMainが落とすので、ここには来ない(requireDBEnv)。
		t.Skip("PostgreSQL unavailable")
	}
	eps, err := routecoverage.Endpoints()
	require.NoError(t, err)
	routes, pluginNamespaces := builtAPIRoutes(t)
	require.Greater(t, len(routes), 500, "the /api routes of the built server were not read")

	var notEndpoint []string
	for name := range routes {
		if name == "*" || slices.ContainsFunc(pluginNamespaces, func(ns string) bool { return strings.HasPrefix(name, ns) }) {
			continue
		}
		if _, ok := eps[name]; !ok {
			notEndpoint = append(notEndpoint, name)
		}
	}
	sort.Strings(notEndpoint)
	assert.Empty(t, notEndpoint, "routes missing from routecoverage.Endpoints (add a paramDef, or the route to undeclared in internal/routecoverage)")

	var notRouted []string
	for name := range eps {
		if _, ok := routes[name]; !ok && testModeOnlyRoutes[name] == "" {
			notRouted = append(notRouted, name)
		}
	}
	sort.Strings(notRouted)
	assert.Empty(t, notRouted, "endpoints of routecoverage.Endpoints that the server does not route")
}
