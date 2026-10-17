package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEndpoint is what one fake run measured for one endpoint.
type fakeEndpoint struct {
	p95, p99 float64
	// p50 replaces the default median (half of p95) when set.
	p50                   float64
	requests, failed      int64
	rateLimited, timeouts int64
	dropped               int64
	// window replaces the default window [0, 10000] of the endpoint's
	// scenario when set.
	window *[2]float64
}

// resHeader is the header of resources.csv.
const resHeader = "t_ms,container,cpu_usage_usec,memory_peak,heap_inuse,nr_periods,nr_throttled,throttled_usec"

// k6Rows are k6 samples over [0, 10000] in which k6 was never throttled.
const k6Rows = "0,k6,0,1,,0,0,0\n10000,k6,1,1,,100,0,0\n"

// fakeRun is one fake run directory.
type fakeRun struct {
	side      Side
	round     int
	completed bool
	reason    string
	// dropped is dropped_iterations outside the endpoints' scenarios.
	dropped   int64
	endpoints map[string]fakeEndpoint
	// cpu is the CPU time of the app in seconds over a 10 second run.
	cpu float64
	// k6Throttled is how many of k6's 100 CPU periods over the run were
	// throttled.
	k6Throttled int64
	// k6CPU replaces k6's CPU time over the run (1 second) when set.
	k6CPU float64
	// noHeap leaves heap_inuse empty.
	noHeap bool
	// resources replaces resources.csv when set.
	resources string
}

func okRun(side Side, round int, p95 float64) fakeRun {
	return fakeRun{
		side: side, round: round, completed: true, cpu: 5,
		endpoints: map[string]fakeEndpoint{
			"ping":     {p95: p95, p99: p95 * 2, requests: 1000},
			"timeline": {p95: p95 * 10, p99: p95 * 20, requests: 500},
		},
	}
}

// pair returns the base and head runs of rounds 1..n, built by f.
func pair(n int, f func(side Side, round int) fakeRun) []fakeRun {
	var runs []fakeRun
	for round := 1; round <= n; round++ {
		runs = append(runs, f(SideBase, round), f(SideHead, round))
	}
	return runs
}

// metric is one k6 summary metric with the "thresholds" object that k6 adds
// to every metric that has thresholds.
func metric(vals map[string]float64) map[string]any {
	m := map[string]any{"thresholds": map[string]bool{"x>=0": false}}
	for k, v := range vals {
		m[k] = v
	}
	return m
}

func writeResults(t *testing.T, runs ...fakeRun) string {
	t.Helper()
	rounds := 0
	for _, r := range runs {
		rounds = max(rounds, r.round)
	}
	return writeResultsRounds(t, rounds, runs...)
}

func writeResultsRounds(t *testing.T, rounds int, runs ...fakeRun) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "run.json"), fmt.Sprintf(`{"base":{"ref":"develop","commit":"0123456789abcdef"},"head":{"ref":"pr","commit":"fedcba9876543210"},"scenario":"prod-like","rounds":%d,"runner":{"os":"linux","cpus":"4"}}`, rounds))
	for i, r := range runs {
		rd := filepath.Join(dir, fmt.Sprintf("%02d-%s-%d", i, r.side, r.round))
		require.NoError(t, os.Mkdir(rd, 0o755))
		meta, err := json.Marshal(RunMeta{Side: r.side, Round: r.round, Completed: r.completed, Reason: r.reason})
		require.NoError(t, err)
		writeFile(t, filepath.Join(rd, "meta.json"), string(meta))
		if !r.completed {
			continue
		}
		metrics := map[string]any{
			"http_reqs":         metric(map[string]float64{"count": 9999}),
			"http_req_duration": metric(map[string]float64{"med": 100, "p(95)": 100, "p(99)": 100}),
		}
		total := r.dropped
		for _, e := range r.endpoints {
			total += e.dropped
		}
		if total > 0 {
			metrics["dropped_iterations"] = map[string]float64{"count": float64(total), "rate": 1}
		}
		for name, e := range r.endpoints {
			metrics["dropped_iterations{scenario:"+name+"}"] = metric(map[string]float64{"count": float64(e.dropped), "rate": 0})
			tag := "{endpoint:" + name + "}"
			med := e.p95 / 2
			if e.p50 != 0 {
				med = e.p50
			}
			metrics["http_req_duration{endpoint:"+name+",expected_response:true}"] = metric(map[string]float64{"med": med, "p(95)": e.p95, "p(99)": e.p99})
			// 失敗も含めた遅延は読まない (成功だけの submetric を読む)
			metrics["http_req_duration"+tag] = metric(map[string]float64{"med": 1, "p(95)": 1, "p(99)": 1})
			metrics["http_reqs"+tag] = metric(map[string]float64{"count": float64(e.requests), "rate": 1})
			metrics["http_req_failed"+tag] = metric(map[string]float64{"passes": float64(e.failed), "fails": float64(e.requests - e.failed), "value": 0})
			metrics["bench_rate_limited"+tag] = metric(map[string]float64{"count": float64(e.rateLimited), "rate": 0})
			metrics["bench_timeouts"+tag] = metric(map[string]float64{"count": float64(e.timeouts), "rate": 0})
			window := [2]float64{0, 10000}
			if e.window != nil {
				window = *e.window
			}
			metrics["bench_window_start_ms"+tag] = metric(map[string]float64{"value": window[0], "min": window[0], "max": window[0]})
			metrics["bench_window_end_ms"+tag] = metric(map[string]float64{"value": window[1], "min": window[1], "max": window[1]})
		}
		summary, err := json.Marshal(map[string]any{"metrics": metrics})
		require.NoError(t, err)
		writeFile(t, filepath.Join(rd, "k6-summary.json"), string(summary))
		res := r.resources
		if res == "" {
			heap := "104857600"
			if r.noHeap {
				heap = ""
			}
			k6CPU := r.k6CPU
			if k6CPU == 0 {
				k6CPU = 1
			}
			res = strings.Join([]string{
				resHeader,
				"0,app,0,104857600," + heap + ",0,0,0",
				"0,postgres,0,52428800,,0,0,0",
				"0,k6,0,10485760,,0,0,0",
				fmt.Sprintf("5000,app,%d,209715200,%s,50,0,0", int64(r.cpu*1e6/2), heap),
				"5000,postgres,1000000,62914560,,50,0,0",
				fmt.Sprintf("5000,k6,%d,10485760,,50,%d,0", int64(k6CPU*1e6/2), r.k6Throttled/2),
				fmt.Sprintf("10000,app,%d,157286400,%s,100,0,0", int64(r.cpu*1e6), heap),
				"10000,postgres,2000000,57671680,,100,0,0",
				fmt.Sprintf("10000,k6,%d,10485760,,100,%d,0", int64(k6CPU*1e6), r.k6Throttled),
			}, "\n") + "\n"
		}
		writeFile(t, filepath.Join(rd, "resources.csv"), res)
	}
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func compareDir(t *testing.T, dir string) *Comparison {
	t.Helper()
	rs, err := loadRunSet(dir)
	require.NoError(t, err)
	return compare(rs)
}

func endpoint(t *testing.T, c *Comparison, name string) EndpointCompare {
	t.Helper()
	for _, e := range c.Endpoints {
		if e.Endpoint == name {
			return e
		}
	}
	t.Fatalf("endpoint %s not in the comparison", name)
	return EndpointCompare{}
}

