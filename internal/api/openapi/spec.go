// Package openapi builds the OpenAPI document served at /api.json.
//
// The upstream part is a port of upstream Misskey's
// server/api/openapi/gen-spec.ts over the embedded golden of
// internal/api/paramschema (paramDef, meta.res, meta.errors and the packed
// schemas), restricted to the endpoints Elythia actually serves. Endpoints
// only Elythia has are listed under their own tag without parameter or
// response types.
package openapi

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/elythia-network/elythia/internal/api/paramschema"
)

// ElythiaTag is the tag of the endpoints upstream Misskey does not have.
const ElythiaTag = "Elythia"

// elythiaTagDescription and elythiaOperationDescription describe the
// endpoints only Elythia has.
//
// 型を付けていないことを、読む人がタグと各エンドポイントの両方で分かるようにする。
const (
	elythiaTagDescription       = "Elythia独自のエンドポイント。引数と応答の型はまだ載せていません。"
	elythiaOperationDescription = "Elythia独自のエンドポイントです。引数と応答の型は、まだ載せていません。"
)

// Route is one endpoint Elythia serves under /api.
type Route struct {
	// Method is the HTTP method ("GET", "POST", ...). Only GET and POST are
	// documented, like upstream.
	Method string
	// Name is the path below /api/ ("notes/create").
	Name string
}

// Options are the instance-dependent parts of the document.
type Options struct {
	// Version is Elythia's version.
	Version string
	// MisskeyVersion is the Misskey version Elythia is compatible with.
	MisskeyVersion string
	// RepositoryURL is the externalDocs link of the document.
	RepositoryURL string
	// APIURL is the base URL of the API ("https://example.com/api").
	APIURL string
	// Routes are the endpoints to list. Anything else is left out.
	Routes []Route
}

// Generator holds the upstream part of the document, converted once from the
// golden.
type Generator struct {
	// operations are the operations of upstream's gen-spec.ts before the
	// operationId gets its method prefix, by endpoint name.
	operations map[string]*object
	// direct are upstream's routes under /api that are not endpoints
	// (ApiServerService.ts), with path parameters normalized.
	direct  map[string]bool
	schemas *object
}

// legacyRoutes are routes Elythia keeps under /api only for clients of older
// upstream versions. Like upstream's direct routes they are left out.
//
// signinは本家の旧いpath(signin-flowより前)の互換で、本家の今の/api.jsonにも
// 無い。Elythia独自のエンドポイントとして載せると取り違える。
var legacyRoutes = map[string]bool{
	"signin": true,
}

// NewDefault builds a Generator from the embedded golden.
//
// **結果を持ち続けない。** 変換済みのGeneratorは十数MBあり、/api.jsonの本文
// (約1.3MB)を作ったら要らない。呼び出し側は本文だけを残し、Generatorは捨てる
// (internal/serverのapiSpecCache)。goldenの解析はここで別に行うので、
// paramschema.Upstreamが持つ解析の結果には触れない。
func NewDefault() (*Generator, error) {
	return New(paramschema.UpstreamGolden())
}

// New builds a Generator from a golden in the format of
// internal/api/paramschema/upstream_paramdefs.json.
func New(golden []byte) (*Generator, error) {
	root, err := decode(golden)
	if err != nil {
		return nil, err
	}
	g, ok := root.(*object)
	if !ok {
		return nil, errors.New("openapi: golden is not an object")
	}
	eps, ok := field(g, "endpoints").(*object)
	if !ok {
		return nil, errors.New("openapi: golden has no endpoints")
	}
	refs, ok := field(g, "refs").(*object)
	if !ok {
		return nil, errors.New("openapi: golden has no refs")
	}
	gen := &Generator{
		operations: make(map[string]*object, len(eps.keys)),
		direct:     map[string]bool{},
		schemas:    componentSchemas(refs),
	}
	directs, _ := field(g, "directRoutes").([]any)
	for _, d := range directs {
		gen.direct[normalizeParams(toString(d))] = true
	}
	for _, name := range eps.keys {
		ep, ok := eps.vals[name].(*object)
		if !ok {
			return nil, fmt.Errorf("openapi: golden endpoint %s is not an object", name)
		}
		gen.operations[name] = operation(name, ep)
	}
	return gen, nil
}

