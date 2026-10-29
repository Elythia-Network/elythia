package mkqdriver

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// roundTrip writes f the way mkq stores the job data and reads it back.
func roundTrip(t *testing.T, f framedPayload) (string, framedPayload) {
	t.Helper()
	data, err := json.Marshal(f)
	require.NoError(t, err)
	var back framedPayload
	require.NoError(t, json.Unmarshal(data, &back))
	return string(data), back
}

// 書く側を有効にしたときの形 (#3614): JSON の payload は base64 にせず、そのまま
// 埋め込むこと。それ以外は以前の形 (base64) で運び、どちらもバイト列として元
// どおりに戻ること。
func TestFramedPayload_Shapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload []byte
		wire    string
	}{
		{"json", []byte(`{"a":1}`), `{"type":"t","bodyJson":{"a":1}}`},
		{"non-json", []byte("hello"), `{"type":"t","body":"aGVsbG8="}`},
		{"json with spaces", []byte(`{"a": 1}`), `{"type":"t","body":"eyJhIjogMX0="}`},
		{"empty", nil, `{"type":"t"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := compactFramedPayload("t", tc.payload)
			data, back := roundTrip(t, f)
			assert.Equal(t, tc.wire, data)
			assert.Equal(t, "t", back.Type)
			assert.Equal(t, string(tc.payload), string(back.payload()))
		})
	}
}

// **#3614 より前に積まれた job を読めること。** 文字列は以前の版が書いた data
// そのもの。
func TestFramedPayload_ReadsDataWrittenBefore3614(t *testing.T) {
	for _, tc := range []struct {
		data string
		want string
	}{
		{`{"type":"ap:deliver","body":"eyJhIjoxfQ=="}`, `{"a":1}`},
		{`{"type":"maintenance:x"}`, ``},
		// bodyJson が null なら body を見る。
		{`{"type":"t","body":"eyJhIjoxfQ==","bodyJson":null}`, `{"a":1}`},
		// 両方あるときは bodyJson を見る (レビュー 3 周目の L-1。書く側は片方しか
		// 出さないが、読む順を固定しておく)。
		{`{"type":"t","body":"eyJhIjoxfQ==","bodyJson":{"b":2}}`, `{"b":2}`},
		{`{"type":"t","bodyJson":{"b":2},"body":"eyJhIjoxfQ=="}`, `{"b":2}`},
	} {
		var f framedPayload
		require.NoError(t, json.Unmarshal([]byte(tc.data), &f), tc.data)
		assert.Equal(t, tc.want, string(f.payload()), tc.data)
	}
}

// **古い版の Elythia が新しい形の job を読んでも、job の組み立ては失敗しない
// こと。** 新しい形を別の key (bodyJson) に置いた理由。body の値をオブジェクトに
// 変えていたら、古い版は []byte として読めずに mkq の buildJob ごと失敗し、
// 管理画面の一覧も 1 件のせいで読めなくなる。古い版は本文の無い job として扱う
// (戻したときの扱いは docs/divergence/config.md)。
func TestFramedPayload_OldReaderSeesNoBody(t *testing.T) {
	type pre3614 struct {
		Type string `json:"type"`
		Body []byte `json:"body,omitempty"`
	}
	data, err := json.Marshal(compactFramedPayload("ap:deliver", []byte(`{"inbox":"x","body":{"type":"Create"}}`)))
	require.NoError(t, err)
	var old pre3614
	require.NoError(t, json.Unmarshal(data, &old))
	assert.Equal(t, "ap:deliver", old.Type)
	assert.Empty(t, old.Body)
}

// **積む job の形は driver.CompactJobPayloads に従うこと (#3614)。** 有効なら
// compactFramedPayload と同じ形を書く。false に戻したら、#3614 より前の版と同じ
// data (payload が base64) を書く。
func TestFramedPayload_WritesTheShapeSelectedByCompactJobPayloads(t *testing.T) {
	type pre3614 struct {
		Type string `json:"type"`
		Body []byte `json:"body,omitempty"`
	}
	for _, payload := range [][]byte{[]byte(`{"a":1}`), []byte("hello"), nil} {
		got, err := json.Marshal(newFramedPayload("t", payload))
		require.NoError(t, err)
		want, err := json.Marshal(pre3614{Type: "t", Body: payload})
		require.NoError(t, err)
		if driver.CompactJobPayloads {
			want, err = json.Marshal(compactFramedPayload("t", payload))
			require.NoError(t, err)
		}
		assert.Equal(t, string(want), string(got))
	}
}

// 埋め込みの候補が JSON として読めなかったら (mkq の json.Marshal が
// *json.MarshalerError で失敗する)、以前の形で積み直すこと。それ以外の失敗は
// そのまま返し、積み直さない。
func TestAddFramed_FallsBackOnInvalidEmbeddedJSON(t *testing.T) {
	// mkq と同じく data を json.Marshal し、失敗は包んで返す。
	var added []string
	add := func(f framedPayload) error {
		data, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("mkq: marshal payload: %w", err)
		}
		added = append(added, string(data))
		return nil
	}
	bad := []byte(`{"a":}`)
	f := compactFramedPayload("t", bad)
	require.True(t, f.embedded(), "形だけ整った不正な JSON も埋め込みの候補になる")
	require.NoError(t, addFramed(f, "t", bad, add))
	assert.Equal(t, []string{`{"type":"t","body":"eyJhIjp9"}`}, added)

	added = nil
	require.NoError(t, addFramed(compactFramedPayload("t", []byte(`{"a":1}`)), "t", []byte(`{"a":1}`), add))
	assert.Equal(t, []string{`{"type":"t","bodyJson":{"a":1}}`}, added)

	calls := 0
	redisErr := errors.New("redis: connection refused")
	err := addFramed(f, "t", bad, func(framedPayload) error { calls++; return redisErr })
	assert.ErrorIs(t, err, redisErr)
	assert.Equal(t, 1, calls, "Marshal の失敗でなければ積み直さない")
}

// **本文が上限ちょうど (64 段) の payload も、包みは新しい形になること**
// (#3614 のレビュー L-1)。payload は本文を 1 段内側に持つので 65 段になる。
// 包みを以前の形にすると、#3614 より前の版は本文 (bodyEscaped) を知らない key
// として捨て、エラーにならずに空の本文で処理を進める。
func TestCompactFramedPayload_EmbedsPayloadsOfMaxDepthBodies(t *testing.T) {
	body := `{"a":"<","b":` + strings.Repeat("[", driver.MaxVerbatimJSONDepth-1) + strings.Repeat("]", driver.MaxVerbatimJSONDepth-1) + `}`
	require.Equal(t, driver.EmbedHTMLEscaped, driver.ClassifyJSONEmbedding([]byte(body)))
	payload := []byte(`{"bodyEscaped":` + string(driver.EscapeJSONHTML([]byte(body))) + `}`)
	f := compactFramedPayload("ap:inbox", payload)
	assert.True(t, f.embedded())
	_, back := roundTrip(t, f)
	assert.Equal(t, string(payload), string(back.payload()))
}

// driver.WithVerbatimJSONPayload の印は、書く側を有効にしたときだけ効き、
// 印の無い payload は今までどおり newFramedPayload の形になる (#3614)。
func TestFramePayload_UsesTheVerbatimMarkOnlyWhenWritingCompactShapes(t *testing.T) {
	for _, payload := range [][]byte{[]byte(`{"a":1}`), []byte(`{ "a": 1 }`), []byte("hello"), nil} {
		assert.Equal(t, newFramedPayload("t", payload), framePayload("t", payload, false))
		want := legacyFramedPayload("t", payload)
		if driver.CompactJobPayloads {
			want = trustedFramedPayload("t", payload)
		}
		assert.Equal(t, want, framePayload("t", payload, true))
	}
}

// 印の付いた payload は、バイト列の検査をせずに埋め込む。オブジェクトでない
// ものは印があっても埋め込まず、印の無いときと同じ判定に回す (#3614)。
func TestTrustedFramedPayload_SkipsTheByteCheckForObjects(t *testing.T) {
	// 空白を含むので、検査をすれば埋め込まない形。
	spaced := []byte(`{ "a": 1 }`)
	require.False(t, compactFramedPayload("t", spaced).embedded())
	got := trustedFramedPayload("t", spaced)
	assert.True(t, got.embedded(), "a marked object is embedded without the byte check")
	assert.Equal(t, string(spaced), string(got.BodyJSON))

	for _, payload := range [][]byte{[]byte("hello"), []byte(`[1]`), []byte(`"s"`), {}, nil} {
		assert.Equal(t, compactFramedPayload("t", payload), trustedFramedPayload("t", payload))
	}
}

// 印が誤っていて JSON として読めない payload でも、mkq の Marshal の失敗で
// 以前の形に積み直し、job を落とさない (#3614)。
func TestAddFramed_FallsBackForAMarkedPayloadThatIsNotJSON(t *testing.T) {
	var added []string
	add := func(f framedPayload) error {
		data, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("mkq: marshal payload: %w", err)
		}
		added = append(added, string(data))
		return nil
	}
	bad := []byte(`{"a":}`)
	require.NoError(t, addFramed(trustedFramedPayload("t", bad), "t", bad, add))
	assert.Equal(t, []string{`{"type":"t","body":"eyJhIjp9"}`}, added)
}
