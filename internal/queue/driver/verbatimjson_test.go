package driver

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// IsVerbatimJSON は「埋め込んで書き出しても 1 バイトも変わらない」値だけを
// 受け入れること (#3614)。受信の本文は受付で Digest と照合したバイト列で、
// worker はそれをそのまま処理する。
func TestIsVerbatimJSON(t *testing.T) {
	nested := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"compact object", `{"a":1,"b":[true,null,"x"]}`, true},
		{"escaped html from json.Marshal", `{"c":"\u003cp\u003e\u0026"}`, true},
		{"non-ascii", `{"c":"日本語"}`, true},
		{"escaped quote and brackets in string", `{"c":"\"[[{"}`, true},
		{"space in string is kept", `{"c":"a b"}`, true},
		{"depth 64", nested(64), true},
		{"empty", ``, false},
		{"space between tokens", `{"a": 1}`, false},
		{"newline", "{\"a\":1}\n", false},
		{"tab", "{\"a\":\t1}", false},
		{"raw < in string", `{"c":"<p>"}`, false},
		{"raw > in string", `{"c":">"}`, false},
		{"raw & in string", `{"c":"&"}`, false},
		{"U+2028", "{\"c\":\"\u2028\"}", false},
		{"U+2029", "{\"c\":\"\u2029\"}", false},
		{"invalid utf-8", "{\"c\":\"\xff\"}", false},
		{"depth 65", nested(65), false},
		{"unclosed object", `{"a":1`, false},
		{"unclosed string", `{"a":"x}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsVerbatimJSON([]byte(tc.in)); got != tc.want {
				t.Fatalf("IsVerbatimJSON(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// IsVerbatimJSON が受け入れた値は、json.Marshal で書き出して読み戻すと元の
// バイト列に戻ること。encoding/json の振る舞い (compact と HTML のエスケープ) に
// 依存しているので、Go の版を上げて変わったら落ちるようにしておく。
func TestIsVerbatimJSON_RoundTripsThroughEncodingJSON(t *testing.T) {
	for _, in := range []string{
		`{"a":1,"b":[true,null,"x"]}`,
		`{"c":"\u003cp\u003e\u0026\u2028"}`,
		`{"c":"日本語 \"q\" \\ \/"}`,
		`[1,2.5e10,-0,{"k":{}}]`,
	} {
		b := []byte(in)
		if !IsVerbatimJSON(b) {
			t.Fatalf("expected %q to be verbatim", in)
		}
		out, err := json.Marshal(struct {
			V json.RawMessage `json:"v"`
		}{json.RawMessage(b)})
		if err != nil {
			t.Fatal(err)
		}
		var back struct {
			V json.RawMessage `json:"v"`
		}
		if err := json.Unmarshal(out, &back); err != nil {
			t.Fatal(err)
		}
		if string(back.V) != in {
			t.Fatalf("round trip changed bytes:\n got %s\nwant %s", back.V, in)
		}
	}
}

// 文法の検査は json.Marshal に任せるので、構造の形だけ整った不正な JSON も
// 受け入れる。その場合は書き出しが *json.MarshalerError で失敗し、
// IsInvalidRawJSON がそれを見分けること。
func TestIsInvalidRawJSON(t *testing.T) {
	bad := []byte(`{"a":}`)
	if !IsVerbatimJSON(bad) {
		t.Fatal("shape-only check should accept {\"a\":}")
	}
	_, err := json.Marshal(struct {
		V json.RawMessage `json:"v"`
	}{json.RawMessage(bad)})
	if !IsInvalidRawJSON(fmt.Errorf("mkq: marshal payload: %w", err)) {
		t.Fatalf("IsInvalidRawJSON(%v) = false", err)
	}
	if IsInvalidRawJSON(errors.New("redis: connection refused")) {
		t.Fatal("IsInvalidRawJSON should not match unrelated errors")
	}
	if IsInvalidRawJSON(nil) {
		t.Fatal("IsInvalidRawJSON(nil) = true")
	}
}

// Misskey の本文 (生の <) は、書き換えた後に全部を戻せば元に戻る。元から
// エスケープの並びを含む本文は、書き換えの結果と区別できないので埋め込まない。
func TestClassifyJSONEmbedding(t *testing.T) {
	esc := `\` + `u003c`
	for _, tc := range []struct {
		name string
		in   string
		want JSONEmbedding
	}{
		{"verbatim", `{"a":"x"}`, EmbedVerbatim},
		{"escaped by json.Marshal", `{"a":"` + esc + `"}`, EmbedVerbatim},
		{"raw html", `{"a":"<p>&amp;</p>"}`, EmbedHTMLEscaped},
		{"raw U+2028", "{\"a\":\"\u2028\"}", EmbedHTMLEscaped},
		{"raw html and an escape", `{"a":"<","b":"` + esc + `"}`, NotEmbeddable},
		{"raw html and an escape after an escaped backslash", `{"a":"<","b":"\\` + esc[1:] + `"}`, NotEmbeddable},
		{"raw html and uppercase escape", `{"a":"<","b":"\u003C"}`, EmbedHTMLEscaped},
		{"raw html with spaces", `{"a": "<"}`, NotEmbeddable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := func(got JSONEmbedding) {
				if got != tc.want {
					t.Fatalf("ClassifyJSONEmbedding(%q) = %v, want %v", tc.in, got, tc.want)
				}
			}
			assert(ClassifyJSONEmbedding([]byte(tc.in)))
		})
	}
}

// EmbedHTMLEscaped の値は、json.Marshal で書き出して読み戻し、UnescapeJSONHTML を
// かけると元のバイト列に戻ること。
func TestUnescapeJSONHTML_RestoresEncodingJSONOutput(t *testing.T) {
	for _, in := range []string{
		`{"content":"<p>今日は<b>晴れ</b> &amp; 散歩</p>","n":1}`,
		"{\"a\":\"\u2028\u2029<>&\"}",
		`{"a":"<","b":"\\","c":"\u003C","d":"\u00e9"}`,
		`{"a":"<","b":"\\u003"}`,
	} {
		b := []byte(in)
		if ClassifyJSONEmbedding(b) != EmbedHTMLEscaped {
			t.Fatalf("expected %q to be EmbedHTMLEscaped", in)
		}
		out, err := json.Marshal(struct {
			V json.RawMessage `json:"v"`
		}{json.RawMessage(b)})
		if err != nil {
			t.Fatal(err)
		}
		var back struct {
			V json.RawMessage `json:"v"`
		}
		if err := json.Unmarshal(out, &back); err != nil {
			t.Fatal(err)
		}
		if string(back.V) == in {
			t.Fatalf("expected encoding/json to escape %q", in)
		}
		// 自前のエスケープは encoding/json と同じバイト列を出す (継ぎ足した payload を
		// mkq がもう一度 json.Marshal しても変わらない)。
		if esc := string(EscapeJSONHTML(b)); esc != string(back.V) {
			t.Fatalf("EscapeJSONHTML differs from encoding/json:\n got %s\nwant %s", esc, back.V)
		}
		if got := string(UnescapeJSONHTML(EscapeJSONHTML(b))); got != in {
			t.Fatalf("UnescapeJSONHTML(EscapeJSONHTML) did not restore: %s", got)
		}
		if got := string(UnescapeJSONHTML(back.V)); got != in {
			t.Fatalf("UnescapeJSONHTML did not restore:\n got %s\nwant %s", got, in)
		}
	}
}
