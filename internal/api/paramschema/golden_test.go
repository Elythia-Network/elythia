package paramschema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/elythia-network/elythia/internal/upstreamsrc"
)

const goldenFile = "upstream_paramdefs.json"

type golden struct {
	raw            []byte
	MisskeyVersion string `json:"misskeyVersion"`
	AJV            struct {
		Version string         `json:"version"`
		Options map[string]any `json:"options"`
		Formats map[string]struct {
			Source string `json:"source"`
			Flags  string `json:"flags"`
		} `json:"formats"`
	} `json:"ajv"`
	Endpoints map[string]struct {
		ParamDef json.RawMessage `json:"paramDef"`
		Meta     json.RawMessage `json:"meta"`
	} `json:"endpoints"`
}

func readGolden(t *testing.T) golden {
	t.Helper()
	b, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatalf("read %s (make paramdef-gen): %v", goldenFile, err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatalf("%s: %v", goldenFile, err)
	}
	g.raw = b
	return g
}

// TestGoldenMatchesPinnedUpstream fails when UPSTREAM_MISSKEY_VERSION moved
// but the golden was not regenerated from that version.
func TestGoldenMatchesPinnedUpstream(t *testing.T) {
	g := readGolden(t)
	v, err := upstreamsrc.Version(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if g.MisskeyVersion != v {
		t.Fatalf("%s is from Misskey %s, UPSTREAM_MISSKEY_VERSION is %s (run make upstream-fetch paramdef-gen)", goldenFile, g.MisskeyVersion, v)
	}
	// 何も読めずに緑になるのを防ぐ下限(実際の件数はmake paramdef-genが出す)。
	if len(g.Endpoints) < 400 {
		t.Fatalf("golden has only %d endpoints", len(g.Endpoints))
	}
	for _, name := range []string{"notes/create", "i/update", "users/show", "admin/update-meta"} {
		if _, ok := g.Endpoints[name]; !ok {
			t.Errorf("golden lacks %s", name)
		}
	}
}

// TestGoldenAJVConfigIsWhatThisPackageImplements pins the ajv setup upstream
// uses (endpoint-base.ts). When upstream changes an option or a format, this
// fails after the golden is regenerated, instead of the validator silently
// keeping the old behavior.
func TestGoldenAJVConfigIsWhatThisPackageImplements(t *testing.T) {
	g := readGolden(t)
	if g.AJV.Version != "8.20.0" {
		t.Errorf("upstream ajv is %s; the corpus is rebuilt with it, but review the ajv changelog for behavior this package mirrors", g.AJV.Version)
	}
	if want := map[string]any{"useDefaults": true}; !reflect.DeepEqual(g.AJV.Options, want) {
		t.Errorf("upstream ajv options are %v, this package implements %v", g.AJV.Options, want)
	}
	if len(g.AJV.Formats) != len(formats) {
		t.Errorf("upstream adds %d formats, this package implements %d", len(g.AJV.Formats), len(formats))
	}
	f, ok := g.AJV.Formats["misskey:id"]
	if !ok || f.Source != "^[a-zA-Z0-9]+$" || f.Flags != "" {
		t.Errorf("upstream misskey:id is %+v, this package implements /^[a-zA-Z0-9]+$/", f)
	}
}

// TestEveryUpstreamParamDefCompiles is the lower bound of the conformance
// test: a paramDef this package cannot compile has no conformance to check.
func TestEveryUpstreamParamDefCompiles(t *testing.T) {
	g := readGolden(t)
	for name, ep := range g.Endpoints {
		if _, err := Compile(ep.ParamDef); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
