// Package entitycompat provides a static "Layer 0" API shape drift detector.
//
// It reflects over mk-go's entity DTO structs (internal/entity) and diffs
// their JSON shape against the Misskey API contract (the misskey-js autogen
// `types.ts`, which mirrors the OpenAPI `components.schemas`). The class of
// mismatch it surfaces — fields the contract declares that mk-go omits, and
// nullability/optionality divergences — is exactly what makes Misskey-compatible
// 3rd-party clients crash on a non-null cast.
//
// The detector needs no running server, no browser, and no Docker, so it runs
// as a fast deterministic `go test` gate. The golden contract is consumed from
// a committed snapshot (testdata/golden_schemas.json) regenerated from the
// upstream source in .cache/misskey/<version> on upstream catch-up, keeping the gate hermetic.
package entitycompat

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// FieldShape is the normalized shape of a single JSON field: whether it may
// serialize to null, whether it may be absent, and a coarse type bucket.
type FieldShape struct {
	Nullable bool   `json:"nullable"`
	Optional bool   `json:"optional"`
	Type     string `json:"type"`
	// Elem is the element type of an array field ("string"/"number"/"boolean"/
	// "object"/"array"/"other"). Empty for non-array fields. Lets the Layer 2
	// validator distinguish e.g. string[] from {id,name}[] which the coarse
	// "array" Type alone cannot (#1298)。
	Elem string `json:"elem,omitempty"`
}

// Schema maps a JSON field name to its shape.
type Schema map[string]FieldShape

// GoldenSet is the committed contract snapshot: golden schema name -> Schema.
type GoldenSet map[string]Schema

// Severity ranks a finding for gating. HIGH and MED are gated by the drift
// test; LOW and INFO are reported but non-blocking.
type Severity string

const (
	SevHigh Severity = "HIGH"
	SevMed  Severity = "MED"
	SevLow  Severity = "LOW"
	SevInfo Severity = "INFO"
)

// Kind classifies what sort of drift a finding represents.
const (
	KindMissing  = "missing"  // golden has a field mk-go does not emit
	KindNullable = "nullable" // mk-go may emit null where golden is non-null
	KindOmit     = "omit"     // mk-go may omit a field golden requires
	KindExtra    = "extra"    // mk-go emits a field absent from golden
	KindLayer    = "layer"    // present in a sibling layer of the same family
)

// Finding is a single detected (or informational) shape difference.
type Finding struct {
	Family string
	Layer  string
	Golden string
	Field  string
	Kind   string
	Sev    Severity
	Detail string
}

// ---- mk-go side: reflection ------------------------------------------------

// reflectOwnFields returns the shape of the non-embedded fields declared
// directly on t. Embedded layers map to a sibling golden schema, so they are
// excluded here and handled by their own Layer entry.
func reflectOwnFields(t reflect.Type) Schema {
	out := Schema{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			continue
		}
		name, opt, ok := jsonName(f)
		if !ok {
			continue
		}
		out[name] = FieldShape{
			// omitempty 付きポインタは nil で「省略」= optional であり null は
			// 出さない。omitempty 無しのポインタだけが nil 時に `null` を出す。
			Nullable: f.Type.Kind() == reflect.Pointer && !opt,
			Optional: opt,
			Type:     coarseGo(f.Type),
		}
	}
	return out
}

// reflectAllFields flattens every json field reachable from t, recursing into
// embedded structs. Used to build the per-family "present somewhere" set.
func reflectAllFields(t reflect.Type) Schema {
	out := Schema{}
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous {
				walk(f.Type)
				continue
			}
			if name, opt, ok := jsonName(f); ok {
				out[name] = FieldShape{
					Nullable: f.Type.Kind() == reflect.Pointer && !opt,
					Optional: opt,
					Type:     coarseGo(f.Type),
				}
			}
		}
	}
	walk(t)
	return out
}

func jsonName(f reflect.StructField) (name string, omitempty, ok bool) {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return "", false, false
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		return "", false, false
	}
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty, true
}

func coarseGo(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	default:
		return "any"
	}
}

// ---- golden side: types.ts parser -----------------------------------------

