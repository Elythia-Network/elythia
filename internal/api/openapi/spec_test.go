package openapi

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/paramschema"
)

// testGolden is a golden in the format of upstream_paramdefs.json.
const testGolden = `{
"misskeyVersion": "2026.10.0",
"directRoutes": ["miauth/:session/check","signup"],
"endpoints": {
"notes/create": {"paramDef":{"type":"object","properties":{"text":{"type":"string","nullable":true}},"required":[]},"meta":{"requireCredential":true,"kind":"write:notes"},"doc":{"description":"Post a note.","tags":["notes","x"],"errors":{"noSuchRenoteTarget":{"message":"No such renote target.","code":"NO_SUCH_RENOTE_TARGET","id":"b5c90186"},"invalidParam":{"message":"Overridden.","code":"INVALID_PARAM","id":"x"}},"res":{"type":"object","properties":{"createdNote":{"type":"object","ref":"Note"}}},"limit":true}},
"notes/show": {"paramDef":{"type":"object","properties":{"noteId":{"type":"string"}},"required":["noteId"]},"meta":{"allowGet":true},"doc":{"res":{"type":"object","ref":"Note","optional":true}}},
"i": {"paramDef":{"type":"object","properties":{}},"meta":{"requireCredential":true,"secure":true},"doc":{}},
"drive/files/create": {"paramDef":{"type":"object","properties":{"name":{"type":"string"}}},"meta":{"requireFile":true},"doc":{"tags":["drive"],"res":{"type":"object","ref":"DriveFile","nullable":true}}},
"one-of": {"paramDef":{"type":"object","oneOf":[]},"meta":{},"doc":{"tags":["notes"]}},
"no-meta": {"paramDef":{"type":"object"}}
},
"refs": {
"Note": {"type":"object","properties":{"id":{"type":"string"},"reply":{"type":"object","ref":"Note","selfRef":true,"optional":true,"nullable":true}}},
"DriveFile": {"type":"object","properties":{"id":{"type":"string"}}}
}
}`

func newTestGenerator(t *testing.T) *Generator {
	t.Helper()
	g, err := New([]byte(testGolden))
	require.NoError(t, err)
	return g
}

type specJSON struct {
	OpenAPI string                               `json:"openapi"`
	Info    map[string]any                       `json:"info"`
	Ext     map[string]any                       `json:"externalDocs"`
	Servers []map[string]any                     `json:"servers"`
	Paths   map[string]map[string]map[string]any `json:"paths"`
	Comp    struct {
		Schemas         map[string]any `json:"schemas"`
		SecuritySchemes map[string]any `json:"securitySchemes"`
	} `json:"components"`
	Tags []map[string]any `json:"tags"`
}

func renderSpec(t *testing.T, g *Generator, routes ...Route) (specJSON, string) {
	t.Helper()
	b, err := g.Spec(Options{Version: "2.0.0", MisskeyVersion: "2026.10.0", RepositoryURL: "https://repo.example", APIURL: "https://example.test/api", Routes: routes})
	require.NoError(t, err)
	var s specJSON
	require.NoError(t, json.Unmarshal(b, &s))
	return s, string(b)
}

func op(t *testing.T, s specJSON, path, method string) map[string]any {
	t.Helper()
	item, ok := s.Paths[path]
	require.True(t, ok, "path %s", path)
	o, ok := item[method]
	require.True(t, ok, "%s %s", method, path)
	return o
}

func TestSpecDocument(t *testing.T) {
	s, raw := renderSpec(t, newTestGenerator(t), Route{"POST", "i"})
	assert.Equal(t, "3.1.0", s.OpenAPI)
	assert.Equal(t, "Elythia API", s.Info["title"])
	assert.Equal(t, "2.0.0 (Misskey 2026.10.0 互換)", s.Info["version"])
	assert.Contains(t, s.Info["description"], "Authorization: Bearer <token>")
	// JSON.stringifyと同じく、<や>をエスケープしない。
	assert.Contains(t, raw, "Bearer <token>")
	assert.Equal(t, "https://repo.example", s.Ext["url"])
	require.Len(t, s.Servers, 1)
	assert.Equal(t, "https://example.test/api", s.Servers[0]["url"])
	assert.Equal(t, map[string]any{"bearerAuth": map[string]any{"type": "http", "scheme": "bearer"}}, s.Comp.SecuritySchemes)
	// 本家の/api.jsonと同じく、型の定義は使われていなくても全部載せる。
	assert.Contains(t, s.Comp.Schemas, "Error")
	assert.Contains(t, s.Comp.Schemas, "Note")
	assert.Contains(t, s.Comp.Schemas, "DriveFile")
	// 項目の順は本家と同じ。
	assert.True(t, strings.HasPrefix(raw, `{"openapi":"3.1.0","info":{"version":`), raw[:60])
	assert.Less(t, strings.Index(raw, `"servers"`), strings.Index(raw, `"paths"`))
	assert.Less(t, strings.Index(raw, `"paths"`), strings.Index(raw, `"components"`))
	assert.Less(t, strings.Index(raw, `"Error"`), strings.Index(raw, `"Note"`))
	assert.Less(t, strings.Index(raw, `"Note":`), strings.Index(raw, `"DriveFile"`))
	// Elythia独自のものが無ければ、タグの一覧は出さない。
	assert.Nil(t, s.Tags)
}

