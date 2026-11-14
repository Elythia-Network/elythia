// Command vulncheckgate reads the JSON output of govulncheck (-format json)
// and fails when the code is affected by a vulnerability that the allowlist
// does not name, or when the allowlist names one the code is not affected by
// (#4176).
//
// "Affected" is what govulncheck's text output calls "Your code is affected":
// a finding whose trace reaches a function of the vulnerable module. Findings
// that only reach the module or the package (the code requires or imports it
// but does not call it) are listed but do not fail, like the text output.
//
// The allowlist is a TSV of id, module and the reason the vulnerability does
// not reach Elythia. Lines starting with # and empty lines are skipped.
//
// Usage:
//
//	go run ./tools/vulncheckgate [-allow tools/vulncheckgate/allow.tsv] govulncheck.json
//
// Exit codes: 0 = no vulnerability outside the allowlist and no unused entry,
// 1 = the gate failed, 2 = bad usage or input.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// defaultAllow is the allowlist the Makefile passes.
const defaultAllow = "tools/vulncheckgate/allow.tsv"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vulncheckgate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	allowPath := fs.String("allow", defaultAllow, "allowlist TSV (id, module, reason)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "vulncheckgate: give one govulncheck JSON file")
		return 2
	}
	allow, err := readAllowlist(*allowPath)
	if err != nil {
		fmt.Fprintf(stderr, "vulncheckgate: %v\n", err)
		return 2
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "vulncheckgate: %v\n", err)
		return 2
	}
	defer f.Close()
	res, err := readResult(f)
	if err != nil {
		fmt.Fprintf(stderr, "vulncheckgate: %v\n", err)
		return 2
	}
	failures := check(res, allow)
	for _, id := range res.affectedIDs() {
		e, ok := allow[id]
		if _, inModule := res.affected[id][e.module]; ok && inModule && len(res.affected[id]) == 1 {
			fmt.Fprintf(stdout, "allowed: %s (%s): %s\n", id, e.module, e.reason)
		}
	}
	if len(failures) > 0 {
		for _, f := range failures {
			fmt.Fprintln(stderr, f)
		}
		return 1
	}
	fmt.Fprintf(stdout, "vulncheckgate: no vulnerability outside %s (%d allowed)\n", *allowPath, len(allow))
	return 0
}

// allowEntry is a row of the allowlist.
type allowEntry struct {
	module string
	reason string
}

var idShape = regexp.MustCompile(`^GO-\d{4}-\d+$`)

// readAllowlist reads the allowlist TSV.
func readAllowlist(path string) (map[string]allowEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	allow := map[string]allowEntry{}
	for i, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 3 {
			return nil, fmt.Errorf("%s:%d: want 3 tab-separated columns (id, module, reason), got %d", path, i+1, len(cols))
		}
		id, module, reason := cols[0], cols[1], strings.TrimSpace(cols[2])
		switch {
		case !idShape.MatchString(id):
			return nil, fmt.Errorf("%s:%d: %q is not a Go vulnerability id (GO-YYYY-NNNN)", path, i+1, id)
		case module == "":
			return nil, fmt.Errorf("%s:%d: %s has no module", path, i+1, id)
		case reason == "":
			// 理由の無い除外は、いつ外してよいかを誰も判断できない。
			return nil, fmt.Errorf("%s:%d: %s has no reason", path, i+1, id)
		}
		if _, dup := allow[id]; dup {
			return nil, fmt.Errorf("%s:%d: %s is listed twice", path, i+1, id)
		}
		allow[id] = allowEntry{module: module, reason: reason}
	}
	return allow, nil
}

// affectedVuln is a vulnerable module the code calls into.
type affectedVuln struct {
	// version is the version of the module in the build list, and fixed the
	// first version govulncheck names as fixed ("" when there is none).
	version string
	fixed   string
	// example is one call path govulncheck reported, innermost first.
	example []frame
}

type result struct {
	scanLevel string
	summaries map[string]string
	// affected maps an OSV id to the modules the code calls it in. One
	// advisory can name several modules (echo/v4 and echo/v5, or the standard
	// library and golang.org/x/net), and each has to be allowed.
	affected map[string]map[string]affectedVuln
}

