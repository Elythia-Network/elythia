package paramschema

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"sync"

	"github.com/elythia-network/elythia/internal/effectivepolicy"
	"github.com/elythia-network/elythia/internal/misc/permissions"
)

// elythiaAPIDoc is elythia_apidoc.json, the meta and the response types of
// the endpoints Elythia has and upstream does not (#4084).
//
// 引数の宣言(elythia_paramdefs.json)とは別のファイルにする。引数の宣言はajvの
// 照合corpusとファイルのhashで結び付いているので、同じファイルに書くと、
// 応答の型を直すたびにcorpusを作り直すことになる。
//
//go:embed elythia_apidoc.json
var elythiaAPIDoc []byte

// ElythiaAPIDocJSON returns the embedded elythia_apidoc.json as is, for
// internal/api/openapi. The slice is shared and must not be modified.
func ElythiaAPIDocJSON() []byte {
	return elythiaAPIDoc
}

// APIMeta is the meta of an endpoint only Elythia has, with the names and
// meanings of upstream's endpoint meta. The tests of internal/server check
// each value against the route by sending requests to the built server.
type APIMeta struct {
	// RequireCredential rejects a request without a valid token with 401.
	RequireCredential bool `json:"requireCredential"`
	// RequireModerator admits moderators and administrators only.
	RequireModerator bool `json:"requireModerator"`
	// RequireAdmin admits administrators only.
	RequireAdmin bool `json:"requireAdmin"`
	// RequiredRolePolicy is the role policy the user must have.
	RequiredRolePolicy string `json:"requiredRolePolicy"`
	// ProhibitMoved rejects accounts that moved to another server.
	ProhibitMoved bool `json:"prohibitMoved"`
	// Secure admits the user's own login token only, not app tokens.
	Secure bool `json:"secure"`
	// Kind is the permission an app token must have. Without it, app tokens
	// are rejected when the endpoint requires a credential, like upstream.
	Kind string `json:"kind"`
}

// ElythiaAPIDoc is the parsed elythia_apidoc.json.
type ElythiaAPIDoc struct {
	// Meta is the meta of each endpoint, by name.
	Meta map[string]APIMeta
}

var elythiaAPIDocOnce = sync.OnceValues(func() (*ElythiaAPIDoc, error) {
	return parseElythiaAPIDoc(elythiaAPIDoc, elythiaParamDefs)
})

// ElythiaAPIDocs parses and checks the embedded elythia_apidoc.json once.
// The result is shared and must not be modified.
func ElythiaAPIDocs() (*ElythiaAPIDoc, error) {
	return elythiaAPIDocOnce()
}

// parseElythiaAPIDoc parses b, which has the shape of elythia_apidoc.json,
// and checks it against the endpoints of paramDefs (elythia_paramdefs.json).
//
// 引数の宣言と名前の集合が一致することを求める。片方にだけ足すと、/api.jsonの
// 型と、本番の拒否やelythia-jsの検査の対象がずれるため。
func parseElythiaAPIDoc(b, paramDefs []byte) (*ElythiaAPIDoc, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f struct {
		Endpoints map[string]struct {
			Meta APIMeta         `json:"meta"`
			Doc  json.RawMessage `json:"doc"`
		} `json:"endpoints"`
		Refs map[string]json.RawMessage `json:"refs"`
	}
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("paramschema: elythia apidoc: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("paramschema: elythia apidoc: trailing data")
	}
	var p struct {
		Endpoints map[string]json.RawMessage `json:"endpoints"`
	}
	if err := json.Unmarshal(paramDefs, &p); err != nil {
		return nil, fmt.Errorf("paramschema: elythia paramDefs: %w", err)
	}
	if len(f.Endpoints) == 0 {
		return nil, errors.New("paramschema: elythia apidoc: no endpoints")
	}
	for name := range p.Endpoints {
		if _, ok := f.Endpoints[name]; !ok {
			return nil, fmt.Errorf("paramschema: elythia apidoc: %s has a paramDef but no entry", name)
		}
	}
	// 独自の型は本家の型と並べてcomponentsに載せるので、同じ名前は書けない。
	// 型の中のrefも、doc.resのrefと同じく実在する型を指すことを求める。
	known, err := upstreamRefs()
	if err != nil {
		return nil, err
	}
	refNames := make([]string, 0, len(f.Refs))
	for name := range f.Refs {
		refNames = append(refNames, name)
	}
	sort.Strings(refNames)
	for _, name := range refNames {
		if known[name] {
			return nil, fmt.Errorf("paramschema: elythia apidoc: ref %s is an upstream type", name)
		}
		var v any
		if err := json.Unmarshal(f.Refs[name], &v); err != nil || v == nil {
			return nil, fmt.Errorf("paramschema: elythia apidoc: ref %s is not a schema", name)
		}
		if err := checkRefs(v, f.Refs, known); err != nil {
			return nil, fmt.Errorf("paramschema: elythia apidoc: ref %s: %w", name, err)
		}
	}
	out := &ElythiaAPIDoc{Meta: make(map[string]APIMeta, len(f.Endpoints))}
	for name, ep := range f.Endpoints {
		if _, ok := p.Endpoints[name]; !ok {
			return nil, fmt.Errorf("paramschema: elythia apidoc: %s has no paramDef", name)
		}
		if err := checkAPIMeta(ep.Meta); err != nil {
			return nil, fmt.Errorf("paramschema: elythia apidoc: %s: %w", name, err)
		}
		out.Meta[name] = ep.Meta
		// docの無いものを許すと、/api.jsonでは本家と同じく204(結果を返さない)に
		// 読めてしまう。結果を返さないものも、空のdocを書かせる。
		if len(ep.Doc) == 0 {
			return nil, fmt.Errorf("paramschema: elythia apidoc: %s has no doc", name)
		}
		if err := checkAPIDocDoc(ep.Doc, f.Refs, known); err != nil {
			return nil, fmt.Errorf("paramschema: elythia apidoc: %s: %w", name, err)
		}
	}
	return out, nil
}

