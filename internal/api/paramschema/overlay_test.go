package paramschema

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// overlaidKeywordPaths walks an overlay and returns, by endpoint, the schema
// paths of the keywords it widens ("#/properties/queue/enum"). Only an input
// upstream rejects at one of these paths may be accepted after the overlay.
func overlaidKeywordPaths(t *testing.T, raw []byte) map[string]map[string]bool {
	t.Helper()
	v, err := decodeOrdered(json.NewDecoder(bytes.NewReader(raw)))
	require.NoError(t, err)
	root, ok := v.(*orderedObject)
	require.True(t, ok)
	out := map[string]map[string]bool{}
	var walk func(ep, path string, o *orderedObject)
	walk = func(ep, path string, o *orderedObject) {
		for _, k := range o.keys {
			switch k {
			case "properties":
				props := o.vals[k].(*orderedObject)
				for _, name := range props.keys {
					walk(ep, path+"/properties/"+escapeFragment(name), props.vals[name].(*orderedObject))
				}
			case "items":
				walk(ep, path+"/items", o.vals[k].(*orderedObject))
			case "enum", "x-elythia-enumPattern":
				out[ep][path+"/enum"] = true
			case "x-elythia-formatAlso":
				out[ep][path+"/format"] = true
			}
		}
	}
	for _, ep := range root.keys {
		out[ep] = map[string]bool{}
		walk(ep, "#", root.vals[ep].(*orderedObject))
	}
	return out
}

// TestEffectiveOnlyAdds replays every corpus case ajv judged against the
// effective paramDefs: an input upstream accepts is still accepted, with the
// same defaults, and an input upstream rejects gets the same first error,
// unless the overlay widened the keyword it failed at and it now passes
// (#3865). The endpoints without an overlay behave exactly as upstream.
func TestEffectiveOnlyAdds(t *testing.T) {
	eff, err := Effective()
	require.NoError(t, err)
	paths := overlaidKeywordPaths(t, ElythiaOverlays())
	require.NotEmpty(t, paths)
	_, eps, cases := readCorpus(t)
	widened := map[string]int{}
	seen := map[string]int{}
	for _, c := range cases {
		if eps[c.ep].ParamDef != nil {
			continue // tools/paramdefspec の合成のschemaは、本家のendpointではない。
		}
		ep, ok := eff[c.ep]
		require.True(t, ok, c.ep)
		require.NoError(t, ep.CompileErr, c.ep)
		// サーバーと同じくUseNumberで読む(float64では1e400を読めない)。
		data, err := decodeInput(c.In, true)
		require.NoError(t, err)
		seen[c.ep]++
		msg := checkCase(t, ep.Schema, c, data)
		if msg == "" {
			continue
		}
		if !*c.OK && paths[c.ep][c.Path] && ep.Schema.Validate(data) == nil {
			widened[c.ep]++
			continue
		}
		t.Errorf("%s (corpus line %d): %s", c.ep, c.line, msg)
	}
	for ep := range paths {
		assert.Positive(t, seen[ep], "the corpus has no case for the overlaid %s", ep)
	}
	t.Logf("corpus inputs accepted only by the overlays: %v", widened)
}