func TestSpecListsOnlyServedRoutes(t *testing.T) {
	s, _ := renderSpec(t, newTestGenerator(t),
		Route{"POST", "notes/create"},
		Route{"POST", "notes/create"},
		Route{"GET", "notes/create"},
		Route{"POST", "notes/show"},
		Route{"PUT", "i"},
		Route{"POST", "signup"},
		Route{"POST", "signin"},
		Route{"POST", "miauth/:token/check"},
		Route{"POST", "*"},
		Route{"GET", "notes/*"},
		Route{"POST", ""},
	)
	paths := make([]string, 0, len(s.Paths))
	for p := range s.Paths {
		paths = append(paths, p)
	}
	// i (PUTだけ)、本家がendpointの外で登録するもの(パラメータ名が違っても)、
	// 本家の旧いpathの互換(signin)、
	// catch-allは載らない。goldenにあってもルートに無いものも載らない。
	assert.ElementsMatch(t, []string{"/notes/create", "/notes/show"}, paths)
	// ルートにあるメソッドだけを載せる。本家がallowGetでも、GETを持たなければ載らない。
	assert.Len(t, s.Paths["/notes/create"], 2)
	assert.Equal(t, "get___notes___create", op(t, s, "/notes/create", "get")["operationId"])
	assert.Equal(t, "post___notes___create", op(t, s, "/notes/create", "post")["operationId"])
	assert.Equal(t, map[string]map[string]any{"post": s.Paths["/notes/show"]["post"]}, s.Paths["/notes/show"])
}

func TestSpecElythiaOnlyEndpoints(t *testing.T) {
	s, raw := renderSpec(t, newTestGenerator(t), Route{"POST", "bubble-game/versus/create"}, Route{"GET", "bubble-game/versus/create"}, Route{"POST", "i"},
		Route{"POST", "notes/show"}, Route{"POST", "notes/create"}, Route{"POST", "drive/files/create"}, Route{"POST", "one-of"})
	o := op(t, s, "/bubble-game/versus/create", "post")
	assert.Equal(t, map[string]any{
		"operationId": "post___bubble-game___versus___create",
		"summary":     "bubble-game/versus/create",
		"description": elythiaOperationDescription,
		"tags":        []any{ElythiaTag},
	}, o)
	assert.Equal(t, "get___bubble-game___versus___create", op(t, s, "/bubble-game/versus/create", "get")["operationId"])
	// 本家互換のタグをpathsに現れた順(名前順)で並べ、独自のタグを最後に置く。
	// タグの無いもの(notes/show)と、2つ目以降のタグ(x)は並べない。
	assert.Equal(t, []map[string]any{{"name": "drive"}, {"name": "notes"}, {"name": ElythiaTag, "description": elythiaTagDescription}}, s.Tags)
	// 本家と同じく、getを先に置く。
	assert.Less(t, strings.Index(raw, `"get___bubble-game`), strings.Index(raw, `"post___bubble-game`))
	// 本家互換のものは独自のタグに入らない。
	assert.NotContains(t, op(t, s, "/i", "post"), "tags")
}

