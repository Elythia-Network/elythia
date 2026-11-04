package activitypub

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"math"
)

// attachment は配列でも単一 object でもよい。`[]any` 決め打ちだと単一 object の
// document で json.Unmarshal ごと失敗し、**その actor / note が一切取り込めなく
// なる** (#2662)。
func TestAPObjectList_UnmarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"array", `{"attachment":[{"type":"PropertyValue"},{"type":"Image"}]}`, 2},
		{"single object", `{"attachment":{"type":"PropertyValue"}}`, 1},
		{"empty array", `{"attachment":[]}`, 0},
		{"null", `{"attachment":null}`, 0},
		// field 自体が無いケースは UnmarshalJSON を呼ばない。ここで見ているのは
		// zero value が nil slice であることだけ (gate ではない)。
		{"absent", `{}`, 0},
		// 配列でも object でもない値も document を落とさない。
		{"single string", `{"attachment":"https://example.com/x"}`, 1},
		{"number", `{"attachment":42}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p Person
			require.NoError(t, json.Unmarshal([]byte(tc.in), &p))
			assert.Len(t, p.Attachment, tc.want)

			var n Note
			require.NoError(t, json.Unmarshal([]byte(tc.in), &n))
			assert.Len(t, n.Attachment, tc.want, "Note も同じ扱い")
		})
	}
}

// 単一 object は捨てずに 1 要素として拾う (upstream の analyzeAttachments は
// 非配列を [] に潰すが、捨てる理由が無い)。
func TestAPObjectList_SingleObjectKeepsContent(t *testing.T) {
	var p Person
	require.NoError(t, json.Unmarshal(
		[]byte(`{"attachment":{"type":"PropertyValue","name":"Web","value":"x"}}`), &p))
	require.Len(t, p.Attachment, 1)
	m, ok := p.Attachment[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "PropertyValue", m["type"])
	assert.Equal(t, "Web", m["name"])
}

// 送出時は配列で出す (単一 object を受けても shape は array のまま)。
func TestAPObjectList_MarshalsAsArray(t *testing.T) {
	var p Person
	require.NoError(t, json.Unmarshal([]byte(`{"attachment":{"type":"PropertyValue"}}`), &p))
	out, err := json.Marshal(p.Attachment)
	require.NoError(t, err)
	assert.Equal(t, `[{"type":"PropertyValue"}]`, string(out))

	// nil は omitempty で key ごと消える。
	var empty Person
	b, err := json.Marshal(empty)
	require.NoError(t, err)
	assert.NotContains(t, string(b), `"attachment"`)
}

// vcard:* を string 決め打ちにすると、object や数値で送る実装で actor ごと
// reject される。読めなければ空にして document は通す (#2662)。
func TestAPLenientString_UnmarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"string", `{"vcard:bday":"1990-05-03"}`, "1990-05-03"},
		{"empty string", `{"vcard:bday":""}`, ""},
		// JSON-LD の展開形は剥がして値を拾う。捨てると「読めたはずの値が
		// 黙って消える」ことになる (CW が付かない等)。
		{"value object", `{"vcard:bday":{"@value":"1990-05-03"}}`, "1990-05-03"},
		{"single element array", `{"vcard:bday":["1990-05-03"]}`, "1990-05-03"},
		{"array of value object", `{"vcard:bday":[{"@value":"1990-05-03"}]}`, "1990-05-03"},
		{"multi element array", `{"vcard:bday":["a","b"]}`, ""},
		{"plain object", `{"vcard:bday":{"a":1}}`, ""},
		{"number", `{"vcard:bday":1990}`, ""},
		{"null", `{"vcard:bday":null}`, ""},
		// 同上: UnmarshalJSON を呼ばない zero value の確認。
		{"absent", `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p Person
			// **document 全体が通ることが要点。** ここで error を返すと actor が
			// 丸ごと reject される。
			require.NoError(t, json.Unmarshal([]byte(tc.in), &p))
			assert.Equal(t, tc.want, p.VcardBday.String())
		})
	}
}

// 非 string の vcard が来ても、同じ document の他の field は読める。
func TestAPLenientString_DoesNotBreakSiblingFields(t *testing.T) {
	var p Person
	require.NoError(t, json.Unmarshal([]byte(`{
		"type":"Person",
		"preferredUsername":"alice",
		"vcard:bday":{"@value":"1990-05-03"},
		"vcard:Address":42
	}`), &p))
	assert.Equal(t, "alice", p.PreferredUsername)
	assert.Equal(t, "1990-05-03", p.VcardBday.String(), "展開形は剥がして拾う")
	assert.Equal(t, "", p.VcardAddress.String(), "読めない形は空")
}

func TestAddContext_Person(t *testing.T) {
	p := &Person{}
	AddContext(p)
	ctx, ok := p.Context.([]any)
	assert.True(t, ok)
	assert.Contains(t, ctx, ContextURL)
	assert.Contains(t, ctx, SecurityContextURL)
}

func TestAddContext_Variants(t *testing.T) {
	cases := []any{
		&Note{},
		&Create{},
		&Follow{},
		&Accept{},
		&Reject{},
		&Undo{},
		&Delete{},
		&Update{},
		&Like{},
		&Announce{},
	}
	for _, c := range cases {
		AddContext(c)
		// 全型で fullContext (AS + Security + MisskeyContext) が設定される
		var ctx any
		switch v := c.(type) {
		case *Note:
			ctx = v.Context
		case *Create:
			ctx = v.Context
		case *Follow:
			ctx = v.Context
		case *Accept:
			ctx = v.Context
		case *Reject:
			ctx = v.Context
		case *Undo:
			ctx = v.Context
		case *Delete:
			ctx = v.Context
		case *Update:
			ctx = v.Context
		case *Like:
			ctx = v.Context
		case *Announce:
			ctx = v.Context
		}
		arr, ok := ctx.([]any)
		assert.True(t, ok)
		assert.Contains(t, arr, ContextURL)
		assert.Contains(t, arr, SecurityContextURL)
	}
}

func TestAddContext_IndependentSlices(t *testing.T) {
	// 異なるオブジェクトが独立したcontextスライスを持つこと
	p := &Person{}
	n := &Note{}
	AddContext(p)
	AddContext(n)
	pCtx := p.Context.([]any)
	nCtx := n.Context.([]any)
	// **append では捕まらない。** 追記は `nCtx` の長さより後ろへ書くので、backing
	// array を共有していても `nCtx` からは見えない。`len(append(s, x)) == len(s)+1`
	// を見る形も恒真。どちらも aliasing を仕込む変異が素通りする (実測)。
	// 既存要素を書き換えて、相手へ波及しないことを直接見る。
	pCtx[0] = "mutated"
	assert.NotEqual(t, "mutated", nCtx[0], "同じ backing array を共有している")
	assert.Len(t, nCtx, 3)
}

func TestAddContext_NoOpForUnknown(t *testing.T) {
	// 引数の型が switch にない場合はno-op
	type other struct{}
	AddContext(&other{})
}

func TestPersonMarshalsCleanly(t *testing.T) {
	p := &Person{
		Object: Object{ID: "https://example.com/users/u1", Type: "Person"},
	}
	AddContext(p)
	b, err := json.Marshal(p)
	assert.NoError(t, err)
	assert.Contains(t, string(b), "Person")
	assert.Contains(t, string(b), ContextURL)
}

func TestNewMention(t *testing.T) {
	m := NewMention("https://example.com/users/u1", "@u1")
	assert.Equal(t, "Mention", m.Type)
	assert.Equal(t, "https://example.com/users/u1", m.Href)
	assert.Equal(t, "@u1", m.Name)

	// name が空でも factory は Type を必ず埋める
	m2 := NewMention("https://example.com/users/u2", "")
	assert.Equal(t, "Mention", m2.Type)
	assert.Empty(t, m2.Name)

	// JSON 出力時も type フィールドが出ていること
	b, err := json.Marshal(m)
	assert.NoError(t, err)
	assert.Contains(t, string(b), `"type":"Mention"`)
}

