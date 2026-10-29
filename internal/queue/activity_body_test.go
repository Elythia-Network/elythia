package queue

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// wireShape reports which shape the activity took in a marshalled payload.
func wireShape(t *testing.T, payload []byte) string {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(payload, &m))
	if _, ok := m["bodyEscaped"]; ok {
		_, both := m["body"]
		require.False(t, both, "body と bodyEscaped を両方は書かない")
		return "escaped"
	}
	if _, ok := m["bodyText"]; ok {
		_, both := m["body"]
		require.False(t, both, "body と bodyText を両方は書かない")
		return "text"
	}
	body, ok := m["body"]
	require.True(t, ok, "body が無い: %s", payload)
	switch body[0] {
	case '{':
		return "object"
	case '"':
		return "base64"
	case 'n':
		return "null"
	}
	t.Fatalf("unexpected body: %s", body)
	return ""
}

func activityBodyCases(t *testing.T) []struct {
	name  string
	body  []byte
	shape string
} {
	t.Helper()
	// mk-go が描く activity は json.Marshal の出力なので < などはエスケープ済み。
	ours, err := json.Marshal(map[string]any{"type": "Create", "object": map[string]any{"content": "<p>こんにちは &amp; hi</p>"}})
	require.NoError(t, err)
	// Misskey の JSON.stringify は < をエスケープしない。
	misskey := []byte(`{"type":"Create","object":{"content":"<p>今日はいい天気でした</p>","_misskey_content":"今日はいい天気でした"}}`)
	dense := []byte(`{"c":"` + strings.Repeat("<a>&</a>", 50) + `"}`)
	// 元から < のエスケープの並び (6 バイト) を含む本文は、書き換えの結果と区別
	// できないので埋め込まない。
	esc := `\` + `u003c`
	pad := `,"pad":"` + strings.Repeat("x", 200) + `"`
	mixed := []byte(`{"a":"<p>","b":"` + esc + `"` + pad + `}`)
	return []struct {
		name  string
		body  []byte
		shape string
	}{
		{"json.Marshal output", ours, "object"},
		{"raw html from misskey", misskey, "escaped"},
		{"raw html and an escape sequence", mixed, "text"},
		{"escape sequence after an escaped backslash", []byte(`{"a":"<","b":"\\` + esc[1:] + `"` + pad + `}`), "text"},
		{"html dense: base64 is smaller", dense, "base64"},
		{"not compact", []byte("{ \"type\": \"Create\" }\n"), "text"},
		{"json string", []byte(`"abc"`), "text"},
		{"array", []byte(`[{"a":1}]`), "text"},
		{"shape ok but invalid json", []byte(`{"a":}`), "text"},
		{"too deep to embed", []byte(`{"a":` + strings.Repeat("[", 64) + strings.Repeat("]", 64) + `}`), "text"},
		{"invalid utf-8", []byte("{\"a\":\"\xff\"}"), "base64"},
		// エスケープした引用符の後ろの空白は文字列の中、"b" の後ろの空白は外
		// (#3614 のレビュー L-3。文字列の中の読み飛ばしを壊すと、外の空白を見落とす)。
		{"space after an escaped quote", []byte(`{"a":"\" ","b": 1` + pad + `}`), "text"},
		{"escaped quote then compact", []byte(`{"a":"\" ","b":1` + pad + `}`), "object"},
		// U+2028 / U+2029 は encoding/json が書き換えるので、エスケープして戻す。
		{"raw U+2028 and U+2029", []byte("{\"a\":\"line\u2028sep\u2029end\"" + pad + "}"), "escaped"},
		{"empty", []byte{}, "text"},
		{"nil", nil, "null"},
	}
}

// 配送と受信の payload は、本文を最も小さい形で書き、どの形からもバイト列として
// 元どおりに読み戻すこと (#3614)。受信の本文は受付で Digest と照合したバイト列
// で、worker はそれをそのまま処理する。
func TestActivityBody_RoundTripsByteForByte(t *testing.T) {
	for _, tc := range activityBodyCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			dp := DeliverPayload{Inbox: "https://remote.example/inbox", Body: tc.body, KeyID: "https://local.example/users/u1#main-key", SignerUserID: "u1", IsSharedInbox: true, NotAfter: 123}
			ip := InboxPayload{Body: tc.body, Method: "POST", Path: "/inbox", Headers: map[string]string{"Digest": "SHA-256=x"}}

			// 書く側を有効にしたときの形 (MarshalCompactJSON)。
			compact, err := dp.MarshalCompactJSON()
			require.NoError(t, err)
			assert.Equal(t, tc.shape, wireShape(t, compact))
			reencoded, err := json.Marshal(json.RawMessage(compact))
			require.NoError(t, err)
			assert.Equal(t, string(compact), string(reencoded), "mkq がもう一度 json.Marshal しても変わらない")
			compactIn, err := ip.MarshalCompactJSON()
			require.NoError(t, err)
			assert.Equal(t, tc.shape, wireShape(t, compactIn))

			// この版が書く形 (MarshalJSON)。EnqueueDeliver は json.Marshal を通さずに
			// 直接呼ぶので、両者は同じバイト列でなければならない。
			current, err := dp.MarshalJSON()
			require.NoError(t, err)
			viaMarshal, err := json.Marshal(dp)
			require.NoError(t, err)
			assert.Equal(t, string(viaMarshal), string(current))
			currentIn, err := ip.MarshalJSON()
			require.NoError(t, err)

			// どの形からも、元のバイト列と他の field を読み戻す。
			for name, raw := range map[string][]byte{"compact": compact, "current": current} {
				got, err := DecodeDeliverPayload(raw)
				require.NoError(t, err, name)
				assertSameBody(t, tc.body, got.Body)
				got.Body = dp.Body
				assert.Equal(t, dp, got, name)

				var viaUnmarshal DeliverPayload
				require.NoError(t, json.Unmarshal(raw, &viaUnmarshal), name)
				assertSameBody(t, tc.body, viaUnmarshal.Body)
			}
			for name, raw := range map[string][]byte{"compact": compactIn, "current": currentIn} {
				gotIn, err := DecodeInboxPayload(raw)
				require.NoError(t, err, name)
				assertSameBody(t, tc.body, gotIn.Body)
				gotIn.Body = ip.Body
				assert.Equal(t, ip, gotIn, name)
			}
		})
	}
}

// **この版が積む payload は以前の形 (本文が base64) のまま (#3614 の 1 段目)。**
// 書く側を有効にする (driver.CompactJobPayloads) までは、#3614 より前の版と同じ
// バイト列を書く。そうしておけば、この版から前の版へ戻しても積まれた job を読める。
// 有効にした後は MarshalCompactJSON と同じ形を書く (切り替えが効いているかも見る)。
func TestActivityBody_WritesTheShapeSelectedByCompactJobPayloads(t *testing.T) {
	for _, tc := range activityBodyCases(t) {
		dp := DeliverPayload{Inbox: "https://remote.example/inbox?a=1&b=<x>", Body: tc.body, KeyID: "k", Ed25519KeyID: "e", IsSharedInbox: true, SignerUserID: "u1", NotAfter: 123}
		ip := InboxPayload{Body: tc.body, Method: "POST", Path: "/inbox?a=1&b=<x>", Headers: map[string]string{"Digest": "SHA-256=x", "Signature": `keyId="https://remote.example/u#k&<>"`}}
		wantDeliver, wantInbox := pre3614DeliverJSON(t, dp), pre3614InboxJSON(t, ip)
		if driver.CompactJobPayloads {
			var err error
			wantDeliver, err = dp.MarshalCompactJSON()
			require.NoError(t, err)
			wantInbox, err = ip.MarshalCompactJSON()
			require.NoError(t, err)
		}
		got, err := dp.MarshalJSON()
		require.NoError(t, err)
		assert.Equal(t, string(wantDeliver), string(got), tc.name)
		got, err = json.Marshal(ip)
		require.NoError(t, err)
		assert.Equal(t, string(wantInbox), string(got), tc.name)
	}
}

func assertSameBody(t *testing.T, want, got []byte) {
	t.Helper()
	assert.True(t, bytes.Equal(want, got), "body changed:\n got %q\nwant %q", got, want)
	assert.Equal(t, want == nil, got == nil, "nil と空を区別して戻す")
}

// **#3614 より前に積まれた job (本文が base64) を読めること。** 再送を待つ job は
// Redis に約 32 時間 (ブレーカーで待たされる job はもっと長く) 残るので、更新した
// 直後の worker はこの形を受け取る。
// 文字列は以前の版の json.Marshal の出力をそのまま書いてある (形を固定する)。
func TestActivityBody_ReadsPayloadsWrittenBefore3614(t *testing.T) {
	activity := []byte(`{"type":"Create"}`)
	require.Equal(t, "eyJ0eXBlIjoiQ3JlYXRlIn0=", base64.StdEncoding.EncodeToString(activity))

	deliver := `{"inbox":"https://remote.example/inbox","body":"eyJ0eXBlIjoiQ3JlYXRlIn0=","keyId":"https://local.example/users/u1#main-key","keyPem":"PEM","ed25519KeyId":"https://local.example/users/u1#ed25519-key","isSharedInbox":true,"signerUserId":"u1","notAfter":1760000000000}`
	got, err := DecodeDeliverPayload([]byte(deliver))
	require.NoError(t, err)
	assert.Equal(t, DeliverPayload{
		Inbox:         "https://remote.example/inbox",
		Body:          activity,
		KeyID:         "https://local.example/users/u1#main-key",
		KeyPEM:        "PEM",
		Ed25519KeyID:  "https://local.example/users/u1#ed25519-key",
		IsSharedInbox: true,
		SignerUserID:  "u1",
		NotAfter:      1760000000000,
	}, got)

	inbox := `{"body":"eyJ0eXBlIjoiQ3JlYXRlIn0=","method":"POST","path":"/inbox?x=1","headers":{"Digest":"SHA-256=x","Signature":"keyId=\"k\""}}`
	gotIn, err := DecodeInboxPayload([]byte(inbox))
	require.NoError(t, err)
	assert.Equal(t, InboxPayload{
		Body:    activity,
		Method:  "POST",
		Path:    "/inbox?x=1",
		Headers: map[string]string{"Digest": "SHA-256=x", "Signature": `keyId="k"`},
	}, gotIn)

	// 本文の無い job (body: null) と、Host だけを持つ古い受信の job。
	gotNull, err := DecodeInboxPayload([]byte(`{"body":null,"host":"remote.example"}`))
	require.NoError(t, err)
	assert.Nil(t, gotNull.Body)
	assert.Equal(t, "remote.example", gotNull.Host)

	// 壊れた base64 は今までどおり decode の失敗になる (processor は再試行しない)。
	_, err = DecodeDeliverPayload([]byte(`{"inbox":"x","body":"%%%"}`))
	require.Error(t, err)
}

// quotedLen は encoding/json が文字列を書き出したときの長さと一致すること。
// 文字列の形と base64 の小さい方を選ぶのに使う。
func TestQuotedLen_MatchesEncodingJSON(t *testing.T) {
	for _, s := range []string{
		"", "abc", `"q"`, `back\slash`, "<p>&amp;</p>", "日本語", "\n\r\t\b\f", "\x00\x01\x1f\x7f",
		"line" + string(rune(0x2028)) + "sep" + string(rune(0x2029)), `{"a":"<b>"}`,
	} {
		enc, err := json.Marshal(s)
		require.NoError(t, err)
		assert.Equal(t, len(enc)-2, quotedLen([]byte(s)), "%q", s)
	}
}

// **worker が読む優先の順は bodyEscaped > bodyText > body** (レビュー 3 周目の
// L-1)。書く側は 1 つの形しか出さないが、管理画面 (`embedActivityBody`) は
// この順に揃えてあるので、こちらを変えると両者で見える本文がずれる。
func TestActivityBody_DecodePrefersBodyEscapedThenBodyText(t *testing.T) {
	escaped := string(driver.EscapeJSONHTML([]byte(`{"from":"<escaped>"}`)))
	for name, tc := range map[string]struct {
		raw  string
		want string
	}{
		"all three":          {`{"path":"/inbox","body":{"from":"body"},"bodyText":"{\"from\":\"text\"}","bodyEscaped":` + escaped + `}`, `{"from":"<escaped>"}`},
		"escaped and base64": {`{"path":"/inbox","body":"eyJhIjoxfQ==","bodyEscaped":` + escaped + `}`, `{"from":"<escaped>"}`},
		"text and object":    {`{"path":"/inbox","body":{"from":"body"},"bodyText":"{\"from\":\"text\"}"}`, `{"from":"text"}`},
		"text and base64":    {`{"path":"/inbox","body":"eyJhIjoxfQ==","bodyText":"{\"from\":\"text\"}"}`, `{"from":"text"}`},
	} {
		got, err := DecodeInboxPayload([]byte(tc.raw))
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, string(got.Body), name)
		gotDeliver, err := DecodeDeliverPayload([]byte(tc.raw))
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, string(gotDeliver.Body), name)
	}
}

// resetDeliverBodyMemo empties deliverBodyMemo for the duration of a test.
//
// deliverBodyMemo はプロセス全体で 1 つなので、これを使うテストは t.Parallel に
// しない。並列の別のテストが配送を積むと、覚えた値が入れ替わって落ちる。
func resetDeliverBodyMemo(t *testing.T) {
	t.Helper()
	reset := func() {
		deliverBodyMemo.mu.Lock()
		deliverBodyMemo.body, deliverBodyMemo.field = nil, activityBodyField{}
		deliverBodyMemo.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// **同じ本文の形の判定は使い回し、中身が変われば判定し直す** (#3614)。1 つの投稿は
// inbox の数だけ同じ本文で積まれるので、宛先ごとに判定しない。本文の配列が後から
// 書き換えられても、前の本文の判定を返さない。
func TestEncodeDeliverActivityBody_ReusesTheShapeOnlyForTheSameBytes(t *testing.T) {
	resetDeliverBodyMemo(t)
	// < を生で含むので bodyEscaped の形になり、値は本文とは別の配列に作られる。
	body := []byte(`{"type":"Create","object":{"content":"<p>` + strings.Repeat("今日は晴れ", 20) + `</p>"}}`)

	first := encodeDeliverActivityBody(body)
	require.Equal(t, encodeActivityBody(body), first)
	require.Equal(t, "bodyEscaped", first.key)

	again := encodeDeliverActivityBody(bytes.Clone(body))
	assert.Equal(t, first, again)
	assert.True(t, &first.value[0] == &again.value[0], "the same bytes must reuse the memoized shape")

	// 同じ長さのまま、呼び出し元の配列を書き換える。
	copy(body, `{"type":"Delete"`)
	changed := encodeDeliverActivityBody(body)
	assert.Equal(t, encodeActivityBody(body), changed)
	assert.Contains(t, string(changed.value), `"Delete"`)
}

// nil (null) と空 (文字列の "") は書く形が違うので、覚えた値と取り違えない。
func TestEncodeDeliverActivityBody_KeepsNilAndEmptyApart(t *testing.T) {
	resetDeliverBodyMemo(t)
	assert.Equal(t, encodeActivityBody(nil), encodeDeliverActivityBody(nil))
	assert.Equal(t, encodeActivityBody([]byte{}), encodeDeliverActivityBody([]byte{}))
	assert.Equal(t, encodeActivityBody(nil), encodeDeliverActivityBody(nil))
	assert.NotEqual(t, encodeActivityBody(nil), encodeActivityBody([]byte{}))
}

// 配送の payload の書き出しは、覚えた判定を通る (受信は 1 件ずつなので通さない)。
func TestDeliverPayload_MarshalCompactJSONGoesThroughTheMemo(t *testing.T) {
	resetDeliverBodyMemo(t)
	body := []byte(`{"type":"Create","object":{"content":"hello"}}`)
	_, err := DeliverPayload{Inbox: "https://remote.example/inbox", Body: body}.MarshalCompactJSON()
	require.NoError(t, err)
	assert.Equal(t, string(body), string(deliverBodyMemo.body))
}
