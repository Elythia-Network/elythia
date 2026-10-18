package paramshadow

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/api/paramschema"
)

func TestReadBodyPutsTheSameBytesBack(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader(`{"a":1}`))
	data, tooLarge, err := readBody(req, DefaultMaxBody)
	if err != nil || tooLarge || string(data) != `{"a":1}` {
		t.Fatalf("readBody = %q, %v, %v", data, tooLarge, err)
	}
	again, _ := io.ReadAll(req.Body)
	if string(again) != `{"a":1}` {
		t.Fatalf("handler reads %q", again)
	}

	for _, body := range []io.ReadCloser{nil, http.NoBody} {
		req := httptest.NewRequest(http.MethodPost, "/api/x", nil)
		req.Body = body
		if data, tooLarge, err := readBody(req, DefaultMaxBody); data != nil || tooLarge || err != nil || req.Body != body {
			t.Errorf("empty body %v: %q, %v, %v", body, data, tooLarge, err)
		}
	}
}

// TestReadBodyLimit pins the boundary: a body of exactly max bytes is read,
// one byte more is not, and the handler reads the whole body either way.
func TestReadBodyLimit(t *testing.T) {
	const max = 16
	for _, n := range []int{max - 1, max, max + 1, 3 * max} {
		body := strings.Repeat("a", n)
		req := httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader(body))
		data, tooLarge, err := readBody(req, max)
		if err != nil {
			t.Fatal(err)
		}
		if wantLarge := n > max; tooLarge != wantLarge || (tooLarge && data != nil) || (!tooLarge && string(data) != body) {
			t.Errorf("%d bytes: data %d bytes, tooLarge %v", n, len(data), tooLarge)
		}
		if got, _ := io.ReadAll(req.Body); string(got) != body {
			t.Errorf("%d bytes: handler reads %d bytes", n, len(got))
		}
	}
	if DefaultMaxBody != 65536 {
		t.Errorf("DefaultMaxBody = %d; the docs say 64KiB", DefaultMaxBody)
	}
}

func TestReadBodyKeepsTheReadError(t *testing.T) {
	boom := errors.New("boom")
	req := httptest.NewRequest(http.MethodPost, "/api/x", nil)
	req.Body = io.NopCloser(io.MultiReader(strings.NewReader(`{"a"`), errReader{boom}))
	if _, _, err := readBody(req, DefaultMaxBody); !errors.Is(err, boom) {
		t.Fatalf("readBody err = %v", err)
	}
	// handlerは、読めた分と同じ誤りを受け取る。
	got, err := io.ReadAll(req.Body)
	if string(got) != `{"a"` || !errors.Is(err, boom) {
		t.Fatalf("handler reads %q, %v", got, err)
	}
}