func TestIsValidActorType(t *testing.T) {
	cases := []struct {
		typ  string
		want bool
	}{
		{"Person", true},
		{"Service", true},
		{"Group", true},
		{"Organization", true},
		{"Application", true},
		{"Note", false},
		{"Tombstone", false},
		{"", false},
		{"person", false}, // case sensitive (TS と同じ)
	}
	for _, c := range cases {
		t.Run(c.typ, func(t *testing.T) {
			assert.Equal(t, c.want, IsValidActorType(c.typ))
		})
	}
}

func TestIsBotActorType(t *testing.T) {
	cases := []struct {
		typ  string
		want bool
	}{
		{"Service", true},
		{"Application", true},
		{"Person", false},
		{"Group", false},
		{"Organization", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.typ, func(t *testing.T) {
			assert.Equal(t, c.want, IsBotActorType(c.typ))
		})
	}
}

// Person.AlsoKnownAs を Mastodon 互換の array 形式 / Friendica 等の single
// string 形式どちらでも JSON Unmarshal が成功し、[]string として等価な値を
// 取れることを実 Person Unmarshal で end-to-end 検証する。
func TestPerson_AlsoKnownAs_BothForms(t *testing.T) {
	t.Run("array_form", func(t *testing.T) {
		body := []byte(`{"id":"https://example.test/users/alice","alsoKnownAs":["https://old.example/users/a","https://older.example/users/a"]}`)
		var p Person
		err := json.Unmarshal(body, &p)
		assert.NoError(t, err)
		assert.Equal(t, APIDList{"https://old.example/users/a", "https://older.example/users/a"}, p.AlsoKnownAs)
	})
	t.Run("single_string_form", func(t *testing.T) {
		body := []byte(`{"id":"https://example.test/users/alice","alsoKnownAs":"https://old.example/users/a"}`)
		var p Person
		err := json.Unmarshal(body, &p)
		assert.NoError(t, err)
		assert.Equal(t, APIDList{"https://old.example/users/a"}, p.AlsoKnownAs)
	})
	t.Run("absent_field", func(t *testing.T) {
		body := []byte(`{"id":"https://example.test/users/alice"}`)
		var p Person
		err := json.Unmarshal(body, &p)
		assert.NoError(t, err)
		assert.Nil(t, p.AlsoKnownAs)
	})
}

// PR #1110: AS2.0 spec で `icon` / `image` は単一 object または array を
// 許容する (#dfn-icon)。iceshrimp / 一部 Pleroma fork が array 形式で
// multi-resolution icon を expose しており、旧 mk-go は単一 object のみ
// 受理していたため avatar URL が取れず TL の @mention 横にアイコンが
// 表示されない bug があった (#1110)。Image.UnmarshalJSON で両形式を
// 吸収する upstream-compat 修正の regression guard。
func TestImage_UnmarshalJSON_SingleObject(t *testing.T) {
	var img Image
	err := json.Unmarshal([]byte(`{"type":"Image","url":"https://example.com/a.png"}`), &img)
	assert.NoError(t, err)
	assert.Equal(t, "Image", img.Type.String())
	assert.Equal(t, "https://example.com/a.png", img.URL.String())
}

func TestImage_UnmarshalJSON_Array_PicksFirstWithURL(t *testing.T) {
	var img Image
	data := []byte(`[
		{"type":"Image","url":""},
		{"type":"Image","url":"https://example.com/large.png"},
		{"type":"Image","url":"https://example.com/small.png"}
	]`)
	err := json.Unmarshal(data, &img)
	assert.NoError(t, err)
	assert.Equal(t, "https://example.com/large.png", img.URL.String(),
		"upstream behavior: take first item with non-empty url")
}

func TestImage_UnmarshalJSON_Array_AllEmptyURLs(t *testing.T) {
	var img Image
	err := json.Unmarshal([]byte(`[{"type":"Image","url":""},{"type":"Image"}]`), &img)
	assert.NoError(t, err)
	assert.Equal(t, "", img.URL.String(), "empty array of empty-url items leaves zero value")
}

func TestImage_UnmarshalJSON_EmptyArray(t *testing.T) {
	var img Image
	err := json.Unmarshal([]byte(`[]`), &img)
	assert.NoError(t, err)
	assert.Equal(t, "", img.URL.String())
	assert.Equal(t, "", img.Type.String())
}

func TestImage_UnmarshalJSON_Null(t *testing.T) {
	var img Image
	err := json.Unmarshal([]byte(`null`), &img)
	assert.NoError(t, err)
	assert.Equal(t, "", img.URL.String())
}

// object / array でない `icon` / `image` は **error にしない**。AS2 の
// `icon` の range は `Image | Link` で、Link は compaction で bare IRI に
// なるため bare string は正当。upstream も ApImageService が null を返し
// resolveAvatarAndBanner が握り潰すので actor は必ず作られる。ここで error を
// 返すと actor document ごと落ちて一切連合できなくなる (#2662)。
func TestImage_UnmarshalJSON_NonObjectIsIgnored(t *testing.T) {
	for _, in := range []string{
		`"https://example.com/a.png"`,
		`42`,
		`true`,
	} {
		var img Image
		require.NoError(t, json.Unmarshal([]byte(in), &img), in)
		assert.Equal(t, "", img.URL.String(), "読めない形は zero value")
	}

	// actor document 全体としても落とさないこと。
	for _, in := range []string{
		`{"icon":"https://e/a.png"}`,
		`{"image":"https://e/b.png"}`,
		`{"icon":{"type":["Image"],"url":"https://e/a.png"}}`,
		`{"icon":{"type":"Image","url":{"type":"Link","href":"https://e/a.png"}}}`,
		`{"icon":{"type":"Image","url":["https://e/a.png"]}}`,
		`{"assertionMethod":{"id":"https://e/u#ed","type":"Multikey","publicKeyMultibase":"z6M"}}`,
		`{"assertionMethod":"nonsense"}`,
	} {
		var p Person
		require.NoError(t, json.Unmarshal([]byte(in), &p), in)
	}

	// **兄弟 field が 1 つ読めないだけで全部捨てない。** upstream は
	// mediaType / name の型を見ずにアバターを採用する。
	for _, in := range []string{
		`{"type":"Image","url":"https://e/a.png","mediaType":123}`,
		`{"type":"Image","url":"https://e/a.png","name":{"und":"x"}}`,
		`{"type":"Image","url":"https://e/a.png","sensitive":"yes"}`,
	} {
		var img Image
		require.NoError(t, json.Unmarshal([]byte(in), &img), in)
		assert.Equal(t, "https://e/a.png", img.URL.String(), "url は救う: "+in)
		assert.Equal(t, "Image", img.Type.String(), "type も救う: "+in)
	}

	// 読める形はちゃんと拾う。
	var p Person
	require.NoError(t, json.Unmarshal([]byte(`{"icon":{"type":["Image"],"url":{"type":"Link","href":"https://e/a.png"}}}`), &p))
	require.NotNil(t, p.Icon)
	assert.Equal(t, "Image", p.Icon.Type.String())
	assert.Equal(t, "https://e/a.png", p.Icon.URL.String())

	var p2 Person
	require.NoError(t, json.Unmarshal([]byte(`{"assertionMethod":{"id":"https://e/u#ed","type":"Multikey","publicKeyMultibase":"z6M"}}`), &p2))
	require.Len(t, p2.AssertionMethod.Keys, 1)
	assert.Equal(t, "https://e/u#ed", p2.AssertionMethod.Keys[0].ID)
}