// checkAPIMeta rejects the combinations Elythia does not use.
//
// 資格情報を要する印は、requireCredentialも書かせる。本家のmetaもそう書いて
// おり、/api.jsonの説明とsecurityはrequireCredentialだけを見るため。
func checkAPIMeta(m APIMeta) error {
	if m.Kind != "" && !slices.Contains(permissions.All, m.Kind) {
		return fmt.Errorf("kind %q is not a permission", m.Kind)
	}
	if m.RequiredRolePolicy != "" {
		v, ok := effectivepolicy.Defaults()[m.RequiredRolePolicy]
		if _, isBool := v.(bool); !ok || !isBool {
			return fmt.Errorf("requiredRolePolicy %q is not a boolean role policy", m.RequiredRolePolicy)
		}
	}
	if m.RequireAdmin && m.RequireModerator {
		return errors.New("requireAdmin and requireModerator are both set")
	}
	if (m.RequireAdmin || m.RequireModerator || m.RequiredRolePolicy != "" || m.ProhibitMoved || m.Secure) && !m.RequireCredential {
		return errors.New("requireCredential must be set with the other requirements")
	}
	return nil
}

// checkAPIDocDoc checks the doc of an endpoint: only res, whose refs name a
// type of upstream's golden or of refs.
func checkAPIDocDoc(raw json.RawMessage, refs map[string]json.RawMessage, known map[string]bool) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d *struct {
		Res json.RawMessage `json:"res"`
	}
	if err := dec.Decode(&d); err != nil {
		return fmt.Errorf("doc: %w", err)
	}
	// nullは書かせない。結果を返さないことは、空のdocで書く。
	if d == nil {
		return errors.New("doc is null; write {} for no content")
	}
	if len(d.Res) == 0 {
		return nil
	}
	var res any
	if err := json.Unmarshal(d.Res, &res); err != nil || res == nil {
		return errors.New("doc.res is not a schema")
	}
	return checkRefs(res, refs, known)
}

// checkRefs reports a ref in v that names neither a type of refs nor one of
// known (upstreamRefs), and a nullable enum without null.
//
// nullableな列挙にはnullも書かせる(本家のgoldenと同じ書き方)。/api.jsonでは
// 型にnullが足されるが、列挙はそのまま写るので、nullが列挙で弾かれる形になる。
func checkRefs(v any, refs map[string]json.RawMessage, known map[string]bool) error {
	switch v := v.(type) {
	case map[string]any:
		if enum, ok := v["enum"].([]any); ok && v["nullable"] == true && !slices.Contains(enum, nil) {
			return errors.New("a nullable enum must contain null")
		}
		if r, ok := v["ref"]; ok {
			name, _ := r.(string)
			if _, ok := refs[name]; !ok && !known[name] {
				return fmt.Errorf("ref %q names no type", r)
			}
		}
		for _, c := range v {
			if err := checkRefs(c, refs, known); err != nil {
				return err
			}
		}
	case []any:
		for _, c := range v {
			if err := checkRefs(c, refs, known); err != nil {
				return err
			}
		}
	}
	return nil
}

// upstreamRefs are the names of the component schemas upstream has: the refs
// of the golden, and Error, which internal/api/openapi adds.
//
// goldenが読めないときは空の集合にせずエラーにする。空にすると、本家の型を指す
// refも、本家と同じ名前の型の拒否も、黙って空振りする。
var upstreamRefs = sync.OnceValues(func() (map[string]bool, error) {
	var g struct {
		Refs map[string]json.RawMessage `json:"refs"`
	}
	if err := json.Unmarshal(upstreamGolden, &g); err != nil {
		return nil, fmt.Errorf("paramschema: upstream golden: %w", err)
	}
	if len(g.Refs) == 0 {
		return nil, errors.New("paramschema: upstream golden has no refs")
	}
	out := map[string]bool{"Error": true}
	for k := range g.Refs {
		out[k] = true
	}
	return out, nil
})