func TestSpecUpstreamOperation(t *testing.T) {
	s, raw := renderSpec(t, newTestGenerator(t), Route{"POST", "notes/create"})
	o := op(t, s, "/notes/create", "post")
	assert.Equal(t, "notes/create", o["summary"])
	assert.Equal(t, "Post a note.\n\n**Credential required**: *Yes* / **Permission**: *write:notes*", o["description"])
	assert.Equal(t, map[string]any{"description": "Source code", "url": "https://github.com/misskey-dev/misskey/blob/develop/packages/backend/src/server/api/endpoints/notes/create.ts"}, o["externalDocs"])
	assert.Equal(t, []any{"notes"}, o["tags"])
	assert.Equal(t, []any{map[string]any{"bearerAuth": []any{}}}, o["security"])
	// 空のrequiredは落とし、nullableは型の和にする。
	assert.Equal(t, map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
		"type": "object", "properties": map[string]any{"text": map[string]any{"type": []any{"string", "null"}}},
	}}}}, o["requestBody"])

	res := o["responses"].(map[string]any)
	assert.Equal(t, map[string]any{"description": "OK (with results)", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
		"type": "object", "properties": map[string]any{"createdNote": map[string]any{"$ref": "#/components/schemas/Note"}}, "required": []any{"createdNote"},
	}}}}, res["200"])
	assert.NotContains(t, res, "204")
	assert.Contains(t, res, "429")
	ex := res["400"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["examples"].(map[string]any)
	assert.Equal(t, map[string]any{"value": map[string]any{"error": map[string]any{"message": "No such renote target.", "code": "NO_SUCH_RENOTE_TARGET", "id": "b5c90186"}}}, ex["NO_SUCH_RENOTE_TARGET"])
	// errors.tsの例が、同じcodeのendpointのエラーを上書きする。
	assert.Equal(t, "Invalid param.", ex["INVALID_PARAM"].(map[string]any)["value"].(map[string]any)["error"].(map[string]any)["message"])
	// 項目の順は本家と同じで、endpointのエラーは上書きされても最初の位置に残る。
	assert.Less(t, strings.Index(raw, `"INVALID_PARAM":{"value":{"error":{"message":"Invalid param."`), strings.Index(raw, `"401":`))
	assert.Less(t, strings.Index(raw, `"NO_SUCH_RENOTE_TARGET":{`), strings.Index(raw, `"INVALID_PARAM":{`))
	for status, code := range map[string]string{"401": "CREDENTIAL_REQUIRED", "403": "AUTHENTICATION_FAILED", "418": "I_AM_AI", "429": "RATE_LIMIT_EXCEEDED", "500": "INTERNAL_ERROR"} {
		r := res[status].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)
		assert.Equal(t, map[string]any{"$ref": "#/components/schemas/Error"}, r["schema"], status)
		assert.Contains(t, r["examples"], code, status)
		assert.Len(t, r["examples"], 1, status)
	}
	assert.Contains(t, raw, `"operationId":"post___notes___create","summary"`)
}

func TestSpecOperationVariants(t *testing.T) {
	s, _ := renderSpec(t, newTestGenerator(t),
		Route{"POST", "notes/show"}, Route{"POST", "i"}, Route{"POST", "drive/files/create"},
		Route{"POST", "one-of"}, Route{"POST", "no-meta"})

	show := op(t, s, "/notes/show", "post")
	assert.Equal(t, "No description provided.\n\n**Credential required**: *No*", show["description"])
	assert.NotContains(t, show, "tags")
	assert.NotContains(t, show, "security")
	res := show["responses"].(map[string]any)
	// 応答がoptionalなら、200に加えて204も載る。
	assert.Contains(t, res, "200")
	assert.Equal(t, map[string]any{"description": "OK (without any results)"}, res["204"])
	assert.NotContains(t, res, "429")

	i := op(t, s, "/i", "post")
	assert.Equal(t, "No description provided.\n\n**Internal Endpoint**: This endpoint is an API for the misskey mainframe and is not intended for use by third parties.\n**Credential required**: *Yes*", i["description"])
	// 引数が無ければrequestBodyは無い。応答が無ければ204だけ。
	assert.NotContains(t, i, "requestBody")
	ires := i["responses"].(map[string]any)
	assert.NotContains(t, ires, "200")
	assert.Contains(t, ires, "204")

	file := op(t, s, "/drive/files/create", "post")
	assert.Equal(t, map[string]any{"required": true, "content": map[string]any{"multipart/form-data": map[string]any{"schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"file": map[string]any{"type": "string", "format": "binary", "description": "The file contents."},
		},
		"required": []any{"file"},
	}}}}, file["requestBody"])
	fres := file["responses"].(map[string]any)
	assert.Equal(t, map[string]any{"oneOf": []any{map[string]any{"$ref": "#/components/schemas/DriveFile"}, map[string]any{"type": "null"}}}, fres["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"])
	assert.Contains(t, fres, "204")

	// 本家の条件はoneOfなどが配列でありさえすれば本文がある。
	assert.Contains(t, op(t, s, "/one-of", "post"), "requestBody")
	assert.NotContains(t, op(t, s, "/no-meta", "post"), "requestBody")
	assert.Equal(t, "No description provided.\n\n**Credential required**: *No*", op(t, s, "/no-meta", "post")["description"])
}

