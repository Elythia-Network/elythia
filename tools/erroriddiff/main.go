// Command erroriddiff regenerates the golden per-endpoint error-id snapshot
// used by the entitycompat error-id drift gate (TestErrorIDDrift).
//
// Misskey identifies API errors not only by their `code` but by a per-endpoint
// UUID `id` (meta.errors[*].id). Clients branch on that id, so an id that does
// not match the upstream endpoint breaks drop-in compatibility even when the
// code is right. This tool extracts every endpoint's (code -> id) map from the
// backend endpoint definitions and writes it to
// internal/entitycompat/testdata/golden_error_ids.json. Regenerate it whenever
// UPSTREAM_MISSKEY_VERSION is bumped (run `make upstream-fetch` first).
//
// Usage:
//
//	go run ./tools/erroriddiff
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

// codeIDRe matches a Misskey meta.errors entry. All 459 upstream entries place
// `id` directly after `code`, so the adjacent pattern captures every pair.
var codeIDRe = regexp.MustCompile(`code:\s*'([^']+)'\s*,\s*id:\s*'([^']+)'`)

// errBlockRe captures a whole meta.errors entry object so per-error attributes
// (httpStatusCode / kind) can be read alongside the code. Misskey error objects
// are flat (no nested braces), so the brace-free body match is sufficient.
var (
	errBlockRe   = regexp.MustCompile(`\{[^{}]*?code:\s*'([A-Z_]+)'[^{}]*?\}`)
	httpStatusRe = regexp.MustCompile(`httpStatusCode:\s*(\d+)`)
	kindRe       = regexp.MustCompile(`kind:\s*'(\w+)'`)
)

// kindStatus maps Misskey's ApiError `kind` to the default HTTP status applied
// by ApiCallService when httpStatusCode is absent (client->400, permission->403,
// server->500). The default kind is 'client', so entries with neither attribute
// are 400 and are intentionally NOT recorded: aligning mk-go's semantic statuses
// to a blanket 400 is out of scope (only explicit upstream statuses are gated).
var kindStatus = map[string]int{"client": 400, "permission": 403, "server": 500}

func main() {
	// 本家は .cache/misskey/<版> から読む (#3378)。版のファイルが
	// 読めなければ up は空になり、下の Check が取得の案内を出して落ちる。
	up, _ := upstreamsrc.Dir(".")
	epDir := flag.String("endpoints", filepath.Join(up, "packages/backend/src/server/api/endpoints"), "path to Misskey backend endpoints dir")
	out := flag.String("out", "internal/entitycompat/testdata/golden_error_ids.json", "golden id snapshot output path")
	statusOut := flag.String("status-out", "internal/entitycompat/testdata/golden_error_status.json", "golden explicit-status snapshot output path")
	kindsOut := flag.String("kinds-out", "internal/entitycompat/testdata/golden_error_kinds.json", "golden explicit-kind snapshot output path")
	flag.Parse()
	if err := upstreamsrc.Check(*epDir); err != nil {
		fmt.Fprintln(os.Stderr, "erroriddiff:", err)
		os.Exit(1)
	}

	golden := map[string]map[string]string{}
	status := map[string]map[string]int{}
	kinds := map[string]map[string]string{}
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
		s := string(data)
		for _, m := range codeIDRe.FindAllStringSubmatch(s, -1) {
			if golden[ep] == nil {
				golden[ep] = map[string]string{}
			}
			golden[ep][m[1]] = m[2]
		}
		// Record only explicit statuses: httpStatusCode wins; otherwise an
		// explicit kind maps to its default. Entries with neither are the
		// implicit-400 majority and are skipped (see kindStatus comment).
		// Explicit kinds are recorded separately for the kind drift gate
		// (TestErrorKindDrift): entries without kind are the implicit-client
		// majority and are intentionally absent (#1608).
		for _, m := range errBlockRe.FindAllStringSubmatch(s, -1) {
			code, blk := m[1], m[0]
			var st int
			if hs := httpStatusRe.FindStringSubmatch(blk); hs != nil {
				st, _ = strconv.Atoi(hs[1])
			}
			if kd := kindRe.FindStringSubmatch(blk); kd != nil {
				if st == 0 {
					st = kindStatus[kd[1]]
				}
				if kinds[ep] == nil {
					kinds[ep] = map[string]string{}
				}
				kinds[ep][code] = kd[1]
			}
			if st != 0 {
				if status[ep] == nil {
					status[ep] = map[string]int{}
				}
				status[ep][code] = st
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "erroriddiff: walk: %v\n", err)
		os.Exit(1)
	}
	if len(golden) == 0 {
		fmt.Fprintln(os.Stderr, "erroriddiff: ERROR no endpoints parsed (wrong path or upstream format change?)")
		os.Exit(1)
	}

	writeJSON(*out, golden)
	writeJSON(*statusOut, status)
	writeJSON(*kindsOut, kinds)

	ids := 0
	for _, m := range golden {
		ids += len(m)
	}
	sts := 0
	for _, m := range status {
		sts += len(m)
	}
	kds := 0
	for _, m := range kinds {
		kds += len(m)
	}
	fmt.Printf("erroriddiff: wrote %d endpoints / %d error ids -> %s\n", len(golden), ids, *out)
	fmt.Printf("erroriddiff: wrote %d explicit statuses -> %s\n", sts, *statusOut)
	fmt.Printf("erroriddiff: wrote %d explicit kinds -> %s\n", kds, *kindsOut)
}

func writeJSON(path string, v any) {
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "erroriddiff: marshal %s: %v\n", path, err)
		os.Exit(1)
	}
	if err := os.WriteFile(path, append(buf, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "erroriddiff: write %s: %v\n", path, err)
		os.Exit(1)
	}
}
