package paramschema

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

const (
	corpusFile = "testdata/ajv_corpus.jsonl"
	// elythiaCorpusFile is the corpus of elythia_paramdefs.json (#3869, make
	// elythia-paramdef-gen).
	elythiaCorpusFile = "testdata/ajv_corpus_elythia.jsonl"
)

type corpusHeader struct {
	MisskeyVersion  string `json:"misskeyVersion"`
	AJV             string `json:"ajv"`
	ParamdefsSha256 string `json:"paramdefsSha256"`
	// ElythiaParamdefsSha256 is set on the corpus of elythia_paramdefs.json.
	ElythiaParamdefsSha256 string `json:"elythiaParamdefsSha256"`
	Cases                  int    `json:"cases"`
}

type corpusLine struct {
	EP string `json:"ep"`
	// ParamDef and CompileError are set on the endpoint line of a synthetic
	// schema (tools/paramdefspec/synthetic.json).
	ParamDef     json.RawMessage `json:"paramDef"`
	CompileError string          `json:"compileError"`

	In   json.RawMessage `json:"in"`
	OK   *bool           `json:"ok"`
	Out  json.RawMessage `json:"out"`
	Path string          `json:"path"`
	Msg  string          `json:"msg"`
}

type corpusCase struct {
	ep   string
	line int
	corpusLine
}

func readCorpus(t *testing.T) (corpusHeader, map[string]corpusLine, []corpusCase) {
	t.Helper()
	return readCorpusFile(t, corpusFile)
}

func readCorpusFile(t *testing.T, file string) (corpusHeader, map[string]corpusLine, []corpusCase) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read corpus (make paramdef-gen): %v", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	var h corpusHeader
	var cases []corpusCase
	eps := map[string]corpusLine{}
	ep := ""
	for n := 1; sc.Scan(); n++ {
		if n == 1 {
			if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
				t.Fatalf("corpus header: %v", err)
			}
			continue
		}
		var l corpusLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("corpus line %d: %v", n, err)
		}
		if l.EP != "" {
			if _, dup := eps[l.EP]; dup {
				t.Fatalf("corpus line %d: endpoint %s appears twice", n, l.EP)
			}
			ep = l.EP
			eps[ep] = l
			continue
		}
		if ep == "" || l.OK == nil || len(l.In) == 0 {
			t.Fatalf("corpus line %d: malformed", n)
		}
		cases = append(cases, corpusCase{ep: ep, line: n, corpusLine: l})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return h, eps, cases
}

// normalize re-decodes v through JSON into float64 numbers, so that values
// built with json.Number, float64 defaults and JavaScript's output compare
// equal when JavaScript sees the same value.
func normalize(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// decodeInput decodes a corpus input like a handler would.
func decodeInput(raw []byte, useNumber bool) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if useNumber {
		dec.UseNumber()
	}
	var v any
	err := dec.Decode(&v)
	return v, err
}

