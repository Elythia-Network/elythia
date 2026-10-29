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
		{"added properties not an object", `{"antennas/update":{"x-elythia-properties":[]}}`, "x-elythia-properties must be a non-empty object"},
		{"added properties empty", `{"antennas/update":{"x-elythia-properties":{}}}`, "x-elythia-properties must be a non-empty object"},
		{"added property of upstream", `{"antennas/update":{"x-elythia-properties":{"name":{"type":"string"}}}}`, `upstream has the property "name"`},
		{"added property inherited", `{"antennas/update":{"x-elythia-properties":{"__proto__":{"type":"string"}}}}`, "inherited by every JavaScript object"},
		{"added property not an object", `{"antennas/update":{"x-elythia-properties":{"isActive":true}}}`, "schema must be an object"},
		{"added property with a default", `{"antennas/update":{"x-elythia-properties":{"isActive":{"type":"boolean","default":true}}}}`, "cannot have a default"},
		{"added property with a nested default", `{"antennas/update":{"x-elythia-properties":{"x":{"type":"object","properties":{"y":{"type":"string","default":"z"}}}}}}`, "cannot have a default"},
		{"added property with a default in an array", `{"antennas/update":{"x-elythia-properties":{"x":{"anyOf":[{"type":"string","default":"z"}]}}}}`, "cannot have a default"},
		{"added property bad schema", `{"antennas/update":{"x-elythia-properties":{"isActive":{"type":"bool"}}}}`, "unsupported type"},
		{"added property of upstream in anyOf", `{"users/show":{"x-elythia-properties":{"userId":{"type":"integer"}}}}`, `upstream has the property "userId"`},
		{"added properties on a string", `{"admin/drive/files":{"properties":{"userId":{"x-elythia-properties":{"x":{"type":"string"}}}}}}`, "x-elythia-properties on a string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseEffective(upstreamGolden, []byte(tc.overlay))
			require.Error(t, err)
			assert.True(t, strings.Contains(err.Error(), tc.want), "got %v, want it to contain %q", err, tc.want)
		})
	}
}

// x-elythia-properties adds a property upstream does not have: a value of
// another type is rejected with upstream's form of error at the added path,
// and upstream's own errors still come first (#3867).
func TestOverlayAddsProperties(t *testing.T) {
	eps, err := parseEffective(upstreamGolden, []byte(`{"antennas/update":{"x-elythia-properties":{"isActive":{"type":"boolean","nullable":true}}}}`))
	require.NoError(t, err)
	up, err := Upstream()
	require.NoError(t, err)
	s := eps["antennas/update"].Schema
	validate := func(in string) *Error {
		t.Helper()
		var data any
		require.NoError(t, json.Unmarshal([]byte(in), &data))
		return s.Validate(data)
	}
	const ok = `"antennaId":"abc","name":"a","src":"all","keywords":[["a"]],"excludeKeywords":[],"users":[],"caseSensitive":false,"withReplies":false,"withFile":false`
	assert.Nil(t, validate(`{`+ok+`,"isActive":true}`))
	assert.Nil(t, validate(`{`+ok+`,"isActive":null}`))
	assert.Nil(t, validate(`{`+ok+`}`))
	got := validate(`{` + ok + `,"isActive":"yes"}`)
	if assert.NotNil(t, got) {
		assert.Equal(t, "#/properties/isActive/type", got.SchemaPath)
		assert.Equal(t, "must be boolean", got.Message)
	}
	var data any
	require.NoError(t, json.Unmarshal([]byte(`{`+ok+`,"isActive":"yes"}`), &data))
	assert.Nil(t, up["antennas/update"].Schema.Validate(data), "upstream accepts the unknown property")
	// 本家の項目の誤りが先に返る。
	got = validate(`{"antennaId":"a-b","isActive":"yes"}`)
	if assert.NotNil(t, got) {
		assert.Equal(t, "#/properties/antennaId/format", got.SchemaPath)
	}
	// 本家の paramDef に object の組の keyword が無い endpoint にも足せる。
	eps, err = parseEffective(upstreamGolden, []byte(`{"admin/captcha/current":{"x-elythia-properties":{"x":{"type":"string"}}}}`))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(`{"x":1}`), &data))
	got = eps["admin/captcha/current"].Schema.Validate(data)
	if assert.NotNil(t, got) {
		assert.Equal(t, "#/properties/x/type", got.SchemaPath)
	}
}