// Upstream reports whether name is an upstream endpoint of the golden.
func (g *Generator) Upstream(name string) bool {
	_, ok := g.operations[name]
	return ok
}

// Spec renders the document for the given routes.
//
// 載せるのは、渡されたrouteにあるエンドポイントとメソッドだけ。本家にあっても
// Elythiaが持たないものは載せない。本家のendpoint-list.tsの外で登録される
// route(signupなど)と、本家の旧いpathの互換(legacyRoutes)は、本家の
// /api.jsonにも無いので載せない。
func (g *Generator) Spec(opts Options) ([]byte, error) {
	methods := map[string][]string{}
	for _, r := range opts.Routes {
		m := strings.ToLower(r.Method)
		if m != "get" && m != "post" {
			continue
		}
		if r.Name == "" || strings.Contains(r.Name, "*") || g.direct[normalizeParams(r.Name)] || legacyRoutes[r.Name] {
			continue
		}
		methods[r.Name] = append(methods[r.Name], m)
	}
	names := make([]string, 0, len(methods))
	for n := range methods {
		names = append(names, n)
	}
	sort.Strings(names)

	paths := newObject()
	hasElythia := false
	// 本家互換のタグを、pathsに初めて現れた順で並べる(Scalarが見出しを並べる順)。
	var upstreamTags []string
	for _, name := range names {
		base, upstream := g.operations[name]
		if !upstream {
			base = elythiaOperation(name)
			hasElythia = true
		} else if tags, ok := field(base, "tags").([]any); ok {
			if t := toString(tags[0]); !slices.Contains(upstreamTags, t) {
				upstreamTags = append(upstreamTags, t)
			}
		}
		item := newObject()
		// 本家と同じく、getを先に置く。
		for _, m := range []string{"get", "post"} {
			if !slices.Contains(methods[name], m) {
				continue
			}
			op := clone(base).(*object)
			op.set("operationId", m+"___"+toString(field(base, "operationId")))
			item.set(m, op)
		}
		paths.set("/"+name, item)
	}

	doc := newObject()
	doc.set("openapi", "3.1.0")
	info := newObject()
	info.set("version", fmt.Sprintf("%s (Misskey %s 互換)", opts.Version, opts.MisskeyVersion))
	info.set("title", "Elythia API")
	info.set("description", "ElythiaのAPIです。本家Misskeyと互換のエンドポイントは、引数と応答の型つきで載せています。"+
		"Elythia独自のエンドポイントは「"+ElythiaTag+"」のタグにまとめています。\n\n"+
		"認証が要るエンドポイントは、本文の`i`かヘッダーの`Authorization: Bearer <token>`でアクセストークンを渡します。")
	doc.set("info", info)
	ext := newObject()
	ext.set("description", "Repository")
	ext.set("url", opts.RepositoryURL)
	doc.set("externalDocs", ext)
	server := newObject()
	server.set("url", opts.APIURL)
	doc.set("servers", []any{server})
	doc.set("paths", paths)
	components := newObject()
	components.set("schemas", g.schemas)
	bearer := newObject()
	bearer.set("type", "http")
	bearer.set("scheme", "bearer")
	sec := newObject()
	sec.set("bearerAuth", bearer)
	components.set("securitySchemes", sec)
	doc.set("components", components)
	// Elythia独自のタグには説明を付け、本家互換のタグの後に置く。Scalarは
	// 一覧にあるタグを先に並べるので、独自のタグだけを書くと先頭に来てしまう。
	// 独自のものが無ければ、本家と同じくタグの一覧を出さない。
	if hasElythia {
		tags := make([]any, 0, len(upstreamTags)+1)
		for _, t := range upstreamTags {
			o := newObject()
			o.set("name", t)
			tags = append(tags, o)
		}
		tag := newObject()
		tag.set("name", ElythiaTag)
		tag.set("description", elythiaTagDescription)
		doc.set("tags", append(tags, tag))
	}
	return marshalValue(doc)
}

// normalizeParams replaces each path parameter with ":" so that routes that
// differ only in parameter names compare equal.
func normalizeParams(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") {
			parts[i] = ":"
		}
	}
	return strings.Join(parts, "/")
}