// TestConformsToAJVCorpus replays every case ajv judged against every
// upstream paramDef (and the synthetic schemas of tools/paramdefspec), and
// requires the same verdict, the same first error (schemaPath and message)
// and, for accepted input, the same value after the defaults were inserted.
// No case is excluded.
func TestConformsToAJVCorpus(t *testing.T) {
	g := readGolden(t)
	h, eps, cases := readCorpus(t)
	sum := sha256.Sum256(g.raw)
	if h.ParamdefsSha256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("%s was not built from the current %s (run make paramdef-gen)", corpusFile, goldenFile)
	}
	if h.AJV != g.AJV.Version || h.MisskeyVersion != g.MisskeyVersion {
		t.Fatalf("corpus header %+v does not match the golden (ajv %s, Misskey %s)", h, g.AJV.Version, g.MisskeyVersion)
	}
	if len(cases) != h.Cases {
		t.Fatalf("corpus has %d cases, header says %d", len(cases), h.Cases)
	}
	schemas := map[string]*Schema{}
	for name, ep := range g.Endpoints {
		if _, ok := eps[name]; !ok {
			t.Errorf("corpus lacks %s", name)
		}
		s, err := Compile(ep.ParamDef)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		schemas[name] = s
	}
	synthetic := 0
	for name, l := range eps {
		if !strings.HasPrefix(name, "synthetic:") {
			if _, ok := g.Endpoints[name]; !ok {
				t.Errorf("corpus has %s, which the golden does not", name)
			}
			continue
		}
		synthetic++
		s, err := Compile(l.ParamDef)
		if l.CompileError != "" {
			// ajvがcompileで落とすもの(strict modeなど)は、こちらも落とす。
			if err == nil {
				t.Errorf("%s: ajv rejects the schema (%s), Compile accepted it", name, l.CompileError)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: ajv compiles the schema, Compile: %v", name, err)
		}
		schemas[name] = s
	}
	if synthetic == 0 {
		t.Fatal("corpus has no synthetic schemas")
	}
	seenEP := map[string]int{}
	failures := 0
	for _, c := range cases {
		s := schemas[c.ep]
		if s == nil {
			t.Fatalf("line %d: unknown endpoint %s", c.line, c.ep)
		}
		seenEP[c.ep]++
		for _, useNumber := range []bool{true, false} {
			data, err := decodeInput(c.In, useNumber)
			if err != nil {
				// float64へは1e400をdecodeできない(UseNumberの方で見る)。
				if !useNumber {
					continue
				}
				t.Fatalf("line %d: %v", c.line, err)
			}
			if msg := checkCase(t, s, c, data); msg != "" {
				failures++
				if failures <= 30 {
					t.Errorf("line %d %s in=%s useNumber=%v: %s", c.line, c.ep, c.In, useNumber, msg)
				}
			}
		}
	}
	if failures > 0 {
		t.Fatalf("%d mismatches", failures)
	}
	// 何も読めずに緑になるのを防ぐ下限。生成器はどのendpointにも、正しい入力と
	// 型の違う入力(null、配列、文字列、数、真偽値)を必ず作る。
	for name := range schemas {
		if _, upstream := g.Endpoints[name]; upstream && seenEP[name] < 6 || seenEP[name] == 0 {
			t.Errorf("corpus has only %d cases for %s", seenEP[name], name)
		}
	}
}