// 先頭が空白文字 (= JSON formatter 由来) でも array / object の判別が
// 正しく動く。for-switch 内の whitespace skip 経路の guard。
func TestImage_UnmarshalJSON_LeadingWhitespace(t *testing.T) {
	t.Run("whitespace + object", func(t *testing.T) {
		var img Image
		err := json.Unmarshal([]byte("   \n\t {\"type\":\"Image\",\"url\":\"https://example.com/a.png\"}"), &img)
		assert.NoError(t, err)
		assert.Equal(t, "https://example.com/a.png", img.URL.String())
	})
	t.Run("whitespace + array", func(t *testing.T) {
		var img Image
		err := json.Unmarshal([]byte(" \r\n [{\"type\":\"Image\",\"url\":\"https://example.com/b.png\"}]"), &img)
		assert.NoError(t, err)
		assert.Equal(t, "https://example.com/b.png", img.URL.String())
	})
}

// 報告経路の end-to-end guard: iceshrimp 風 actor JSON (array 形式 icon /
// image) を Person に unmarshal して avatar/banner URL が正しく抽出される
// ことを確認。
func TestPerson_UnmarshalJSON_ArrayIconAndImage(t *testing.T) {
	body := []byte(`{
		"type": "Person",
		"id": "https://iceshrimp.example/users/alice",
		"inbox": "https://iceshrimp.example/inbox",
		"outbox": "https://iceshrimp.example/users/alice/outbox",
		"followers": "https://iceshrimp.example/users/alice/followers",
		"following": "https://iceshrimp.example/users/alice/following",
		"preferredUsername": "alice",
		"publicKey": {"id":"x","owner":"y","publicKeyPem":"z"},
		"icon": [
			{"type":"Image","url":"https://iceshrimp.example/avatar.png"}
		],
		"image": [
			{"type":"Image","url":"https://iceshrimp.example/banner.png"}
		]
	}`)
	var p Person
	err := json.Unmarshal(body, &p)
	assert.NoError(t, err)
	if assert.NotNil(t, p.Icon) {
		assert.Equal(t, "https://iceshrimp.example/avatar.png", p.Icon.URL.String())
	}
	if assert.NotNil(t, p.Image) {
		assert.Equal(t, "https://iceshrimp.example/banner.png", p.Image.URL.String())
	}
}

// apFirstRef / APLenientID は upstream getOneApId と同じ形を受ける。
func TestAPLenientID_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare string", `"https://e/n1"`, "https://e/n1"},
		{"object with id", `{"id":"https://e/n1","type":"Note"}`, "https://e/n1"},
		{"array of strings takes first", `["https://e/n1","https://e/n2"]`, "https://e/n1"},
		{"array of objects takes first", `[{"id":"https://e/n1"},{"id":"https://e/n2"}]`, "https://e/n1"},
		{"empty array", `[]`, ""},
		{"object without id", `{"type":"Note"}`, ""},
		{"object with non-string id", `{"id":42}`, ""},
		{"number", `42`, ""},
		{"null", `null`, ""},
		{"nested array is not unwrapped twice", `[["https://e/n1"]]`, ""},
		// 読まない兄弟 field のレンジ外数値で参照ごと落とさない (#2729)。
		// `attributedTo` が空になると note ごと落ちる
		// (`ingestNoteWithCreated` の明示 gate。host 照合より前)。
		{"huge number in sibling field", `{"id":"https://e/n1","x":1e999}`, "https://e/n1"},
		{"huge number later in array", `["https://e/n1",1e999]`, "https://e/n1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var v APLenientID
			require.NoError(t, json.Unmarshal([]byte(tc.in), &v), "document を落とさない")
			assert.Equal(t, tc.want, v.String())
		})
	}
}

// APLenientHref は upstream getOneApHrefNullable と同じく `href` を見る。
func TestAPLenientHref_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare string", `"https://e/@a"`, "https://e/@a"},
		{"Link object", `{"type":"Link","href":"https://e/@a"}`, "https://e/@a"},
		{"array takes first", `[{"type":"Link","href":"https://e/@a"},"https://e/@b"]`, "https://e/@a"},
		{"object with id but no href", `{"id":"https://e/@a"}`, ""},
		{"empty array", `[]`, ""},
		{"null", `null`, ""},
		// **読まない兄弟 field のレンジ外数値で参照ごと落とさない** (#2729)。
		// 素の `any` へ decode すると数値が float64 になり、`1e999` で
		// `cannot unmarshal number` になっていた。読むのは string だけなのに、
		// リモートが 1 トークン置くだけで permalink も featured も空にできた。
		{"huge number in sibling field", `{"href":"https://e/@a","x":1e999}`, "https://e/@a"},
		{"huge number later in array", `["https://e/@a",1e999]`, "https://e/@a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var v APLenientHref
			require.NoError(t, json.Unmarshal([]byte(tc.in), &v), "document を落とさない")
			assert.Equal(t, tc.want, v.String())
		})
	}
}

// APIDList は upstream getApIds と同じ形を受ける。読めない要素は落とす。
func TestAPIDList_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"array of strings", `["https://e/a","https://e/b"]`, []string{"https://e/a", "https://e/b"}},
		{"single string", `"https://e/a"`, []string{"https://e/a"}},
		{"single object", `{"id":"https://e/a"}`, []string{"https://e/a"}},
		{"mixed", `["https://e/a",{"id":"https://e/b"}]`, []string{"https://e/a", "https://e/b"}},
		{"unreadable element dropped", `["https://e/a",42,{"type":"Note"}]`, []string{"https://e/a"}},
		{"empty array", `[]`, []string{}},
		{"null", `null`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var v APIDList
			require.NoError(t, json.Unmarshal([]byte(tc.in), &v), "document を落とさない")
			assert.Equal(t, tc.want, []string(v))
		})
	}
}