// operationID is upstream's operationId before the method prefix.
// 本家の注記どおり、operationIdにスラッシュは使えない。
func operationID(name string) string {
	return strings.ReplaceAll(name, "/", "___")
}

// elythiaOperation is the operation of an endpoint only Elythia has.
func elythiaOperation(name string) *object {
	op := newObject()
	op.set("operationId", operationID(name))
	op.set("summary", name)
	op.set("description", elythiaOperationDescription)
	op.set("tags", []any{ElythiaTag})
	return op
}

// componentSchemas is getSchemas of upstream's schemas.ts with
// includeSelfRef = false: the Error schema followed by every packed schema.
func componentSchemas(refs *object) *object {
	out := newObject()
	out.set("Error", errorSchema())
	for _, k := range refs.keys {
		out.set(k, convertSchema(refs.vals[k], kindRes))
	}
	return out
}

func errorSchema() *object {
	str := func(desc string, format string) *object {
		o := newObject()
		o.set("type", "string")
		if format != "" {
			o.set("format", format)
		}
		o.set("description", desc)
		return o
	}
	props := newObject()
	props.set("code", str("An error code. Unique within the endpoint.", ""))
	props.set("message", str("An error message.", ""))
	props.set("id", str("An error ID. This ID is static.", "uuid"))
	inner := newObject()
	inner.set("type", "object")
	inner.set("description", "An error object.")
	inner.set("properties", props)
	inner.set("required", []any{"code", "id", "message"})
	outerProps := newObject()
	outerProps.set("error", inner)
	o := newObject()
	o.set("type", "object")
	o.set("properties", outerProps)
	o.set("required", []any{"error"})
	return o
}

// basicError is one entry of upstream's server/api/openapi/errors.ts.
type basicError struct {
	status, code, message, id string
}

var basicErrors = []basicError{
	{"400", "INVALID_PARAM", "Invalid param.", "3d81ceae-475f-4600-b2a8-2bc116157532"},
	{"401", "CREDENTIAL_REQUIRED", "Credential required.", "1384574d-a912-4b81-8601-c7b1c4085df1"},
	{"403", "AUTHENTICATION_FAILED", "Authentication failed. Please ensure your token is correct.", "b0a7f5f8-dc2f-4171-b91f-de88ad238e14"},
	{"418", "I_AM_AI", "You sent a request to Ai-chan, Misskey's showgirl, instead of the server.", "60c46cd1-f23a-46b1-bebe-5d2b73951a84"},
	{"429", "RATE_LIMIT_EXCEEDED", "Rate limit exceeded. Please try again later.", "d5826d14-3982-4d2e-8011-b9e9f02499ef"},
	{"500", "INTERNAL_ERROR", "Internal error occurred. Please contact us if the error persists.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"},
}

// basicExamples returns the examples of errors.ts for status, added to ex.
func basicExamples(status string, ex *object) *object {
	for _, e := range basicErrors {
		if e.status != status {
			continue
		}
		er := newObject()
		er.set("message", e.message)
		er.set("code", e.code)
		er.set("id", e.id)
		value := newObject()
		value.set("error", er)
		item := newObject()
		item.set("value", value)
		ex.set(e.code, item)
	}
	return ex
}

func errorResponse(description string, examples *object) *object {
	ref := newObject()
	ref.set("$ref", "#/components/schemas/Error")
	media := newObject()
	media.set("schema", ref)
	media.set("examples", examples)
	content := newObject()
	content.set("application/json", media)
	r := newObject()
	r.set("description", description)
	r.set("content", content)
	return r
}