// readElythiaParamDefs returns the paramDefs of elythia_paramdefs.json by
// endpoint name.
func readElythiaParamDefs(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	var f struct {
		Endpoints map[string]struct {
			ParamDef json.RawMessage `json:"paramDef"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(ElythiaParamDefs(), &f); err != nil {
		t.Fatal(err)
	}
	out := map[string]json.RawMessage{}
	for name, ep := range f.Endpoints {
		out[name] = ep.ParamDef
	}
	return out
}

// TestElythiaConformsToAJVCorpus replays every case ajv judged against the
// paramDefs of elythia_paramdefs.json (#3869) on the schemas Effective
// compiled from them, like TestConformsToAJVCorpus does for upstream's.
func TestElythiaConformsToAJVCorpus(t *testing.T) {
	g := readGolden(t)
	h, eps, cases := readCorpusFile(t, elythiaCorpusFile)
	sum := sha256.Sum256(g.raw)
	esum := sha256.Sum256(ElythiaParamDefs())
	if h.ParamdefsSha256 != hex.EncodeToString(sum[:]) || h.ElythiaParamdefsSha256 != hex.EncodeToString(esum[:]) {
		t.Fatalf("%s was not built from the current %s and elythia_paramdefs.json (run make elythia-paramdef-gen)", elythiaCorpusFile, goldenFile)
	}
	if h.AJV != g.AJV.Version {
		t.Fatalf("corpus header %+v does not match the golden (ajv %s)", h, g.AJV.Version)
	}
	if len(cases) != h.Cases {
		t.Fatalf("corpus has %d cases, header says %d", len(cases), h.Cases)
	}
	eff, err := Effective()
	if err != nil {
		t.Fatal(err)
	}
	declared := readElythiaParamDefs(t)
	if len(declared) == 0 {
		t.Fatal("elythia_paramdefs.json declares no endpoint")
	}
	for name := range declared {
		if _, ok := eps[name]; !ok {
			t.Errorf("corpus lacks %s", name)
		}
		if ep := eff[name]; !ep.Elythia || ep.Schema == nil {
			t.Errorf("Effective does not have %s as an Elythia endpoint", name)
		}
	}
	for name := range eps {
		if _, ok := declared[name]; !ok {
			t.Errorf("corpus has %s, which elythia_paramdefs.json does not", name)
		}
	}
	seenEP := map[string]int{}
	failures := 0
	for _, c := range cases {
		ep, ok := eff[c.ep]
		if !ok || ep.Schema == nil {
			t.Fatalf("line %d: unknown endpoint %s", c.line, c.ep)
		}
		seenEP[c.ep]++
		for _, useNumber := range []bool{true, false} {
			data, err := decodeInput(c.In, useNumber)
			if err != nil {
				if !useNumber {
					continue
				}
				t.Fatalf("line %d: %v", c.line, err)
			}
			if msg := checkCase(t, ep.Schema, c, data); msg != "" {
				failures++
				if failures <= 30 {
					t.Errorf("line %d %s in=%s useNumber=%v: %s", c.line, c.ep, c.In, useNumber, msg)
				}
			}
		}
	}
	if failures > 0 {
		t.Fatalf("%d mismatches", failures)
	}
	for name := range declared {
		if seenEP[name] < 6 {
			t.Errorf("corpus has only %d cases for %s", seenEP[name], name)
		}
	}
}

func checkCase(t *testing.T, s *Schema, c corpusCase, data any) string {
	err := s.Validate(data)
	if *c.OK {
		if err != nil {
			return fmt.Sprintf("ajv accepted, got %v", err)
		}
		want := c.In
		if len(c.Out) > 0 {
			want = c.Out
		}
		var w any
		if e := json.Unmarshal(want, &w); e != nil {
			t.Fatal(e)
		}
		if got := normalize(t, data); !reflect.DeepEqual(got, w) {
			return fmt.Sprintf("after defaults: got %v, want %s", got, want)
		}
		return ""
	}
	if err == nil {
		return fmt.Sprintf("ajv rejected with %s %q, got valid", c.Path, c.Msg)
	}
	if err.SchemaPath != c.Path || err.Message != c.Msg {
		return fmt.Sprintf("got %s %q, want %s %q", err.SchemaPath, err.Message, c.Path, c.Msg)
	}
	return ""
}

// TestElythiaCorpusAcceptsEveryAnyOfBranch requires, for each branch of a
// top-level anyOf in elythia_paramdefs.json, an accepted case that has the
// branch's required properties and misses one of every other branch.
// 分岐を1つしか通らないcorpusでは、2つ目以降の分岐を通る入力が、検証器とも
// handlerとも一度も照合されない(#3872のchat/messages/createのtoRoomId)。
func TestElythiaCorpusAcceptsEveryAnyOfBranch(t *testing.T) {
	_, _, cases := readCorpusFile(t, elythiaCorpusFile)
	branches := 0
	for name, raw := range readElythiaParamDefs(t) {
		var pd struct {
			AnyOf []struct {
				Required []string `json:"required"`
			} `json:"anyOf"`
		}
		if err := json.Unmarshal(raw, &pd); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for i, br := range pd.AnyOf {
			branches++
			found := false
			for _, c := range cases {
				var in map[string]any
				if c.ep != name || c.OK == nil || !*c.OK || json.Unmarshal(c.In, &in) != nil {
					continue
				}
				if hasAll(in, br.Required) && missesOneOfEveryOther(in, pd.AnyOf, i) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: no accepted case takes only anyOf/%d (fix tools/paramdefspec/corpus.mjs)", name, i)
			}
		}
	}
	// 宣言にanyOfが無くなったら、この検査は何も見ない。
	if branches == 0 {
		t.Error("no anyOf in elythia_paramdefs.json; drop this test")
	}
}

func hasAll(in map[string]any, keys []string) bool {
	for _, k := range keys {
		if _, ok := in[k]; !ok {
			return false
		}
	}
	return true
}

func missesOneOfEveryOther(in map[string]any, branches []struct {
	Required []string `json:"required"`
}, self int) bool {
	for j, br := range branches {
		if j != self && hasAll(in, br.Required) {
			return false
		}
	}
	return true
}
