package elythiajs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/api/paramschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGeneratedFileIsCurrent keeps the checked-in type checks in step with
// elythia_paramdefs.json: a declaration changed without make
// elythia-js-paramdefs would leave elythia-js checking the old one.
func TestGeneratedFileIsCurrent(t *testing.T) {
	want, err := Render(paramschema.ElythiaParamDefs())
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join("..", "..", "..", "..", filepath.FromSlash(Path)))
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got), "%s is stale; run make elythia-js-paramdefs", Path)
	// 宣言の件数だけ検査の組があること(何も書かない生成で緑にならないため)。
	// anyOfを持つ宣言は、分岐ごとに2つと、分岐で全員を覆う1つになる。
	var defs struct {
		Endpoints map[string]struct {
			ParamDef struct {
				AnyOf []json.RawMessage `json:"anyOf"`
			} `json:"paramDef"`
		} `json:"endpoints"`
	}
	require.NoError(t, json.Unmarshal(paramschema.ElythiaParamDefs(), &defs))
	checks := 0
	for _, ep := range defs.Endpoints {
		checks += 2
		if n := len(ep.ParamDef.AnyOf); n > 0 {
			checks += 2*n - 1
		}
	}
	assert.Equal(t, checks, strings.Count(string(got), "\tCheck<"))
}

func TestRender(t *testing.T) {
	b, err := Render([]byte(`{"endpoints":{
		"b/x":{"paramDef":{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"integer"}},"required":["z","a"]},"meta":{}},
		"a/y":{"paramDef":{"type":"object","properties":{},"required":[]},"meta":{}}
	}}`))
	require.NoError(t, err)
	s := string(b)
	assert.True(t, strings.HasPrefix(s, header+checksHeader), "the file starts with the header, without the types for anyOf")
	assert.True(t, strings.HasSuffix(s, "\tCheck<Equal<Keys<Req<'b/x'>>, 'a' | 'z'>>,\n\tCheck<Equal<RequiredKeys<Req<'b/x'>>, 'a' | 'z'>>,\n];\n"), s)
	// 名前の順に並べ、項目の無いものはneverにする。
	assert.Less(t, strings.Index(s, "'a/y'"), strings.Index(s, "'b/x'"))
	assert.Contains(t, s, "\tCheck<Equal<Keys<Req<'a/y'>>, never>>,\n\tCheck<Equal<RequiredKeys<Req<'a/y'>>, never>>,\n")
}

// 分岐ごとに必須を分けるanyOf(本家のdrive/files/showと同じ書き方)は、union型と
// 突き合わせる(#3872)。
func TestRenderAnyOf(t *testing.T) {
	b, err := Render([]byte(`{"endpoints":{
		"c/z":{"paramDef":{"type":"object","properties":{"to":{"type":"string"},"text":{"type":"string"}},"required":["text"],
			"anyOf":[{"required":["to"]},{"type":"object","properties":{"room":{"type":"string"}},"required":["room"]}]},"meta":{}}
	}}`))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(b), header+anyOfHeader+checksHeader), "the types for anyOf follow the header")
	assert.True(t, strings.HasSuffix(string(b), strings.Join([]string{
		"\tCheck<Equal<Keys<Member<Req<'c/z'>, 'to'>>, 'text' | 'to'>>,",
		"\tCheck<Equal<RequiredKeys<Member<Req<'c/z'>, 'to'>>, 'text' | 'to'>>,",
		"\tCheck<Equal<Keys<Member<Req<'c/z'>, 'room'>>, 'room' | 'text' | 'to'>>,",
		"\tCheck<Equal<RequiredKeys<Member<Req<'c/z'>, 'room'>>, 'room' | 'text'>>,",
		"\tCheck<Equal<Req<'c/z'>, Member<Req<'c/z'>, 'to'> | Member<Req<'c/z'>, 'room'>>>,",
		"];\n",
	}, "\n")), string(b))
}

func TestRenderRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, defs, want string
	}{
		{"not JSON", `{`, "elythiajs:"},
		{"no endpoints", `{"endpoints":{}}`, "no endpoints"},
		{"required without a property", `{"endpoints":{"a":{"paramDef":{"properties":{"x":{}},"required":["y"]}}}}`, "required y is not a property"},
		{"unsupported keyword", `{"endpoints":{"a":{"paramDef":{"properties":{},"oneOf":[]}}}}`, "a: oneOf is not supported"},
		{"anyOf in a branch", `{"endpoints":{"a":{"paramDef":{"anyOf":[{"anyOf":[]}]}}}}`, "a: anyOf/0: anyOf is not supported"},
		{"malformed properties", `{"endpoints":{"a":{"paramDef":{"properties":[]}}}}`, "a: properties:"},
		{"malformed anyOf", `{"endpoints":{"a":{"paramDef":{"anyOf":{}}}}}`, "a: anyOf:"},
		{"branch requiring nothing", `{"endpoints":{"a":{"paramDef":{"properties":{"x":{}},"anyOf":[{"required":[]}]}}}}`, "a: anyOf/0 requires nothing"},
		{"branch required without a property", `{"endpoints":{"a":{"paramDef":{"properties":{"x":{}},"anyOf":[{"required":["y"]}]}}}}`, "a: anyOf/0: required y is not a property"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Render([]byte(tc.defs))
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"admin/ip/accounts": `'admin/ip/accounts'`,
		`it's`:              `'it\'s'`,
		`say "hi"`:          `'say "hi"'`,
		`back\slash`:        `'back\\slash'`,
		"line\nbreak":       `'line\nbreak'`,
	} {
		assert.Equal(t, want, quote(in), in)
	}
}