// esc writes a JSON escape: every "~" in s becomes a backslash, so the test
// cases keep their \u escapes as written.
func esc(s string) []byte { return []byte(strings.ReplaceAll(s, "~", `\`)) }

func TestJSONInput(t *testing.T) {
	in := jsonInput(esc(`{"limit":10.0,"big":1e400,"s":"~ud83d~ude00"}`))
	if in.skip != "" {
		t.Fatalf("skip = %s", in.skip)
	}
	m := in.data.(map[string]any)
	// UseNumberで読む。1e400はfloat64では読めない(本家はInfinity)。
	if m["limit"] != json.Number("10.0") || m["big"] != json.Number("1e400") || m["s"] != "\U0001F600" {
		t.Errorf("data = %#v", m)
	}

	for body, want := range map[string]string{
		"{\"a\":\"\xff\"}":           SkipInvalidUTF8,
		`{"a":"~ud800"}`:             SkipLoneSurrogate,
		`{"a":`:                      SkipBody,
		`[1,2]`:                      "",
		`{"a":"~~ud800"}`:            "",
		`{"a":"~u00e9~uD83D~uDE00"}`: "",
	} {
		if got := jsonInput(esc(body)).skip; got != want {
			t.Errorf("jsonInput(%s).skip = %q, want %q", body, got, want)
		}
	}
}

func TestHasLoneSurrogate(t *testing.T) {
	for body, want := range map[string]bool{
		`{"a":"plain"}`:                   false,
		`{"a":"~u00e9"}`:                  false,
		`{"a":"~ud83d~ude00"}`:            false,
		`{"a":"~uD83D~uDE00"}`:            false,
		`{"a":"~uD800"}`:                  true,
		`{"a":"~uDFFF"}`:                  true,
		`{"a":"~uDBFF~uDC00"}`:            false,
		`{"a":"~ud800"}`:                  true,
		`{"a":"~udc00"}`:                  true,
		`{"a":"~ud800~u0041"}`:            true,
		`{"a":"~ud800~~u0041"}`:           true,
		`{"a":"~ude00~ud83d"}`:            true,
		`{"a":"x~ud83d"}`:                 true,
		`{"a":"~~ud800"}`:                 false,
		`{"a":"~~~ud800"}`:                true,
		`{"a":"~"~ud83d~ude00~""}`:        false,
		`{"~ud800":1}`:                    true,
		`{"a":["~ud83d~ude00","~udfff"]}`: true,
		// 正しいJSONでは来ない形でも、範囲の外を読まない。
		`~u12`:       false,
		`~uzzzz`:     false,
		`~`:          false,
		`~ud83d~u`:   true,
		`~ud83d~uzz`: true,
	} {
		if got := hasLoneSurrogate(esc(body)); got != want {
			t.Errorf("hasLoneSurrogate(%s) = %v, want %v", body, got, want)
		}
	}
}

func mustCompile(t *testing.T, def string) *paramschema.Schema {
	t.Helper()
	s, err := paramschema.Compile([]byte(def))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestQueryInput(t *testing.T) {
	s := mustCompile(t, `{"type":"object","properties":{
		"span":{"type":"string"},"limit":{"type":"integer"},"flag":{"type":"boolean"},"ratio":{"type":"number"}}}`)

	in := queryInput("span=day&limit=10&flag=true&ratio=1e400&x=1&x=2&span2=10", s)
	want := map[string]any{
		"span": "day", "limit": json.Number("10"), "flag": true, "ratio": json.Number("1e400"),
		"x": []any{"1", "2"}, "span2": "10",
	}
	if in.skip != "" || in.castFailed != "" || !reflect.DeepEqual(in.data, want) {
		t.Errorf("queryInput = %+v", in)
	}

	// 文字列の項目は変換しない。配列になった値も変換しない(本家は文字列のときだけ)。
	in = queryInput("span=10&limit=1&limit=2", s)
	if want := map[string]any{"span": "10", "limit": []any{"1", "2"}}; !reflect.DeepEqual(in.data, want) {
		t.Errorf("queryInput = %+v", in)
	}

	// 変換できない値は、schemaの順で最初のものが本家の答えになる。
	for q, want := range map[string]string{
		"ratio=x&limit=y": "limit",
		"flag=yes":        "flag",
		"limit=":          "limit",
		"limit=1%202":     "limit",
		"limit=%20%201":   "",
	} {
		if got := queryInput(q, s).castFailed; got != want {
			t.Errorf("queryInput(%s).castFailed = %q, want %q", q, got, want)
		}
	}

	for q, want := range map[string]string{
		"a=1;b=2":   SkipQuery,
		"a=%zz":     SkipQuery,
		"a=%ff":     SkipInvalidUTF8,
		"%ff=1":     SkipInvalidUTF8,
		"a=1&a=%ff": SkipInvalidUTF8,
		"":          "",
	} {
		if got := queryInput(q, s).skip; got != want {
			t.Errorf("queryInput(%s).skip = %q, want %q", q, got, want)
		}
	}
}

// TestTooManyValues pins the boundary of the token limit: exactly max tokens
// pass, one more does not. Each key, scalar and bracket is one token.
func TestTooManyValues(t *testing.T) {
	for _, tt := range []struct {
		body string
		max  int
		want bool
	}{
		{`{}`, 2, false},
		{`{}`, 1, true},
		{`{"a":1}`, 4, false}, // { "a" 1 }
		{`{"a":1}`, 3, true},
		{`[1e400,1]`, 4, false}, // 1e400もTokenの誤りにしない
		{`[1e400,1]`, 3, true},
		{`[{},{},{}]`, 8, false},
		{`[{},{},{}]`, 7, true},
		{`"s"`, 1, false},
	} {
		if got := tooManyValues([]byte(tt.body), tt.max); got != tt.want {
			t.Errorf("tooManyValues(%s, %d) = %v, want %v", tt.body, tt.max, got, tt.want)
		}
	}
	if DefaultMaxValues != 4096 {
		t.Errorf("DefaultMaxValues = %d; the docs say 4096", DefaultMaxValues)
	}
}

func TestQueryTooManyValues(t *testing.T) {
	for _, tt := range []struct {
		q    string
		max  int
		want bool
	}{
		{"", 0, false},
		{"a=1", 1, false},
		{"a=1&b=2", 2, false},
		{"a=1&b=2&c=3", 2, true},
		{"a=1&a=2", 1, true},
	} {
		if got := queryTooManyValues(tt.q, tt.max); got != tt.want {
			t.Errorf("queryTooManyValues(%q, %d) = %v, want %v", tt.q, tt.max, got, tt.want)
		}
	}
}

// decoderTokens counts the tokens json.Decoder.Token returns for body.
func decoderTokens(t *testing.T, body []byte) int {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	n := 0
	for {
		if _, err := dec.Token(); err == io.EOF {
			return n
		} else if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		n++
	}
}

// TestTooManyValuesMatchesDecoderToken checks the byte scanner against
// json.Decoder.Token: for every input of the ajv corpus (#3673) and for
// hand-written bodies with escapes, numbers and literals next to every
// delimiter, the scanner reports more than c-1 tokens and not more than c,
// where c is what Token counts.
func TestTooManyValuesMatchesDecoderToken(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "api", "paramschema", "testdata", "ajv_corpus.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var bodies [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var line struct {
			In json.RawMessage `json:"in"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		if line.In != nil {
			bodies = append(bodies, line.In)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	// 何も読めずに緑になるのを防ぐ下限。
	if len(bodies) < 20000 {
		t.Fatalf("read only %d corpus inputs", len(bodies))
	}
	for _, s := range []string{
		`{"a\"b":"c\\","d":[true,false,null,-1.5e+3,0]}`,
		`{"k":"~u005c~"}"}`,
		` [ 1 , "x" , { "y" : [ ] } , null ] `,
		"[1,\n2,\t3\r]",
		`{"":""}`,
		`-0`,
		`"]"`,
	} {
		bodies = append(bodies, esc(s))
	}
	for _, b := range bodies {
		c := decoderTokens(t, b)
		if tooManyValues(b, c) {
			t.Errorf("%s: more than %d tokens by the scanner", b, c)
		}
		if c > 0 && !tooManyValues(b, c-1) {
			t.Errorf("%s: not more than %d tokens by the scanner (Token counts %d)", b, c-1, c)
		}
	}
}

// TestReadBodyAllocatesOnceWithContentLength pins that a body of known
// length is read into one buffer of about that size, not grown from 512
// bytes (#3677 review 2 M-1).
func TestReadBodyAllocatesOnceWithContentLength(t *testing.T) {
	body := strings.Repeat("a", 40000)
	allocs := testing.AllocsPerRun(20, func() {
		req := httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader(body))
		data, _, _ := readBody(req, DefaultMaxBody)
		if len(data) != len(body) || cap(data) > len(body)+1+bytes.MinRead {
			t.Fatalf("len %d cap %d", len(data), cap(data))
		}
	})
	// リクエストの組み立ての分を除いて、bodyの写しは1回(多くて数回の小さな確保)。
	reqOnly := testing.AllocsPerRun(20, func() {
		_ = httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader(body))
	})
	if allocs-reqOnly > 4 {
		t.Errorf("readBody allocates %v times", allocs-reqOnly)
	}
}

// countingReader counts the Read calls on it.
type countingReader struct {
	r     io.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

// TestReadBodyDoesNotReadOverContentLength pins that a body whose
// Content-Length is over max is reported as too large without reading it or
// allocating, and that the handler gets the body untouched (#3677 review 3
// L-1, L-2).
func TestReadBodyDoesNotReadOverContentLength(t *testing.T) {
	const max = 64
	for _, cl := range []int64{max + 1, 1 << 20} {
		cr := &countingReader{r: strings.NewReader(strings.Repeat("a", int(cl)))}
		req := httptest.NewRequest(http.MethodPost, "/api/x", nil)
		body := io.NopCloser(cr)
		req.Body, req.ContentLength = body, cl
		data, tooLarge, err := readBody(req, max)
		if data != nil || !tooLarge || err != nil || cr.reads != 0 || req.Body != body {
			t.Errorf("Content-Length %d: data %d bytes, tooLarge %v, err %v, reads %d, body replaced %v",
				cl, len(data), tooLarge, err, cr.reads, req.Body != body)
		}
		allocs := testing.AllocsPerRun(10, func() { _, _, _ = readBody(req, max) })
		if allocs != 0 {
			t.Errorf("Content-Length %d: %v allocations", cl, allocs)
		}
	}
	// 上限ちょうどのContent-Lengthは読む。
	req := httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader(strings.Repeat("a", max)))
	if data, tooLarge, _ := readBody(req, max); tooLarge || len(data) != max {
		t.Errorf("Content-Length at max: %d bytes, tooLarge %v", len(data), tooLarge)
	}
}

// TestReadBodyUnknownLength pins that a body of unknown length (chunked) is
// read into one buffer of max+1+MinRead, and that one over max is still
// found too large with the whole body left for the handler.
func TestReadBodyUnknownLength(t *testing.T) {
	const max = DefaultMaxBody
	body := strings.Repeat("a", 60000)
	newReq := func(b string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader(b))
		req.ContentLength = -1
		return req
	}
	allocs := testing.AllocsPerRun(20, func() {
		data, tooLarge, _ := readBody(newReq(body), max)
		if tooLarge || len(data) != len(body) || cap(data) > max+1+bytes.MinRead {
			t.Fatalf("len %d cap %d tooLarge %v", len(data), cap(data), tooLarge)
		}
	})
	reqOnly := testing.AllocsPerRun(20, func() { _ = newReq(body) })
	if allocs-reqOnly > 4 {
		t.Errorf("readBody allocates %v times for a body of unknown length", allocs-reqOnly)
	}
	big := strings.Repeat("b", max+10)
	req := newReq(big)
	if _, tooLarge, _ := readBody(req, max); !tooLarge {
		t.Error("an unknown-length body over max was not too large")
	}
	if got, _ := io.ReadAll(req.Body); string(got) != big {
		t.Errorf("the handler reads %d bytes", len(got))
	}
}