// wire 形式は従来どおり: APIDList は array、APLenientID / APLenientHref は
// 素の string で出る。送信側の shape を変えていないことを固定する。
func TestLenientTypes_MarshalUnchanged(t *testing.T) {
	note := Note{
		AttributedTo: "https://e/u",
		InReplyTo:    "https://e/n0",
		To:           APIDList{"https://e/a"},
	}
	b, err := json.Marshal(note)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"attributedTo":"https://e/u"`)
	assert.Contains(t, string(b), `"inReplyTo":"https://e/n0"`)
	assert.Contains(t, string(b), `"to":["https://e/a"]`)

	// omitempty / nil の扱いも従来どおり。
	empty, err := json.Marshal(Note{})
	require.NoError(t, err)
	assert.NotContains(t, string(empty), "inReplyTo")
	assert.NotContains(t, string(empty), `"cc"`)
	// **`url` を送らないことを固定する。** #2729 で inbound 用に足した field で、
	// 「outbound には影響しない」の根拠は `omitempty` 一点。落とすと送信する
	// すべての Note に `"url":""` が乗り、**受信側 Misskey は `""` を falsy と
	// して素通しして保存する**ので (`NoteCreateService`)、相手側の permalink が
	// 壊れる。upstream の `renderNote` も `url` を出さない。
	assert.NotContains(t, string(empty), `"url"`)
	assert.Contains(t, string(empty), `"to":null`, "to は omitempty 無しなので null で出る")
}

// 単一 object / 単一 string の tag・to・cc・inReplyTo・attributedTo・url は
// upstream なら通る。**document ごと落とさない**ことを固定する (#2662)。
func TestPersonNote_LenientShapes(t *testing.T) {
	t.Run("person", func(t *testing.T) {
		body := `{"type":"Person","id":"https://e/u","preferredUsername":"a",
			"tag":{"type":"Emoji","name":":x:"},
			"url":{"type":"Link","href":"https://e/@a"}}`
		var p Person
		require.NoError(t, json.Unmarshal([]byte(body), &p))
		assert.Len(t, p.Tag, 1)
		assert.Equal(t, "https://e/@a", p.URL.String())
	})
	t.Run("note", func(t *testing.T) {
		body := `{"type":"Note","id":"https://e/n1","content":"hi",
			"tag":{"type":"Hashtag","name":"#x"},
			"to":"https://www.w3.org/ns/activitystreams#Public",
			"cc":"https://e/u/followers",
			"inReplyTo":{"id":"https://e/n0"},
			"attributedTo":["https://e/u"]}`
		var n Note
		require.NoError(t, json.Unmarshal([]byte(body), &n))
		assert.Len(t, n.Tag, 1)
		assert.Equal(t, []string{Public}, []string(n.To))
		assert.Equal(t, []string{"https://e/u/followers"}, []string(n.CC))
		assert.Equal(t, "https://e/n0", n.InReplyTo.String())
		assert.Equal(t, "https://e/u", n.AttributedTo.String())
		assert.Equal(t, "hi", n.Content, "他の field を巻き込まない")
	})
}

// APType は upstream getApType と同じく配列の先頭 string を採る。
// JSON-LD の @type は配列表現が正規なので、string 決め打ちだと
// `"type": ["Person"]` で document ごと落ちる (#2662)。
func TestAPType_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"string", `"Person"`, "Person"},
		{"array takes first", `["Person","Service"]`, "Person"},
		// upstream getApType は index 0 しか見ない。走査すると upstream が
		// 型判定不能とする document を受け入れてしまう。
		{"array with non-string head", `[42,"Person"]`, ""},
		{"array of objects", `[{"@id":"as:Person"}]`, ""},
		{"empty array", `[]`, ""},
		{"object", `{"@id":"as:Person"}`, ""},
		{"number", `42`, ""},
		{"null", `null`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var v APType
			require.NoError(t, json.Unmarshal([]byte(tc.in), &v), "document を落とさない")
			assert.Equal(t, tc.want, v.String())
		})
	}
}

// actor / Note の残りの id 系 field も単一 object を受ける。upstream は
// getApId / toArray でこれらを吸収するので、素通しだと mk-go だけが
// document ごと reject する (#2662)。
func TestPerson_LenientIDFields(t *testing.T) {
	body := `{"type":["Person"],"id":"https://e/u","preferredUsername":"a",
		"inbox":"https://e/u/inbox",
		"outbox":{"id":"https://e/u/outbox"},
		"followers":{"id":"https://e/u/followers"},
		"following":{"id":"https://e/u/following"},
		"sharedInbox":{"id":"https://e/inbox"},
		"endpoints":{"sharedInbox":{"id":"https://e/inbox"}},
		"movedTo":{"id":"https://e2/u"},
		"alsoKnownAs":[{"id":"https://old/u"},"https://older/u"]}`
	var p Person
	require.NoError(t, json.Unmarshal([]byte(body), &p))
	assert.Equal(t, "Person", p.Type.String())
	assert.Equal(t, "https://e/u/outbox", p.Outbox.String())
	assert.Equal(t, "https://e/u/followers", p.Followers.String())
	assert.Equal(t, "https://e/u/following", p.Following.String())
	assert.Equal(t, "https://e/inbox", p.SharedInbox.String())
	assert.Equal(t, "https://e/inbox", p.Endpoints.SharedInbox.String())
	assert.Equal(t, "https://e2/u", p.MovedTo.String())
	assert.Equal(t, []string{"https://old/u", "https://older/u"}, []string(p.AlsoKnownAs))
	// inbox は upstream も string を要求する (getApId を通さない) ので素通し。
	assert.Equal(t, "https://e/u/inbox", p.Inbox)
}

// wire 形式は従来どおり素の string / 配列で出る。
func TestPerson_LenientIDFields_MarshalUnchanged(t *testing.T) {
	p := Person{
		Outbox:       "https://e/u/outbox",
		Followers:    "https://e/u/followers",
		Following:    "https://e/u/following",
		SharedInbox:  "https://e/inbox",
		Endpoints:    Endpoints{SharedInbox: "https://e/inbox"},
		Featured:     "https://e/u/featured",
		MovedTo:      "https://e2/u",
		AlsoKnownAs:  APIDList{"https://old/u"},
		URL:          "https://e/@a",
		VcardBday:    "1990-05-03",
		VcardAddress: "Kyoto",
	}
	p.Type = "Person"
	b, err := json.Marshal(p)
	require.NoError(t, err)
	for _, want := range []string{
		`"type":"Person"`,
		`"outbox":"https://e/u/outbox"`,
		`"followers":"https://e/u/followers"`,
		`"following":"https://e/u/following"`,
		`"sharedInbox":"https://e/inbox"`,
		`"endpoints":{"sharedInbox":"https://e/inbox"}`,
		`"featured":"https://e/u/featured"`,
		`"movedTo":"https://e2/u"`,
		`"alsoKnownAs":["https://old/u"]`,
		`"url":"https://e/@a"`,
		`"vcard:bday":"1990-05-03"`,
		`"vcard:Address":"Kyoto"`,
	} {
		assert.Contains(t, string(b), want)
	}

	// 空の Person では omitempty / omitzero が従来どおり効く。
	empty, err := json.Marshal(Person{})
	require.NoError(t, err)
	for _, absent := range []string{
		`"movedTo"`, `"alsoKnownAs"`, `"featured"`, `"url"`,
		`"vcard:bday"`, `"vcard:Address"`, `"endpoints"`, `"sharedInbox"`,
	} {
		assert.NotContains(t, string(empty), absent)
	}
}

// endpoints が object でない actor も落とさない。upstream は
// `x.endpoints ? x.endpoints.sharedInbox : undefined` なので通る。
func TestPerson_EndpointsNonObject(t *testing.T) {
	for _, in := range []string{
		`{"endpoints":"https://e/inbox"}`,
		`{"endpoints":[{"sharedInbox":"https://e/inbox"}]}`,
		`{"endpoints":null}`,
		`{"endpoints":42}`,
	} {
		var p Person
		require.NoError(t, json.Unmarshal([]byte(in), &p), in)
		assert.Empty(t, p.Endpoints.SharedInbox.String())
	}
	// 正しい形は従来どおり読める。
	var p Person
	require.NoError(t, json.Unmarshal([]byte(`{"endpoints":{"sharedInbox":{"id":"https://e/inbox"}}}`), &p))
	assert.Equal(t, "https://e/inbox", p.Endpoints.SharedInbox.String())
}

// featured が object でも actor を落とさない。upstream は getApId を通す。
func TestPerson_FeaturedIDObject(t *testing.T) {
	var p Person
	body := `{"featured":{"id":"https://e/u/featured","type":"OrderedCollection"}}`
	require.NoError(t, json.Unmarshal([]byte(body), &p))
	assert.Equal(t, "https://e/u/featured", p.Featured.String())
}

// MultikeyList は「無い」と「読めなかった」を区別し、配列は要素ごとに decode
// する。呼び出し側 (cacheAssertionMethods) が Unreadable のとき purge を
// 止めるので、この区別が Ed25519 鍵の全消しを防いでいる (#2662)。
func TestMultikeyList_UnmarshalJSON(t *testing.T) {
	const one = `{"id":"https://e/u#k1","type":"Multikey","controller":"https://e/u","publicKeyMultibase":"z6M"}`
	tests := []struct {
		name           string
		in             string
		wantKeys       []string
		wantRefs       []string
		wantUnreadable bool
	}{
		{"array", "[" + one + "]", []string{"https://e/u#k1"}, nil, false},
		{"single object", one, []string{"https://e/u#k1"}, nil, false},
		{"single bare IRI", `"https://e/u#k1"`, nil, []string{"https://e/u#k1"}, false},
		{"empty array", `[]`, nil, nil, false},
		{"null", `null`, nil, nil, false},
		{"string", `"nonsense"`, nil, nil, true},
		// bare IRI は DID Core / FEP-521a の参照形式。鍵素材は無いが
		// 「その keyId を申告している」ことは読めているので Unreadable にしない。
		{"array of bare IRIs", `["https://e/u#k1"]`, nil, []string{"https://e/u#k1"}, false},
		// URL の形をしていない文字列は参照と見なさない (purge を走らせない)。
		{"array of junk strings", `["nonsense"]`, nil, nil, true},
		{"number", `42`, nil, nil, true},
		// **1 件でも読めれば拾う。** 一括 decode に戻すとここが nil になり、
		// 正常な鍵が混ざっていても取り込めなくなる。
		{"one good one broken", "[" + one + `,{"id":42}]`, []string{"https://e/u#k1"}, nil, true},
		{"broken first", `[{"id":42},` + one + "]", []string{"https://e/u#k1"}, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var l MultikeyList
			require.NoError(t, json.Unmarshal([]byte(tc.in), &l), "document を落とさない")
			ids := make([]string, 0, len(l.Keys))
			for _, k := range l.Keys {
				ids = append(ids, k.ID)
			}
			if len(tc.wantKeys) == 0 {
				assert.Empty(t, ids)
			} else {
				assert.Equal(t, tc.wantKeys, ids)
			}
			if len(tc.wantRefs) == 0 {
				assert.Empty(t, l.Refs)
			} else {
				assert.Equal(t, tc.wantRefs, l.Refs)
			}
			assert.Equal(t, tc.wantUnreadable, l.Unreadable)
		})
	}
}

// wire 形式は従来の配列のまま。空なら key ごと消える (omitzero)。
func TestMultikeyList_MarshalUnchanged(t *testing.T) {
	p := Person{AssertionMethod: MultikeyList{Keys: []Multikey{{ID: "https://e/u#k1", Type: MultikeyType}}}}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"assertionMethod":[{"id":"https://e/u#k1"`)

	empty, err := json.Marshal(Person{})
	require.NoError(t, err)
	assert.NotContains(t, string(empty), "assertionMethod",
		"空なら key ごと消える (struct なので omitempty は効かない)")
}