// TestEffectiveDeclaresTheAddedProperties pins the properties Elythia adds to
// upstream endpoints (#3867): on top of an input upstream accepts (the first
// accepted corpus input of the endpoint), each valid value passes and each
// value outside the declaration gets upstream's form of error at the added
// property, where upstream passed it as an unknown property.
func TestEffectiveDeclaresTheAddedProperties(t *testing.T) {
	eff, err := Effective()
	require.NoError(t, err)
	up, err := Upstream()
	require.NoError(t, err)
	_, eps, cases := readCorpus(t)
	base := map[string]json.RawMessage{}
	for _, c := range cases {
		if _, ok := base[c.ep]; !ok && *c.OK && eps[c.ep].ParamDef == nil && bytes.HasPrefix(bytes.TrimSpace(c.In), []byte("{")) {
			base[c.ep] = c.In
		}
	}
	type bad struct {
		v         any
		path, msg string
	}
	long := strings.Repeat("a", 129)
	for _, tc := range []struct {
		ep, prop string
		good     []any
		bad      []bad
	}{
		{"admin/abuse-user-reports", "resolved", []any{true, false, nil}, []bad{{"yes", "type", "must be boolean"}}},
		{"admin/abuse-user-reports", "reportId", []any{"abc123"}, []bad{{"", "format", `must match format "misskey:id"`}, {nil, "type", "must be string"}}},
		{"admin/accounts/create", "federationStatus", []any{"on", "off", "pending", "", nil}, []bad{{true, "type", "must be string"}, {"unknown", "enum", "must be equal to one of the allowed values"}}},
		{"admin/emoji/copy", "name", []any{"a_b", nil, strings.Repeat("a", 128)}, []bad{{"a b", "pattern", `must match pattern "^[a-zA-Z0-9_]+$"`}, {long, "maxLength", "must NOT have more than 128 characters"}}},
		{"admin/emoji/copy", "category", []any{"", "face", nil}, []bad{{1.0, "type", "must be string"}}},
		{"admin/emoji/copy", "aliases", []any{[]any{}, []any{"a"}, nil}, []bad{{"a", "type", "must be array"}}},
		{"admin/emoji/copy", "license", []any{"", nil}, []bad{{true, "type", "must be string"}}},
		{"admin/emoji/copy", "isSensitive", []any{true, nil}, []bad{{"true", "type", "must be boolean"}}},
		{"admin/emoji/list", "category", []any{"face", "", nil}, []bad{{1.0, "type", "must be string"}}},
		{"admin/queue/jobs", "limit", []any{1.0, 100.0}, []bad{{0.0, "minimum", "must be >= 1"}, {101.0, "maximum", "must be <= 100"}, {1.5, "type", "must be integer"}, {nil, "type", "must be integer"}}},
		{"admin/queue/jobs", "page", []any{1.0, 1000.0}, []bad{{0.0, "minimum", "must be >= 1"}}},
		{"antennas/update", "isActive", []any{true, nil}, []bad{{"yes", "type", "must be boolean"}}},
		{"chat/history", "untilId", []any{"abc123"}, []bad{{"", "format", `must match format "misskey:id"`}}},
		{"pages/create", "visibility", []any{"public", "private"}, []bad{{"", "enum", "must be equal to one of the allowed values"}, {nil, "type", "must be string"}}},
		{"pages/update", "visibility", []any{"public", "private", nil}, []bad{{"secret", "enum", "must be equal to one of the allowed values"}}},
		{"i/regenerate-token", "token", []any{"123456", nil}, []bad{{123456.0, "type", "must be string"}}},
		{"admin/show-user", "withSignins", []any{false, nil}, []bad{{"false", "type", "must be boolean"}}},
		{"notes", "noteIds", []any{[]any{}, []any{"abc123"}}, []bad{{[]any{"a-b"}, "items/format", `must match format "misskey:id"`}, {nil, "type", "must be array"}}},
		{"admin/drive/files", "host", []any{"remote.example", nil}, []bad{{1.0, "type", "must be string"}}},
		{"i/move", "password", []any{"", "p", nil}, []bad{{1.0, "type", "must be string"}}},
		{"notes/global-timeline", "allowPartial", []any{true, false}, []bad{{"true", "type", "must be boolean"}, {nil, "type", "must be boolean"}}},
		{"federation/instances", "untilId", []any{"abc123"}, []bad{{"a-b", "format", `must match format "misskey:id"`}}},
		{"federation/instances", "sinceId", []any{"abc123"}, []bad{{1.0, "type", "must be string"}}},
		{"federation/instances", "sinceDate", []any{0.0, 1e12}, []bad{{1.5, "type", "must be integer"}}},
		{"federation/instances", "untilDate", []any{1e12}, []bad{{"1", "type", "must be integer"}}},
	} {
		raw, ok := base[tc.ep]
		require.True(t, ok, "the corpus has no accepted object input for %s", tc.ep)
		with := func(v any) map[string]any {
			var m map[string]any
			require.NoError(t, json.Unmarshal(raw, &m))
			m[tc.prop] = v
			return m
		}
		for _, v := range tc.good {
			assert.Nil(t, eff[tc.ep].Schema.Validate(with(v)), "%s %s=%v", tc.ep, tc.prop, v)
		}
		for _, b := range tc.bad {
			assert.Nil(t, up[tc.ep].Schema.Validate(with(b.v)), "%s %s=%v: upstream rejects it already", tc.ep, tc.prop, b.v)
			got := eff[tc.ep].Schema.Validate(with(b.v))
			if assert.NotNil(t, got, "%s %s=%v", tc.ep, tc.prop, b.v) {
				assert.Equal(t, "#/properties/"+tc.prop+"/"+b.path, got.SchemaPath, "%s %s=%v", tc.ep, tc.prop, b.v)
				assert.Equal(t, b.msg, got.Message, "%s %s=%v", tc.ep, tc.prop, b.v)
			}
		}
	}
	// noteIds は notes/show-partial-bulk と同じく 100 件まで。
	ids := make([]any, 101)
	for i := range ids {
		ids[i] = "abc"
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal(base["notes"], &m))
	m["noteIds"] = ids
	got := eff["notes"].Schema.Validate(m)
	if assert.NotNil(t, got) {
		assert.Equal(t, "#/properties/noteIds/maxItems", got.SchemaPath)
	}
}

// A property named "default" inside an added object schema is a name, not the
// keyword, so checkNoDefault lets it through (#3867).
func TestOverlayAddsAPropertyNamedDefault(t *testing.T) {
	_, err := parseEffective(upstreamGolden, []byte(`{"antennas/update":{"x-elythia-properties":{"x":{"type":"object","properties":{"default":{"type":"string"}}}}}}`))
	require.NoError(t, err)
}

// A name upstream only lists in required (no properties entry) is accepted
// with any value upstream, so it cannot be added either (#3867). The golden
// has no such name now; a synthetic one pins it.
func TestOverlayRefusesAPropertyOnlyInRequired(t *testing.T) {
	golden := []byte(`{"endpoints":{"x/y":{"paramDef":{"type":"object","properties":{"b":{"type":"string"}},"required":["a"]},"meta":{}}}}`)
	_, err := parseEffective(golden, []byte(`{"x/y":{"x-elythia-properties":{"a":{"type":"integer"}}}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `upstream has the property "a"`)
	_, err = parseEffective(golden, []byte(`{"x/y":{"x-elythia-properties":{"c":{"type":"integer"}}}}`))
	require.NoError(t, err)
}
