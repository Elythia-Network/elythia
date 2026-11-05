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
//
// Exit codes: 0 = summarized, 1 = the hits cannot be trusted (no line read,
// or a required endpoint was not passed through), 2 = bad usage or input.
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

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("routecoverage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	layer := fs.String("layer", "", "name of the test layer the access logs come from")
	out := fs.String("o", "", "write the hits as JSON to this file")
	require := fs.String("require", "notes/create", "comma-separated endpoints that must be passed through (2xx or 3xx); empty for none")
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
	if err := hits.WriteMarkdown(stdout, endpoints); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var required []string
	for _, name := range strings.Split(*require, ",") {
		if name = strings.TrimSpace(name); name != "" {
			required = append(required, name)
		}
	}
	for _, name := range required {
		if _, ok := endpoints[name]; !ok {
			fmt.Fprintf(stderr, "routecoverage: -require names %s, which is no endpoint\n", name)
			return 2
		}
	}
	if problems := hits.Problems(required); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintf(stderr, "routecoverage: the hits cannot be trusted: %s\n", p)
		}
		return 1
	}
	return 0
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
