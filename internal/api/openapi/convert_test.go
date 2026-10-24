package openapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// 期待値は、本家のconvertSchemaToOpenApiSchema(includeSelfRef = false)に同じ
// 入力を渡したときの出力。ただし、配列でない組み合わせ(anyOfなど)と配列の
// schemaは、本家では例外になる入力で、goldenには現れない。こちらは落ちずに返す。
func TestConvertSchema(t *testing.T) {
	cases := []struct {
		name string
		kind schemaKind
		in   string
		want string
	}{
		{
			name: "param drops misskey keys and makes nullable a type union",
			kind: kindParam,
			in:   `{"type":"string","optional":true,"nullable":true,"ref":"User","selfRef":true,"maxLength":3}`,
			want: `{"type":["string","null"],"maxLength":3}`,
		},
		{
			name: "nullable type array gets null once",
			kind: kindParam,
			in:   `{"type":["string","number"],"nullable":true}`,
			want: `{"type":["string","number","null"]}`,
		},
		{
			name: "nullable type array that has null is kept",
			kind: kindParam,
			in:   `{"type":["string","null"],"nullable":true}`,
			want: `{"type":["string","null"]}`,
		},
		{
			name: "nullable without type is left as is",
			kind: kindParam,
			in:   `{"nullable":true,"enum":[1]}`,
			want: `{"enum":[1]}`,
		},
		{
			name: "param object does not get required from optional",
			kind: kindParam,
			in:   `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer","nullable":true}},"required":["a"]}`,
			want: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["integer","null"]}},"required":["a"]}`,
		},
		{
			name: "res object gets required from optional, at the end",
			kind: kindRes,
			in:   `{"type":"object","properties":{"a":{"type":"string","optional":false},"b":{"type":"string","optional":true},"c":{"type":"string"}}}`,
			want: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"},"c":{"type":"string"}},"required":["a","c"]}`,
		},
		{
			name: "res object with all optional keeps the original required",
			kind: kindRes,
			in:   `{"type":"object","required":["x"],"properties":{"b":{"type":"string","optional":true}}}`,
			want: `{"type":"object","required":["x"],"properties":{"b":{"type":"string"}}}`,
		},
		{
			name: "res object replaces an existing required in place",
			kind: kindRes,
			in:   `{"type":"object","required":[],"properties":{"a":{"type":"string"}}}`,
			want: `{"type":"object","required":["a"],"properties":{"a":{"type":"string"}}}`,
		},
		{
			name: "object without properties is copied",
			kind: kindRes,
			in:   `{"type":"object","properties":null}`,
			want: `{"type":"object","properties":null}`,
		},
		{
			name: "res ref becomes $ref and loses type",
			kind: kindRes,
			in:   `{"type":"object","ref":"Note","optional":false}`,
			want: `{"$ref":"#/components/schemas/Note"}`,
		},
		{
			name: "nullable res ref becomes oneOf with null",
			kind: kindRes,
			in:   `{"type":"object","ref":"Note","nullable":true}`,
			want: `{"oneOf":[{"$ref":"#/components/schemas/Note"},{"type":"null"}]}`,
		},
		{
			name: "self ref is not a $ref",
			kind: kindRes,
			in:   `{"type":"object","ref":"Note","selfRef":true,"nullable":true}`,
			want: `{"type":["object","null"]}`,
		},
		{
			name: "param ref is not a $ref",
			kind: kindParam,
			in:   `{"type":"object","ref":"Note"}`,
			want: `{"type":"object"}`,
		},
		{
			name: "array items are converted",
			kind: kindRes,
			in:   `{"type":"array","items":{"type":"object","ref":"User"}}`,
			want: `{"type":"array","items":{"$ref":"#/components/schemas/User"}}`,
		},
		{
			name: "items of a non-array are copied",
			kind: kindRes,
			in:   `{"type":"string","items":{"type":"object","ref":"User"}}`,
			want: `{"type":"string","items":{"type":"object","ref":"User"}}`,
		},
		{
			name: "array without items",
			kind: kindRes,
			in:   `{"type":"array","items":null}`,
			want: `{"type":"array","items":null}`,
		},
		{
			name: "combinators are converted",
			kind: kindRes,
			in:   `{"anyOf":[{"type":"string","nullable":true}],"oneOf":[{"type":"object","ref":"A"}],"allOf":[{"type":"integer","optional":true}]}`,
			want: `{"anyOf":[{"type":["string","null"]}],"oneOf":[{"$ref":"#/components/schemas/A"}],"allOf":[{"type":"integer"}]}`,
		},
		{
			name: "combinator that is not an array becomes empty",
			kind: kindRes,
			in:   `{"anyOf":{"x":1}}`,
			want: `{"anyOf":[]}`,
		},
		{
			name: "non-object schema",
			kind: kindRes,
			in:   `[1,{"a":2}]`,
			want: `[1,{"a":2}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := mustDecode(t, tc.in)
			got := convertSchema(in, tc.kind)
			assert.Equal(t, tc.want, mustMarshal(t, got))
			// 入力は書き換えない。
			assert.Equal(t, mustMarshal(t, mustDecode(t, tc.in)), mustMarshal(t, in))
		})
	}
}

func TestTruthy(t *testing.T) {
	for _, src := range []string{`true`, `"x"`, `1`, `-0.5`, `{}`, `[]`} {
		assert.True(t, truthy(mustDecode(t, src)), src)
	}
	for _, src := range []string{`false`, `""`, `0`, `-0`, `0.0`, `null`} {
		assert.False(t, truthy(mustDecode(t, src)), src)
	}
}

func TestToString(t *testing.T) {
	assert.Equal(t, "a", toString("a"))
	assert.Equal(t, "12", toString(mustDecode(t, `12`)))
}

func TestField(t *testing.T) {
	assert.Nil(t, field(nil, "a"))
	assert.Nil(t, field("x", "a"))
	assert.Equal(t, "b", field(mustDecode(t, `{"a":"b"}`), "a"))
}