func measure(t *testing.T, ms []Measure, name string) Measure {
	t.Helper()
	for _, m := range ms {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("measure %s not found", name)
	return Measure{}
}

// withEndpoint returns r with one endpoint changed by f.
func withEndpoint(r fakeRun, name string, f func(*fakeEndpoint)) fakeRun {
	eps := make(map[string]fakeEndpoint, len(r.endpoints))
	for k, v := range r.endpoints {
		eps[k] = v
	}
	e := eps[name]
	f(&e)
	eps[name] = e
	r.endpoints = eps
	return r
}

// TestLoad_RealK6Output reads a --summary-export written by k6 0.55 (the
// script is testdata/k6-0.55-summary.js, served by busybox httpd): metrics
// carry a "thresholds" object, latency comes from the expected_response:true
// sub-metric, the window gauges carry Unix milliseconds, and
// dropped_iterations is counted per scenario by the scenario tag.
func TestLoad_RealK6Output(t *testing.T) {
	b, err := os.ReadFile("testdata/k6-0.55-summary.json")
	require.NoError(t, err)
	var s k6Summary
	require.NoError(t, json.Unmarshal(b, &s))
	got, dropped, err := s.endpoints()
	require.NoError(t, err)
	assert.Equal(t, int64(19), dropped)
	require.Len(t, got, 3)

	ping := got["ping"]
	assert.Equal(t, int64(20), ping.Requests)
	assert.Zero(t, ping.Failed)
	assert.Zero(t, ping.Dropped)
	assert.InDelta(t, 0.5083105, ping.Latency["med"], 1e-9)
	assert.InDelta(t, 0.8848505299999998, ping.Latency["p(99)"], 1e-9)
	// 区間の gauge は Unix ミリ秒のまま読める (float64 で桁が落ちない)
	assert.Equal(t, int64(1792226036831), ping.WindowStartMs)
	assert.Equal(t, int64(1792226066831), ping.WindowEndMs)

	// 送れなかった分は、シナリオのタグ (endpoint) ではなく scenario のタグに付く
	slow := got["slow"]
	assert.Equal(t, int64(2), slow.Requests)
	assert.Equal(t, int64(19), slow.Dropped)

	limited := got["limited"]
	assert.Equal(t, int64(20), limited.Requests)
	assert.Equal(t, int64(20), limited.Failed, "k6 counts 429 in http_req_failed")
	assert.Equal(t, int64(20), limited.RateLimited)
}

func TestLoadRun_DeliveryStats(t *testing.T) {
	dir := writeResults(t, okRun(SideBase, 1, 10))
	statsPath := filepath.Join(dir, "00-base-1", "delivery.json")
	writeFile(t, statsPath, `{"finalized":true,"expected":4,"attempts":5,"verified":4,"pending":0,"valid":2,"unique":2,"duplicates":0,"invalid":2,"contentMismatch":1,"unexpected":1,"unexpectedAttempts":1,"missing":2}`)

	run, err := loadRun(filepath.Join(dir, "00-base-1"))
	require.NoError(t, err)
	require.NotNil(t, run.Delivery)
	assert.Equal(t, DeliveryStats{Finalized: true, Expected: 4, Attempts: 5, Verified: 4, Pending: 0, Valid: 2, Unique: 2, Duplicates: 0, Invalid: 2, ContentMismatch: 1, Unexpected: 1, UnexpectedAttempts: 1, Missing: 2}, *run.Delivery)
}

func TestLoadRun_RetainsInconsistentDeliveryStats(t *testing.T) {
	dir := writeResults(t, okRun(SideBase, 1, 10))
	statsPath := filepath.Join(dir, "00-base-1", "delivery.json")
	writeFile(t, statsPath, `{"finalized":true,"expected":1,"attempts":1,"verified":1,"pending":0,"valid":1,"unique":1,"duplicates":1,"invalid":0,"contentMismatch":0,"unexpected":0,"unexpectedAttempts":0,"missing":0}`)

	run, err := loadRun(filepath.Join(dir, "00-base-1"))
	require.NoError(t, err)
	require.NotNil(t, run.Delivery)
	assert.Equal(t, uint64(1), run.Delivery.Attempts)
	assert.Equal(t, uint64(1), run.Delivery.Duplicates)
}

func TestLoadRun_RetainsUnfinalizedDeliveryStats(t *testing.T) {
	dir := writeResults(t, okRun(SideBase, 1, 10))
	statsPath := filepath.Join(dir, "00-base-1", "delivery.json")
	writeFile(t, statsPath, `{"finalized":false,"expected":0,"attempts":1,"verified":1,"pending":1,"valid":0,"unique":0,"duplicates":0,"invalid":0,"contentMismatch":0,"unexpected":0,"unexpectedAttempts":0,"missing":0}`)

	run, err := loadRun(filepath.Join(dir, "00-base-1"))
	require.NoError(t, err)
	require.NotNil(t, run.Delivery)
	assert.False(t, run.Delivery.Finalized)
	assert.Equal(t, uint64(1), run.Delivery.Pending)
}

func TestLoadRun_RetainsEmptyDeliveryStats(t *testing.T) {
	dir := writeResults(t, okRun(SideBase, 1, 10))
	statsPath := filepath.Join(dir, "00-base-1", "delivery.json")
	writeFile(t, statsPath, `{"finalized":true,"expected":0,"attempts":0,"verified":0,"pending":0,"valid":0,"unique":0,"duplicates":0,"invalid":0,"contentMismatch":0,"unexpected":0,"unexpectedAttempts":0,"missing":0}`)

	run, err := loadRun(filepath.Join(dir, "00-base-1"))
	require.NoError(t, err)
	require.NotNil(t, run.Delivery)
	assert.Equal(t, uint64(0), run.Delivery.Expected)
}

func TestRun_UnfinalizedDeliveryJSONProducesIncomparableReport(t *testing.T) {
	dir := writeResults(t, pair(2, func(side Side, round int) fakeRun {
		return okRun(side, round, 10)
	})...)
	validStats := `{"finalized":true,"expected":1,"attempts":1,"verified":1,"pending":0,"valid":1,"unique":1,"duplicates":0,"invalid":0,"contentMismatch":0,"unexpected":0,"unexpectedAttempts":0,"missing":0}`
	for _, runDir := range []string{"00-base-1", "01-head-1", "02-base-2", "03-head-2"} {
		writeFile(t, filepath.Join(dir, runDir, "delivery.json"), validStats)
	}
	// A setup failure can leave this syntactically valid, unfinalized record.
	writeFile(t, filepath.Join(dir, "00-base-1", "delivery.json"), `{"finalized":false}`)
	// Counter inconsistencies are also reported with the parsed raw values.
	writeFile(t, filepath.Join(dir, "02-base-2", "delivery.json"), `{"finalized":true,"expected":1,"attempts":1,"verified":1,"pending":0,"valid":1,"unique":1,"duplicates":1,"invalid":0,"contentMismatch":0,"unexpected":0,"unexpectedAttempts":0,"missing":0}`)

	outputDir := t.TempDir()
	mdPath, jsonPath := filepath.Join(outputDir, "report.md"), filepath.Join(outputDir, "report.json")
	var stdout, stderr bytes.Buffer
	err := run([]string{"-dir", dir, "-md", mdPath, "-json", jsonPath}, &stdout, &stderr)
	require.ErrorIs(t, err, errIncomparable, "valid JSON with unfinished stats is a reported comparison failure, not a fatal input error")

	var got Comparison
	reportJSON, readErr := os.ReadFile(jsonPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(reportJSON, &got))
	assert.False(t, got.Valid)
	require.Len(t, got.Delivery, 4)
	assert.Equal(t, DeliveryStatusInvalid, got.Delivery[0].Status)
	assert.Equal(t, DeliveryStats{}, got.Delivery[0].DeliveryStats, "raw parsed counters remain visible")
	assert.Contains(t, got.Delivery[0].Reasons, "manifest is not finalized")
	assert.Contains(t, got.Delivery[0].Reasons, "expected delivery count is zero")
	assert.Contains(t, got.Problems, "base round 1 delivery invalid: manifest is not finalized, expected delivery count is zero")
	assert.Equal(t, DeliveryStatusInvalid, got.Delivery[2].Status)
	assert.Equal(t, uint64(1), got.Delivery[2].DeliveryStats.Duplicates)
	assert.Contains(t, got.Delivery[2].Reasons, "valid 1 != unique 1 + duplicates 1")

	markdown, readErr := os.ReadFile(mdPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(markdown), "| base | 1 | invalid | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |")
	assert.Contains(t, string(markdown), "| base | 2 | invalid | 1 | 1 | 1 | 0 | 1 | 1 | 1 | 0 | 0 | 0 | 0 | 0 |")
}

func TestDeliveryStatusTreatsCountInconsistencyAsInvalid(t *testing.T) {
	cases := []struct {
		name   string
		stats  DeliveryStats
		reason string
	}{
		{"attempts", DeliveryStats{Finalized: true, Expected: 1, Attempts: 1, Missing: 1}, "attempts 1 != valid 0 + invalid 0 + unexpected attempts 0"},
		{"content mismatch", DeliveryStats{Finalized: true, Expected: 1, Attempts: 2, Verified: 3, Invalid: 1, ContentMismatch: 2, UnexpectedAttempts: 1, Unexpected: 1, Missing: 1}, "content mismatch 2 exceeds invalid 1"},
		{"verified", DeliveryStats{Finalized: true, Expected: 1, Attempts: 1, Verified: 0, Valid: 1, Unique: 1}, "verified 0 != valid 1 + content mismatch 0 + unexpected attempts 0"},
		{"duplicates", DeliveryStats{Finalized: true, Expected: 1, Attempts: 1, Verified: 1, Valid: 1, Unique: 1, Duplicates: 1}, "valid 1 != unique 1 + duplicates 1"},
		{"unique", DeliveryStats{Finalized: true, Expected: 1, Attempts: 2, Verified: 2, Valid: 2, Unique: 2}, "unique 2 exceeds expected 1"},
		{"missing", DeliveryStats{Finalized: true, Expected: 2, Attempts: 1, Verified: 1, Valid: 1, Unique: 1}, "missing 0 != expected 2 - unique 1"},
		{"unexpected pairs", DeliveryStats{Finalized: true, Expected: 1, Attempts: 1, Verified: 1, Valid: 1, Unique: 1, Unexpected: 1}, "unexpected pairs 1 exceeds unexpected attempts 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, reasons := deliveryStatus(tc.stats)
			assert.Equal(t, DeliveryStatusInvalid, status)
			assert.Contains(t, reasons, tc.reason)
		})
	}
}

func TestDeliveryStatusReportsUnfinalizedAndPendingStatsAsIncomplete(t *testing.T) {
	cases := []struct {
		name   string
		stats  DeliveryStats
		reason string
	}{
		{"unfinalized", DeliveryStats{Expected: 1, Missing: 1}, "manifest is not finalized"},
		{"pending", DeliveryStats{Finalized: true, Expected: 1, Pending: 1, Missing: 1}, "1 arrivals are pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, reasons := deliveryStatus(tc.stats)
			assert.Equal(t, DeliveryStatusIncomplete, status)
			assert.Contains(t, reasons, tc.reason)
		})
	}
}

func TestLoadRun_UnexpectedDeliveryDoesNotCoverMissingExpectedTuple(t *testing.T) {
	dir := writeResults(t, okRun(SideBase, 1, 10))
	statsPath := filepath.Join(dir, "00-base-1", "delivery.json")
	writeFile(t, statsPath, `{"finalized":true,"expected":1,"attempts":1,"verified":1,"pending":0,"valid":0,"unique":0,"duplicates":0,"invalid":0,"contentMismatch":0,"unexpected":1,"unexpectedAttempts":1,"missing":1}`)

	run, err := loadRun(filepath.Join(dir, "00-base-1"))
	require.NoError(t, err)
	require.NotNil(t, run.Delivery)
	assert.Equal(t, uint64(1), run.Delivery.Unexpected)
	assert.Equal(t, uint64(1), run.Delivery.Missing)
}

func TestCompare_DeliveryIssuesInvalidatePerformanceComparisonAndPreserveCounts(t *testing.T) {
	valid := &DeliveryStats{Finalized: true, Expected: 4, Attempts: 4, Verified: 4, Valid: 4, Unique: 4}
	incomplete := &DeliveryStats{Finalized: true, Expected: 4, Attempts: 2, Verified: 2, Valid: 2, Unique: 2, Missing: 2}
	invalid := &DeliveryStats{Finalized: true, Expected: 4, Attempts: 4, Verified: 4, Valid: 2, Unique: 2, Invalid: 1, ContentMismatch: 1, Unexpected: 1, UnexpectedAttempts: 1, Missing: 2}
	runs := []*Run{
		{Meta: RunMeta{Side: SideBase, Round: 1, Completed: true}, Delivery: incomplete},
		{Meta: RunMeta{Side: SideBase, Round: 2, Completed: true}, Delivery: invalid},
		{Meta: RunMeta{Side: SideHead, Round: 1, Completed: true}, Delivery: valid},
		{Meta: RunMeta{Side: SideHead, Round: 2, Completed: true}, Delivery: valid},
	}
	c := compare(&RunSet{Rounds: 2, Runs: runs})

	assert.False(t, c.Valid)
	assert.Empty(t, c.Endpoints, "invalid delivery data must not receive a performance verdict")
	assert.Empty(t, c.Resources, "invalid delivery data must not receive a performance verdict")
	require.Len(t, c.Delivery, 4)
	assert.Equal(t, DeliveryStatusIncomplete, c.Delivery[0].Status)
	assert.Equal(t, DeliveryStatusInvalid, c.Delivery[1].Status)
	assert.Equal(t, DeliveryStatusValid, c.Delivery[2].Status)
	assert.Equal(t, uint64(2), c.Delivery[1].Missing)
	assert.Equal(t, uint64(1), c.Delivery[1].ContentMismatch)
	assert.Contains(t, c.Problems, "base round 1 delivery incomplete: 2 expected deliveries are missing")
	assert.Contains(t, c.Problems, "base round 2 delivery invalid: 2 expected deliveries are missing, 1 invalid deliveries, 1 content mismatches, 1 unexpected delivery pairs")
	payload, err := json.Marshal(c)
	require.NoError(t, err)
	assert.Contains(t, string(payload), `"status":"incomplete"`)
	assert.Contains(t, string(payload), `"status":"invalid"`)
	assert.Contains(t, string(payload), `"missing":2`)

	var report bytes.Buffer
	require.NoError(t, renderMarkdown(&report, c))
	assert.Contains(t, report.String(), "## 比較不能")
	assert.Contains(t, report.String(), "| base | 1 | incomplete | 4 | 2 | 2 |")
	assert.Contains(t, report.String(), "| base | 2 | invalid | 4 | 4 | 4 |")
	assert.Contains(t, report.String(), "content mismatch")
}