// TestEffectiveAcceptsTheDeclaredValues pins what each overlay adds: the
// declared values pass where upstream rejects them, and a value still outside
// gets upstream's error.
func TestEffectiveAcceptsTheDeclaredValues(t *testing.T) {
	eff, err := Effective()
	require.NoError(t, err)
	up, err := Upstream()
	require.NoError(t, err)
	type rejected struct{ in, path, msg string }
	for _, tc := range []struct {
		ep       string
		accepted []string
		rejected []rejected
	}{
		{"admin/queue/clear", []string{`{"queue":"push","state":"wait"}`, `{"queue":"maintenance","state":"wait"}`, `{"queue":"plugin:x","state":"wait"}`},
			[]rejected{
				{`{"queue":"pluginx","state":"wait"}`, "#/properties/queue/enum", "must be equal to one of the allowed values"},
				{`{"queue":"plugin:A B","state":"wait"}`, "#/properties/queue/enum", "must be equal to one of the allowed values"},
			}},
		{"admin/queue/show-job", []string{`{"queue":"export","jobId":"1"}`},
			[]rejected{{`{"queue":"bogus","jobId":"1"}`, "#/properties/queue/enum", "must be equal to one of the allowed values"}}},
		{"admin/queue/pause", []string{`{"queue":"webhook"}`, `{"queue":"plugin:status"}`},
			[]rejected{{`{"queue":"plugin:"}`, "#/properties/queue/enum", "must be equal to one of the allowed values"}}},
		{"admin/queue/resume", []string{`{"queue":"plugin:status"}`}, nil},
		{"i/notifications", []string{`{"includeTypes":["importCompleted","note"]}`, `{"excludeTypes":["abuseReport"]}`},
			[]rejected{{`{"includeTypes":["bogus"]}`, "#/properties/includeTypes/items/enum", "must be equal to one of the allowed values"}}},
		{"i/notifications-grouped", []string{`{"includeTypes":["signupApplicationReceived"]}`, `{"excludeTypes":["emojiApplicationReceived"]}`}, nil},
		{"federation/instances", []string{`{"sort":"+host"}`, `{"sort":"-host"}`},
			[]rejected{{`{"sort":"+bogus"}`, "#/properties/sort/enum", "must be equal to one of the allowed values"}}},
		{"reversi/match", []string{`{"userId":"@alice"}`, `{"userId":"@alice@remote.example"}`},
			[]rejected{
				{`{"userId":"@"}`, "#/properties/userId/format", `must match format "misskey:id"`},
				{`{"userId":"a-b"}`, "#/properties/userId/format", `must match format "misskey:id"`},
			}},
		{"admin/drive/files", []string{`{"userId":"@system"}`},
			[]rejected{{`{"userId":"@other"}`, "#/properties/userId/format", `must match format "misskey:id"`}}},
	} {
		for _, in := range tc.accepted {
			var data any
			require.NoError(t, json.Unmarshal([]byte(in), &data))
			assert.Nil(t, eff[tc.ep].Schema.Validate(data), "%s %s", tc.ep, in)
			require.NoError(t, json.Unmarshal([]byte(in), &data))
			assert.NotNil(t, up[tc.ep].Schema.Validate(data), "%s %s: upstream accepts it; nothing to declare", tc.ep, in)
		}
		for _, r := range tc.rejected {
			var data any
			require.NoError(t, json.Unmarshal([]byte(r.in), &data))
			got := eff[tc.ep].Schema.Validate(data)
			if assert.NotNil(t, got, "%s %s", tc.ep, r.in) {
				assert.Equal(t, r.path, got.SchemaPath, "%s %s", tc.ep, r.in)
				assert.Equal(t, r.msg, got.Message, "%s %s", tc.ep, r.in)
			}
		}
	}
}

// Effective compiles the golden afresh: the overlays must not leak into the
// schemas Upstream shares, which the conformance with ajv relies on.
func TestEffectiveLeavesUpstreamUntouched(t *testing.T) {
	_, err := Effective()
	require.NoError(t, err)
	up, err := Upstream()
	require.NoError(t, err)
	for ep, in := range map[string]string{
		"admin/queue/clear": `{"queue":"push","state":"wait"}`,
		"admin/drive/files": `{"userId":"@system"}`,
		"admin/queue/pause": `{"queue":"plugin:x"}`,
	} {
		var data any
		require.NoError(t, json.Unmarshal([]byte(in), &data))
		assert.NotNil(t, up[ep].Schema.Validate(data), ep)
	}
}

