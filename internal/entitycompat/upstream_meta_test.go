package entitycompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"github.com/elythia-network/elythia/internal/api/paramschema"
)

// upstreamMeta is the part of an upstream endpoint's meta the access-control
// gates read, as tools/paramdefspec writes it into upstream_paramdefs.json.
// A key the meta does not declare is nil.
type upstreamMeta struct {
	RequireCredential  *bool   `json:"requireCredential"`
	RequireModerator   *bool   `json:"requireModerator"`
	RequireAdmin       *bool   `json:"requireAdmin"`
	RequiredRolePolicy *string `json:"requiredRolePolicy"`
	RequireFile        *bool   `json:"requireFile"`
	ProhibitMoved      *bool   `json:"prohibitMoved"`
	Secure             *bool   `json:"secure"`
	AllowGet           *bool   `json:"allowGet"`
	CacheSec           *int    `json:"cacheSec"`
	Kind               *string `json:"kind"`
}

// upstreamMetas returns endpoint -> meta of the upstream endpoints in the
// embedded upstream_paramdefs.json (make paramdef-gen).
//
// 本家の meta は、以前は tools/permspec・securespec が endpoint の .ts を正規表現で
// 読んで golden に出していた。コメント・クォート・`60 * 60`・`.test.ts` の書式の
// 揺れで何度も穴が空いたので、TypeScript の AST で評価する paramdef-gen の結果を
// 1 つの出どころにする (#3369)。評価できない値はあちらが problems で止める。
func upstreamMetas(t *testing.T) map[string]upstreamMeta {
	t.Helper()
	metas, err := upstreamMetasFrom(paramschema.UpstreamGolden())
	if err != nil {
		t.Fatal(err)
	}
	return metas
}

// upstreamMetasFrom parses the meta of every endpoint of an
// upstream_paramdefs.json. A meta key it does not know, or a value of an
// unexpected type, is an error.
func upstreamMetasFrom(golden []byte) (map[string]upstreamMeta, error) {
	var g struct {
		Endpoints map[string]struct {
			Meta json.RawMessage `json:"meta"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(golden, &g); err != nil {
		return nil, fmt.Errorf("parse upstream_paramdefs.json: %w", err)
	}
	if len(g.Endpoints) == 0 {
		return nil, fmt.Errorf("upstream_paramdefs.json has no endpoint")
	}
	out := make(map[string]upstreamMeta, len(g.Endpoints))
	for name, ep := range g.Endpoints {
		// 知らない key は止める。paramdef-gen が残す meta の key を増やしたとき、
		// ここで読むべきものか (アクセスの段階に効くか) を見直させるため。
		dec := json.NewDecoder(bytes.NewReader(ep.Meta))
		dec.DisallowUnknownFields()
		var m upstreamMeta
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("upstream_paramdefs.json: %s.meta: %w (a new meta key: add it to upstreamMeta and decide whether a gate must read it; a value of an unexpected type: check tools/paramdefspec/extract.mjs)", name, err)
		}
		out[name] = m
	}
	return out, nil
}

func isTrue(b *bool) bool { return b != nil && *b }

// accessLevel reduces a meta to Misskey's access level. The hierarchy is
// admin > moderator > auth > public; admin and moderator imply credential.
func (m upstreamMeta) accessLevel() string {
	switch {
	case isTrue(m.RequireAdmin):
		return "admin"
	case isTrue(m.RequireModerator):
		return "moderator"
	case isTrue(m.RequireCredential):
		return "auth"
	default:
		return "public"
	}
}

// upstreamPermissions returns endpoint -> access level of every endpoint.
func upstreamPermissions(metas map[string]upstreamMeta) map[string]string {
	out := make(map[string]string, len(metas))
	for ep, m := range metas {
		out[ep] = m.accessLevel()
	}
	return out
}

// upstreamSecureEndpoints returns the endpoints whose meta has secure: true,
// sorted.
func upstreamSecureEndpoints(metas map[string]upstreamMeta) []string {
	var out []string
	for ep, m := range metas {
		if isTrue(m.Secure) {
			out = append(out, ep)
		}
	}
	sort.Strings(out)
	return out
}

// upstreamOAuthKinds returns endpoint -> meta.kind of every endpoint that
// declares one.
func upstreamOAuthKinds(metas map[string]upstreamMeta) map[string]string {
	out := map[string]string{}
	for ep, m := range metas {
		if m.Kind != nil {
			out[ep] = *m.Kind
		}
	}
	return out
}

// upstreamCacheSecs returns endpoint -> cacheSec (0 when it has none) of every
// allowGet: true endpoint. A cacheSec without allowGet is an error.
func upstreamCacheSecs(metas map[string]upstreamMeta) (map[string]int, error) {
	out := map[string]int{}
	var orphan []string
	for ep, m := range metas {
		sec := 0
		if m.CacheSec != nil {
			sec = *m.CacheSec
		}
		switch {
		case isTrue(m.AllowGet):
			out[ep] = sec
		case m.CacheSec != nil:
			// 本家の ApiCallService は GET にしか cacheSec を使わないので、
			// allowGet の無い cacheSec は意味を持たない。現れたら形式が変わった
			// 疑いがあるので止める (旧 tools/permspec と同じ規則)。
			orphan = append(orphan, ep)
		}
	}
	if len(orphan) > 0 {
		sort.Strings(orphan)
		return nil, fmt.Errorf("upstream cacheSec without allowGet: %v", orphan)
	}
	return out, nil
}

// paramdefs の meta に知らない key があるときと、型が違うときに止まること。
func TestUpstreamMetasFromRejectsUnknownShape(t *testing.T) {
	ok := `{"endpoints":{"i":{"meta":{"requireCredential":true,"kind":"read:account"}}}}`
	metas, err := upstreamMetasFrom([]byte(ok))
	if err != nil {
		t.Fatalf("valid meta: %v", err)
	}
	if metas["i"].accessLevel() != "auth" || *metas["i"].Kind != "read:account" {
		t.Fatalf("valid meta read as %+v", metas["i"])
	}
	for name, raw := range map[string]string{
		"unknown key":      `{"endpoints":{"i":{"meta":{"requireCredential":true,"requireSomething":true}}}}`,
		"non-bool flag":    `{"endpoints":{"i":{"meta":{"requireAdmin":"yes"}}}}`,
		"non-int cacheSec": `{"endpoints":{"i":{"meta":{"allowGet":true,"cacheSec":"60 * 60"}}}}`,
		"no endpoint":      `{"endpoints":{}}`,
		"not json":         `{`,
	} {
		if _, err := upstreamMetasFrom([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// allowGet の無い cacheSec で止まること。allowGet の cacheSec 無しは 0 になること。
func TestUpstreamCacheSecsRejectsCacheSecWithoutAllowGet(t *testing.T) {
	sec := 60
	yes := true
	got, err := upstreamCacheSecs(map[string]upstreamMeta{
		"a": {AllowGet: &yes, CacheSec: &sec},
		"b": {AllowGet: &yes},
		"c": {},
	})
	if err != nil {
		t.Fatalf("valid metas: %v", err)
	}
	if len(got) != 2 || got["a"] != 60 || got["b"] != 0 {
		t.Fatalf("got %v", got)
	}
	if _, err := upstreamCacheSecs(map[string]upstreamMeta{"x": {CacheSec: &sec}}); err == nil {
		t.Error("cacheSec without allowGet was accepted")
	}
	no := false
	if _, err := upstreamCacheSecs(map[string]upstreamMeta{"x": {AllowGet: &no, CacheSec: &sec}}); err == nil {
		t.Error("cacheSec with allowGet: false was accepted")
	}
}
