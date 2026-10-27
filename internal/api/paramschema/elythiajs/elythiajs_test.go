package elythiajs

import (
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
	assert.Equal(t, 2*strings.Count(string(paramschema.ElythiaParamDefs()), `"paramDef":`), strings.Count(string(got), "\tCheck<"))
}

func TestRender(t *testing.T) {
	b, err := Render([]byte(`{"endpoints":{
		"b/x":{"paramDef":{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"integer"}},"required":["z","a"]},"meta":{}},
		"a/y":{"paramDef":{"type":"object","properties":{},"required":[]},"meta":{}}
	}}`))
	require.NoError(t, err)
	s := string(b)
	assert.True(t, strings.HasPrefix(s, header), "the file starts with the header")
	assert.True(t, strings.HasSuffix(s, "\tCheck<Equal<Keys<Req<'b/x'>>, 'a' | 'z'>>,\n\tCheck<Equal<RequiredKeys<Req<'b/x'>>, 'a' | 'z'>>,\n];\n"), s)
	// 名前の順に並べ、項目の無いものはneverにする。
	assert.Less(t, strings.Index(s, "'a/y'"), strings.Index(s, "'b/x'"))
	assert.Contains(t, s, "\tCheck<Equal<Keys<Req<'a/y'>>, never>>,\n\tCheck<Equal<RequiredKeys<Req<'a/y'>>, never>>,\n")
}

func TestRenderRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, defs, want string
	}{
		{"not JSON", `{`, "elythiajs:"},
		{"no endpoints", `{"endpoints":{}}`, "no endpoints"},
		{"required without a property", `{"endpoints":{"a":{"paramDef":{"properties":{"x":{}},"required":["y"]}}}}`, "required y is not a property"},
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
