// Command benchcompare compares the k6 results and resource usage of two
// commits measured in alternating rounds on the same stack (#3542).
//
// Input is a results directory:
//
//	run.json                        base / head commits, scenario, rounds, runner
//	<run>/meta.json                 side ("base" or "head"), round, completed, reason
//	<run>/k6-summary.json           k6 --summary-export
//	<run>/resources.csv             t_ms,container,cpu_usage_usec,memory_peak,heap_inuse
//
// Each measure is compared over the rounds of both sides: it is reported as
// different only when the ranges (min to max) of base and head do not
// overlap and the medians differ by at least 5% (and a per-measure absolute
// amount, such as 1ms for latency). An endpoint whose error rate or
// rate-limited rate differs between base and head by more than one point is
// incomparable, since failing faster is not an improvement.
//
// A run missing one of the required k6 sub-metrics is a bug of the scenario
// and exits 1, without a report.
//
// Usage:
//
//	go run ./tools/benchcompare -dir <results> [-md report.md] [-json report.json]
//
// Exit status: 0 when the runs could be compared (whatever the verdicts),
// 1 when the input could not be read, 2 when the runs could not be compared
// (a run did not complete, dropped iterations, too few rounds).
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errIncomparable is returned when the report was written but the runs did
// not measure the same thing.
var errIncomparable = errors.New("the runs could not be compared")

func main() {
	err := run(os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == nil:
	case errors.Is(err, errIncomparable):
		fmt.Fprintln(os.Stderr, "benchcompare:", err)
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "benchcompare:", err)
		os.Exit(1)
	}
}

func run(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("benchcompare", flag.ContinueOnError)
	fs.SetOutput(errOut)
	dir := fs.String("dir", "", "results directory")
	mdPath := fs.String("md", "", "write the Markdown report here (default: stdout)")
	jsonPath := fs.String("json", "", "write the JSON report here")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || fs.NArg() != 0 {
		return errors.New("usage: benchcompare -dir <results> [-md report.md] [-json report.json]")
	}
	rs, err := loadRunSet(*dir)
	if err != nil {
		return err
	}
	c := compare(rs)

	md := out
	if *mdPath != "" {
		f, err := os.Create(*mdPath)
		if err != nil {
			return err
		}
		defer f.Close()
		md = f
	}
	if err := renderMarkdown(md, c); err != nil {
		return err
	}
	if *jsonPath != "" {
		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*jsonPath, append(b, '\n'), 0o644); err != nil {
			return err
		}
	}
	if !c.Valid {
		return errIncomparable
	}
	return nil
}