func TestCompare_MissingDeliveryStatsInvalidatePerformanceComparison(t *testing.T) {
	valid := &DeliveryStats{Finalized: true, Expected: 1, Attempts: 1, Verified: 1, Valid: 1, Unique: 1}
	runs := []*Run{
		{Meta: RunMeta{Side: SideBase, Round: 1, Completed: true}, Delivery: valid},
		{Meta: RunMeta{Side: SideBase, Round: 2, Completed: true}, Delivery: valid},
		{Meta: RunMeta{Side: SideHead, Round: 1, Completed: true}, Delivery: valid},
		{Meta: RunMeta{Side: SideHead, Round: 2, Completed: true}},
	}
	c := compare(&RunSet{Rounds: 2, Runs: runs})

	assert.False(t, c.Valid)
	assert.Empty(t, c.Endpoints)
	assert.Contains(t, c.Problems, "head round 2 delivery stats are missing")
	require.Len(t, c.Delivery, 3)
}

func TestCompare_ValidDeliveryWithDuplicateAttemptsRemainsComparable(t *testing.T) {
	withDuplicate := &DeliveryStats{Finalized: true, Expected: 3, Attempts: 4, Verified: 4, Valid: 4, Unique: 3, Duplicates: 1}
	runs := []*Run{
		{Meta: RunMeta{Side: SideBase, Round: 1, Completed: true}, Delivery: withDuplicate},
		{Meta: RunMeta{Side: SideBase, Round: 2, Completed: true}, Delivery: withDuplicate},
		{Meta: RunMeta{Side: SideHead, Round: 1, Completed: true}, Delivery: withDuplicate},
		{Meta: RunMeta{Side: SideHead, Round: 2, Completed: true}, Delivery: withDuplicate},
	}
	c := compare(&RunSet{Rounds: 2, Runs: runs})

	assert.True(t, c.Valid, c.Problems)
	require.Len(t, c.Delivery, 4)
	assert.Equal(t, DeliveryStatusValid, c.Delivery[0].Status)
	assert.Equal(t, uint64(1), c.Delivery[0].Duplicates)
}

// TestCompare_OverlappingRangesAreSame checks that rounds whose ranges overlap
// are "same" even when the medians differ.
func TestCompare_OverlappingRangesAreSame(t *testing.T) {
	p95 := map[Side][]float64{SideBase: {10, 12, 11}, SideHead: {11, 9, 13}}
	c := compareDir(t, writeResults(t, pair(3, func(s Side, r int) fakeRun { return okRun(s, r, p95[s][r-1]) })...))
	require.True(t, c.Valid, c.Problems)
	require.Len(t, c.Endpoints, 2)
	for _, e := range c.Endpoints {
		assert.Equal(t, VerdictSame, e.Verdict, e.Endpoint)
	}
	m := measure(t, endpoint(t, c, "ping").Measures, "p(95)")
	assert.Equal(t, Spread{Median: 11, Min: 10, Max: 12, Rounds: 3}, m.Base)
	assert.Equal(t, Spread{Median: 11, Min: 9, Max: 13, Rounds: 3}, m.Head)
	assert.Nil(t, m.Better)
}

// TestCompare_SeparatedRangesAreDifferent checks "different" and its
// direction for latency (lower is better) and for a higher-is-better
// measure, and that requests are shown but not judged (#3656).
func TestCompare_SeparatedRangesAreDifferent(t *testing.T) {
	c := compareDir(t, writeResults(t, pair(2, func(s Side, r int) fakeRun {
		if s == SideBase {
			return okRun(s, r, 10+float64(r-1))
		}
		return withEndpoint(okRun(s, r, 20), "ping", func(e *fakeEndpoint) { e.requests = 1200 })
	})...))
	require.True(t, c.Valid, c.Problems)
	ping := endpoint(t, c, "ping")
	assert.Equal(t, VerdictDifferent, ping.Verdict)

	p95 := measure(t, ping.Measures, "p(95)")
	assert.Equal(t, VerdictDifferent, p95.Verdict)
	require.NotNil(t, p95.Better)
	assert.False(t, *p95.Better, "slower is worse")
	require.NotNil(t, p95.DeltaPercent)
	assert.InDelta(t, (20-10.5)/10.5*100, *p95.DeltaPercent, 1e-9)

	// リクエスト数は到着率で決まるので、差は出すが判定しない
	reqs := measure(t, ping.Measures, "requests")
	assert.Equal(t, VerdictReference, reqs.Verdict)
	assert.Nil(t, reqs.Better)
	require.NotNil(t, reqs.DeltaPercent)
	assert.InDelta(t, 20, *reqs.DeltaPercent, 1e-9)

	higher := newMeasure("throughput", false, []float64{100, 101}, []float64{120, 121})
	assert.Equal(t, VerdictDifferent, higher.Verdict)
	require.NotNil(t, higher.Better)
	assert.True(t, *higher.Better, "more is better for a higher-is-better measure")

	// 幅がちょうど接する (head の最小 = base の最大) ときは重なりとして扱う
	c = compareDir(t, writeResults(t, pair(2, func(s Side, r int) fakeRun {
		if s == SideBase {
			return okRun(s, r, []float64{10, 11}[r-1])
		}
		return okRun(s, r, []float64{11, 12}[r-1])
	})...))
	assert.Equal(t, VerdictSame, measure(t, endpoint(t, c, "ping").Measures, "p(95)").Verdict)
}

// TestCompare_SmallSeparatedDifferenceIsSame checks that ranges that do not
// overlap but differ by less than minEffectPercent are "same".
func TestCompare_SmallSeparatedDifferenceIsSame(t *testing.T) {
	run := func(head float64) []fakeRun {
		return pair(3, func(s Side, r int) fakeRun {
			if s == SideBase {
				return okRun(s, r, 100+float64(r)/10)
			}
			return okRun(s, r, head+float64(r)/10)
		})
	}
	// 中央値 100.2 と 104.2 の差は約 4.0%
	m := measure(t, endpoint(t, compareDir(t, writeResults(t, run(104)...)), "ping").Measures, "p(95)")
	assert.Equal(t, VerdictSame, m.Verdict)
	require.NotNil(t, m.DeltaPercent)
	assert.InDelta(t, 3.99, *m.DeltaPercent, 0.01)
	// 中央値 100.2 と 105.4 の差は約 5.2%
	m = measure(t, endpoint(t, compareDir(t, writeResults(t, run(105.2)...)), "ping").Measures, "p(95)")
	assert.Equal(t, VerdictDifferent, m.Verdict)

	// ちょうど 5% は差ありにする
	m = newMeasure("cpu_seconds", true, []float64{100, 100}, []float64{105, 105})
	require.NotNil(t, m.DeltaPercent)
	assert.InDelta(t, 5.0, *m.DeltaPercent, 1e-9)
	assert.Equal(t, VerdictDifferent, m.Verdict)
}

// TestCompare_SmallAbsoluteDifferenceIsSame checks that a large relative
// difference of a tiny amount is "same", at the floor of each measure.
func TestCompare_SmallAbsoluteDifferenceIsSame(t *testing.T) {
	cases := []struct {
		name       string
		base, head float64
		want       Verdict
	}{
		{"cpu_seconds", 0.0011, 0.00305, VerdictSame},
		{"cpu_seconds", 1.0, 1.5, VerdictDifferent},
		{"p(95)", 0.3, 0.9, VerdictSame},
		{"p(95)", 0.3, 1.3, VerdictDifferent},
		{"memory_peak_mib", 100, 107, VerdictSame},
		{"memory_peak_mib", 100, 108, VerdictDifferent},
		{"cpu_percent", 10, 11.5, VerdictSame},
		{"cpu_percent", 10, 12.5, VerdictDifferent},
		{"cpu_seconds", 1.0, 1.45, VerdictSame},
		{"med", 0.3, 0.9, VerdictSame},
		{"med", 0.3, 1.3, VerdictDifferent},
		{"heap_inuse_max_mib", 0, 7, VerdictSame},
	}
	for _, tc := range cases {
		m := newMeasure(tc.name, true, []float64{tc.base, tc.base}, []float64{tc.head, tc.head})
		assert.Equal(t, tc.want, m.Verdict, "%s %v -> %v", tc.name, tc.base, tc.head)
	}
}

// TestCompare_P99IsReferenceOnly checks that p99 is shown with its
// difference but never judged, using the home-timeline p99 of an A/A run
// that was reported as different before #3559.
func TestCompare_P99IsReferenceOnly(t *testing.T) {
	p99 := map[Side][]float64{SideBase: {511.0, 516.4, 519.0}, SideHead: {577.4, 614.7, 704.2}}
	c := compareDir(t, writeResults(t, pair(3, func(s Side, r int) fakeRun {
		return withEndpoint(okRun(s, r, 100), "ping", func(e *fakeEndpoint) { e.p99 = p99[s][r-1] })
	})...))
	e := endpoint(t, c, "ping")
	m := measure(t, e.Measures, "p(99)")
	assert.Equal(t, VerdictReference, m.Verdict)
	assert.Nil(t, m.Better)
	require.NotNil(t, m.DeltaPercent)
	assert.InDelta(t, 19.03, *m.DeltaPercent, 0.01)
	assert.Equal(t, 516.4, m.Base.Median)
	assert.Equal(t, VerdictSame, e.Verdict)

	var md strings.Builder
	require.NoError(t, renderMarkdown(&md, c))
	assert.Contains(t, md.String(), "| ping | p99 (ms) | 516.4 [511.0, 519.0] | 614.7 [577.4, 704.2] | +19.0% | 参考 |")
	assert.Contains(t, md.String(), "| ping | 差なし |")

	// 失敗の割合が違うエンドポイントでは、p99 も他の指標と同じく比較不能にする
	c = compareDir(t, writeResults(t, pair(3, func(s Side, r int) fakeRun {
		run := withEndpoint(okRun(s, r, 100), "ping", func(e *fakeEndpoint) { e.p99 = p99[s][r-1] })
		if s == SideHead {
			run = withEndpoint(run, "ping", func(e *fakeEndpoint) { e.failed = e.requests / 10 })
		}
		return run
	})...))
	assert.Equal(t, VerdictIncomparable, measure(t, endpoint(t, c, "ping").Measures, "p(99)").Verdict)
}

