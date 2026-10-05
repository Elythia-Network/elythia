// Command limitspec regenerates the golden per-endpoint pagination-limit
// snapshot used by the entitycompat limit-spec drift gate (TestLimitSpecDrift).
//
// Misskey declares each list endpoint's `limit` param as
// `{ type: 'integer', minimum, maximum, default }` in its paramDef; ajv applies
// the default when omitted and rejects out-of-range values. mk-go clamps
// imperatively, so the default/maximum VALUES must still match upstream or an
// omitted limit returns a different page size. This tool extracts every
// endpoint's (default, maximum) and writes it to
// internal/entitycompat/testdata/golden_limit_specs.json. Regenerate whenever
// UPSTREAM_MISSKEY_VERSION is bumped (run `make upstream-fetch` first).
//
// Usage:
//
//	go run ./tools/limitspec
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/elythia-network/elythia/internal/upstreamsrc"
)

// limitRe captures a `limit: { type: 'integer', ... }` paramDef body so the
// default / maximum can be read from it.
var (
	limitRe   = regexp.MustCompile(`limit:\s*\{\s*type:\s*'integer'([^}]*)\}`)
	defaultRe = regexp.MustCompile(`default:\s*(\d+)`)
	maximumRe = regexp.MustCompile(`maximum:\s*(\d+)`)
)

// limitSpec is the gated subset of a Misskey limit paramDef.
type limitSpec struct {
	Default int `json:"default"`
	Max     int `json:"max"`
}

func main() {
	// 本家は .cache/misskey/<版> から読む (#3378)。版のファイルが
	// 読めなければ up は空になり、下の Check が取得の案内を出して落ちる。
	up, _ := upstreamsrc.Dir(".")
	epDir := flag.String("endpoints", filepath.Join(up, "packages/backend/src/server/api/endpoints"), "path to Misskey backend endpoints dir")
	out := flag.String("out", "internal/entitycompat/testdata/golden_limit_specs.json", "golden snapshot output path")
	flag.Parse()
	if err := upstreamsrc.Check(*epDir); err != nil {
		fmt.Fprintln(os.Stderr, "limitspec:", err)
		os.Exit(1)
	}

	specs := map[string]limitSpec{}
	err := filepath.WalkDir(*epDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".ts") {
			return nil
		}
		rel, err := filepath.Rel(*epDir, path)
		if err != nil {
			return err
		}
		ep := strings.TrimSuffix(filepath.ToSlash(rel), ".ts")
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		m := limitRe.FindStringSubmatch(string(data))
		if m == nil {
			return nil
		}
		// default/maximum are both optional in a paramDef; only record an
		// endpoint when at least one is present (otherwise nothing to gate).
		dm := defaultRe.FindStringSubmatch(m[1])
		mm := maximumRe.FindStringSubmatch(m[1])
		if dm == nil && mm == nil {
			return nil
		}
		var spec limitSpec
		if dm != nil {
			spec.Default, _ = strconv.Atoi(dm[1])
		}
		if mm != nil {
			spec.Max, _ = strconv.Atoi(mm[1])
		}
		specs[ep] = spec
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "limitspec: walk: %v\n", err)
		os.Exit(1)
	}
	if len(specs) == 0 {
		fmt.Fprintln(os.Stderr, "limitspec: ERROR no limit params parsed (wrong path or upstream format change?)")
		os.Exit(1)
	}

	buf, err := json.MarshalIndent(specs, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "limitspec: marshal: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, append(buf, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "limitspec: write: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("limitspec: wrote %d endpoints -> %s\n", len(specs), *out)
}
