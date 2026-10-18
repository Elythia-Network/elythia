package paramschema

import (
	"bytes"
	"os"
	"reflect"
	"testing"
)

func TestUpstreamIsTheGolden(t *testing.T) {
	b, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, upstreamGolden) {
		t.Fatal("embedded golden differs from the file")
	}
	eps, err := Upstream()
	if err != nil {
		t.Fatal(err)
	}
	g := readGolden(t)
	if len(eps) != len(g.Endpoints) {
		t.Fatalf("Upstream has %d endpoints, the golden %d", len(eps), len(g.Endpoints))
	}
	for name, ep := range eps {
		if ep.CompileErr != nil || ep.Schema == nil {
			t.Errorf("%s: %v", name, ep.CompileErr)
		}
	}
	if !eps["drive/files/create"].RequireFile || eps["notes/create"].RequireFile {
		t.Error("RequireFile is not meta.requireFile")
	}
	if !eps["charts/notes"].AllowGet || eps["notes/timeline"].AllowGet {
		t.Error("AllowGet is not meta.allowGet")
	}
	// 同じmapを返す(1度だけcompileする)。
	again, _ := Upstream()
	if reflect.ValueOf(again).Pointer() != reflect.ValueOf(eps).Pointer() {
		t.Error("Upstream compiled the golden twice")
	}
}

func TestParseUpstream(t *testing.T) {
	if _, err := parseUpstream([]byte(`{"endpoints":`)); err == nil {
		t.Error("broken golden parsed")
	}
	eps, err := parseUpstream([]byte(`{"endpoints":{
		"ok":{"paramDef":{"type":"object","properties":{"a":{"type":"string"}}},"meta":{"allowGet":true}},
		"bad":{"paramDef":{"type":"object","properties":{"a":{"type":"string","not":{}}}},"meta":{"requireFile":true}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ep := eps["ok"]; ep.Schema == nil || ep.CompileErr != nil || !ep.AllowGet || ep.RequireFile {
		t.Errorf("ok: %+v", ep)
	}
	if ep := eps["bad"]; ep.Schema != nil || ep.CompileErr == nil || ep.AllowGet || !ep.RequireFile {
		t.Errorf("bad: %+v", ep)
	}
}

func TestScalarParams(t *testing.T) {
	s, err := Compile([]byte(`{"type":"object","properties":{
		"limit":{"type":"integer"},"q":{"type":"string"},"flag":{"type":"boolean","nullable":true},
		"ratio":{"type":"number"},"n":{"type":"null"},"any":{},"obj":{"type":"object"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []ScalarParam{{"limit", "integer"}, {"flag", "boolean"}, {"ratio", "number"}}
	if got := s.ScalarParams(); !reflect.DeepEqual(got, want) {
		t.Errorf("ScalarParams = %v, want %v", got, want)
	}
	empty, _ := Compile([]byte(`{}`))
	if got := empty.ScalarParams(); got != nil {
		t.Errorf("ScalarParams of {} = %v", got)
	}
}