// applyOverlays refuses an overlay that is not an addition, or that adds
// nothing, so that a declaration cannot narrow upstream's paramDef or be a
// silent no-op (#3865).
func TestApplyOverlaysRejects(t *testing.T) {
	for _, tc := range []struct {
		name, overlay, want string
	}{
		{"not an object", `[]`, "overlays must be an object"},
		{"trailing data", `{} {}`, "trailing data"},
		{"bad JSON", `{`, "overlays:"},
		{"unknown endpoint", `{"nope/x":{"properties":{}}}`, "not an upstream endpoint"},
		{"endpoint overlay not an object", `{"admin/queue/clear":[]}`, "must be an object"},
		{"empty overlay", `{"admin/queue/clear":{}}`, "empty overlay"},
		{"unsupported keyword", `{"admin/queue/clear":{"required":[]}}`, `unsupported overlay keyword "required"`},
		{"description not a string", `{"admin/queue/clear":{"description":1}}`, "description must be a string"},
		{"properties not an object", `{"admin/queue/clear":{"properties":[]}}`, "properties must be an object"},
		{"unknown property", `{"admin/queue/clear":{"properties":{"nope":{"enum":["x"]}}}}`, `upstream has no property "nope"`},
		{"property overlay not an object", `{"admin/queue/clear":{"properties":{"queue":1}}}`, "overlay must be an object"},
		{"items without items", `{"admin/queue/clear":{"properties":{"queue":{"items":{"enum":["x"]}}}}}`, "upstream has no items"},
		{"items not an object", `{"i/notifications":{"properties":{"includeTypes":{"items":1}}}}`, "overlay must be an object"},
		{"enum without enum", `{"admin/drive/files":{"properties":{"userId":{"enum":["x"]}}}}`, "upstream has no enum"},
		{"enum not an array", `{"admin/queue/clear":{"properties":{"queue":{"enum":"x"}}}}`, "enum must be a non-empty array"},
		{"enum value already allowed", `{"admin/queue/clear":{"properties":{"queue":{"enum":["deliver"]}}}}`, "already allowed"},
		{"enum value of another type", `{"admin/queue/clear":{"properties":{"queue":{"enum":[1]}}}}`, "is not of the type"},
		{"enum value an object", `{"admin/queue/clear":{"properties":{"queue":{"enum":[{}]}}}}`, "object or array value"},
		{"enumPattern without enum", `{"admin/drive/files":{"properties":{"userId":{"x-elythia-enumPattern":"^x$"}}}}`, "upstream has no enum"},
		{"enumPattern not a string", `{"admin/queue/clear":{"properties":{"queue":{"x-elythia-enumPattern":1}}}}`, "must be a string"},
		{"enumPattern bad regexp", `{"admin/queue/clear":{"properties":{"queue":{"x-elythia-enumPattern":"("}}}}`, "#/properties/queue"},
		{"enumPattern matches an allowed value", `{"admin/queue/clear":{"properties":{"queue":{"x-elythia-enumPattern":"^deliver$"}}}}`, "matches the allowed value"},
		{"enum value the enumPattern already allows", `{"admin/queue/clear":{"properties":{"queue":{"x-elythia-enumPattern":"^plugin:[a-z]+$","enum":["plugin:x"]}}}}`, "already allowed"},
		{"formatAlso without format", `{"admin/queue/clear":{"properties":{"queue":{"x-elythia-formatAlso":{"values":["x"]}}}}}`, "upstream has no format"},
		{"formatAlso empty", `{"admin/drive/files":{"properties":{"userId":{"x-elythia-formatAlso":{}}}}}`, "must be a non-empty object"},
		{"formatAlso unknown key", `{"admin/drive/files":{"properties":{"userId":{"x-elythia-formatAlso":{"nope":1}}}}}`, `unsupported x-elythia-formatAlso key "nope"`},
		{"formatAlso values not an array", `{"admin/drive/files":{"properties":{"userId":{"x-elythia-formatAlso":{"values":"x"}}}}}`, "values must be a non-empty array"},
		{"formatAlso value already matches", `{"admin/drive/files":{"properties":{"userId":{"x-elythia-formatAlso":{"values":["abc"]}}}}}`, "already matches the format"},
		{"formatAlso duplicate value", `{"admin/drive/files":{"properties":{"userId":{"x-elythia-formatAlso":{"values":["@a","@a"]}}}}}`, "distinct strings"},
		{"formatAlso pattern not a string", `{"admin/drive/files":{"properties":{"userId":{"x-elythia-formatAlso":{"pattern":1}}}}}`, "pattern must be a string"},
		{"formatAlso bad pattern", `{"admin/drive/files":{"properties":{"userId":{"x-elythia-formatAlso":{"pattern":"("}}}}}`, "#/properties/userId"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseEffective(upstreamGolden, []byte(tc.overlay))
			require.Error(t, err)
			assert.True(t, strings.Contains(err.Error(), tc.want), "got %v, want it to contain %q", err, tc.want)
		})
	}
}
