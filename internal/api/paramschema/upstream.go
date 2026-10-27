package paramschema

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// upstreamGolden is upstream_paramdefs.json, the paramDef and meta of every
// upstream endpoint (make paramdef-gen).
//
//go:embed upstream_paramdefs.json
var upstreamGolden []byte

// Endpoint is one upstream endpoint of the embedded golden.
type Endpoint struct {
	// Schema is the compiled paramDef, nil when CompileErr is set.
	Schema *Schema
	// CompileErr is why the paramDef did not compile. The tests require
	// every upstream paramDef to compile, so it is only set when the golden
	// and this package disagree in a build that skipped them.
	CompileErr error
	// AllowGet is meta.allowGet: upstream also answers GET with the query
	// as the params.
	AllowGet bool
	// RequireFile is meta.requireFile: upstream reads the params from the
	// multipart fields.
	RequireFile bool
	// Elythia marks an endpoint upstream does not have, declared in
	// elythia_paramdefs.json (#3869). Only Effective has them.
	Elythia bool
}

// ScalarParam is a top-level property upstream casts from a string for GET
// and multipart requests (ApiCallService.ts "Cast non JSON input").
type ScalarParam struct {
	Name string
	// Type is the property's type keyword: "boolean", "number" or "integer".
	Type string
}

// ScalarParams returns, in schema order, the top-level properties whose type
// keyword is boolean, number or integer. Upstream walks
// Object.keys(paramDef.properties) in that order and JSON.parses the string
// value of each of them before ajv runs.
func (s *Schema) ScalarParams() []ScalarParam {
	var out []ScalarParam
	for _, p := range s.root.properties {
		switch p.node.typeName {
		case "boolean", "number", "integer":
			out = append(out, ScalarParam{Name: p.name, Type: p.node.typeName})
		}
	}
	return out
}

var upstreamOnce = sync.OnceValues(func() (map[string]Endpoint, error) {
	return parseUpstream(upstreamGolden)
})

// Upstream returns the upstream endpoints of the embedded golden by name
// ("notes/create"), each with its compiled paramDef. The golden is parsed and
// compiled once; the map is shared and must not be modified.
func Upstream() (map[string]Endpoint, error) {
	return upstreamOnce()
}

func parseUpstream(golden []byte) (map[string]Endpoint, error) {
	var g struct {
		Endpoints map[string]struct {
			ParamDef json.RawMessage `json:"paramDef"`
			Meta     struct {
				AllowGet    bool `json:"allowGet"`
				RequireFile bool `json:"requireFile"`
			} `json:"meta"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(golden, &g); err != nil {
		return nil, fmt.Errorf("paramschema: upstream golden: %w", err)
	}
	out := make(map[string]Endpoint, len(g.Endpoints))
	for name, ep := range g.Endpoints {
		// RawMessageはgoldenのバイト列そのままなので、propertiesの順が残る。
		s, err := Compile(ep.ParamDef)
		out[name] = Endpoint{Schema: s, CompileErr: err, AllowGet: ep.Meta.AllowGet, RequireFile: ep.Meta.RequireFile}
	}
	return out, nil
}

// UpstreamGolden returns the embedded golden (upstream_paramdefs.json) as is.
// Besides the paramDefs it holds what upstream's /api.json is built from
// (each endpoint's doc and the packed schemas as refs), which
// internal/api/openapi reads. The slice is shared and must not be modified.
func UpstreamGolden() []byte {
	return upstreamGolden
}
