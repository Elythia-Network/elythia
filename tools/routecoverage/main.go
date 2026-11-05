// Command routecoverage counts the /api endpoints a test layer hit, from the
// access logs of the Elythia server it ran against, and prints a Markdown
// summary for $GITHUB_STEP_SUMMARY (#4037).
//
// Inputs ending in .json are route-hits files written by -o; the others are
// access logs (docker compose logs, or the log the upstream e2e harness
// writes). They are all added up, so the same command summarizes one shard,
// the shards of a workflow, or every layer.
//
// Usage:
//
//	go run ./tools/routecoverage [-layer name] [-o route-hits.json] [-require notes/create] input...
//	go run ./tools/routecoverage -gate -layers a,b,c route-hits-a.json ...
//	go run ./tools/routecoverage -layers a,b,c -write-uncovered internal/routecoverage/uncovered.tsv route-hits-a.json ...
//
// -gate also compares the hits with uncovered.tsv (#4039). -write-uncovered
// rewrites uncovered.tsv for the hits, keeping the issue of the rows already
// listed. Both fail unless every layer of -layers is there.
//
// Exit codes: 0 = summarized, 1 = the hits cannot be trusted (no line read,
// or a required endpoint was not passed through) or the gate failed, 2 = bad
// usage or input.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/elythia-network/elythia/internal/routecoverage"
)

// uncoveredList returns uncovered.tsv. Tests replace it.
var uncoveredList = routecoverage.UncoveredList

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("routecoverage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	layer := fs.String("layer", "", "name of the test layer the access logs come from")
	out := fs.String("o", "", "write the hits as JSON to this file")
	require := fs.String("require", "notes/create", "comma-separated endpoints that must be passed through (2xx or 3xx); empty for none")
	gate := fs.Bool("gate", false, "compare the hits with uncovered.tsv")
	layers := fs.String("layers", "", "with -gate: comma-separated layers the hits must come from")
	writeUncovered := fs.String("write-uncovered", "", "rewrite this uncovered.tsv for the hits")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "routecoverage: no input")
		return 2
	}
	endpoints, err := routecoverage.Endpoints()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	hits, err := readInputs(fs.Args(), *layer, routecoverage.NewMatcher(endpoints))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *out != "" {
		b, err := json.MarshalIndent(hits, "", "  ")
		if err == nil {
			err = os.WriteFile(*out, append(b, '\n'), 0o644)
		}
		if err != nil {
			fmt.Fprintf(stderr, "routecoverage: write %s: %v\n", *out, err)
			return 2
		}
	}
	rows, malformed := routecoverage.ParseUncovered(uncoveredList())
	if len(malformed) > 0 {
		for _, p := range malformed {
			fmt.Fprintln(stderr, "routecoverage:", p)
		}
		return 2
	}
	if err := hits.WriteMarkdown(stdout, endpoints); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	required := splitList(*require)
	for _, name := range required {
		if _, ok := endpoints[name]; !ok {
			fmt.Fprintf(stderr, "routecoverage: -require names %s, which is no endpoint\n", name)
			return 2
		}
	}
	problems := hits.Problems(required)
	if *gate || *writeUncovered != "" {
		// 層が欠けた集計で一覧を作り直すと、欠けた層だけが通すAPIが一覧に入り、
		// 週次のゲートまで気付けない。突き合わせと同じく、全層を要求する
		want := splitList(*layers)
		if len(want) == 0 {
			fmt.Fprintln(stderr, "routecoverage: -gate and -write-uncovered need -layers")
			return 2
		}
		for _, l := range hits.MissingLayers(want) {
			problems = append(problems, "no hits of the layer "+l)
		}
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintf(stderr, "routecoverage: the hits cannot be trusted: %s\n", p)
		}
		return 1
	}
	if *writeUncovered != "" {
		// 埋め込みの一覧 (rows) は、ビルドした時点のファイルの中身。シェルの
		// リダイレクトで書くと、ビルドの前に空にされて既存の行の issue が消えるので、
		// ここでファイルに書く
		var b strings.Builder
		err := hits.WriteUncovered(&b, endpoints, rows, map[routecoverage.Origin]string{
			routecoverage.Upstream: "#4024",
			routecoverage.Elythia:  "#4024(18)",
		})
		if err == nil {
			err = os.WriteFile(*writeUncovered, []byte(b.String()), 0o644)
		}
		if err != nil {
			fmt.Fprintf(stderr, "routecoverage: write %s: %v\n", *writeUncovered, err)
			return 2
		}
	}
	if !*gate {
		return 0
	}
	failures := hits.CheckUncovered(endpoints, rows)
	if err := writeGate(stdout, failures); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if len(failures) > 0 {
		for _, f := range failures {
			fmt.Fprintln(stderr, "routecoverage:", f)
		}
		return 1
	}
	return 0
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// writeGate writes the result of -gate for $GITHUB_STEP_SUMMARY.
func writeGate(w io.Writer, failures []string) error {
	var b strings.Builder
	b.WriteString("## uncovered.tsv との突き合わせ\n\n")
	if len(failures) == 0 {
		b.WriteString("一致している。\n")
	} else {
		b.WriteString("一致していない。直し方は docs/ci.md の「API の通過の集計」。\n\n")
		for _, f := range failures {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func readInputs(paths []string, layer string, m *routecoverage.Matcher) (*routecoverage.Hits, error) {
	hits := routecoverage.NewHits(layer)
	for _, p := range paths {
		if err := readInput(hits, p, m); err != nil {
			return nil, err
		}
	}
	return hits, nil
}

func readInput(hits *routecoverage.Hits, path string, m *routecoverage.Matcher) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("routecoverage: %w", err)
	}
	defer f.Close()
	if !strings.HasSuffix(path, ".json") {
		return hits.ReadLog(path, f, m)
	}
	var h routecoverage.Hits
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return fmt.Errorf("routecoverage: %s: %w", path, err)
	}
	if h.Endpoints == nil || h.Unknown == nil {
		return errors.New("routecoverage: " + path + " is not a route-hits file")
	}
	hits.Merge(&h)
	return nil
}