// upstream は `note.source?.mediaType` と optional chaining で読むので、
// 非 object の `source` でも Note は通る。struct 決め打ちだと Note document
// ごと unmarshal に失敗してそのノートが取り込めない (#2662)。
func TestNote_LenientScalarShapes(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"source as string", `{"type":"Note","id":"https://r/1","content":"hi","source":"raw text"}`},
		{"source as array", `{"type":"Note","id":"https://r/1","content":"hi","source":["x"]}`},
		{"sensitive as string", `{"type":"Note","id":"https://r/1","content":"hi","sensitive":"true"}`},
		{"sensitive as number", `{"type":"Note","id":"https://r/1","content":"hi","sensitive":1}`},
		{"quoteUrl as object", `{"type":"Note","id":"https://r/1","content":"hi","quoteUrl":{"id":"https://r/0"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var n Note
			require.NoError(t, json.Unmarshal([]byte(tc.in), &n), "Note document を落とさない")
			assert.Equal(t, "hi", n.Content, "他の field を巻き込まない")
		})
	}

	// 読める形はちゃんと拾う。
	var n Note
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Note","source":{"content":"**mfm**","mediaType":"text/x.misskeymarkdown"},"sensitive":true,"quoteUrl":{"id":"https://r/0"}}`), &n))
	require.NotNil(t, n.Source)
	assert.Equal(t, "text/x.misskeymarkdown", n.Source.MediaType)
	assert.True(t, n.Sensitive.Bool())
	assert.Equal(t, "https://r/0", n.QuoteURL.String())

	// **sensitive は読めなければ true に倒す。** false に倒すと
	// 「送信側が sensitive と宣言したノートが CW 無しで表示される」= 安全側が
	// 逆になる。**upstream 追従ではない** — upstream は sensitive から CW を
	// 作らず (`cw` は `summary` のみ)、mk-go 独自実装の都合 (#2662)。
	//
	// 値の読み方は PostgreSQL の boolean 入力構文に合わせる (JS の truthy では
	// ない)。`"false"` を true と読むと意味が反転する。
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{`true`, true},
		{`false`, false},
		{`"true"`, true},
		{`"false"`, false},
		{`"0"`, false},
		{`"off"`, false},
		{`1`, true},
		{`0`, false},
		{`[true]`, true},
		{`[false]`, false},
		{`[{"@value":false}]`, false},
		{`{"@value":false}`, false},
		{`{"@value":true}`, true},
		{`[]`, true},
		{`null`, false},
		// **数値も PostgreSQL 入力構文。** `node-postgres` は `String(val)` で
		// 送るので `2` は `'2'::boolean` = invalid input syntax。読めない側に
		// 倒して既定値 (隠す) にする。`1.0` は `String(1.0)` が "1" なので true。
		{`2`, true},
		{`1.0`, true},
		{`0.0`, false},
		{`-0`, false},
		// レンジ外は JS では `Infinity` になり PostgreSQL が受けない。
		{`1e999`, true},
		// PostgreSQL が受け付けない形は既定値 (= 隠す側)。
		{`""`, true},
		{`[false,true]`, true},
		{`{"a":1}`, true},
	} {
		var n2 Note
		require.NoError(t, json.Unmarshal([]byte(`{"type":"Note","sensitive":`+tc.in+"}"), &n2), tc.in)
		assert.Equal(t, tc.want, n2.Sensitive.Bool(), tc.in)
	}
}

// **boolean は PostgreSQL の入力構文で読む。JS の truthy ではない。**
// upstream が生値を代入する field は TypeORM の丸めが効かず PostgreSQL が
// キャストするので、`"true"` は true だが `"false"` / `"0"` / `"no"` / `"off"`
// は **false**。JS truthy にすると軒並み反転する (#2662)。
//
// 読めない形の既定値だけ field ごとに違う (`APTruthyBool` = true /
// `APLenientBool` = false)。
func TestPerson_LenientBooleans(t *testing.T) {
	for _, tc := range []struct {
		in         string
		wantLocked bool // manuallyApprovesFollowers (APTruthyBool: 既定 true)
		wantCat    bool // isCat (APLenientBool: 既定 false)
	}{
		{`true`, true, true},
		{`false`, false, false},
		{`"true"`, true, true},
		{`"True"`, true, true},
		{`"t"`, true, true},
		{`"yes"`, true, true},
		{`"on"`, true, true},
		{`"1"`, true, true},
		// **ここが反転していた。** JS truthy だと `"false"` が true になる。
		{`"false"`, false, false},
		{`"f"`, false, false},
		{`"no"`, false, false},
		{`"off"`, false, false},
		{`"0"`, false, false},
		{`1`, true, true},
		{`0`, false, false},
		{`1.0`, true, true},
		// `2` は `'2'::boolean` = invalid input syntax なので読めない扱い。
		{`2`, true, false},
		{`null`, false, false},
		// 空配列は「読めない」= field ごとの既定値。
		{`[]`, true, false},
		// PostgreSQL が受け付けない形 = upstream では insert ごと落ちる。
		// mk-go は field ごとの既定値に倒す。
		{`"hello"`, true, false},
		{`{"a":1}`, true, false},
		{`[true,false]`, true, false},
	} {
		body := `{"type":"Person","manuallyApprovesFollowers":` + tc.in +
			`,"isCat":` + tc.in + "}"
		var p Person
		require.NoError(t, json.Unmarshal([]byte(body), &p), body)
		assert.Equal(t, tc.wantLocked, p.ManuallyApproves.Bool(), "manuallyApprovesFollowers: "+body)
		assert.Equal(t, tc.wantCat, p.IsCat.Bool(), "isCat: "+body)
	}

	// wire 形式は従来どおり素の boolean。omitempty 無しなので false でも出る。
	// discoverable は pointer (#4022) で、RenderPerson と同じく値を入れて出す
	b, err := json.Marshal(Person{Discoverable: new(APLenientBool(false))})
	require.NoError(t, err)
	for _, want := range []string{`"manuallyApprovesFollowers":false`, `"discoverable":false`, `"isCat":false`} {
		assert.Contains(t, string(b), want)
	}
}

