package entitycompat

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/api/paramschema"
)

// limitSpec is the default and maximum of an endpoint's limit param.
type limitSpec struct {
	Default int
	Max     int
}

// upstreamLimitSpecs returns the default and maximum of the limit param of
// every upstream endpoint that has one, read from upstream's paramDefs
// (paramschema.UpstreamGolden, the golden paramschema.Upstream compiles).
//
// 以前はtools/limitspecが本家の.tsを正規表現で読んだgolden
// (golden_limit_specs.json)と突き合わせていた。#3673で本家のparamDefをそのまま
// 持つようになり、113件のdefaultとmaximumがすべて一致したので、こちらから読む
// (#3890)。users/followersのように、limitを最上位のallOfの分岐に持つendpointも
// ある。
func upstreamLimitSpecs(t *testing.T) map[string]limitSpec {
	t.Helper()
	var g struct {
		Endpoints map[string]struct {
			ParamDef json.RawMessage `json:"paramDef"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(paramschema.UpstreamGolden(), &g); err != nil {
		t.Fatalf("parse upstream_paramdefs.json: %v", err)
	}
	type limitDef struct {
		Default *int `json:"default"`
		Maximum *int `json:"maximum"`
	}
	type objectDef struct {
		Properties struct {
			Limit *limitDef `json:"limit"`
		} `json:"properties"`
		AllOf []json.RawMessage `json:"allOf"`
		AnyOf []json.RawMessage `json:"anyOf"`
		OneOf []json.RawMessage `json:"oneOf"`
	}
	specs := map[string]limitSpec{}
	for name, ep := range g.Endpoints {
		var found []limitDef
		var walk func(raw json.RawMessage)
		walk = func(raw json.RawMessage) {
			var o objectDef
			if err := json.Unmarshal(raw, &o); err != nil {
				t.Fatalf("parse the paramDef of %s: %v", name, err)
			}
			if o.Properties.Limit != nil {
				found = append(found, *o.Properties.Limit)
			}
			for _, branches := range [][]json.RawMessage{o.AllOf, o.AnyOf, o.OneOf} {
				for _, b := range branches {
					walk(b)
				}
			}
		}
		walk(ep.ParamDef)
		if len(found) == 0 {
			continue
		}
		var spec limitSpec
		if d := found[0]; d.Default != nil {
			spec.Default = *d.Default
		}
		if d := found[0]; d.Maximum != nil {
			spec.Max = *d.Maximum
		}
		for _, d := range found[1:] {
			// 分岐ごとに違う値を持つと、どれと比べるか決められない。
			if (d.Default == nil) != (found[0].Default == nil) || (d.Default != nil && *d.Default != spec.Default) ||
				(d.Maximum == nil) != (found[0].Maximum == nil) || (d.Maximum != nil && *d.Maximum != spec.Max) {
				t.Fatalf("%s has limit params that differ between the branches of its paramDef", name)
			}
		}
		if spec.Default == 0 && spec.Max == 0 {
			continue
		}
		specs[name] = spec
	}
	return specs
}

// resolveLimitRe matches pagination.ResolveLimit(<limit>, <def>, <max>) and
// captures the def / max integer literals.
var resolveLimitRe = regexp.MustCompile(`pagination\.ResolveLimit\([^,]+,\s*(\d+)\s*,\s*(\d+)\s*\)`)

// TestLimitSpecDrift is the pagination limit gate. Handlers normalize their
// page-size limit via pagination.ResolveLimit(limit, def, max); this test reads
// the def / max literals at each call site, resolves the enclosing method to
// its route, and checks them against upstream's paramDef (default / maximum,
// upstreamLimitSpecs).
//
// Only call sites that resolve to an endpoint with an upstream limit param are
// gated; a handler that does not call ResolveLimit is unchecked rather than
// failing.
func TestLimitSpecDrift(t *testing.T) {
	root := repoRoot(t)

	upstream := upstreamLimitSpecs(t)
	// 読み方が崩れて空振りしないよう、件数の下限と、実在するものを名指しで要求
	// する(users/followersはallOfの分岐に持つ)。
	if len(upstream) < 100 {
		t.Fatalf("read the limit param of only %d upstream endpoints (expected >=100)", len(upstream))
	}
	for _, name := range []string{"notes/timeline", "users/followers"} {
		if upstream[name] != (limitSpec{Default: 10, Max: 100}) {
			t.Fatalf("the limit param of %s is %+v, not {10 100}", name, upstream[name])
		}
	}

	routes := parseRoutes(t, serverSourcePaths(t)...)

	type drift struct {
		endpoint  string
		field     string
		got, want int
	}
	var drifts []drift
	checked := 0

	apiDir := filepath.Join(root, "internal/api")
	err := filepath.WalkDir(apiDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		pkg := filepath.Base(filepath.Dir(path))
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		s := string(src)
		locs := funcDeclRe.FindAllStringSubmatchIndex(s, -1)
		for i, loc := range locs {
			fn := s[loc[2]:loc[3]]
			end := len(s)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			for _, m := range resolveLimitRe.FindAllStringSubmatch(s[loc[0]:end], -1) {
				def, _ := strconv.Atoi(m[1])
				max, _ := strconv.Atoi(m[2])
				for _, route := range routes[pkg+"\x00"+fn] {
					ep := strings.TrimPrefix(route, "/")
					spec, ok := upstream[ep]
					if !ok {
						continue
					}
					checked++
					if spec.Default != 0 && def != spec.Default {
						drifts = append(drifts, drift{ep, "default", def, spec.Default})
					}
					if spec.Max != 0 && max != spec.Max {
						drifts = append(drifts, drift{ep, "max", max, spec.Max})
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk api dir: %v", err)
	}

	if checked == 0 {
		t.Fatal("limit gate resolved 0 ResolveLimit call sites; the parser likely broke")
	}

	if len(drifts) > 0 {
		sort.Slice(drifts, func(i, j int) bool {
			if drifts[i].endpoint != drifts[j].endpoint {
				return drifts[i].endpoint < drifts[j].endpoint
			}
			return drifts[i].field < drifts[j].field
		})
		var b strings.Builder
		b.WriteString("pagination limit drift: ResolveLimit literal differs from Misskey paramDef.\n")
		for _, d := range drifts {
			b.WriteString("  " + d.endpoint + " " + d.field + ": got " + strconv.Itoa(d.got) + " want " + strconv.Itoa(d.want) + "\n")
		}
		t.Fatal(b.String())
	}
}