// TestCompare_LatencyWithoutSuccess checks that a round with no successful
// response, for which k6 writes latencies of 0, is not compared as 0.
func TestCompare_LatencyWithoutSuccess(t *testing.T) {
	allFail := func(e *fakeEndpoint) { e.failed, e.p95, e.p99 = e.requests, 0, 0 }
	cases := map[string]struct {
		base, head func(side Side, round int) fakeRun
	}{
		// base は全部失敗、head は 99.6% 失敗。失敗率の差は 1 ポイント以内
		"base never succeeded": {
			base: func(s Side, r int) fakeRun { return withEndpoint(okRun(s, r, 10), "timeline", allFail) },
			head: func(s Side, r int) fakeRun {
				return withEndpoint(okRun(s, r, 10), "timeline", func(e *fakeEndpoint) { e.failed = 498 })
			},
		},
		"neither side succeeded": {
			base: func(s Side, r int) fakeRun { return withEndpoint(okRun(s, r, 10), "timeline", allFail) },
			head: func(s Side, r int) fakeRun { return withEndpoint(okRun(s, r, 10), "timeline", allFail) },
		},
		"one head round without success": {
			base: func(s Side, r int) fakeRun { return okRun(s, r, 10) },
			head: func(s Side, r int) fakeRun {
				if r == 1 {
					return withEndpoint(okRun(s, r, 10), "timeline", func(e *fakeEndpoint) { e.failed, e.p95, e.p99 = 5, 0, 0; e.requests = 5 })
				}
				return okRun(s, r, 10)
			},
		},
		"one base round without success": {
			base: func(s Side, r int) fakeRun {
				if r == 2 {
					return withEndpoint(okRun(s, r, 10), "timeline", func(e *fakeEndpoint) { e.failed, e.p95, e.p99 = 5, 0, 0; e.requests = 5 })
				}
				return okRun(s, r, 10)
			},
			head: func(s Side, r int) fakeRun { return okRun(s, r, 10) },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := compareDir(t, writeResults(t, pair(3, func(s Side, r int) fakeRun {
				if s == SideBase {
					return tc.base(s, r)
				}
				return tc.head(s, r)
			})...))
			require.True(t, c.Valid)
			tl := endpoint(t, c, "timeline")
			assert.Equal(t, VerdictIncomparable, tl.Verdict)
			assert.Contains(t, tl.Reasons, "a round had no successful response, so its latency was not measured")
			for _, st := range latencyStats {
				m := measure(t, tl.Measures, st)
				assert.Equal(t, VerdictIncomparable, m.Verdict, st)
				assert.Nil(t, m.Better, st)
			}
		})
	}
	// 成功が 1 件でもあれば、その回の遅延を使う (両側とも 500 件中 1 件だけ成功)
	c := compareDir(t, writeResults(t, pair(3, func(s Side, r int) fakeRun {
		return withEndpoint(okRun(s, r, 10), "timeline", func(e *fakeEndpoint) { e.failed = 499 })
	})...))
	m := measure(t, endpoint(t, c, "timeline").Measures, "p(95)")
	assert.Equal(t, VerdictSame, m.Verdict)
	assert.Equal(t, Spread{Median: 100, Min: 100, Max: 100, Rounds: 3}, m.Head)
}

// TestCompare_FailingFasterIsIncomparable checks that an endpoint whose error
// or rate-limited rate differs is incomparable, however fast it got.
func TestCompare_FailingFasterIsIncomparable(t *testing.T) {
	cases := map[string]func(*fakeEndpoint){
		"errors":       func(e *fakeEndpoint) { e.failed = 50 },
		"rate limited": func(e *fakeEndpoint) { e.rateLimited = 50 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := compareDir(t, writeResults(t, pair(2, func(s Side, r int) fakeRun {
				if s == SideBase {
					return okRun(s, r, 10)
				}
				return withEndpoint(okRun(s, r, 1), "ping", mutate)
			})...))
			require.True(t, c.Valid)
			ping := endpoint(t, c, "ping")
			assert.Equal(t, VerdictIncomparable, ping.Verdict)
			assert.NotEmpty(t, ping.Reasons)
			for _, m := range ping.Measures {
				assert.Equal(t, VerdictIncomparable, m.Verdict, m.Name)
				assert.Nil(t, m.Better, m.Name)
			}
			assert.Equal(t, VerdictDifferent, endpoint(t, c, "timeline").Verdict, "other endpoints are still judged")
		})
	}
}

// TestCompare_RateGapBoundary checks that a gap of exactly one point is still
// comparable, for both rates and from either side, and that timeouts are not
// counted twice (k6 already counts them in http_req_failed).
func TestCompare_RateGapBoundary(t *testing.T) {
	none := func(*fakeEndpoint) {}
	cases := map[string]struct {
		base, head func(*fakeEndpoint)
		want       Verdict
	}{
		// timeline は 500 件。5 件で 1.0%、6 件で 1.2%
		"errors at 1 point":         {none, func(e *fakeEndpoint) { e.failed = 5 }, VerdictSame},
		"errors over 1 point":       {none, func(e *fakeEndpoint) { e.failed = 6 }, VerdictIncomparable},
		"limited at 1 point":        {none, func(e *fakeEndpoint) { e.rateLimited = 5 }, VerdictSame},
		"limited over 1 point":      {none, func(e *fakeEndpoint) { e.rateLimited = 6 }, VerdictIncomparable},
		"timeouts not doubled":      {none, func(e *fakeEndpoint) { e.failed, e.timeouts = 5, 5 }, VerdictSame},
		"errors on the base side":   {func(e *fakeEndpoint) { e.failed = 6 }, none, VerdictIncomparable},
		"limited on the base side":  {func(e *fakeEndpoint) { e.rateLimited = 6 }, none, VerdictIncomparable},
		"same errors on both sides": {func(e *fakeEndpoint) { e.failed = 50 }, func(e *fakeEndpoint) { e.failed = 50 }, VerdictSame},
		// 1.0 と 1.2 の間 (1.1%)。閾値が 1 ポイントより緩むと通ってしまう
		"errors at 1.1 points": {none, func(e *fakeEndpoint) { e.requests, e.failed = 1000, 11 }, VerdictIncomparable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := compareDir(t, writeResults(t, pair(2, func(s Side, r int) fakeRun {
				f := tc.head
				if s == SideBase {
					f = tc.base
				}
				return withEndpoint(okRun(s, r, 10), "timeline", f)
			})...))
			assert.Equal(t, tc.want, endpoint(t, c, "timeline").Verdict)
		})
	}
}

// TestCompare_EndpointMissingInARoundIsIncomparable checks an endpoint that
// one round did not measure.
func TestCompare_EndpointMissingInARoundIsIncomparable(t *testing.T) {
	head2 := okRun(SideHead, 2, 10)
	delete(head2.endpoints, "timeline")
	c := compareDir(t, writeResults(t, okRun(SideBase, 1, 10), okRun(SideHead, 1, 10), okRun(SideBase, 2, 10), head2))
	require.True(t, c.Valid)
	tl := endpoint(t, c, "timeline")
	assert.Equal(t, VerdictIncomparable, tl.Verdict)
	assert.Contains(t, tl.Reasons, "not measured in every round")
	assert.Equal(t, VerdictSame, endpoint(t, c, "ping").Verdict)
}

// TestCompare_InvalidRunSets checks the problems that make the whole
// comparison invalid.
func TestCompare_InvalidRunSets(t *testing.T) {
	ok := func(s Side, r int) fakeRun { return okRun(s, r, 10) }
	dropped := okRun(SideHead, 2, 10)
	dropped.dropped = 1
	restarted := okRun(SideBase, 2, 10)
	restarted.resources = resHeader + "\n0,app,500,1,,,,\n1000,app,10,1,,,,\n"
	k6Back := okRun(SideHead, 2, 10)
	k6Back.resources = resHeader + "\n0,k6,0,1,,100,5,0\n1000,k6,10,1,,10,0,0\n"
	// 区切りの数は進んでも、止められた数だけが戻ったのも作り直し
	k6ThrottledBack := okRun(SideBase, 2, 10)
	k6ThrottledBack.resources = resHeader + "\n0,k6,0,1,,100,5,0\n1000,k6,10,1,,110,4,0\n"
	clockBack := okRun(SideHead, 1, 10)
	clockBack.resources = resHeader + "\n1000,app,0,1,,,,\n500,app,10,1,,,,\n"
	cases := map[string]struct {
		rounds int
		runs   []fakeRun
		want   string
	}{
		"incomplete with reason": {
			runs: []fakeRun{ok(SideBase, 1), ok(SideHead, 1), ok(SideBase, 2), {side: SideHead, round: 2, reason: "seed failed"}},
			want: "head round 2 did not complete: seed failed",
		},
		"incomplete without reason": {
			runs: []fakeRun{ok(SideBase, 1), ok(SideHead, 1), {side: SideBase, round: 2}, ok(SideHead, 2)},
			want: "no reason recorded",
		},
		"dropped iterations": {
			runs: []fakeRun{ok(SideBase, 1), ok(SideHead, 1), ok(SideBase, 2), dropped},
			want: "head round 2 dropped 1 iterations outside the endpoints' scenarios",
		},
		"container restarted": {
			runs: []fakeRun{ok(SideBase, 1), ok(SideHead, 1), restarted, ok(SideHead, 2)},
			want: "base round 2 did not complete: measurement did not hold",
		},
		"k6 throttling counters went backwards": {
			runs: []fakeRun{ok(SideBase, 1), ok(SideHead, 1), ok(SideBase, 2), k6Back},
			want: "head round 2 did not complete: measurement did not hold: line 3: k6: throttling counters went backwards",
		},
		"k6 throttled count went backwards": {
			runs: []fakeRun{ok(SideBase, 1), ok(SideHead, 1), k6ThrottledBack, ok(SideHead, 2)},
			want: "base round 2 did not complete: measurement did not hold: line 3: k6: throttling counters went backwards",
		},
		"clock went backwards": {
			runs: []fakeRun{ok(SideBase, 1), clockBack, ok(SideBase, 2), ok(SideHead, 2)},
			want: "time or cpu went backwards",
		},
		"base round missing": {
			rounds: 2,
			runs:   []fakeRun{ok(SideBase, 1), ok(SideHead, 1), ok(SideHead, 2)},
			want:   "base round 2 is missing",
		},
		"head round missing": {
			rounds: 2,
			runs:   []fakeRun{ok(SideBase, 1), ok(SideHead, 1), ok(SideBase, 2)},
			want:   "head round 2 is missing",
		},
		"round recorded twice": {
			runs: []fakeRun{ok(SideBase, 1), ok(SideHead, 1), ok(SideBase, 2), ok(SideHead, 2), ok(SideBase, 1)},
			want: "base round 1 was recorded 2 times",
		},
		"round beyond run.json": {
			rounds: 2,
			runs:   []fakeRun{ok(SideBase, 1), ok(SideHead, 1), ok(SideBase, 2), ok(SideHead, 2), ok(SideHead, 3)},
			want:   "head round 3 is beyond the 2 rounds",
		},
		"one round": {
			runs: []fakeRun{ok(SideBase, 1), ok(SideHead, 1)},
			want: "asks for 1 rounds",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rounds := tc.rounds
			if rounds == 0 {
				for _, r := range tc.runs {
					rounds = max(rounds, r.round)
				}
			}
			dir := writeResultsRounds(t, rounds, tc.runs...)
			c := compareDir(t, dir)
			assert.False(t, c.Valid)
			assert.Empty(t, c.Endpoints, "no verdicts on an invalid comparison")
			assert.Contains(t, strings.Join(c.Problems, "\n"), tc.want)

			var out, errOut bytes.Buffer
			err := run([]string{"-dir", dir}, &out, &errOut)
			assert.ErrorIs(t, err, errIncomparable)
			assert.Contains(t, out.String(), "## 比較不能")
			assert.NotContains(t, out.String(), "差なし")
		})
	}
}