func TestSpecFileWithoutProperties(t *testing.T) {
	g, err := New([]byte(`{"endpoints":{"up":{"paramDef":{"type":"object","required":["a"]},"meta":{"requireFile":true}}},"refs":{}}`))
	require.NoError(t, err)
	s, _ := renderSpec(t, g, Route{"POST", "up"})
	body := op(t, s, "/up", "post")["requestBody"].(map[string]any)["content"].(map[string]any)["multipart/form-data"].(map[string]any)["schema"]
	assert.Equal(t, map[string]any{
		"type":       "object",
		"required":   []any{"a", "file"},
		"properties": map[string]any{"file": map[string]any{"type": "string", "format": "binary", "description": "The file contents."}},
	}, body)
}

func TestComponentSchemas(t *testing.T) {
	s, _ := renderSpec(t, newTestGenerator(t))
	assert.Equal(t, map[string]any{
		"type":       "object",
		"properties": map[string]any{"id": map[string]any{"type": "string"}, "reply": map[string]any{"type": []any{"object", "null"}}},
		"required":   []any{"id"},
	}, s.Comp.Schemas["Note"])
	errSchema := s.Comp.Schemas["Error"].(map[string]any)
	assert.Equal(t, []any{"error"}, errSchema["required"])
	inner := errSchema["properties"].(map[string]any)["error"].(map[string]any)
	assert.Equal(t, []any{"code", "id", "message"}, inner["required"])
	assert.Equal(t, "uuid", inner["properties"].(map[string]any)["id"].(map[string]any)["format"])
	assert.Empty(t, s.Paths)
}

func TestNewErrors(t *testing.T) {
	for _, src := range []string{
		`{`,
		`[]`,
		`{"refs":{}}`,
		`{"endpoints":{}}`,
		`{"endpoints":{"a":1},"refs":{}}`,
	} {
		_, err := New([]byte(src))
		assert.Error(t, err, src)
	}
}

func TestUpstream(t *testing.T) {
	g := newTestGenerator(t)
	assert.True(t, g.Upstream("notes/create"))
	assert.False(t, g.Upstream("bubble-game/versus/create"))
	assert.False(t, g.Upstream("signup"))
}

func TestNormalizeParams(t *testing.T) {
	assert.Equal(t, "miauth/:/check", normalizeParams("miauth/:session/check"))
	assert.Equal(t, "notes/create", normalizeParams("notes/create"))
}

// 埋め込みのgoldenから作った文書が、全endpointを載せ、参照する型の定義が全部
// あることを確かめる。値が本家の/api.jsonと一致することは、本家の公式imageで
// 作った/api.jsonと突き合わせて確かめた(docs/upstream-catch-up.md)。
func TestDefaultCoversTheGolden(t *testing.T) {
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
	s, raw := renderSpec(t, g, routes...)
	assert.Len(t, s.Paths, len(eps))
	assert.Greater(t, len(eps), 400)
	assert.Greater(t, len(s.Comp.Schemas), 50)
	withRes := 0
	for name, ep := range eps {
		item := s.Paths["/"+name]
		require.NotNil(t, item, name)
		assert.Equal(t, ep.AllowGet, item["get"] != nil, name)
		if _, ok := item["post"]["responses"].(map[string]any)["200"]; ok {
			withRes++
		}
	}
	// 応答の型を取り込めていなければ、全部が204になる。
	assert.Greater(t, withRes, 250)
	for _, m := range refPattern.FindAllStringSubmatch(raw, -1) {
		assert.Contains(t, s.Comp.Schemas, m[1])
	}
	assert.Greater(t, len(refPattern.FindAllStringSubmatch(raw, -1)), 100)
	// 持ち続けないので、呼ぶたびに作り直す。
	g2, err := NewDefault()
	require.NoError(t, err)
	assert.NotSame(t, g, g2)
}

var refPattern = regexp.MustCompile(`"\$ref":"#/components/schemas/([^"]+)"`)