func (r *result) affectedIDs() []string {
	ids := make([]string, 0, len(r.affected))
	for id := range r.affected {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

type frame struct {
	Module   string `json:"module"`
	Version  string `json:"version"`
	Package  string `json:"package"`
	Function string `json:"function"`
	Receiver string `json:"receiver"`
}

type message struct {
	Config *struct {
		ScanLevel string `json:"scan_level"`
	} `json:"config"`
	OSV *struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
	} `json:"osv"`
	Finding *struct {
		OSV          string  `json:"osv"`
		FixedVersion string  `json:"fixed_version"`
		Trace        []frame `json:"trace"`
	} `json:"finding"`
	SBOM *json.RawMessage `json:"SBOM"`
}

// readResult reads the stream of JSON messages govulncheck writes.
func readResult(r io.Reader) (*result, error) {
	res := &result{summaries: map[string]string{}, affected: map[string]map[string]affectedVuln{}}
	dec := json.NewDecoder(bufio.NewReader(r))
	sawConfig, sawSBOM := false, false
	for {
		var m message
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("reading govulncheck output: %w", err)
		}
		switch {
		case m.Config != nil:
			sawConfig = true
			res.scanLevel = m.Config.ScanLevel
		case m.SBOM != nil:
			sawSBOM = true
		case m.OSV != nil:
			res.summaries[m.OSV.ID] = m.OSV.Summary
		case m.Finding != nil && len(m.Finding.Trace) > 0:
			// trace は内側 (脆弱な module の側) から並ぶ。関数まで届いたものだけが、
			// テキストの出力の「Your code is affected」に数えられる。
			inner := m.Finding.Trace[0]
			if inner.Function == "" {
				continue
			}
			mods := res.affected[m.Finding.OSV]
			if mods == nil {
				mods = map[string]affectedVuln{}
				res.affected[m.Finding.OSV] = mods
			}
			if _, seen := mods[inner.Module]; !seen {
				mods[inner.Module] = affectedVuln{version: inner.Version, fixed: m.Finding.FixedVersion, example: m.Finding.Trace}
			}
		}
	}
	// 何も読めないまま緑にしない。symbol の単位で調べていなければ、関数まで
	// 届いたかを判定できず、どの脆弱性も「呼んでいない」に見える。
	if !sawConfig {
		return nil, errors.New("the input has no govulncheck config message; is it the output of govulncheck -format json?")
	}
	// SBOM は解析した module の一覧で、パッケージの読み込みに成功したときに出る
	// (解析を終えた印ではない。最後まで進んだことは govulncheck の終了コードと
	// Makefile の set -e が保証する)。許可の一覧が空になっても、読み込みに
	// 失敗した出力を緑にしないための下限。
	if !sawSBOM {
		return nil, errors.New("the input has no SBOM message; govulncheck did not load the packages")
	}
	if res.scanLevel != "symbol" {
		return nil, fmt.Errorf("govulncheck scanned at the %q level; run it at the symbol level (the default) so calls can be told from imports", res.scanLevel)
	}
	return res, nil
}

// check returns a message for every vulnerability the code is affected by
// that the allowlist does not name (or names for another module), and for
// every allowlist entry the code is not affected by.
func check(res *result, allow map[string]allowEntry) []string {
	var failures []string
	for _, id := range res.affectedIDs() {
		e, ok := allow[id]
		mods := make([]string, 0, len(res.affected[id]))
		for m := range res.affected[id] {
			mods = append(mods, m)
		}
		slices.Sort(mods)
		for _, m := range mods {
			v := res.affected[id][m]
			switch {
			case !ok:
				failures = append(failures, fmt.Sprintf("affected: %s (%s): %s\n  found in %s@%s, fixed in %s\n  called via %s",
					id, m, res.summaries[id], m, v.version, fixedOrNone(v.fixed), describe(v.example)))
			case e.module != m:
				failures = append(failures, fmt.Sprintf("affected: %s is allowed for %s, but the code calls it in %s@%s (fixed in %s)",
					id, e.module, m, v.version, fixedOrNone(v.fixed)))
			}
		}
	}
	ids := make([]string, 0, len(allow))
	for id := range allow {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if _, ok := res.affected[id][allow[id].module]; !ok {
			// 使われなくなった除外を残すと、同じ ID が別の経路で戻ってきたときに
			// 黙って通してしまう。
			failures = append(failures, fmt.Sprintf("unused: %s is in the allowlist for %s, but govulncheck no longer reports the code as calling it there; remove the entry", id, allow[id].module))
		}
	}
	return failures
}

func fixedOrNone(v string) string {
	if v == "" {
		return "(no fixed version)"
	}
	return v
}

// describe renders a trace from the caller side, like govulncheck's text
// output.
func describe(trace []frame) string {
	parts := make([]string, 0, len(trace))
	for i := len(trace) - 1; i >= 0; i-- {
		f := trace[i]
		name := f.Function
		if f.Receiver != "" {
			name = strings.TrimPrefix(f.Receiver, "*") + "." + name
		}
		if f.Package != "" {
			name = f.Package + "." + name
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, " -> ")
}
