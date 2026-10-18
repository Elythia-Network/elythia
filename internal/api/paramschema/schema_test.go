package paramschema

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// TestCompileRejects pins the schemas Compile refuses. ajv's own compile
// errors (strict mode) are judged by ajv in the corpus; these are the shapes
// ajv accepts but this package does not implement, and malformed input.
func TestCompileRejects(t *testing.T) {
	tests := []struct {
		name, schema, want string
	}{
		{name: "not JSON", schema: `{`, want: "paramschema"},
		{name: "not an object", schema: `[]`, want: "must be an object"},
		{name: "trailing data", schema: `{} {}`, want: "trailing data"},
		{name: "const", schema: `{"const":1}`, want: `unsupported keyword "const"`},
		{name: "not", schema: `{"not":{}}`, want: `unsupported keyword "not"`},
		{name: "type array", schema: `{"type":["string","null"]}`, want: "unsupported type"},
		{name: "nullable not boolean", schema: `{"type":"string","nullable":1}`, want: "nullable must be a boolean"},
		{name: "anyOf not array", schema: `{"anyOf":{}}`, want: "anyOf must be a non-empty array"},
		{name: "anyOf empty", schema: `{"anyOf":[]}`, want: "anyOf must be a non-empty array"},
		{name: "boolean subschema", schema: `{"allOf":[true]}`, want: "#/allOf/0: schema must be an object"},
		{name: "bad branch", schema: `{"oneOf":[{"type":1}]}`, want: "#/oneOf/0: unsupported type"},
		{name: "if not object", schema: `{"if":true,"then":{}}`, want: "#/if: schema must be an object"},
		{name: "bad then", schema: `{"if":{},"then":{"type":1}}`, want: "#/then: unsupported type"},
		{name: "maximum not number", schema: `{"maximum":"1"}`, want: "maximum must be a number"},
		{name: "fractional maxLength", schema: `{"maxLength":1.5}`, want: "maxLength must be a non-negative integer"},
		{name: "minLength not number", schema: `{"minLength":"1"}`, want: "minLength must be a non-negative integer"},
		{name: "pattern not string", schema: `{"pattern":1}`, want: "pattern must be a string"},
		{name: "lookbehind pattern", schema: `{"pattern":"(?<=a)b"}`, want: "unsupported group"},
		{name: "format not string", schema: `{"format":1}`, want: "unknown format"},
		{name: "maxItems negative", schema: `{"maxItems":-1}`, want: "maxItems must be a non-negative integer"},
		{name: "minItems fractional", schema: `{"minItems":0.5}`, want: "minItems must be a non-negative integer"},
		{name: "uniqueItems not boolean", schema: `{"uniqueItems":1}`, want: "uniqueItems must be a boolean"},
		{name: "tuple items", schema: `{"items":[{}]}`, want: "items must be a schema object"},
		{name: "bad items", schema: `{"items":{"type":1}}`, want: "#/items: unsupported type"},
		{name: "additionalProperties false", schema: `{"additionalProperties":false}`, want: "only additionalProperties: true"},
		{name: "additionalProperties schema", schema: `{"additionalProperties":{}}`, want: "only additionalProperties: true"},
		{name: "required not array", schema: `{"required":"a"}`, want: "required must be an array"},
		{name: "required duplicate", schema: `{"required":["a","a"]}`, want: "distinct strings"},
		{name: "required inherited name", schema: `{"required":["toString"]}`, want: "inherited by every JavaScript object"},
		{name: "properties not object", schema: `{"properties":[]}`, want: "properties must be an object"},
		{name: "property inherited name", schema: `{"properties":{"constructor":{}}}`, want: "inherited by every JavaScript object"},
		{name: "property not object", schema: `{"properties":{"a/b":1}}`, want: "#/properties/a~1b: schema must be an object"},
		{name: "bad property", schema: `{"properties":{"a":{"type":1}}}`, want: "#/properties/a: unsupported type"},
		{name: "default in nested anyOf", schema: `{"anyOf":[{"properties":{"a":{"properties":{"b":{"default":1}}}}}]}`, want: "default is ignored inside anyOf"},
		// ajvがfast-deep-equalで比べる形。比べるobjectのconstructor / toString /
		// valueOfキーで判定が変わる(または投げる)のを写していないので拒む。
		{name: "enum with an object", schema: `{"enum":[{"a":1}]}`, want: "enum with an object or array value"},
		{name: "enum with an array", schema: `{"enum":[1,[1]]}`, want: "enum with an object or array value"},
		{name: "uniqueItems without items", schema: `{"type":"array","uniqueItems":true}`, want: "uniqueItems needs items with a type"},
		{name: "uniqueItems over untyped items", schema: `{"type":"array","uniqueItems":true,"items":{"oneOf":[{"type":"string"},{"type":"integer"}]}}`, want: "uniqueItems needs items with a type"},
		{name: "uniqueItems over objects", schema: `{"type":"array","uniqueItems":true,"items":{"type":"object","nullable":true}}`, want: "uniqueItems over object items"},
		{name: "uniqueItems over arrays", schema: `{"type":"array","uniqueItems":true,"items":{"type":"array"}}`, want: "uniqueItems over array items"},
		{name: "description not string", schema: `{"description":1}`, want: "description must be a string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Compile([]byte(tt.schema))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Compile(%s) = %v, want an error containing %q", tt.schema, err, tt.want)
			}
		})
	}
}

