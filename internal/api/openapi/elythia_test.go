package openapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/paramschema"
)

// testElythiaParamDefs and testElythiaAPIDoc are in the formats of
// elythia_paramdefs.json and elythia_apidoc.json.
const (
	testElythiaParamDefs = `{"endpoints":{
"own/typed": {"paramDef":{"type":"object","properties":{"gameId":{"type":"string","format":"misskey:id"}},"required":["gameId"]},"meta":{}},
"own/admin": {"paramDef":{"type":"object","properties":{}},"meta":{"allowGet":true}},
"own/public": {"paramDef":{"type":"object","properties":{"q":{"type":"string"}}},"meta":{}}
}}`
	testElythiaAPIDoc = `{"endpoints":{
"own/typed": {"meta":{"requireCredential":true,"kind":"write:account","secure":true},"doc":{"res":{"type":"object","properties":{"game":{"type":"object","ref":"OwnGame"},"note":{"type":"object","ref":"Note","nullable":true}}}}},
"own/admin": {"meta":{"requireCredential":true,"requireModerator":true,"kind":"read:admin:meta"},"doc":{}},
"own/public": {"meta":{},"doc":{}}
},"refs":{"OwnGame":{"type":"object","properties":{"id":{"type":"string"}}}}}`
)

func newElythiaTestGenerator(t *testing.T) *Generator {
	t.Helper()
	g := newTestGenerator(t)
	require.NoError(t, g.AddElythia([]byte(testElythiaParamDefs), []byte(testElythiaAPIDoc)))
	return g
}

func TestSpecElythiaTypedOperation(t *testing.T) {
	s, _ := renderSpec(t, newElythiaTestGenerator(t), Route{"POST", "own/typed"})
	o := op(t, s, "/own/typed", "post")
	assert.Equal(t, "post___own___typed", o["operationId"])
	assert.Equal(t, []any{ElythiaTag}, o["tags"])
	assert.NotContains(t, o, "externalDocs")
	assert.Equal(t, elythiaTypedDescription+"\n\n**Internal Endpoint**: This endpoint is an API for the misskey mainframe and is not intended for use by third parties.\n"+
		"**Credential required**: *Yes* / **Permission**: *write:account*", o["description"])
	assert.Equal(t, []any{map[string]any{"bearerAuth": []any{}}}, o["security"])
	body := o["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	assert.Equal(t, []any{"gameId"}, body["required"])
	responses := o["responses"].(map[string]any)
	res := responses["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	props := res["properties"].(map[string]any)
	assert.Equal(t, map[string]any{"$ref": "#/components/schemas/OwnGame"}, props["game"])
	assert.Equal(t, map[string]any{"oneOf": []any{map[string]any{"$ref": "#/components/schemas/Note"}, map[string]any{"type": "null"}}}, props["note"])
	assert.NotContains(t, responses, "204")
	// 独自の型は本家の型と並んでcomponentsに載る。
	assert.Contains(t, s.Comp.Schemas, "OwnGame")
	assert.Contains(t, s.Comp.Schemas, "Note")
}

func TestSpecElythiaNoContent(t *testing.T) {
	s, _ := renderSpec(t, newElythiaTestGenerator(t), Route{"GET", "own/admin"}, Route{"POST", "own/admin"}, Route{"POST", "own/public"})
	// docが空なら、本家と同じく結果を返さない。
	o := op(t, s, "/own/admin", "post")
	assert.Equal(t, elythiaTypedDescription+"\n\n**Credential required**: *Yes* / **Permission**: *read:admin:meta*", o["description"])
	responses := o["responses"].(map[string]any)
	assert.Contains(t, responses, "204")
	assert.NotContains(t, responses, "200")
	assert.Contains(t, responses, "400")
	// allowGetは引数の宣言のmetaから読む。
	assert.Equal(t, "get___own___admin", op(t, s, "/own/admin", "get")["operationId"])

	pub := op(t, s, "/own/public", "post")
	assert.Equal(t, elythiaTypedDescription+"\n\n**Credential required**: *No*", pub["description"])
	assert.Contains(t, pub["responses"], "204")
	assert.NotContains(t, pub["responses"], "200")
	assert.NotContains(t, pub, "security")
}

func TestAddElythiaErrors(t *testing.T) {
	for name, tc := range map[string]struct{ paramDefs, apiDoc string }{
		"bad paramDefs":     {`{`, testElythiaAPIDoc},
		"no paramDef list":  {`{}`, testElythiaAPIDoc},
		"bad apidoc":        {testElythiaParamDefs, `{`},
		"no apidoc list":    {testElythiaParamDefs, `{}`},
		"shadows a schema":  {testElythiaParamDefs, `{"endpoints":{},"refs":{"Note":{"type":"object"}}}`},
		"upstream endpoint": {`{"endpoints":{"i":{"paramDef":{"type":"object"}}}}`, `{"endpoints":{"i":{"meta":{}}}}`},
		"no paramDef":       {testElythiaParamDefs, `{"endpoints":{"own/other":{"meta":{}}}}`},
		"no apidoc entry":   {testElythiaParamDefs, `{"endpoints":{"own/typed":{"meta":{},"doc":{}},"own/admin":{"meta":{},"doc":{}}}}`},
		"no doc":            {testElythiaParamDefs, strings.Replace(testElythiaAPIDoc, `"own/public": {"meta":{},"doc":{}}`, `"own/public": {"meta":{}}`, 1)},
		"null doc":          {testElythiaParamDefs, strings.Replace(testElythiaAPIDoc, `"own/public": {"meta":{},"doc":{}}`, `"own/public": {"meta":{},"doc":null}`, 1)},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, newTestGenerator(t).AddElythia([]byte(tc.paramDefs), []byte(tc.apiDoc)))
		})
	}
}

