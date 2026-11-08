package paramschema

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAPIDocParamDefs = `{"endpoints":{"own/a":{"paramDef":{"type":"object"}},"own/b":{"paramDef":{"type":"object"}}}}`

func TestParseElythiaAPIDoc(t *testing.T) {
	d, err := parseElythiaAPIDoc([]byte(`{"endpoints":{
"own/a":{"meta":{"requireCredential":true,"requireAdmin":true,"secure":true,"prohibitMoved":true,"requiredRolePolicy":"canManageCustomEmojis","kind":"write:account"},"doc":{"res":{"type":"array","items":{"type":"object","ref":"Own"}}}},
"own/b":{"meta":{}}
},"refs":{"Own":{"type":"object"}}}`), []byte(testAPIDocParamDefs))
	require.NoError(t, err)
	assert.Equal(t, APIMeta{RequireCredential: true, RequireAdmin: true, Secure: true, ProhibitMoved: true, RequiredRolePolicy: "canManageCustomEmojis", Kind: "write:account"}, d.Meta["own/a"])
	assert.Equal(t, APIMeta{}, d.Meta["own/b"])
	// docの無いものだけが、応答の型の宣言を待つものになる。
	assert.Equal(t, []string{"own/b"}, d.Pending)
}

func TestParseElythiaAPIDocRefuses(t *testing.T) {
	entry := func(meta, doc string) string {
		s := `{"endpoints":{"own/b":{"meta":{}},"own/a":{"meta":` + meta
		if doc != "" {
			s += `,"doc":` + doc
		}
		return s + `}},"refs":{"Own":{"type":"object"}}}`
	}
	for name, b := range map[string]string{
		"not json":                 `{`,
		"trailing data":            `{"endpoints":{"own/a":{"meta":{}},"own/b":{"meta":{}}}} {}`,
		"no endpoints":             `{"endpoints":{}}`,
		"unknown top-level field":  `{"endpoints":{"own/a":{"meta":{}},"own/b":{"meta":{}}},"x":1}`,
		"unknown meta field":       entry(`{"limit":true}`, ""),
		"unknown doc field":        entry(`{}`, `{"errors":{}}`),
		"unknown kind":             entry(`{"requireCredential":true,"kind":"write:everything"}`, ""),
		"admin and moderator":      entry(`{"requireCredential":true,"requireAdmin":true,"requireModerator":true}`, ""),
		"admin without credential": entry(`{"requireAdmin":true}`, ""),
		"moderator w/o credential": entry(`{"requireModerator":true}`, ""),
		"policy w/o credential":    entry(`{"requiredRolePolicy":"canManageCustomEmojis"}`, ""),
		"moved w/o credential":     entry(`{"prohibitMoved":true}`, ""),
		"secure w/o credential":    entry(`{"secure":true}`, ""),
		"unknown ref":              entry(`{}`, `{"res":{"type":"object","properties":{"x":{"type":"object","ref":"Missing"}}}}`),
		"unknown ref in array":     entry(`{}`, `{"res":{"anyOf":[{"type":"object","ref":"Missing"}]}}`),
		"unknown policy":           entry(`{"requireCredential":true,"requiredRolePolicy":"canFly"}`, ""),
		"non-boolean policy":       entry(`{"requireCredential":true,"requiredRolePolicy":"driveCapacityMb"}`, ""),
		"null doc":                 entry(`{}`, `null`),
		"null res":                 entry(`{}`, `{"res":null}`),
		"unknown ref in a ref":     `{"endpoints":{"own/a":{"meta":{}},"own/b":{"meta":{}}},"refs":{"Own":{"type":"object","properties":{"x":{"type":"object","ref":"Missing"}}}}}`,
		"null ref":                 `{"endpoints":{"own/a":{"meta":{}},"own/b":{"meta":{}}},"refs":{"Own":null}}`,
		"ref named like upstream":  `{"endpoints":{"own/a":{"meta":{}},"own/b":{"meta":{}}},"refs":{"UserLite":{"type":"object"}}}`,
		"ref named Error":          `{"endpoints":{"own/a":{"meta":{}},"own/b":{"meta":{}}},"refs":{"Error":{"type":"object"}}}`,
		"missing an endpoint":      `{"endpoints":{"own/a":{"meta":{}}}}`,
		"extra endpoint":           `{"endpoints":{"own/a":{"meta":{}},"own/b":{"meta":{}},"own/c":{"meta":{}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseElythiaAPIDoc([]byte(b), []byte(testAPIDocParamDefs))
			assert.Error(t, err)
		})
	}
	_, err := parseElythiaAPIDoc([]byte(`{"endpoints":{"own/a":{"meta":{}}}}`), []byte(`{`))
	assert.Error(t, err, "paramDefs that do not parse")
}

func TestParseElythiaAPIDocAcceptsRefs(t *testing.T) {
	// 本家の型と、独自の型の中から別の独自の型を指すもの。
	_, err := parseElythiaAPIDoc([]byte(`{"endpoints":{"own/a":{"meta":{},"doc":{"res":{"type":"object","ref":"UserLite"}}},"own/b":{"meta":{},"doc":{"res":{"type":"object","ref":"Own"}}}},`+
		`"refs":{"Own":{"type":"object","properties":{"inner":{"type":"object","ref":"OwnInner"},"user":{"type":"object","ref":"UserLite"}}},"OwnInner":{"type":"object"}}}`), []byte(testAPIDocParamDefs))
	assert.NoError(t, err)
}

// TestElythiaAPIDocsMatchParamDefs checks the embedded file: it parses, and
// has an entry for exactly the endpoints of elythia_paramdefs.json.
func TestElythiaAPIDocsMatchParamDefs(t *testing.T) {
	d, err := ElythiaAPIDocs()
	require.NoError(t, err)
	var p struct {
		Endpoints map[string]json.RawMessage `json:"endpoints"`
	}
	require.NoError(t, json.Unmarshal(ElythiaParamDefs(), &p))
	require.Greater(t, len(p.Endpoints), 80)
	assert.Len(t, d.Meta, len(p.Endpoints))
	for name := range p.Endpoints {
		assert.Contains(t, d.Meta, name)
	}
}