// TestCompare_FewerCompletedRounds checks the per-side round counts once
// every round is present: an incomplete round leaves a side short.
func TestCompare_FewerCompletedRounds(t *testing.T) {
	rs := &RunSet{Rounds: 2}
	for _, side := range []Side{SideBase, SideHead} {
		for round := 1; round <= 2; round++ {
			rs.Runs = append(rs.Runs, &Run{Meta: RunMeta{Side: side, Round: round, Completed: true}})
		}
	}
	rs.Runs[0].Meta.Completed = false
	c := compare(rs)
	assert.Contains(t, c.Problems, "base has 1 completed rounds and head has 2")
	assert.Contains(t, c.Problems, "fewer than 2 completed rounds per side")

	rs.Runs[0].Meta.Completed = true
	rs.Runs[3].Meta.Completed = false
	c = compare(rs)
	assert.Contains(t, c.Problems, "base has 2 completed rounds and head has 1")
	assert.Contains(t, c.Problems, "fewer than 2 completed rounds per side")
}

// TestCompare_Resources checks the per-container usage and that the Go heap
// row is left out where no side has one.
func TestCompare_Resources(t *testing.T) {
	c := compareDir(t, writeResults(t, pair(2, func(s Side, r int) fakeRun {
		run := okRun(s, r, 10)
		if s == SideHead {
			run.cpu = 8
		}
		return run
	})...))
	require.True(t, c.Valid)
	require.Len(t, c.Resources, 3)
	app, k6, pg := c.Resources[0], c.Resources[1], c.Resources[2]
	assert.Equal(t, "app", app.Container)
	cpu := measure(t, app.Measures, "cpu_seconds")
	assert.Equal(t, VerdictDifferent, cpu.Verdict)
	require.NotNil(t, cpu.Better)
	assert.False(t, *cpu.Better, "more CPU is worse")
	assert.InDelta(t, 80, measure(t, app.Measures, "cpu_percent").Head.Median, 1e-9, "8s over 10s is 80% of one core")
	// memory.peak の最大は最後のサンプル (150MiB) ではなく途中の 200MiB
	assert.InDelta(t, 200, measure(t, app.Measures, "memory_peak_mib").Base.Median, 1e-9)
	assert.InDelta(t, 100, measure(t, app.Measures, "heap_inuse_max_mib").Base.Median, 1e-9)

	// 負荷をかける側 (k6) の CPU も並べるが、サーバーの良し悪しではないので参考にする (#3656)
	assert.Equal(t, "k6", k6.Container)
	assert.InDelta(t, 1, measure(t, k6.Measures, "cpu_seconds").Base.Median, 1e-9)
	for _, m := range k6.Measures {
		assert.Equal(t, VerdictReference, m.Verdict, m.Name)
	}
	c = compareDir(t, writeResults(t, pair(2, func(s Side, r int) fakeRun {
		run := okRun(s, r, 10)
		if s == SideHead {
			run.k6CPU = 3
		}
		return run
	})...))
	k6cpu := measure(t, c.Resources[1].Measures, "cpu_seconds")
	assert.Equal(t, VerdictReference, k6cpu.Verdict, "a real difference of k6's CPU is not judged")
	assert.Nil(t, k6cpu.Better)
	require.NotNil(t, k6cpu.DeltaPercent)
	assert.InDelta(t, 200, *k6cpu.DeltaPercent, 1e-9, "the difference is still shown")

	assert.Equal(t, "postgres", pg.Container)
	for _, m := range pg.Measures {
		assert.NotEqual(t, "heap_inuse_max_mib", m.Name, "postgres has no Go heap")
	}

	// app のヒープを測れなかった回があると、0 として比べず、その指標は判定しない
	noHeap := okRun(SideHead, 2, 10)
	noHeap.noHeap = true
	c = compareDir(t, writeResults(t, okRun(SideBase, 1, 10), okRun(SideHead, 1, 10), okRun(SideBase, 2, 10), noHeap))
	heap := measure(t, c.Resources[0].Measures, "heap_inuse_max_mib")
	assert.Equal(t, VerdictIncomparable, heap.Verdict)
	assert.Equal(t, Spread{Median: 100, Min: 100, Max: 100, Rounds: 1}, heap.Head, "the round without a heap is not counted as 0")
	assert.Equal(t, VerdictSame, measure(t, c.Resources[0].Measures, "cpu_seconds").Verdict, "other measures are still judged")
}

// TestParseResources_Heap checks the heap of a container whose samples mix
// measured and empty heap values, and the largest heap rather than the last.
func TestParseResources_Heap(t *testing.T) {
	got, _, err := parseResources(strings.NewReader(resHeader + "\n0,app,0,1,300,,,\n1000,app,10,1,,,,\n2000,app,20,1,200,,,\n"))
	require.NoError(t, err)
	assert.True(t, got["app"].HasHeap)
	assert.Equal(t, int64(300), got["app"].HeapInuseMaxBytes)

	got, _, err = parseResources(strings.NewReader(resHeader + "\n0,app,0,1,,,,\n1000,app,10,1,200,,,\n"))
	require.NoError(t, err)
	assert.True(t, got["app"].HasHeap, "a heap in a later sample counts")

	got, _, err = parseResources(strings.NewReader(resHeader + "\n0,app,0,1,200,,,\n1000,app,10,1,,,,\n"))
	require.NoError(t, err)
	assert.True(t, got["app"].HasHeap, "a heap in an earlier sample counts")
}

// TestCompare_ContainerMissingInARoundIsIncomparable checks a container that
// one round did not sample.
func TestCompare_ContainerMissingInARoundIsIncomparable(t *testing.T) {
	for _, side := range []Side{SideBase, SideHead} {
		t.Run(string(side), func(t *testing.T) {
			c := compareDir(t, writeResults(t, pair(2, func(s Side, r int) fakeRun {
				run := okRun(s, r, 10)
				if s == side && r == 2 {
					run.resources = resHeader + "\n0,app,0,1,1,,,\n10000,app,1,1,1,,,\n" + k6Rows
				}
				return run
			})...))
			require.True(t, c.Valid)
			require.Equal(t, "postgres", c.Resources[2].Container)
			for _, m := range c.Resources[2].Measures {
				assert.Equal(t, VerdictIncomparable, m.Verdict, m.Name)
			}
		})
	}
}

// TestRun_WritesReports checks the Markdown and JSON outputs.
func TestRun_WritesReports(t *testing.T) {
	dir := writeResults(t, pair(2, func(s Side, r int) fakeRun {
		if s == SideBase {
			return okRun(s, r, 10)
		}
		return okRun(s, r, 5)
	})...)
	writeFile(t, filepath.Join(dir, "run.json"), "{\"base\":{\"ref\":\"develop\",\"commit\":\"0123456789abcdef\"},\"head\":{\"ref\":\"a`b|c\",\"commit\":\"fedcba9876543210\"},\"scenario\":\"prod-like\",\"rounds\":2,\"runner\":{\"cpus\":\"4\",\"note\":\"x|y\\nz\"}}")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "logs"), 0o755))
	out := t.TempDir()
	mdPath, jsonPath := filepath.Join(out, "r.md"), filepath.Join(out, "r.json")
	var stdout, stderr bytes.Buffer
	require.NoError(t, run([]string{"-dir", dir, "-md", mdPath, "-json", jsonPath}, &stdout, &stderr), "a subdirectory without meta.json is not a run")
	assert.Empty(t, stdout.String(), "the report goes to -md")

	md, err := os.ReadFile(mdPath)
	require.NoError(t, err)
	for _, want := range []string{
		"- base: `develop` (`0123456789ab`)",
		"- head: `a'b\\|c` (`fedcba987654`)",
		"- note: x\\|y z",
		"| ping | 差あり (改善 2 / 悪化 0) |",
		"| ping | p95 (ms) | 10.0 [10.0, 10.0] | 5.0 [5.0, 5.0] | -50.0% | 差あり (改善) |",
		"| ping | リクエスト数 | 1000 [1000, 1000] | 1000 [1000, 1000] | +0.0% | 参考 |",
		"| ping | 失敗率 / 429の率 | 0.0% / 0.0% | 0.0% / 0.0% | |  |",
		"| ping | k6が止められた区切り (%、最大) | 0.0% | 0.0% | |  |",
		"| app | CPU (%) | 50.0 [50.0, 50.0] | 50.0 [50.0, 50.0] | +0.0% | 差なし |",
	} {
		assert.Contains(t, string(md), want)
	}

	var got Comparison
	b, err := os.ReadFile(jsonPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &got))
	assert.True(t, got.Valid)
	assert.Equal(t, "fedcba9876543210", got.Head.Commit)
	assert.Len(t, got.Endpoints, 2)

	// 比較不能の理由と、悪化の表示
	stdout.Reset()
	require.NoError(t, run([]string{"-dir", writeResults(t, pair(2, func(s Side, r int) fakeRun {
		if s == SideBase {
			return okRun(s, r, 10)
		}
		return withEndpoint(okRun(s, r, 20), "timeline", func(e *fakeEndpoint) { e.failed = 100 })
	})...)}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "| ping | 差あり (改善 0 / 悪化 2) |")
	assert.Contains(t, stdout.String(), "| timeline | 比較不能 |")
	assert.Contains(t, stdout.String(), "| timeline | 失敗率 / 429の率 | 0.0% / 0.0% | 20.0% / 0.0% | | 比較不能 |")
	assert.Contains(t, stdout.String(), "- timeline: error rate differs: 0.0% vs 20.0%")
}

// TestRun_Errors checks the usage and output errors.
func TestRun_Errors(t *testing.T) {
	dir := writeResults(t, pair(2, func(s Side, r int) fakeRun { return okRun(s, r, 10) })...)
	var stdout, stderr bytes.Buffer
	for _, args := range [][]string{
		{},
		{"-dir", dir, "extra"},
		{"-unknown"},
		{"-dir", t.TempDir()},
		{"-dir", dir, "-md", filepath.Join(t.TempDir(), "missing", "r.md")},
		{"-dir", dir, "-json", filepath.Join(t.TempDir(), "missing", "r.json")},
	} {
		err := run(args, &stdout, &stderr)
		assert.Error(t, err, "%v", args)
		assert.NotErrorIs(t, err, errIncomparable, "%v", args)
	}
}