// TestDefaultTypesTheElythiaEndpoints checks that NewDefault renders every
// endpoint of elythia_apidoc.json as declared, and leaves the undeclared ones
// (plugin routes) untyped.
func TestDefaultTypesTheElythiaEndpoints(t *testing.T) {
	docs, err := paramschema.ElythiaAPIDocs()
	require.NoError(t, err)
	// 抽出の下限。実在する宣言を名指しで要求する。
	require.Greater(t, len(docs.Meta), 80)
	require.True(t, docs.Meta["admin/backup/list"].Secure)
	require.Equal(t, "write:chat", docs.Meta["chat/messages/create"].Kind)

	g, err := NewDefault()
	require.NoError(t, err)
	routes := []Route{{"POST", "plugin/example/hello"}}
	for name := range docs.Meta {
		routes = append(routes, Route{"POST", name})
	}
	b, err := g.Spec(Options{Version: "x", MisskeyVersion: "x", RepositoryURL: "https://repo.example", APIURL: "https://example.test/api", Routes: routes})
	require.NoError(t, err)
	var s specJSON
	require.NoError(t, json.Unmarshal(b, &s))
	for name, m := range docs.Meta {
		o := op(t, s, "/"+name, "post")
		assert.Equal(t, []any{ElythiaTag}, o["tags"], name)
		if m.RequireCredential {
			assert.Contains(t, o, "security", name)
		} else {
			assert.NotContains(t, o, "security", name)
		}
		if m.Kind != "" {
			assert.Contains(t, o["description"], "**Permission**: *"+m.Kind+"*", name)
		}
		// 本家の"No description provided."を置き換えている。
		assert.True(t, strings.HasPrefix(toString(o["description"]), elythiaTypedDescription), name)
		// 型の無い一覧の扱い(elythiaOperation)に落ちていない。
		assert.NotEqual(t, elythiaOperationDescription, o["description"], name)
		responses, _ := o["responses"].(map[string]any)
		_, has200 := responses["200"]
		_, has204 := responses["204"]
		assert.True(t, has200 || has204, name)
	}
	assert.Equal(t, elythiaOperationDescription, op(t, s, "/plugin/example/hello", "post")["description"])

	// 分割アップロードの append は multipart で、ファイルの欄は meta.fileField の
	// "chunk" になる。本家の決まった名前の "file" は足さない (#4090)。
	body := op(t, s, "/drive/files/create-chunked/append", "post")["requestBody"].(map[string]any)["content"].(map[string]any)
	require.Contains(t, body, "multipart/form-data")
	schema := body["multipart/form-data"].(map[string]any)["schema"].(map[string]any)
	props := schema["properties"].(map[string]any)
	assert.Equal(t, map[string]any{"type": "string", "format": "binary", "description": "The file contents."}, props["chunk"])
	assert.NotContains(t, props, "file")
	assert.ElementsMatch(t, []any{"uploadId", "index", "chunk"}, schema["required"])
}