var propRe = regexp.MustCompile(`^            (\w+)(\??):\s*(.*)$`)

// ParseTypesFile parses the openapi-typescript schema blocks named in `names`
// out of the given types.ts content (split into lines). Missing schemas are
// silently skipped; callers can compare the returned key set against `names`.
func ParseTypesFile(lines []string, names []string) GoldenSet {
	out := GoldenSet{}
	for _, n := range names {
		if s, ok := parseSchema(lines, n); ok {
			out[n] = s
		}
	}
	return out
}

// parseSchema extracts the top-level properties of one openapi-typescript
// schema block. Brace depth is tracked so fields of nested inline object types
// are not mistaken for top-level properties.
func parseSchema(lines []string, schemaName string) (Schema, bool) {
	open := "        " + schemaName + ": {"
	start := -1
	for i, ln := range lines {
		if ln == open {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, false
	}

	out := Schema{}
	depth := 1

	var curName string
	var have bool
	var optional bool
	var typ string
	var buf strings.Builder
	finalize := func() {
		if !have {
			return
		}
		// prop 行の coarseTS では多行 `{...}[]` を取りこぼす (m[3] が `{` で
		// "object" 判定) ため、蓄積した field 全文 (buf) から array 判定と
		// 要素型を再計算する。array でなければ既存の typ を維持する (#1298)。
		fieldType, elem := typ, ""
		if at, ae, ok := classifyArray(curName, buf.String()); ok {
			fieldType, elem = at, ae
		}
		out[curName] = FieldShape{
			Nullable: strings.Contains(buf.String(), "| null"),
			Optional: optional,
			Type:     fieldType,
			Elem:     elem,
		}
		have = false
		buf.Reset()
	}

	for i := start + 1; i < len(lines); i++ {
		ln := lines[i]
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		if depth == 1 {
			if m := propRe.FindStringSubmatch(ln); m != nil {
				finalize()
				curName = m[1]
				optional = m[2] == "?"
				typ = coarseTS(m[3])
				have = true
			}
		}
		delta := strings.Count(ln, "{") - strings.Count(ln, "}")
		// schema を閉じる行 (depth が 0 に戻る `};`) は最後の field の buf に
		// 混入させない。混入すると多行 array の末尾 `[]` 判定が後続の `}` で
		// 崩れて object と誤型付けされる (#1298)。
		if have && depth+delta > 0 {
			buf.WriteString(ln)
			buf.WriteString("\n")
		}
		depth += delta
		if depth == 0 {
			finalize()
			return out, true
		}
	}
	finalize()
	return out, true
}

func coarseTS(rhs string) string {
	rhs = strings.TrimSuffix(strings.TrimSpace(rhs), ";")
	rhs = strings.TrimSpace(strings.TrimSuffix(rhs, "| null"))
	rhs = strings.TrimSpace(rhs)
	switch {
	case rhs == "":
		return "object"
	case strings.HasSuffix(rhs, "[]"):
		return "array"
	case strings.HasPrefix(rhs, "{"):
		return "object"
	case strings.HasPrefix(rhs, "'") || strings.Contains(rhs, "' | '"):
		return "string"
	case rhs == "string":
		return "string"
	case rhs == "boolean":
		return "boolean"
	case rhs == "number":
		return "number"
	case strings.Contains(rhs, "components['schemas']"):
		return "object"
	default:
		return "other"
	}
}

// classifyArray inspects a field's full (possibly multi-line) text and, when
// the type is an array, returns ("array", <element type>, true). For non-array
// fields it returns ("", "", false) so the caller keeps the prop-line coarseTS
// result. Handles single-line `T[]` and multi-line `{ ... }[]` alike (#1298)。
func classifyArray(name, raw string) (typ, elem string, ok bool) {
	s := raw
	// strip the leading `<name>?:` prefix.
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	// 多行を 1 行に潰して末尾判定を安定させる。
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), ";"))
	// 末尾の `| null` を剥がす (nullable は別途 buf で判定済)。
	if strings.HasSuffix(s, "| null") {
		s = strings.TrimSpace(strings.TrimSuffix(s, "| null"))
	}
	if !strings.HasSuffix(s, "[]") {
		return "", "", false
	}
	inner := strings.TrimSpace(strings.TrimSuffix(s, "[]"))
	return "array", elemKind(inner), true
}