// TestLoad_Errors checks the inputs that cannot be read.
func TestLoad_Errors(t *testing.T) {
	cases := map[string]struct {
		file, content, want string
	}{
		"bad side":              {"meta.json", `{"side":"left","round":1,"completed":true}`, "side is"},
		"round 0":               {"meta.json", `{"side":"base","round":0,"completed":true}`, "round is 0"},
		"broken meta":           {"meta.json", `{`, "meta.json"},
		"broken delivery stats": {"delivery.json", `{`, "delivery.json"},
		"no endpoint metrics":   {"k6-summary.json", `{"metrics":{"http_reqs":{"count":1}}}`, "no endpoint metrics"},
		"non-numeric value":     {"k6-summary.json", `{"metrics":{"http_reqs{endpoint:ping}":{"count":"x"}}}`, "count"},
		"bad dropped":           {"k6-summary.json", strings.Replace(onlyPingSummary, `"dropped_iterations":{"count":0}`, `"dropped_iterations":{"count":"x"}`, 1), "dropped_iterations"},
		"bad header":            {"resources.csv", "t,container\n", "header is"},
		"empty csv":             {"resources.csv", "", "header"},
		"bad number":            {"resources.csv", resHeader + "\nx,app,0,0,,,,\n", "line 2: t_ms"},
		"short row":             {"resources.csv", resHeader + "\n0,app\n", "wrong number of fields"},
		"one sample":            {"resources.csv", resHeader + "\n0,app,5,0,,,,\n", "fewer than 2 samples"},
		"same time":             {"resources.csv", resHeader + "\n0,app,5,0,,,,\n0,app,6,0,,,,\n", "fewer than 2 samples"},
		"no samples":            {"resources.csv", resHeader + "\n", "no samples"},
		"no k6":                 {"resources.csv", resHeader + "\n0,app,0,1,,0,0,0\n1000,app,1,1,,10,0,0\n", "throttling samples of the k6 container"},
		"k6 without counters":   {"resources.csv", resHeader + "\n0,k6,0,1,,,,\n1000,k6,1,1,,,,\n", "throttling samples of the k6 container"},
		"one k6 sample":         {"resources.csv", resHeader + "\n0,k6,0,1,,0,0,0\n1000,k6,1,1,,,,\n", "throttling samples of the k6 container"},
		"partial counters":      {"resources.csv", resHeader + "\n0,k6,0,1,,1,,\n", "line 2: nr_throttled"},
		"no periods":            {"resources.csv", resHeader + "\n0,k6,0,1,,,1,1\n", "line 2: nr_periods"},
		"bad counter":           {"resources.csv", resHeader + "\n0,k6,0,1,,1,1,x\n", "line 2: throttled_usec"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeResults(t, okRun(SideBase, 1, 10))
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			for _, e := range entries {
				if e.IsDir() {
					writeFile(t, filepath.Join(dir, e.Name(), tc.file), tc.content)
				}
			}
			_, err = loadRunSet(dir)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	t.Run("missing files", func(t *testing.T) {
		for _, file := range []string{"k6-summary.json", "resources.csv"} {
			dir := writeResults(t, okRun(SideBase, 1, 10))
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			for _, e := range entries {
				if e.IsDir() {
					require.NoError(t, os.Remove(filepath.Join(dir, e.Name(), file)))
				}
			}
			_, err = loadRunSet(dir)
			assert.Error(t, err, file)
		}
	})
	t.Run("no runs", func(t *testing.T) {
		dir := writeResults(t)
		writeFile(t, filepath.Join(dir, "note.txt"), "not a run")
		require.NoError(t, os.Mkdir(filepath.Join(dir, "logs"), 0o755))
		_, err := loadRunSet(dir)
		assert.ErrorContains(t, err, "no run directories")
	})
	t.Run("unreadable run directory", func(t *testing.T) {
		dir := writeResults(t, okRun(SideBase, 1, 10))
		locked := filepath.Join(dir, "locked")
		require.NoError(t, os.Mkdir(locked, 0o755))
		require.NoError(t, os.Chmod(locked, 0o000))
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		if _, err := os.Stat(filepath.Join(locked, "meta.json")); errors.Is(err, os.ErrNotExist) {
			t.Skip("permissions are not enforced (running as root)")
		}
		_, err := loadRunSet(dir)
		assert.ErrorContains(t, err, "locked")
	})
	t.Run("no run.json", func(t *testing.T) {
		dir := writeResults(t)
		require.NoError(t, os.Remove(filepath.Join(dir, "run.json")))
		_, err := loadRunSet(dir)
		assert.Error(t, err)
	})
}

// onlyPingSummary is a valid summary with one endpoint.
const onlyPingSummary = `{"metrics":{
"dropped_iterations":{"count":0},
"http_req_duration{endpoint:ping,expected_response:true}":{"med":1,"p(95)":2,"p(99)":3,"thresholds":{"p(95)>=0":false}},
"http_reqs{endpoint:ping}":{"count":10,"rate":1,"thresholds":{"count>=0":false}},
"http_req_failed{endpoint:ping}":{"passes":1,"fails":9,"value":0.1,"thresholds":{"rate>=0":false}},
"bench_rate_limited{endpoint:ping}":{"count":0,"rate":0,"thresholds":{"count>=0":false}},
"bench_timeouts{endpoint:ping}":{"count":0,"rate":0,"thresholds":{"count>=0":false}},
"bench_window_start_ms{endpoint:ping}":{"value":1000,"min":1000,"max":1000,"thresholds":{"value>=0":false}},
"bench_window_end_ms{endpoint:ping}":{"value":31000,"min":31000,"max":31000,"thresholds":{"value>=0":false}},
"dropped_iterations{scenario:ping}":{"count":0,"rate":0,"thresholds":{"count>=0":false}}}}`

// TestSummary_Endpoints checks which keys are read and that an endpoint
// missing one of the required sub-metrics fails instead of reading 0.
func TestSummary_Endpoints(t *testing.T) {
	parse := func(t *testing.T, js string) (map[string]EndpointResult, int64, error) {
		t.Helper()
		var s k6Summary
		require.NoError(t, json.Unmarshal([]byte(js), &s))
		return s.endpoints()
	}
	got, dropped, err := parse(t, onlyPingSummary)
	require.NoError(t, err)
	assert.Zero(t, dropped)
	assert.Equal(t, EndpointResult{Requests: 10, Failed: 1, Latency: map[string]float64{"med": 1, "p(95)": 2, "p(99)": 3}, WindowStartMs: 1000, WindowEndMs: 31000}, got["ping"])

	// 他のタグの submetric と、読めない key は無視する
	extra := strings.Replace(onlyPingSummary, `"dropped_iterations":{"count":0},`, `"dropped_iterations":{"count":0},
"http_reqs{endpoint:ping,status:200}":{"count":999},
"http_reqs{status:200}":{"count":999},
"http_reqs{endpoint:}":{"count":999},
"http_reqs{endpoint:a{b}":{"count":999},
"http_reqs{endpoint:x":{"count":999},
"{endpoint:ping}":{"count":999},
"http_reqs{endpoint}":{"count":999},`, 1)
	got, _, err = parse(t, extra)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, int64(10), got["ping"].Requests)

	for _, missing := range []string{"http_reqs{endpoint:ping}", "http_req_failed{endpoint:ping}", "bench_rate_limited{endpoint:ping}", "bench_timeouts{endpoint:ping}", "http_req_duration{endpoint:ping,expected_response:true}", "bench_window_start_ms{endpoint:ping}", "bench_window_end_ms{endpoint:ping}", "dropped_iterations{scenario:ping}"} {
		var s k6Summary
		require.NoError(t, json.Unmarshal([]byte(onlyPingSummary), &s))
		delete(s.Metrics, missing)
		_, _, err := s.endpoints()
		assert.ErrorContains(t, err, "is missing", missing)
	}
	// 成功だけの遅延の代わりに、別のタグで絞った遅延しか無ければ読まない
	otherTag := strings.Replace(onlyPingSummary, "expected_response:true", "status:200", 1)
	_, _, err = parse(t, otherTag)
	assert.ErrorContains(t, err, "http_req_duration{endpoint:ping} is missing")

	noP99 := strings.Replace(onlyPingSummary, `,"p(99)":3`, "", 1)
	_, _, err = parse(t, noP99)
	assert.ErrorContains(t, err, "latency p(99) is missing")
	zero := strings.Replace(onlyPingSummary, `{"count":10,`, `{"count":0,`, 1)
	_, _, err = parse(t, zero)
	assert.ErrorContains(t, err, "no requests")

	// 区間が読めない (値が無い、終わりが始まりより前) と、k6 の CPU を区切れないので読まない
	noValue := strings.Replace(onlyPingSummary, `{"value":1000,`, `{`, 1)
	_, _, err = parse(t, noValue)
	assert.ErrorContains(t, err, "do not give a window")
	reversed := strings.Replace(onlyPingSummary, `{"value":31000,`, `{"value":1000,`, 1)
	_, _, err = parse(t, reversed)
	assert.ErrorContains(t, err, "do not give a window")

	// 送れなかった件数はシナリオ名 (= エンドポイント名) で引く。読めない値は止め、形の
	// 違う key は読まない (エンドポイントの分が無いことになるので、上の「is missing」で止まる)
	badDropped := strings.Replace(onlyPingSummary, `"dropped_iterations{scenario:ping}":{"count":0,`, `"dropped_iterations{scenario:ping}":{"count":"x",`, 1)
	_, _, err = parse(t, badDropped)
	assert.ErrorContains(t, err, "dropped_iterations{scenario:ping}: count")
	for _, key := range []string{"dropped_iterations{scenario:ping,x:y}", "dropped_iterations{scenario:ping"} {
		renamed := strings.Replace(onlyPingSummary, `"dropped_iterations{scenario:ping}"`, `"`+key+`"`, 1)
		_, _, err = parse(t, renamed)
		assert.ErrorContains(t, err, "dropped_iterations{scenario:ping} is missing", key)
	}
	got, _, err = parse(t, strings.Replace(onlyPingSummary, `"dropped_iterations{scenario:ping}":{"count":0,`, `"dropped_iterations{scenario:ping}":{"count":3,`, 1))
	require.NoError(t, err)
	assert.Equal(t, int64(3), got["ping"].Dropped)
}

func TestSpread_Median(t *testing.T) {
	assert.Equal(t, Spread{Median: 2, Min: 1, Max: 3, Rounds: 3}, spread([]float64{3, 1, 2}))
	assert.Equal(t, Spread{Median: 2.5, Min: 1, Max: 4, Rounds: 4}, spread([]float64{4, 1, 3, 2}))
	assert.Equal(t, "-", fmtSpread(Spread{}, "p(95)"), "a side without values is not shown as 0")
}

func TestNewMeasure_EdgeCases(t *testing.T) {
	m := newMeasure("p(95)", true, nil, []float64{1})
	assert.Equal(t, VerdictIncomparable, m.Verdict)

	// base の中央値が 0 なら差の % は出せない。幅が離れていれば差ありにする
	m = newMeasure("heap_inuse_max_mib", true, []float64{0, 0}, []float64{20, 21})
	assert.Nil(t, m.DeltaPercent)
	assert.Equal(t, VerdictDifferent, m.Verdict)
	assert.Equal(t, "n/a", fmtDelta(m))

	assert.Equal(t, VerdictIncomparable, worst([]Measure{{Verdict: VerdictDifferent}, {Verdict: VerdictIncomparable}}))
	assert.Equal(t, "abc", short("abc"))
	assert.Equal(t, "custom", measureLabel("custom"))
	assert.Equal(t, "", fmtDelta(Measure{Verdict: VerdictIncomparable}))
	assert.Zero(t, percent(1, 0))
	assert.Equal(t, `a\|b c d e`, cell("a|b\nc\r\nd\re"))
}

// writeLabeledResults is writeResults with labels in run.json, and without
// the Go heap on the base side (upstream Misskey has none).
func writeLabeledResults(t *testing.T, runs ...fakeRun) string {
	t.Helper()
	return writeLabeledResultsNoHeap(t, SideBase, runs...)
}

// writeLabeledResultsNoHeap is writeLabeledResults with the Go heap missing on
// the given side instead.
func writeLabeledResultsNoHeap(t *testing.T, noHeap Side, runs ...fakeRun) string {
	t.Helper()
	for i := range runs {
		if runs[i].side == noHeap {
			runs[i].noHeap = true
		}
	}
	dir := writeResults(t, runs...)
	writeFile(t, filepath.Join(dir, "run.json"), `{"base":{"ref":"misskey/misskey:2026.10.0","commit":"sha256:0123456789abcdef0123"},"head":{"ref":"develop","commit":"fedcba9876543210"},"scenario":"prod-like","rounds":2,"labels":{"base":"本家","head":"Elythia"}}`)
	return dir
}

// TestCompare_LabeledHeapIsOneSidedReference checks that, between two
// implementations, the Go heap only Elythia has is a reference rather than
// incomparable, while two commits still treat a missing heap as incomparable.
func TestCompare_LabeledHeapIsOneSidedReference(t *testing.T) {
	c := compareDir(t, writeLabeledResults(t, pair(2, func(s Side, r int) fakeRun { return okRun(s, r, 10) })...))
	require.True(t, c.Valid)
	require.NotNil(t, c.Labels)
	heap := measure(t, c.Resources[0].Measures, "heap_inuse_max_mib")
	assert.Equal(t, VerdictReference, heap.Verdict)
	assert.Equal(t, 0, heap.Base.Rounds)
	assert.Equal(t, Spread{Median: 100, Min: 100, Max: 100, Rounds: 2}, heap.Head)
	assert.Equal(t, VerdictSame, measure(t, c.Resources[0].Measures, "cpu_seconds").Verdict)

	// Elythia の側のヒープが途中の回だけ欠けたのは、測れなかったので比較不能のまま
	c = compareDir(t, writeLabeledResults(t, pair(2, func(s Side, r int) fakeRun {
		run := okRun(s, r, 10)
		run.noHeap = r == 2
		return run
	})...))
	assert.Equal(t, VerdictIncomparable, measure(t, c.Resources[0].Measures, "heap_inuse_max_mib").Verdict)

	// ヒープ以外の指標が片側の全回で欠けたのも、参考ではなく比較不能
	c = compareDir(t, writeLabeledResults(t, pair(2, func(s Side, r int) fakeRun {
		run := okRun(s, r, 10)
		if s == SideBase {
			run.resources = resHeader + "\n0,app,0,1,,,,\n10000,app,1,1,,,,\n" + k6Rows
		}
		return run
	})...))
	require.Equal(t, "postgres", c.Resources[2].Container)
	for _, m := range c.Resources[2].Measures {
		assert.Equal(t, VerdictIncomparable, m.Verdict, m.Name)
	}

	// 逆の側 (base にだけヒープがある) も同じ扱い: 全回あれば参考、途中の回の欠けは比較不能
	c = compareDir(t, writeLabeledResultsNoHeap(t, SideHead, pair(2, func(s Side, r int) fakeRun { return okRun(s, r, 10) })...))
	heap = measure(t, c.Resources[0].Measures, "heap_inuse_max_mib")
	assert.Equal(t, VerdictReference, heap.Verdict)
	assert.Equal(t, Spread{Median: 100, Min: 100, Max: 100, Rounds: 2}, heap.Base)
	assert.Equal(t, 0, heap.Head.Rounds)
	c = compareDir(t, writeLabeledResultsNoHeap(t, SideHead, pair(2, func(s Side, r int) fakeRun {
		run := okRun(s, r, 10)
		run.noHeap = r == 2
		return run
	})...))
	assert.Equal(t, VerdictIncomparable, measure(t, c.Resources[0].Measures, "heap_inuse_max_mib").Verdict)

	// labels が無ければ、片側にヒープが無いのは測れなかったということなので判定しない
	c = compareDir(t, writeResults(t, pair(2, func(s Side, r int) fakeRun {
		run := okRun(s, r, 10)
		run.noHeap = s == SideBase
		return run
	})...))
	assert.Equal(t, VerdictIncomparable, measure(t, c.Resources[0].Measures, "heap_inuse_max_mib").Verdict)
}

// TestRun_LabeledReport checks the report of two implementations: the sides
// are named by the labels, and the ratio section shows base / head.
func TestRun_LabeledReport(t *testing.T) {
	dir := writeLabeledResults(t, pair(2, func(s Side, r int) fakeRun {
		if s == SideBase {
			run := okRun(s, r, 20)
			run.cpu = 8
			return run
		}
		return okRun(s, r, 5)
	})...)
	var stdout, stderr bytes.Buffer
	require.NoError(t, run([]string{"-dir", dir}, &stdout, &stderr))
	md := stdout.String()
	for _, want := range []string{
		"# 性能比較 (本家 と Elythia)",
		"- 本家: `misskey/misskey:2026.10.0` (`sha256:0123456789abcdef0123`)",
		"- Elythia: `develop` (`fedcba9876543210`)",
		"回をまたいで絶対値を比べない",
		"| ping | 差あり (Elythiaが良い 2 / 本家が良い 0) |",
		"## 比 (本家 / Elythia)",
		"| ping | 10.0 | 2.5 | 4.00 | 20.0 | 5.0 | 4.00 | Elythiaが速い / Elythiaが速い |",
		"| app | CPU時間 (秒) | 8.0 | 5.0 | 1.60 | Elythiaが少ない |",
		"| app | メモリの最大 (MiB) | 200.0 | 200.0 | 1.00 | 差なし |",
		"| エンドポイント | 指標 | 本家 (中央値 [最小, 最大]) | Elythia (中央値 [最小, 最大]) | 差 | 判定 |",
		"| ping | p95 (ms) | 20.0 [20.0, 20.0] | 5.0 [5.0, 5.0] | -75.0% | 差あり (Elythiaが良い) |",
		"| app | Goのヒープの最大 (MiB) | - | 100.0 [100.0, 100.0] | n/a | 参考 |",
	} {
		assert.Contains(t, md, want)
	}
	assert.NotContains(t, md, "改善")

	// 本家のほうが速いときは本家の名前を出す
	stdout.Reset()
	require.NoError(t, run([]string{"-dir", writeLabeledResults(t, pair(2, func(s Side, r int) fakeRun {
		if s == SideBase {
			return okRun(s, r, 5)
		}
		return okRun(s, r, 20)
	})...)}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "| ping | 2.5 | 10.0 | 0.25 | 5.0 | 20.0 | 0.25 | 本家が速い / 本家が速い |")

	// 比較不能のエンドポイントは、比の節でも比を出さない (失敗の差で速く見えるだけかもしれない)
	stdout.Reset()
	require.NoError(t, run([]string{"-dir", writeLabeledResults(t, pair(2, func(s Side, r int) fakeRun {
		if s == SideBase {
			return withEndpoint(okRun(s, r, 20), "timeline", func(e *fakeEndpoint) { e.rateLimited, e.failed = 100, 100 })
		}
		return okRun(s, r, 5)
	})...)}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "| timeline | 100.0 | 25.0 | - | 200.0 | 50.0 | - | 比較不能 / 比較不能 |")

	// p50 と p95 の判定は別々に出す (p50 だけ差あり)
	stdout.Reset()
	require.NoError(t, run([]string{"-dir", writeLabeledResults(t, pair(2, func(s Side, r int) fakeRun {
		run := okRun(s, r, 10)
		p50 := 10.0
		if s == SideHead {
			p50 = 2
		}
		return withEndpoint(run, "ping", func(e *fakeEndpoint) { e.p50 = p50 })
	})...)}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "| ping | 10.0 | 2.0 | 5.00 | 10.0 | 10.0 | 1.00 | Elythiaが速い / 差なし |")

	// 成功が 1 件も無かった側は、中央値も「-」にする (0 と書かない)
	stdout.Reset()
	require.NoError(t, run([]string{"-dir", writeLabeledResults(t, pair(2, func(s Side, r int) fakeRun {
		if s == SideBase {
			return withEndpoint(okRun(s, r, 20), "timeline", func(e *fakeEndpoint) { e.failed = e.requests })
		}
		return okRun(s, r, 5)
	})...)}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "| timeline | - | - | - | - | - | - | 比較不能 / 比較不能 |")

	// 2 つの commit の比較には比の節を出さない
	stdout.Reset()
	require.NoError(t, run([]string{"-dir", writeResults(t, pair(2, func(s Side, r int) fakeRun { return okRun(s, r, 10) })...)}, &stdout, &stderr))
	assert.NotContains(t, stdout.String(), "## 比 (")
}

// TestLoad_LabelsMustNameBothSides checks that a run.json naming only one side
// is an input error.
func TestLoad_LabelsMustNameBothSides(t *testing.T) {
	for _, labels := range []string{`{"base":"本家","head":""}`, `{"base":" ","head":"Elythia"}`, `{}`} {
		dir := writeResults(t, pair(2, func(s Side, r int) fakeRun { return okRun(s, r, 10) })...)
		writeFile(t, filepath.Join(dir, "run.json"), `{"base":{"ref":"a","commit":"b"},"head":{"ref":"c","commit":"d"},"scenario":"prod-like","rounds":2,"labels":`+labels+`}`)
		_, err := loadRunSet(dir)
		assert.ErrorContains(t, err, "labels must name both", labels)
	}
}

// TestCompare_K6Bottleneck checks that an endpoint is incomparable when k6 was
// throttled in more than maxK6ThrottledPercent of its CPU periods in any
// round of either side, and comparable at the limit itself (#3656).
func TestCompare_K6Bottleneck(t *testing.T) {
	for _, tc := range []struct {
		name      string
		side      Side
		round     int
		throttled int64
		want      bool
	}{
		{"base over the limit", SideBase, 2, 3, true},
		{"head over the limit", SideHead, 2, 3, true},
		// 後の回が律速でなくても、前の回の律速は消えない (各側の最大で見る)
		{"first round over the limit", SideHead, 1, 3, true},
		{"at the limit", SideHead, 2, 2, false},
		{"at the limit on base", SideBase, 2, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := compareDir(t, writeResults(t, pair(2, func(s Side, r int) fakeRun {
				run := okRun(s, r, 10)
				if s == SideHead {
					run = okRun(s, r, 5)
				}
				if s == tc.side && r == tc.round {
					run.k6Throttled = tc.throttled
				}
				return run
			})...))
			require.True(t, c.Valid, "a k6 bottleneck is per endpoint, not the whole comparison")
			for _, name := range []string{"ping", "timeline"} {
				e := endpoint(t, c, name)
				got, other := e.BaseK6ThrottledPercent, e.HeadK6ThrottledPercent
				if tc.side == SideHead {
					got, other = other, got
				}
				require.NotNil(t, got)
				require.NotNil(t, other)
				assert.InDelta(t, float64(tc.throttled), *got, 1e-9, "the largest share over the rounds")
				assert.Zero(t, *other)
				if !tc.want {
					assert.Equal(t, VerdictDifferent, e.Verdict, name)
					assert.Empty(t, e.Reasons, name)
					continue
				}
				assert.Equal(t, VerdictIncomparable, e.Verdict, name)
				assert.Contains(t, strings.Join(e.Reasons, "\n"), "k6 was the bottleneck", name)
				for _, m := range e.Measures {
					assert.Equal(t, VerdictIncomparable, m.Verdict, m.Name)
				}
			}
		})
	}
}

// TestCompare_K6ThrottlingIsCutByWindow checks that k6's throttling is read
// in each endpoint's own window, and that an endpoint whose window has fewer
// than two k6 samples is incomparable rather than read as not throttled.
func TestCompare_K6ThrottlingIsCutByWindow(t *testing.T) {
	// k6 は 5000ms までは止められず、5000〜10000ms の 50 区切りのうち 10 で止められた
	res := resHeader + "\n" + strings.Join([]string{
		"0,app,0,1,,0,0,0", "0,k6,0,1,,0,0,0",
		"5000,app,1,1,,50,0,0", "5000,k6,1,1,,50,0,0",
		"10000,app,2,1,,100,0,0", "10000,k6,2,1,,100,10,0",
	}, "\n") + "\n"
	windows := func(timeline [2]float64) []fakeRun {
		return pair(2, func(s Side, r int) fakeRun {
			run := okRun(s, r, 10)
			run.resources = res
			run = withEndpoint(run, "ping", func(e *fakeEndpoint) { e.window = &[2]float64{0, 5000} })
			return withEndpoint(run, "timeline", func(e *fakeEndpoint) { e.window = &timeline })
		})
	}

	c := compareDir(t, writeResults(t, windows([2]float64{5000, 10000})...))
	require.True(t, c.Valid)
	ping, tl := endpoint(t, c, "ping"), endpoint(t, c, "timeline")
	assert.Equal(t, VerdictSame, ping.Verdict, "ping's window had no throttling")
	assert.Zero(t, *ping.BaseK6ThrottledPercent)
	assert.Equal(t, VerdictIncomparable, tl.Verdict)
	assert.InDelta(t, 20, *tl.HeadK6ThrottledPercent, 1e-9, "10 of the 50 periods in the window, not 10 of 100 over the run")

	// 区間の中のサンプルが 0 個 (6000〜9000ms) でも 1 個 (4000〜9000ms) でも、差が取れない
	for _, w := range [][2]float64{{6000, 9000}, {4000, 9000}} {
		c = compareDir(t, writeResults(t, windows(w)...))
		tl = endpoint(t, c, "timeline")
		assert.Equal(t, VerdictIncomparable, tl.Verdict, w)
		assert.Contains(t, tl.Reasons, "k6's CPU was not sampled in the endpoint's window in every round", w)
		assert.Nil(t, tl.BaseK6ThrottledPercent, w)
	}

	var out, errOut bytes.Buffer
	require.NoError(t, run([]string{"-dir", writeResults(t, windows([2]float64{6000, 9000})...)}, &out, &errOut))
	assert.Contains(t, out.String(), "| timeline | k6が止められた区切り (%、最大) | - | - | | 比較不能 |")
	assert.Contains(t, out.String(), "| ping | k6が止められた区切り (%、最大) | 0.0% | 0.0% | |  |")

	out.Reset()
	require.NoError(t, run([]string{"-dir", writeResults(t, windows([2]float64{5000, 10000})...)}, &out, &errOut))
	assert.Contains(t, out.String(), "| timeline | k6が止められた区切り (%、最大) | 20.0% | 20.0% | | 比較不能 |")
	assert.Contains(t, out.String(), "- timeline: k6 was the bottleneck: throttled in up to 20.0% / 20.0% of its CPU periods (more than 2%)")
}

// TestThrottledPercent_NoPeriods checks a window in which k6 was never
// runnable: no period was counted, so none was throttled.
func TestThrottledPercent_NoPeriods(t *testing.T) {
	p, ok := throttledPercent([]ThrottleSample{{TimeMs: 0, Periods: 5}, {TimeMs: 1000, Periods: 5}}, 0, 1000)
	assert.True(t, ok)
	assert.Zero(t, p)
}

// TestCompare_DroppedIterationsPerEndpoint checks that iterations dropped in
// an endpoint's scenario make that endpoint incomparable, on either side,
// without invalidating the other endpoints (#3656).
func TestCompare_DroppedIterationsPerEndpoint(t *testing.T) {
	for _, side := range []Side{SideBase, SideHead} {
		t.Run(string(side), func(t *testing.T) {
			dir := writeResults(t, pair(2, func(s Side, r int) fakeRun {
				run := okRun(s, r, 10)
				if s == side && r == 1 {
					run = withEndpoint(run, "timeline", func(e *fakeEndpoint) { e.dropped = 7 })
				}
				return run
			})...)
			c := compareDir(t, dir)
			require.True(t, c.Valid, "dropped iterations of one scenario do not invalidate the comparison")
			tl := endpoint(t, c, "timeline")
			assert.Equal(t, VerdictIncomparable, tl.Verdict)
			got, other := tl.BaseDropped, tl.HeadDropped
			if side == SideHead {
				got, other = other, got
			}
			assert.Equal(t, int64(7), got)
			assert.Zero(t, other)
			assert.Contains(t, strings.Join(tl.Reasons, "\n"), "of the iterations were dropped")
			assert.Equal(t, VerdictSame, endpoint(t, c, "ping").Verdict)

			var out, errOut bytes.Buffer
			require.NoError(t, run([]string{"-dir", dir}, &out, &errOut))
			want := "| timeline | 送れなかった件数 (合計) | 7 | 0 | | 比較不能 |"
			if side == SideHead {
				want = "| timeline | 送れなかった件数 (合計) | 0 | 7 | | 比較不能 |"
			}
			assert.Contains(t, out.String(), want)
			assert.Contains(t, out.String(), "| ping | 送れなかった件数 (合計) | 0 | 0 | |  |")
		})
	}
}

// TestCompare_DroppedIterationsLimit checks the share of dropped iterations
// over all rounds of a side: maxDroppedPercent itself is comparable, more is
// not, on either side.
func TestCompare_DroppedIterationsLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		side    Side
		dropped [2]int64
		want    bool
	}{
		// 2 回で 2000 件を求め、2 件 (0.1%) 落ちた
		{"head at the limit", SideHead, [2]int64{1, 1}, false},
		{"base at the limit", SideBase, [2]int64{1, 1}, false},
		// 3 件 (0.15%)
		{"head over the limit", SideHead, [2]int64{1, 2}, true},
		{"base over the limit", SideBase, [2]int64{2, 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := compareDir(t, writeResults(t, pair(2, func(s Side, r int) fakeRun {
				run := withEndpoint(okRun(s, r, 10), "ping", func(e *fakeEndpoint) { e.requests = 1000 })
				if s == tc.side {
					d := tc.dropped[r-1]
					run = withEndpoint(run, "ping", func(e *fakeEndpoint) { e.requests, e.dropped = 1000-d, d })
				}
				return run
			})...))
			require.True(t, c.Valid)
			ping := endpoint(t, c, "ping")
			assert.Equal(t, tc.want, ping.DroppedTooMany)
			if tc.want {
				assert.Equal(t, VerdictIncomparable, ping.Verdict)
				assert.Contains(t, strings.Join(ping.Reasons, "\n"), "more than 0.1% of the iterations were dropped")
			} else {
				assert.Equal(t, VerdictSame, ping.Verdict, ping.Reasons)
			}
		})
	}
}

// TestRun_RunnerCPUModel checks that the runner's CPU model comes first among
// the runner fields, and that the model of #3659 gets a note, also when the
// comparison is incomparable. The note does not stop the comparison.
func TestRun_RunnerCPUModel(t *testing.T) {
	const note = "**注意: この機種(AMD EPYC 9V74)では書き込みが揺れうる (#3659)。**"
	report := func(t *testing.T, model string, runs []fakeRun) string {
		t.Helper()
		dir := writeResults(t, runs...)
		writeFile(t, filepath.Join(dir, "run.json"), fmt.Sprintf(`{"base":{"ref":"develop","commit":"0123456789abcdef"},"head":{"ref":"pr","commit":"fedcba9876543210"},"scenario":"prod-like","rounds":2,"runner":{"cpus":"4","cpu_model":%q,"kernel":"6.8"}}`, model))
		var stdout, stderr bytes.Buffer
		err := run([]string{"-dir", dir}, &stdout, &stderr)
		if err != nil {
			require.ErrorIs(t, err, errIncomparable)
		}
		return stdout.String()
	}
	same := pair(2, func(s Side, r int) fakeRun { return okRun(s, r, 10) })

	md := report(t, "AMD EPYC 9V74 80-Core Processor", same)
	cpu := strings.Index(md, "- runnerのCPU: AMD EPYC 9V74 80-Core Processor\n")
	require.GreaterOrEqual(t, cpu, 0, md)
	assert.Less(t, cpu, strings.Index(md, "- cpus: 4\n"), "the CPU model comes before the other runner fields")
	assert.Less(t, strings.Index(md, "- シナリオ:"), cpu)
	assert.Less(t, cpu, strings.Index(md, note), md)
	assert.Contains(t, md, "- runnerのCPU: AMD EPYC 9V74 80-Core Processor\n- "+note, "the note follows the CPU line")
	assert.Less(t, strings.Index(md, note), strings.Index(md, "## まとめ"), "the note is at the top of the report")
	assert.NotContains(t, md, "cpu_model", "the raw key is not shown")
	assert.Contains(t, md, "- kernel: 6.8\n")
	assert.Contains(t, md, "| ping | 差なし |", "the model does not stop the comparison")

	md = report(t, "AMD EPYC 7763 64-Core Processor", same)
	assert.Contains(t, md, "- runnerのCPU: AMD EPYC 7763 64-Core Processor\n")
	assert.NotContains(t, md, "#3659")

	// 大文字小文字の違う書き方でも注記する
	md = report(t, "amd epyc 9v74 80-core processor", same)
	assert.Contains(t, md, "- runnerのCPU: amd epyc 9v74 80-core processor\n- "+note)

	// model name が取れなかったときは空の行にしない
	md = report(t, "", same)
	assert.Contains(t, md, "- runnerのCPU: (不明)\n")
	assert.NotContains(t, md, "#3659")
	md = report(t, "  ", same)
	assert.Contains(t, md, "- runnerのCPU: (不明)\n")

	// 比較不能の report にも出す
	incomplete := pair(2, func(s Side, r int) fakeRun { return okRun(s, r, 10) })
	incomplete[3].completed, incomplete[3].reason = false, "app did not start"
	md = report(t, "AMD EPYC 9V74 80-Core Processor", incomplete)
	assert.Contains(t, md, "## 比較不能")
	assert.Contains(t, md, note)
}