func TestCompileKeepsPropertyOrderAndLastDuplicate(t *testing.T) {
	s, err := Compile([]byte(`{"type":"object","properties":{"b":{"type":"string"},"a":{"type":"string"},"b":{"type":"integer"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	// JSON.parseと同じく、重複したキーは最初の位置に最後の値が入る。
	e := s.Validate(map[string]any{"a": 1.0, "b": "x"})
	if e == nil || e.SchemaPath != "#/properties/b/type" || e.Message != "must be integer" {
		t.Fatalf("got %v", e)
	}
}

func TestDecodeOrderedErrors(t *testing.T) {
	for _, src := range []string{`{"a":}`, `{"a":1`, `{"a":1]`, `[1`, `[1,}`, `{"a":[}`, `]`} {
		if _, err := Compile([]byte(src)); err == nil {
			t.Errorf("Compile(%s) succeeded", src)
		}
	}
}

func TestValidateDefaultsAreFreshCopies(t *testing.T) {
	s, err := Compile([]byte(`{"type":"object","properties":{"o":{"type":"object","default":{"list":[1]}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	first := map[string]any{}
	if e := s.Validate(first); e != nil {
		t.Fatal(e)
	}
	// 1つ目の要求がdefaultの値を書き換えても、2つ目の要求には効かない。
	first["o"].(map[string]any)["list"].([]any)[0] = "changed"
	second := map[string]any{}
	if e := s.Validate(second); e != nil {
		t.Fatal(e)
	}
	if got := second["o"].(map[string]any)["list"].([]any)[0]; got != 1.0 {
		t.Fatalf("second default = %v, want 1", got)
	}
}

func TestValidateAcceptsUseNumberAndFloat64(t *testing.T) {
	s, err := Compile([]byte(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":100}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{json.Number("10.0"), json.Number("1e1"), 10.0} {
		if e := s.Validate(map[string]any{"limit": v}); e != nil {
			t.Errorf("limit %v: %v", v, e)
		}
	}
	for _, v := range []any{json.Number("1e400"), json.Number("not a number"), 10, int64(10)} {
		e := s.Validate(map[string]any{"limit": v})
		if e == nil || e.Message != "must be integer" {
			t.Errorf("limit %#v: got %v, want a type error", v, e)
		}
	}
}

func TestErrorString(t *testing.T) {
	e := &Error{SchemaPath: "#/properties/limit/maximum", Message: "must be <= 100"}
	if got := e.Error(); got != "#/properties/limit/maximum: must be <= 100" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestJSNumberString(t *testing.T) {
	tests := map[float64]string{
		0: "0", math.Copysign(0, -1): "0", 1: "1", -0.25: "-0.25", 100: "100",
		1e21: "1e+21", 1.5e22: "1.5e+22", 1e-7: "1e-7", -2.5e-8: "-2.5e-8", 1e-6: "0.000001",
		123456789012345680000: "123456789012345680000",
	}
	for f, want := range tests {
		if got := jsNumberString(f); got != want {
			t.Errorf("jsNumberString(%v) = %q, want %q", f, got, want)
		}
	}
}

func TestEscapeFragment(t *testing.T) {
	tests := map[string]string{
		"limit": "limit", "a/b": "a~1b", "a~b": "a~0b", "a b%": "a%20b%25",
		"日": "%E6%97%A5", "x!*'()-_.~": "x!*'()-_.~0",
	}
	for in, want := range tests {
		if got := escapeFragment(in); got != want {
			t.Errorf("escapeFragment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJSValueHelpers(t *testing.T) {
	if kindOf(int(1)) != kindUnknown || kindOf(json.Number("x")) != kindUnknown {
		t.Error("values encoding/json does not produce must be unknown")
	}
	if kindOf(json.Number("-1e400")) != kindNonFinite || kindOf(math.NaN()) != kindNonFinite {
		t.Error("non-finite numbers")
	}
	if isType("date", "x") {
		t.Error("unknown type matched")
	}
	if !jsEqual(json.Number("1.0"), 1.0) || jsEqual(true, false) || !jsEqual(false, false) || jsEqual(int(1), int(1)) {
		t.Error("jsEqual")
	}
	// object・配列はprimitiveとしか比べない(fast-deep-equalの形はCompileが拒む)。
	if jsEqual(map[string]any{}, map[string]any{}) || jsEqual([]any{}, []any{}) || jsEqual([]any{1.0}, 1.0) {
		t.Error("jsEqual on containers")
	}
	if ucs2length("a\U0001F600") != 2 {
		t.Error("ucs2length")
	}
}
