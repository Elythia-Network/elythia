package activitypub

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// asMap re-decodes a JSON byte slice into a map for inspection.
func asMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func TestNormalize_Empty(t *testing.T) {
	body, err := Normalize(nil)
	require.NoError(t, err)
	assert.Empty(t, body)
}

func TestNormalize_BadJSON(t *testing.T) {
	_, err := Normalize([]byte(`{not json`))
	assert.Error(t, err)
}

func TestNormalize_Canonical_NoOp(t *testing.T) {
	in := []byte(`{
		"type": "Follow",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	assert.Equal(t, "Follow", m["type"])
	assert.Equal(t, "https://remote.example/users/alice", m["actor"])
	assert.Equal(t, "https://example.com/users/bob", m["object"])
}

func TestNormalize_PrefixedKeys(t *testing.T) {
	in := []byte(`{
		"as:type": "Follow",
		"as:actor": "https://remote.example/users/alice",
		"as:object": "https://example.com/users/bob"
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	assert.Equal(t, "Follow", m["type"])
	assert.Equal(t, "https://remote.example/users/alice", m["actor"])
	assert.Equal(t, "https://example.com/users/bob", m["object"])
}

func TestNormalize_IRIDirectKeys(t *testing.T) {
	in := []byte(`{
		"https://www.w3.org/ns/activitystreams#type": "Like",
		"https://www.w3.org/ns/activitystreams#actor": "https://remote.example/users/alice",
		"https://www.w3.org/ns/activitystreams#object": "https://example.com/notes/n1"
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	assert.Equal(t, "Like", m["type"])
	assert.Equal(t, "https://remote.example/users/alice", m["actor"])
}

func TestNormalize_IRINonTermLocalNameIsKept(t *testing.T) {
	// AS / security の IRI の後ろが語彙の語でない名前 (@ で始まる予約語、IRI の重なり) は
	// 短縮しない。短縮すると、もう一度正規化したときに type・id・actor へ変わる (#4116)
	for _, key := range []string{
		"https://www.w3.org/ns/activitystreams#@type",
		"https://www.w3.org/ns/activitystreams#@id",
		"https://w3id.org/security#@type",
		"https://www.w3.org/ns/activitystreams#https://www.w3.org/ns/activitystreams#actor",
		"https://w3id.org/security#https://www.w3.org/ns/activitystreams#id",
		"https://www.w3.org/ns/activitystreams#as:actor",
	} {
		in := []byte(`{"` + key + `":"Delete"}`)
		out, err := Normalize(in)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{key: "Delete"}, asMap(t, out), key)
		again, err := Normalize(out)
		require.NoError(t, err)
		assert.JSONEq(t, string(out), string(again), key)
	}
}

func TestNormalize_IRIUnknownLocalName(t *testing.T) {
	// AS IRI prefix だが local name が canonical テーブルに無い → そのまま残す
	in := []byte(`{
		"https://www.w3.org/ns/activitystreams#weirdField": "x"
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	assert.Equal(t, "x", m["weirdField"])
}

func TestNormalize_SecurityIRI(t *testing.T) {
	in := []byte(`{
		"https://w3id.org/security#publicKey": {
			"https://w3id.org/security#publicKeyPem": "PEM"
		}
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	pk, ok := m["publicKey"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "PEM", pk["publicKeyPem"])
}

func TestNormalize_SecurityIRIUnknownLocalName(t *testing.T) {
	in := []byte(`{
		"https://w3id.org/security#weirdSec": "x"
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	assert.Equal(t, "x", m["weirdSec"])
}

func TestNormalize_TypeArray(t *testing.T) {
	in := []byte(`{
		"type": ["Note", "https://example.com/Article"],
		"actor": "https://remote.example/users/alice"
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	assert.Equal(t, "Note", m["type"])
}

func TestNormalize_TypeArrayEmpty(t *testing.T) {
	in := []byte(`{
		"type": [],
		"actor": "https://remote.example/users/alice"
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	// 空配列 → flattenType が空文字を返すので type フィールドは元の値 (空 slice) になる
	_, isArr := m["type"].([]any)
	assert.True(t, isArr)
}

func TestNormalize_TypeArrayNonString(t *testing.T) {
	in := []byte(`{
		"type": [42, true]
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	// 文字列が無いので flattenType は "" を返し、配列のまま残る
	_, isArr := m["type"].([]any)
	assert.True(t, isArr)
}

func TestNormalize_LanguageMap(t *testing.T) {
	in := []byte(`{
		"name": {"@language": "en", "@value": "Hello"}
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	assert.Equal(t, "Hello", m["name"])
}

func TestNormalize_IDOnlyObjectShortcuts(t *testing.T) {
	in := []byte(`{
		"object": {"@id": "https://example.com/notes/n1"}
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	assert.Equal(t, "https://example.com/notes/n1", m["object"])
}

func TestNormalize_IDOnlyObjectNonString(t *testing.T) {
	// @id が string でないケース → そのまま map を残す
	in := []byte(`{
		"object": {"@id": 42}
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	o, ok := m["object"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(42), o["id"])
}

func TestNormalize_NestedObject(t *testing.T) {
	in := []byte(`{
		"as:type": "Create",
		"as:actor": "https://remote.example/users/alice",
		"as:object": {
			"as:type": "Note",
			"as:attributedTo": "https://remote.example/users/alice",
			"as:content": "hi"
		}
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	assert.Equal(t, "Create", m["type"])
	o, ok := m["object"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Note", o["type"])
	assert.Equal(t, "hi", o["content"])
}

func TestNormalize_DropsContext(t *testing.T) {
	in := []byte(`{
		"@context": "https://www.w3.org/ns/activitystreams",
		"type": "Note"
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	_, hasCtx := m["@context"]
	assert.False(t, hasCtx)
	assert.Equal(t, "Note", m["type"])
}

func TestNormalize_PreservesChatRoomContext(t *testing.T) {
	// CherryPick group chat は note の @context に room URI を string で載せる。
	// dispatcher が room を識別できるよう、この場合のみ @context を保持する (#1209)。
	in := []byte(`{
		"type": "Note",
		"_misskey_talk": true,
		"@context": "https://remote.example/chat/rooms/room1"
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	assert.Equal(t, "https://remote.example/chat/rooms/room1", m["@context"])
}

func TestNormalize_UnknownKeyPassthrough(t *testing.T) {
	in := []byte(`{
		"_misskey_quote": "https://example.com/notes/q1"
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	assert.Equal(t, "https://example.com/notes/q1", m["_misskey_quote"])
}

func TestNormalize_TopLevelArray(t *testing.T) {
	in := []byte(`[
		{"as:type": "Note"},
		{"as:type": "Follow"}
	]`)
	out, err := Normalize(in)
	require.NoError(t, err)
	var arr []any
	require.NoError(t, json.Unmarshal(out, &arr))
	require.Len(t, arr, 2)
	first := arr[0].(map[string]any)
	assert.Equal(t, "Note", first["type"])
}

func TestNormalize_TopLevelScalar(t *testing.T) {
	in := []byte(`"plain-string"`)
	out, err := Normalize(in)
	require.NoError(t, err)
	var s string
	require.NoError(t, json.Unmarshal(out, &s))
	assert.Equal(t, "plain-string", s)
}

func TestNormalize_AtTypeKeyword(t *testing.T) {
	in := []byte(`{
		"@type": "Note",
		"as:content": "hi"
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	assert.Equal(t, "Note", m["type"])
	assert.Equal(t, "hi", m["content"])
}

func TestNormalize_ValueObjectWithOnlyValue(t *testing.T) {
	in := []byte(`{
		"name": {"@value": "Bare"}
	}`)
	out, err := Normalize(in)
	require.NoError(t, err)
	m := asMap(t, out)
	assert.Equal(t, "Bare", m["name"])
}

// flattenType は APType (= upstream getApType) と同じく index 0 だけを見る。
// 走査すると同じ document が inbox 経由と生 fetch 経路で違う結果になる (#2662)。
func TestNormalize_TypeArrayUsesFirstElementOnly(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want any
	}{
		{"array of strings", `{"type":["Person","Service"]}`, "Person"},
		// `{"@id": ...}` は normalizeValue が先に string へ潰すので head 判定の
		// 材料にならない。潰れない値で見る。
		{"non-string head", `{"type":[42,"Person"]}`, nil},
		{"id object head is unwrapped first", `{"type":[{"@id":"as:Person"}]}`, "as:Person"},
		{"empty array", `{"type":[]}`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Normalize([]byte(tc.in))
			require.NoError(t, err)
			var m map[string]any
			require.NoError(t, json.Unmarshal(out, &m))
			if tc.want == nil {
				// flatten できなければ元の値がそのまま残る (string にはならない)。
				_, isString := m["type"].(string)
				assert.False(t, isString, "先頭が string でなければ採らない")
				return
			}
			assert.Equal(t, tc.want, m["type"])
		})
	}
}

// **正規化は決定的でなければならない。** `actor` と `as:actor` はどちらも
// canonical `actor` へ畳まれるが、Go の map 走査順はランダムなので、両方を持つ
// document は呼び出しごとに違う結果を返していた。inbox は同じバイト列を
// 複数回 (認可ゲート / dispatch / activity id の取り出し) 独立に正規化するので、
// **ゲートが署名者の actor を見て通し、本処理が詐称 actor で動く**組み合わせが
// 実測で約 10% 成立していた。
func TestNormalizeRejectsConflictingCanonicalKeys(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"actor と as:actor", `{"type":"Delete","actor":"https://evil.example/users/m","as:actor":"https://victim.example/users/a"}`},
		{"id と @id", `{"type":"Delete","id":"https://evil.example/activities/1","@id":"https://victim.example/activities/1","actor":"https://evil.example/users/m"}`},
		{"type と as:type", `{"type":"Delete","as:type":"Create","actor":"https://evil.example/users/m"}`},
		{"object と as:object", `{"type":"Delete","actor":"https://e.example/users/m","object":"https://e.example/notes/1","as:object":"https://v.example/notes/2"}`},
		{"入れ子の object の中", `{"type":"Create","actor":"https://e.example/users/m","object":{"type":"Note","attributedTo":"https://e.example/users/m","as:attributedTo":"https://v.example/users/a"}}`},
		{"配列の要素の中", `{"type":"Create","actor":"https://e.example/users/m","object":{"type":"Note","tag":[{"type":"Mention","name":"@m@e.example","as:name":"@a@v.example"}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Normalize([]byte(tc.body))
			require.ErrorIs(t, err, ErrConflictingKeys,
				"同じ canonical に畳まれる別の値を受け入れている (正規化のたびに勝者が変わる)")
		})
	}
}

// 同じ値が重複しているだけなら異常ではないので通す。expand 形式と compact 形式
// の両方を出す実装が理論上ありうるため、そこまでは弾かない。
func TestNormalizeAcceptsDuplicateKeysWithSameValue(t *testing.T) {
	out, err := Normalize([]byte(`{"type":"Delete","actor":"https://a.example/users/x","as:actor":"https://a.example/users/x"}`))
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	require.Equal(t, "https://a.example/users/x", got["actor"])
}

// **同じ入力からは必ず同じ出力が出る。** 衝突を弾いたので、map 走査順に
// 左右される余地が無いことを繰り返しで確かめる。1 回でも違う結果が出れば、
// 認可ゲートと本処理が別の actor を見る窓が残っていることになる。
func TestNormalizeIsDeterministic(t *testing.T) {
	body := []byte(`{"@context":"https://www.w3.org/ns/activitystreams","type":"Create",` +
		`"id":"https://a.example/activities/1","actor":"https://a.example/users/x",` +
		`"object":{"type":"Note","id":"https://a.example/notes/1","attributedTo":"https://a.example/users/x",` +
		`"content":"hi","to":["https://www.w3.org/ns/activitystreams#Public"],` +
		`"tag":[{"type":"Mention","href":"https://b.example/users/y"}]}}`)

	first, err := Normalize(body)
	require.NoError(t, err)
	for i := 0; i < 200; i++ {
		got, err := Normalize(body)
		require.NoError(t, err)
		require.Equal(t, string(first), string(got), "正規化の結果が呼び出しごとに変わっている")
	}
}