// elemKind maps an array element type expression to a coarse element kind.
func elemKind(inner string) string {
	switch {
	case inner == "string":
		return "string"
	case inner == "number":
		return "number"
	case inner == "boolean":
		return "boolean"
	case strings.HasSuffix(inner, "[]"):
		return "array"
	case strings.HasPrefix(inner, "{"):
		return "object"
	case strings.HasPrefix(inner, "'") || strings.Contains(inner, "' | '"):
		return "string"
	case strings.Contains(inner, "components['schemas']"):
		return "object"
	default:
		return "other"
	}
}

// ---- diff -------------------------------------------------------------------

// newFinding builds a Finding for a family/layer diff result. Centralizing the
// (otherwise 7-field positional) construction keeps call sites readable and
// resilient to future field additions.
func newFinding(fam Family, l Layer, field, kind string, sev Severity, detail string) Finding {
	return Finding{
		Family: fam.Name,
		Layer:  l.Label,
		Golden: l.Golden,
		Field:  field,
		Kind:   kind,
		Sev:    sev,
		Detail: detail,
	}
}

// DiffFamily compares every layer of fam against the golden snapshot and
// returns findings sorted by severity. famPresent is the union of all fields
// emitted anywhere in the family, used to downgrade a layer-local "missing" to
// an informational layer-placement note when the field exists elsewhere.
func DiffFamily(fam Family, golden GoldenSet) []Finding {
	famPresent := map[string]bool{}
	for _, l := range fam.Layers {
		for name := range reflectAllFields(l.Type) {
			famPresent[name] = true
		}
	}

	var findings []Finding
	for _, l := range fam.Layers {
		gold, ok := golden[l.Golden]
		if !ok {
			continue
		}
		mk := reflectOwnFields(l.Type)

		for name, g := range gold {
			if _, ok := mk[name]; ok {
				continue
			}
			if famPresent[name] {
				findings = append(findings, newFinding(fam, l, name, KindLayer, SevInfo,
					"present in another layer of the same family"))
				continue
			}
			sev := SevHigh
			detail := "declared in golden, absent in all mk-go structs"
			if g.Optional {
				sev = SevLow
				detail = "declared in golden (optional), absent in mk-go"
			}
			findings = append(findings, newFinding(fam, l, name, KindMissing, sev, detail))
		}

		for name, g := range gold {
			m, ok := mk[name]
			if !ok {
				continue
			}
			if !g.Nullable && m.Nullable {
				findings = append(findings, newFinding(fam, l, name, KindNullable, SevHigh,
					"golden=non-null, mk-go=nullable: may emit null where client casts non-null"))
			}
			if !g.Optional && m.Optional {
				findings = append(findings, newFinding(fam, l, name, KindOmit, SevMed,
					"golden=required, mk-go=omitempty: may omit a field the client requires"))
			}
		}

		for name := range mk {
			if _, ok := gold[name]; !ok {
				findings = append(findings, newFinding(fam, l, name, KindExtra, SevInfo,
					"in mk-go but not in golden (extension/alias?)"))
			}
		}
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Layer != findings[j].Layer {
			return findings[i].Layer < findings[j].Layer
		}
		if sevRank(findings[i].Sev) != sevRank(findings[j].Sev) {
			return sevRank(findings[i].Sev) < sevRank(findings[j].Sev)
		}
		return findings[i].Field < findings[j].Field
	})
	return findings
}

func sevRank(s Severity) int {
	switch s {
	case SevHigh:
		return 0
	case SevMed:
		return 1
	case SevLow:
		return 2
	default:
		return 3
	}
}

// GoldenSchemaNames returns the set of golden schema names referenced by the
// family table, so the generator knows which schemas to extract.
func GoldenSchemaNames() []string {
	seen := map[string]bool{}
	var names []string
	for _, fam := range Families() {
		for _, l := range fam.Layers {
			if !seen[l.Golden] {
				seen[l.Golden] = true
				names = append(names, l.Golden)
			}
		}
	}
	sort.Strings(names)
	return names
}