// operation ports the loop body of upstream's genOpenapiSpec for one
// endpoint of the golden.
func operation(name string, ep *object) *object {
	meta, _ := field(ep, "meta").(*object)
	if meta == nil {
		meta = newObject()
	}
	doc, _ := field(ep, "doc").(*object)
	if doc == nil {
		doc = newObject()
	}

	errExamples := newObject()
	if errs, ok := field(doc, "errors").(*object); ok {
		for _, k := range errs.keys {
			e := errs.vals[k]
			value := newObject()
			value.set("error", clone(e))
			item := newObject()
			item.set("value", value)
			errExamples.set(toString(field(e, "code")), item)
		}
	}

	res, hasRes := doc.get("res")
	hasRes = hasRes && truthy(res)

	desc := "No description provided."
	if d := field(doc, "description"); truthy(d) {
		desc = toString(d)
	}
	desc += "\n\n"
	if truthy(field(meta, "secure")) {
		desc += "**Internal Endpoint**: This endpoint is an API for the misskey mainframe and is not intended for use by third parties.\n"
	}
	cred := "No"
	if truthy(field(meta, "requireCredential")) {
		cred = "Yes"
	}
	desc += "**Credential required**: *" + cred + "*"
	if kind := field(meta, "kind"); truthy(kind) {
		desc += " / **Permission**: *" + toString(kind) + "*"
	}

	requireFile := truthy(field(meta, "requireFile"))
	requestType := "application/json"
	if requireFile {
		requestType = "multipart/form-data"
	}
	schema := convertSchema(field(ep, "paramDef"), kindParam).(*object)
	if requireFile {
		props, _ := field(schema, "properties").(*object)
		if props == nil {
			props = newObject()
		}
		props = clone(props).(*object)
		file := newObject()
		file.set("type", "string")
		file.set("format", "binary")
		file.set("description", "The file contents.")
		props.set("file", file)
		schema.set("properties", props)
		req, _ := field(schema, "required").([]any)
		schema.set("required", append(slices.Clone(req), "file"))
	}
	// 空の配列はOpenAPIで許されないので、本家と同じく落とす。
	if req, ok := field(schema, "required").([]any); ok && len(req) == 0 {
		schema.del("required")
	}

	hasBody := false
	if field(schema, "type") == "object" {
		if props, ok := field(schema, "properties").(*object); ok && len(props.keys) >= 1 {
			hasBody = true
		}
	}
	for _, o := range []string{"allOf", "oneOf", "anyOf"} {
		// 本家の条件は`length >= 0`で、配列でありさえすれば本文があるとみなす。
		if _, ok := field(schema, o).([]any); ok {
			hasBody = true
		}
	}

	op := newObject()
	op.set("operationId", operationID(name))
	op.set("summary", name)
	op.set("description", desc)
	extDocs := newObject()
	extDocs.set("description", "Source code")
	extDocs.set("url", "https://github.com/misskey-dev/misskey/blob/develop/packages/backend/src/server/api/endpoints/"+name+".ts")
	op.set("externalDocs", extDocs)
	if tags, ok := field(doc, "tags").([]any); ok && len(tags) > 0 {
		op.set("tags", []any{tags[0]})
	}
	if truthy(field(meta, "requireCredential")) {
		bearer := newObject()
		bearer.set("bearerAuth", []any{})
		op.set("security", []any{bearer})
	}
	if hasBody {
		media := newObject()
		media.set("schema", schema)
		content := newObject()
		content.set(requestType, media)
		body := newObject()
		body.set("required", true)
		body.set("content", content)
		op.set("requestBody", body)
	}

	responses := newObject()
	noContent := func() *object {
		r := newObject()
		r.set("description", "OK (without any results)")
		return r
	}
	if hasRes {
		media := newObject()
		media.set("schema", convertSchema(res, kindRes))
		content := newObject()
		content.set("application/json", media)
		ok := newObject()
		ok.set("description", "OK (with results)")
		ok.set("content", content)
		responses.set("200", ok)
	} else {
		responses.set("204", noContent())
	}
	if field(res, "optional") == true || field(res, "nullable") == true {
		responses.set("204", noContent())
	}
	responses.set("400", errorResponse("Client error", basicExamples("400", errExamples)))
	responses.set("401", errorResponse("Authentication error", basicExamples("401", newObject())))
	responses.set("403", errorResponse("Forbidden error", basicExamples("403", newObject())))
	responses.set("418", errorResponse("I'm Ai", basicExamples("418", newObject())))
	if field(doc, "limit") == true {
		responses.set("429", errorResponse("Too many requests", basicExamples("429", newObject())))
	}
	responses.set("500", errorResponse("Internal server error", basicExamples("500", newObject())))
	op.set("responses", responses)
	return op
}
