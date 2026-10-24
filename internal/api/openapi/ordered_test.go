package openapi

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustDecode(t *testing.T, s string) any {
	t.Helper()
	v, err := decode([]byte(s))
	require.NoError(t, err)
	return v
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := marshalValue(v)
	require.NoError(t, err)
	return string(b)
}

func TestDecodeKeepsKeyOrder(t *testing.T) {
	src := `{"z":1,"a":{"y":[1,"<b>",true,null,{"q":2.50}],"b":{}},"m":[]}`
	assert.Equal(t, src, mustMarshal(t, mustDecode(t, src)))
}

func TestDecodeDuplicateKeyKeepsFirstPosition(t *testing.T) {
	// JSON.parseと同じく、後の値で上書きして位置は最初のまま。
	assert.Equal(t, `{"a":3,"b":2}`, mustMarshal(t, mustDecode(t, `{"a":1,"b":2,"a":3}`)))
}

func TestDecodeErrors(t *testing.T) {
	for _, src := range []string{``, `{"a":}`, `{"a":1`, `[1,`, `[1 2]`, `{"a":1}{}`, `{1:2}`} {
		_, err := decode([]byte(src))
		assert.Error(t, err, src)
	}
}

func TestObjectSetAndDel(t *testing.T) {
	o := newObject()
	o.set("a", 1)
	o.set("b", 2)
	o.set("c", 3)
	o.set("a", 4)
	assert.Equal(t, []string{"a", "b", "c"}, o.keys)
	o.del("b")
	o.del("missing")
	assert.Equal(t, []string{"a", "c"}, o.keys)
	v, ok := o.get("b")
	assert.False(t, ok)
	assert.Nil(t, v)
	o.set("b", 5)
	assert.Equal(t, `{"a":4,"c":3,"b":5}`, mustMarshal(t, o))
}

func TestMarshalDoesNotEscapeHTML(t *testing.T) {
	o := newObject()
	o.set("<k>", "a & b")
	assert.Equal(t, `{"<k>":"a & b"}`, mustMarshal(t, o))
}

func TestMarshalErrors(t *testing.T) {
	o := newObject()
	o.set("f", func() {})
	_, err := json.Marshal(o)
	assert.Error(t, err)
}

func TestCloneIsDeep(t *testing.T) {
	orig := mustDecode(t, `{"a":{"b":[1,{"c":2}]}}`).(*object)
	c := clone(orig).(*object)
	field(c, "a").(*object).set("x", 1)
	field(field(c, "a"), "b").([]any)[1].(*object).set("c", 9)
	assert.Equal(t, `{"a":{"b":[1,{"c":2}]}}`, mustMarshal(t, orig))
}