// discoverable は、無いときと null を「指定なし」(nil) として区別する (#4022)。
// 本家は無いときに isExplorable を書かない (作るときは既定値の true、更新では今の値)。
func TestPerson_DiscoverableKeepsAbsence(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want *bool
	}{
		{``, nil},
		{`,"discoverable":null`, nil},
		{`,"discoverable":false`, new(false)},
		{`,"discoverable":true`, new(true)},
		{`,"discoverable":"false"`, new(false)},
	} {
		var p Person
		require.NoError(t, json.Unmarshal([]byte(`{"type":"Person"`+tc.in+`}`), &p), tc.in)
		if tc.want == nil {
			assert.Nil(t, p.Discoverable, tc.in)
			continue
		}
		require.NotNil(t, p.Discoverable, tc.in)
		assert.Equal(t, *tc.want, p.Discoverable.Bool(), tc.in)
	}
}

// **`_misskey_canChat` は true = DM 許可なので、読めないときに true へ倒さない。**
// 相手が拒否しているのに送ってしまう。
func TestPerson_CanChatDefaultsToDeny(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{`true`, true},
		{`"true"`, true},
		{`false`, false},
		{`"false"`, false},
		{`"0"`, false},
		{`"off"`, false},
		// 読めない形は拒否側。
		{`"yes please"`, false},
		{`{"a":1}`, false},
	} {
		var p Person
		body := `{"type":"Person","_misskey_canChat":` + tc.in + "}"
		require.NoError(t, json.Unmarshal([]byte(body), &p), body)
		require.NotNil(t, p.MisskeyCanChat, body)
		assert.Equal(t, tc.want, p.MisskeyCanChat.Bool(), body)
	}

	// 未指定は nil のまま (= 旧実装互換で許可)。
	var absent Person
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Person"}`), &absent))
	assert.Nil(t, absent.MisskeyCanChat)
}

// **actor の boolean も JSON-LD 展開形を剥がす。** 剥がさずに既定値へ倒すと
// `manuallyApprovesFollowers` が落ちて承認制のリモートユーザーへのフォローが
// 即成立する (#2662)。
func TestPerson_LenientBooleans_LDForms(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{`true`, true},
		{`[true]`, true},
		{`{"@value":true}`, true},
		{`[{"@value":true}]`, true},
		{`[{"@value":"true"}]`, true},
		{`false`, false},
		{`[false]`, false},
		{`{"@value":false}`, false},
		{`[{"@value":false}]`, false},
		// xsd:boolean の文字列表現も剥がして読む。
		{`[{"@value":"false","@type":"xsd:boolean"}]`, false},
	} {
		var p Person
		body := `{"type":"Person","manuallyApprovesFollowers":` + tc.in + `,"isCat":` + tc.in + "}"
		require.NoError(t, json.Unmarshal([]byte(body), &p), body)
		assert.Equal(t, tc.want, p.ManuallyApproves.Bool(), body)
		assert.Equal(t, tc.want, p.IsCat.Bool(), body)
	}
}

// `tag` / `attachment` がスカラーでも actor / Note を落とさない。
// field の UnmarshalJSON が error を返すと `encoding/json` は**親 struct の
// decode をその場で打ち切る**ので、actor は丸ごと落ち、Note は後続の
// `content` / `summary` / `to` を失う (#2662)。
func TestAPObjectList_ScalarDoesNotAbortParent(t *testing.T) {
	var p Person
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Person","tag":1e999,"preferredUsername":"alice"}`), &p),
		"actor document を落とさない")
	assert.Equal(t, "alice", p.PreferredUsername, "後続 field が消えない")
	assert.Empty(t, p.Tag)

	var n Note
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Note","id":"https://r/1","tag":1e999,"content":"hello","summary":"cw","to":["https://www.w3.org/ns/activitystreams#Public"]}`), &n))
	assert.Equal(t, "hello", n.Content, "tag より後ろの field が消えない")
	assert.Equal(t, "cw", n.Summary.String())
	assert.Equal(t, []string{Public}, []string(n.To))
}

// `source` / `replies` も同じ理由で catch-all では代替できない。
func TestNote_FieldUnmarshalerDoesNotAbortParent(t *testing.T) {
	var n Note
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Note","id":"https://r/1","source":"raw text","content":"hello","summary":"cw"}`), &n))
	assert.Equal(t, "hello", n.Content, "source より後ろの field が消えない")
	assert.Equal(t, "cw", n.Summary.String())

	var q Note
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Question","id":"https://r/1","oneOf":[{"type":"Note","name":"a","replies":1e999}],"content":"hello"}`), &q))
	assert.Equal(t, "hello", q.Content, "replies より後ろの field が消えない")
}

// APLenientInt は整数として先に読む。float64 経由だと巨大な整数が精度を失い、
// 範囲外は実装依存の値 (負値) に化ける。
func TestAPLenientInt_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"int", `3`, 3},
		{"float form", `3.0`, 3},
		// `"3"` / `"3.0"` は encoding/json が json.Number に入れるので上の
		// 分岐で処理される。**string fallback が実際に効くのは JSON number
		// literal でない形**なので、そちらもケースに入れる。
		{"string int", `"3"`, 3},
		{"string float", `"3.0"`, 3},
		{"padded string", `" 3 "`, 3},
		{"signed string", `"+3"`, 3},
		{"leading zeros string", `"007"`, 7},
		{"negative", `-5`, -5},
		{"zero", `0`, 0},
		{"max int64", `9223372036854775807`, math.MaxInt64},
		{"min int64", `-9223372036854775808`, math.MinInt64},
		// **`f <= math.MaxInt64` に戻すとここが負値に化ける。** 定数が float64 に
		// 丸め上がって 2^63 になり、比較をすり抜けて実装依存変換になる。
		{"max int64 as float", `9223372036854775807.0`, 0},
		{"just over max as float", `9223372036854775808`, 0},
		// 下限は float64 で厳密表現できるので読める (`>` にすると 0 に落ちる)。
		{"min int64 as float", `-9223372036854775808.0`, math.MinInt64},
		{"overflow float", `1e999`, 0},
		{"huge float", `1e300`, 0},
		{"object", `{"a":1}`, 0},
		{"null", `null`, 0},
		{"junk string", `"abc"`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var v APLenientInt
			require.NoError(t, json.Unmarshal([]byte(tc.in), &v), "document を落とさない")
			assert.Equal(t, tc.want, v.Int())
		})
	}
}

// Question の選択肢は upstream が `type` を一切見ない (`ApQuestionService` は
// name と replies.totalItems しか読まない)。硬いと **Note ごと reject** する。
// この経路は Normalize を通らない生 fetch (inReplyTo / Announce / featured)
// でも通る (#2662)。
func TestNote_LenientQuestionChoices(t *testing.T) {
	body := `{"type":"Question","id":"https://r/1","content":"poll",
		"oneOf":[
			{"type":["Note"],"name":"a","replies":{"type":["Collection"],"totalItems":3.0}},
			{"type":"Note","name":"b","replies":{"type":"Collection","totalItems":"5"}}
		]}`
	var n Note
	require.NoError(t, json.Unmarshal([]byte(body), &n), "Note document を落とさない")
	require.Len(t, n.OneOf, 2)
	assert.Equal(t, "a", n.OneOf[0].Name)
	assert.Equal(t, 3, n.OneOf[0].Replies.TotalItems.Int(), "3.0 も整数として読む")
	assert.Equal(t, 5, n.OneOf[1].Replies.TotalItems.Int(), `"5" も整数として読む`)

	// wire 形式は従来どおり素の数値。
	out, err := json.Marshal(n)
	require.NoError(t, err)
	assert.Contains(t, string(out), `"totalItems":3`)
}

