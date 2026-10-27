package paramschema

import (
	"strings"
	"testing"
)

func TestAddElythiaEndpoints(t *testing.T) {
	eps := map[string]Endpoint{}
	err := addElythiaEndpoints(eps, []byte(`{"endpoints":{
		"x/list":{"paramDef":{"type":"object","properties":{"limit":{"type":"integer","minimum":1}},"required":[]},"meta":{}},
		"x/upload":{"paramDef":{"type":"object","properties":{},"required":[]},"meta":{"allowGet":true,"requireFile":true}}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	ep := eps["x/list"]
	if !ep.Elythia || ep.Schema == nil || ep.AllowGet || ep.RequireFile {
		t.Fatalf("x/list: %+v", ep)
	}
	if err := ep.Schema.Validate(map[string]any{"limit": float64(0)}); err == nil || err.SchemaPath != "#/properties/limit/minimum" {
		t.Errorf("x/list: limit 0: %v", err)
	}
	if up := eps["x/upload"]; !up.Elythia || !up.AllowGet || !up.RequireFile {
		t.Errorf("x/upload: %+v", up)
	}
}

func TestAddElythiaEndpointsRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, defs, want string
	}{
		{"not JSON", `{`, "elythia paramDefs:"},
		{"unknown top-level key", `{"endpoints":{},"x":1}`, "unknown field"},
		{"unknown meta key", `{"endpoints":{"x/y":{"paramDef":{"type":"object"},"meta":{"kind":"read"}}}}`, "unknown field"},
		{"trailing data", `{"endpoints":{"x/y":{"paramDef":{"type":"object"}}}} {}`, "trailing data"},
		{"no endpoints", `{"endpoints":{}}`, "no endpoints"},
		{"upstream name", `{"endpoints":{"notes/show":{"paramDef":{"type":"object"}}}}`, "notes/show: an upstream endpoint"},
		{"paramDef not an object", `{"endpoints":{"x/y":{"paramDef":true}}}`, "must be an object"},
		{"paramDef missing", `{"endpoints":{"x/y":{"meta":{}}}}`, "x/y:"},
		{"default", `{"endpoints":{"x/y":{"paramDef":{"type":"object","properties":{"limit":{"type":"integer","default":10}}}}}}`, "cannot have a default"},
		{"nested default", `{"endpoints":{"x/y":{"paramDef":{"type":"object","properties":{"a":{"anyOf":[{"type":"string","default":"z"}]}}}}}}`, "cannot have a default"},
		{"does not compile", `{"endpoints":{"x/y":{"paramDef":{"type":"object","properties":{"a":{"type":"nope"}}}}}}`, "x/y:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eps := map[string]Endpoint{"notes/show": {}}
			err := addElythiaEndpoints(eps, []byte(tc.defs))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}

// TestEffectiveHasTheElythiaEndpoints checks that Effective adds every
// declaration of elythia_paramdefs.json, and that Upstream stays upstream's.
func TestEffectiveHasTheElythiaEndpoints(t *testing.T) {
	eff, err := Effective()
	if err != nil {
		t.Fatal(err)
	}
	up, err := Upstream()
	if err != nil {
		t.Fatal(err)
	}
	declared := readElythiaParamDefs(t)
	if len(declared) == 0 {
		t.Fatal("elythia_paramdefs.json declares no endpoint")
	}
	for name := range declared {
		if ep, ok := eff[name]; !ok || !ep.Elythia || ep.Schema == nil {
			t.Errorf("Effective lacks %s", name)
		}
		if _, ok := up[name]; ok {
			t.Errorf("Upstream has %s", name)
		}
	}
	elythia := 0
	for name, ep := range eff {
		if !ep.Elythia {
			continue
		}
		elythia++
		if _, ok := declared[name]; !ok {
			t.Errorf("%s is marked Elythia, but elythia_paramdefs.json does not declare it", name)
		}
	}
	if elythia != len(declared) || len(eff) != len(up)+len(declared) {
		t.Errorf("Effective has %d endpoints (%d Elythia), want %d + %d", len(eff), elythia, len(up), len(declared))
	}
	for name, ep := range up {
		if ep.Elythia {
			t.Errorf("Upstream marks %s as Elythia", name)
		}
	}
}
