package openapi

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/paramschema"
)

// TestSpecMatchesUpstreamAPIJSON compares the document built from the
// embedded golden with the /api.json that upstream's own genOpenapiSpec
// produced (make openapi-upstream-check runs it in upstream's official image
// and sets MK_UPSTREAM_API_JSON).
//
// 本家互換の部分(全endpointのpathと、components)が、項目の順まで本家と同じ
// JSONになることを見る。goldenの抽出(extract.mjsのASTの評価)とGoへの移植の
// どちらかがずれると落ちる。本家の公式imageが要るので、`make test`では回さない。
func TestSpecMatchesUpstreamAPIJSON(t *testing.T) {
	path := os.Getenv("MK_UPSTREAM_API_JSON")
	if path == "" {
		t.Skip("MK_UPSTREAM_API_JSON is not set (make openapi-upstream-check)")
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	up, err := decode(data)
	require.NoError(t, err)

	// 本番と同じ組み立て(独自の宣言を足したもの)で比べ、独自の型はcomponents
	// から外す。独自の宣言を足す処理が本家互換の部分を書き換えたら落ちるように。
	g, err := NewDefault()
	require.NoError(t, err)
	eps, err := paramschema.Upstream()
	require.NoError(t, err)
	var routes []Route
	for name, ep := range eps {
		routes = append(routes, Route{"POST", name})
		if ep.AllowGet {
			routes = append(routes, Route{"GET", name})
		}
	}
	b, err := g.Spec(Options{APIURL: "https://example.test/api", Routes: routes})
	require.NoError(t, err)
	mine, err := decode(b)
	require.NoError(t, err)

	upPaths := field(up, "paths").(*object)
	minePaths := field(mine, "paths").(*object)
	require.NotEmpty(t, upPaths.keys)
	assert.ElementsMatch(t, upPaths.keys, minePaths.keys)
	for _, p := range upPaths.keys {
		assert.Equal(t, mustMarshal(t, upPaths.vals[p]), mustMarshal(t, minePaths.vals[p]), p)
	}
	apiDoc, err := decode(paramschema.ElythiaAPIDocJSON())
	require.NoError(t, err)
	schemas := field(field(mine, "components"), "schemas").(*object)
	if refs, ok := field(apiDoc, "refs").(*object); ok {
		for _, k := range refs.keys {
			schemas.del(k)
		}
	}
	assert.Equal(t, mustMarshal(t, field(up, "components")), mustMarshal(t, field(mine, "components")))
}