// upstream が読まない / 型検査しない拡張 field で actor ごと落とさない。
// これらは mk-go 独自 (`_misskey_canChat` は CherryPick 由来) なので、
// 相手が違う型で送ってきても document を通すべき (#2662)。
func TestPerson_LenientMisskeyExtensions(t *testing.T) {
	for _, in := range []string{
		`{"type":"Person","_misskey_requireSigninToViewContents":"true"}`,
		`{"type":"Person","_misskey_canChat":"yes"}`,
		`{"type":"Person","_misskey_followedMessage":42}`,
		`{"type":"Person","_misskey_makeNotesFollowersOnlyBefore":"x"}`,
		`{"type":"Person","_misskey_makeNotesHiddenBefore":[1]}`,
		`{"type":"Person","publicKey":{"publicKeyPem":1}}`,
	} {
		var p Person
		require.NoError(t, json.Unmarshal([]byte(in), &p), in)
	}

	var n Note
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Note","_misskey_talk":"true"}`), &n))
	// PostgreSQL の boolean 入力構文で読むので `"true"` は true。
	assert.True(t, n.MisskeyTalk.Bool())
	var n2 Note
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Note","_misskey_talk":"nonsense"}`), &n2))
	assert.False(t, n2.MisskeyTalk.Bool(), "読めない形は既定 false")

	// 読める形はちゃんと拾う。
	var p Person
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Person","_misskey_canChat":true,"_misskey_makeNotesHiddenBefore":7,"publicKey":{"publicKeyPem":"PEM"}}`), &p))
	require.NotNil(t, p.MisskeyCanChat)
	assert.True(t, p.MisskeyCanChat.Bool())
	require.NotNil(t, p.MisskeyMakeNotesHiddenBefore)
	assert.Equal(t, 7, p.MisskeyMakeNotesHiddenBefore.Int())
	assert.Equal(t, "PEM", p.PublicKey.PublicKeyPEM.String())
}

// `_misskey_summary` / `_misskey_content` も他の `_misskey_*` と同じだけ
// 寛容にする。硬いと actor / Note ごと落ちる (#2662)。
func TestLenientMisskeySummaryAndContent(t *testing.T) {
	for _, in := range []string{
		`{"type":"Person","_misskey_summary":{"@value":"bio"}}`,
		`{"type":"Person","_misskey_summary":42}`,
		`{"type":"Person","_misskey_summary":["bio"]}`,
	} {
		var p Person
		require.NoError(t, json.Unmarshal([]byte(in), &p), in)
	}
	for _, in := range []string{
		`{"type":"Note","_misskey_content":{"@value":"hi"}}`,
		`{"type":"Note","_misskey_content":42}`,
	} {
		var n Note
		require.NoError(t, json.Unmarshal([]byte(in), &n), in)
	}

	var p Person
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Person","_misskey_summary":"bio"}`), &p))
	require.NotNil(t, p.MisskeySummary)
	assert.Equal(t, "bio", p.MisskeySummary.String())
}

// Question の endTime / closed / publicKey.owner も document を落とさない。
func TestLenientPollAndOwnerFields(t *testing.T) {
	for _, in := range []string{
		`{"type":"Question","endTime":{"@value":"2020-01-01T00:00:00Z"}}`,
		`{"type":"Question","closed":["2020-01-01T00:00:00Z"]}`,
	} {
		var n Note
		require.NoError(t, json.Unmarshal([]byte(in), &n), in)
	}
	var p Person
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Person","publicKey":{"owner":{"id":"https://e/u"},"publicKeyPem":"PEM"}}`), &p))
	assert.Equal(t, "PEM", p.PublicKey.PublicKeyPEM.String())
}

// APRawList は単一値も 1 件として拾う。`[]json.RawMessage` 決め打ちだと
// その field の unmarshal が失敗し、**decode の error を見て捨てる呼び出し側
// (featured.go の fetchFeaturedItems) では collection ごと落ちる** (#2662)。
func TestAPRawList_UnmarshalJSON(t *testing.T) {
	var v struct {
		Items        APRawList `json:"items"`
		OrderedItems APRawList `json:"orderedItems"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"items":{"id":"https://a/1"},"orderedItems":["https://a/2","https://a/3"]}`), &v))
	require.Len(t, v.Items, 1)
	assert.JSONEq(t, `{"id":"https://a/1"}`, string(v.Items[0]))
	assert.Len(t, v.OrderedItems, 2)

	var v2 struct {
		Items        APRawList `json:"items"`
		OrderedItems APRawList `json:"orderedItems"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"items":"x","orderedItems":["https://a/2"]}`), &v2))
	// **`encoding/json` は型エラーの後も他 field を decode し続ける**ので、
	// sibling が埋まること自体は素の `[]json.RawMessage` でも同じ。ここで
	// 差が出るのは 1 行上の `require.NoError` (= error にしない) 側。
	assert.Len(t, v2.OrderedItems, 1, "スカラーの兄弟があっても要素を読めること")

	var v3 struct {
		Items APRawList `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"items":null}`), &v3))
	assert.Nil(t, v3.Items)
}

// Note は読める field だけ採用して document を落とさない。upstream は JS なので
// 型検査をほとんどせず、`content` が非 string でも text=null のノートを作る。
// mk-go は struct 決め打ちで丸ごと捨てていた (#2662)。
func TestNote_UnmarshalKeepsReadableFields(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"content object", `{"type":"Note","id":"https://r/1","content":{"@value":"hi"},"summary":"cw"}`},
		{"content number", `{"type":"Note","id":"https://r/1","content":42,"summary":"cw"}`},
		{"summary object", `{"type":"Note","id":"https://r/1","summary":{"@value":"cw"}}`},
		{"name array", `{"type":"Note","id":"https://r/1","name":["title"]}`},
		{"summary unreadable", `{"type":"Note","id":"https://r/1","summary":{"a":1}}`},
		{"oneOf single object", `{"type":"Question","id":"https://r/1","oneOf":{"type":"Note","name":"a"}}`},
		{"anyOf single object", `{"type":"Question","id":"https://r/1","anyOf":{"type":"Note","name":"a"}}`},
		{"choice name non-string", `{"type":"Question","id":"https://r/1","oneOf":[{"type":"Note","name":42}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var n Note
			require.NoError(t, json.Unmarshal([]byte(tc.in), &n), "Note document を落とさない")
			assert.Equal(t, "https://r/1", n.ID, "読めた field は採用する")
		})
	}

	// **読めた兄弟 field は残る。**
	var n Note
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Note","id":"https://r/1","content":{"@value":"hi"},"summary":"cw","to":["https://r/x"]}`), &n))
	assert.Equal(t, "cw", n.Summary.String())
	assert.Equal(t, []string{"https://r/x"}, []string(n.To))
	assert.Empty(t, n.Content, "読めなかった field は zero value")

	// **CW と vote の名前は「読めたら必ず拾う」。** 空へ倒すと、CW 付きの
	// ノートが CW 無しで表示され、AP 投票が本文空の reply note として保存される
	// (upstream は summary に typeof ガードを持たないので値を保持する、#2662)。
	for _, tc := range []struct {
		in          string
		wantSummary string
		wantName    string
	}{
		{`{"type":"Note","summary":["NSFW: gore"],"name":["choice A"]}`, "NSFW: gore", "choice A"},
		{`{"type":"Note","summary":{"@value":"cw"},"name":{"@value":"c"}}`, "cw", "c"},
		{`{"type":"Note","summary":[{"@value":"cw"}]}`, "cw", ""},
		{`{"type":"Note","summary":{"a":1}}`, "", ""},
	} {
		var n Note
		require.NoError(t, json.Unmarshal([]byte(tc.in), &n), tc.in)
		assert.Equal(t, tc.wantSummary, n.Summary.String(), tc.in)
		assert.Equal(t, tc.wantName, n.Name.String(), tc.in)
	}

	// **構文エラーは従来どおり弾く。** ただしこれを保証しているのは
	// `json.Unmarshal` の `checkValid` であって `Note.UnmarshalJSON` ではない
	// (壊れた JSON はこの関数に到達しない)。関数側の `errors.As` 分岐は
	// 「型エラー以外は握らない」という防御で、現状は到達しない。
	var broken Note
	assert.Error(t, json.Unmarshal([]byte(`{"type":"Note",}`), &broken))
}

// `published` は upstream が `new Date(...)` に投げるだけなので、JS の変換規則で
// 単一要素配列や epoch ミリ秒が通る。string 決め打ちだと Note ごと落ちる
// (#2662)。読めない形は空にして呼び出し側の fallback に委ねる。
func TestAPLenientTimestamp_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"string", `"2024-01-01T00:00:00Z"`, "2024-01-01T00:00:00Z"},
		{"single element array", `["2024-01-01T00:00:00Z"]`, "2024-01-01T00:00:00Z"},
		{"value object", `{"@value":"2024-01-01T00:00:00Z"}`, "2024-01-01T00:00:00Z"},
		{"array of value object", `[{"@value":"2024-01-01T00:00:00Z"}]`, "2024-01-01T00:00:00Z"},
		{"epoch millis", `1704067200000`, "2024-01-01T00:00:00Z"},
		{"multi element array", `["a","b"]`, ""},
		{"object", `{"a":1}`, ""},
		{"null", `null`, ""},
		{"bool", `true`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var v APLenientTimestamp
			require.NoError(t, json.Unmarshal([]byte(tc.in), &v), "document を落とさない")
			assert.Equal(t, tc.want, v.String())
		})
	}
}

// upstream が受理する published の形で Note を落とさない。**先行する field の
// 型エラーで判定が飛ぶ順序依存も無いこと**を、content を壊した組み合わせで見る。
func TestNote_LenientPublished(t *testing.T) {
	for _, in := range []string{
		`{"type":"Note","id":"https://r/1","published":["2024-01-01T00:00:00Z"]}`,
		`{"type":"Note","id":"https://r/1","published":1704067200000}`,
		`{"type":"Note","id":"https://r/1","published":0}`,
		`{"type":"Note","id":"https://r/1","published":false}`,
		`{"type":"Note","id":"https://r/1","content":1,"published":["2024-01-01T00:00:00Z"]}`,
	} {
		var n Note
		require.NoError(t, json.Unmarshal([]byte(in), &n), in)
		assert.Equal(t, "https://r/1", n.ID)
	}

	var n Note
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Note","id":"https://r/1","content":1,"published":["2024-01-01T00:00:00Z"]}`), &n))
	assert.Equal(t, "2024-01-01T00:00:00Z", n.Published.String())

	out, err := json.Marshal(Note{Published: "2024-01-01T00:00:00Z"})
	require.NoError(t, err)
	assert.Contains(t, string(out), `"published":"2024-01-01T00:00:00Z"`)
}

// `_misskey_quote` は `quoteUrl` と一緒に resolveQuoteTarget へ渡す値なので
// 同じだけ寛容にする。片方だけ硬いと Note ごと落ちる (#2662)。
func TestNote_LenientMisskeyQuote(t *testing.T) {
	var n Note
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Note","_misskey_quote":{"id":"https://r/0"}}`), &n))
	assert.Equal(t, "https://r/0", n.MisskeyQuote.String())

	out, err := json.Marshal(Note{MisskeyQuote: "https://r/0"})
	require.NoError(t, err)
	assert.Contains(t, string(out), `"_misskey_quote":"https://r/0"`)
}

// AS2 では `replies` を IRI 参照にするのも正規。upstream は optional chaining で
// 読むので票数 0 でアンケートは作られる。硬いと Note ごと落ちる (#2662)。
func TestNote_LenientQuestionReplies(t *testing.T) {
	for _, in := range []string{
		`{"type":"Question","oneOf":[{"type":"Note","name":"a","replies":"https://r/1/replies"}]}`,
		`{"type":"Question","oneOf":[{"type":"Note","name":"a","replies":[{"totalItems":3}]}]}`,
		`{"type":"Question","oneOf":[{"type":"Note","name":"a","replies":42}]}`,
	} {
		var n Note
		require.NoError(t, json.Unmarshal([]byte(in), &n), in)
		require.Len(t, n.OneOf, 1)
		assert.Equal(t, "a", n.OneOf[0].Name)
		require.NotNil(t, n.OneOf[0].Replies)
		assert.Equal(t, 0, n.OneOf[0].Replies.TotalItems.Int(), "読めなければ 0 票")
	}
}

// 1 要素が数値レンジ外なだけで tag / attachment の配列ごと捨てない。
// 捨てると mention / emoji / hashtag が全滅する (#2662)。
func TestAPObjectList_KeepsReadableElements(t *testing.T) {
	var n Note
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Note","tag":[{"type":"Mention","href":"https://e/u"},1e999]}`), &n))
	require.Len(t, n.Tag, 2, "読めた要素は残る")
	m, ok := n.Tag[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Mention", m["type"])
}

// `APIDList` も `APObjectList` と同じく、1 要素が数値レンジ外なだけで
// 配列ごと捨てない。`Person` には `Note` のような catch-all が無いので、
// 捨てると `alsoKnownAs` で actor が丸ごと落ちる (#2662)。
func TestAPIDList_KeepsReadableElements(t *testing.T) {
	var p Person
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Person","alsoKnownAs":["https://old/u",1e999]}`), &p),
		"actor document を落とさない")
	assert.Equal(t, []string{"https://old/u"}, []string(p.AlsoKnownAs))

	var n Note
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Note","to":["https://www.w3.org/ns/activitystreams#Public",1e999]}`), &n))
	assert.Equal(t, []string{Public}, []string(n.To))
}

// actor の `name` / `summary` も JSON-LD の展開形を拾う。**upstream
// `validateActor` は truthy な非 string で throw するので、ここは mk-go の
// ほうが緩い** (#2662)。値は下流で truncate + NUL 除去する。
func TestPerson_LenientNameAndSummary(t *testing.T) {
	for _, tc := range []struct {
		in          string
		wantName    string
		wantSummary string
	}{
		{`{"type":"Person","name":{"@value":"Alice"},"summary":{"@value":"bio"}}`, "Alice", "bio"},
		{`{"type":"Person","name":["Alice"],"summary":["bio"]}`, "Alice", "bio"},
		{`{"type":"Person","name":[{"@value":"Alice"}]}`, "Alice", ""},
		{`{"type":"Person","name":0,"summary":false}`, "", ""},
		{`{"type":"Person","name":{"a":1},"summary":["a","b"]}`, "", ""},
	} {
		var p Person
		require.NoError(t, json.Unmarshal([]byte(tc.in), &p), tc.in)
		assert.Equal(t, tc.wantName, p.Name.String(), tc.in)
		assert.Equal(t, tc.wantSummary, p.Summary.String(), tc.in)
	}

	out, err := json.Marshal(Person{Summary: "bio"})
	require.NoError(t, err)
	assert.Contains(t, string(out), `"summary":"bio"`)
}

// TypeOf は decode 済みの値から AS `type` を取る (生 body を map で覗く経路用)。
// `APType` / `apTypeOf` / `flattenType` と同じ head 方式。
func TestTypeOf(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"string", "Note", "Note"},
		{"array head string", []any{"Note", "Article"}, "Note"},
		{"array head non-string", []any{42, "Note"}, ""},
		{"empty array", []any{}, ""},
		{"object", map[string]any{"@id": "as:Note"}, ""},
		{"nil", nil, ""},
		{"number", float64(42), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, TypeOf(tc.in))
		})
	}
}

// URLOrEmpty は nil receiver を許す (icon / image は省略されうる)。
func TestImage_URLOrEmpty(t *testing.T) {
	var nilImage *Image
	assert.Equal(t, "", nilImage.URLOrEmpty(), "nil receiver で panic しない")
	assert.Equal(t, "", (&Image{}).URLOrEmpty())
	assert.Equal(t, "https://e/a.png", (&Image{URL: "https://e/a.png"}).URLOrEmpty())

	// Person 経由でも同じ (icon 省略時に落ちないこと)。
	var p Person
	require.NoError(t, json.Unmarshal([]byte(`{"type":"Person"}`), &p))
	assert.Equal(t, "", p.Icon.URLOrEmpty())
}
