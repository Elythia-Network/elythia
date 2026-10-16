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
	p95, p99              float64
	requests, failed      int64
	rateLimited, timeouts int64
}

// fakeRun is one fake run directory.
type fakeRun struct {
	side      Side
	round     int
	completed bool
	reason    string
	dropped   int64
	endpoints map[string]fakeEndpoint
	// cpu is the CPU time of the app in seconds over a 10 second run.
	cpu float64
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
		if r.dropped > 0 {
			metrics["dropped_iterations"] = map[string]float64{"count": float64(r.dropped), "rate": 1}
		}
		for name, e := range r.endpoints {
			tag := "{endpoint:" + name + "}"
			metrics["http_req_duration{endpoint:"+name+",expected_response:true}"] = metric(map[string]float64{"med": e.p95 / 2, "p(95)": e.p95, "p(99)": e.p99})
			// 失敗も含めた遅延は読まない (成功だけの submetric を読む)
			metrics["http_req_duration"+tag] = metric(map[string]float64{"med": 1, "p(95)": 1, "p(99)": 1})
			metrics["http_reqs"+tag] = metric(map[string]float64{"count": float64(e.requests), "rate": 1})
			metrics["http_req_failed"+tag] = metric(map[string]float64{"passes": float64(e.failed), "fails": float64(e.requests - e.failed), "value": 0})
			metrics["bench_rate_limited"+tag] = metric(map[string]float64{"count": float64(e.rateLimited), "rate": 0})
			metrics["bench_timeouts"+tag] = metric(map[string]float64{"count": float64(e.timeouts), "rate": 0})
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
			res = strings.Join([]string{
				"t_ms,container,cpu_usage_usec,memory_peak,heap_inuse",
				"0,app,0,104857600," + heap,
				"0,postgres,0,52428800,",
				fmt.Sprintf("5000,app,%d,209715200,%s", int64(r.cpu*1e6/2), heap),
				"5000,postgres,1000000,62914560,",
				fmt.Sprintf("10000,app,%d,157286400,%s", int64(r.cpu*1e6), heap),
				"10000,postgres,2000000,57671680,",
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
// script is testdata/k6-0.55-summary.js): metrics carry a "thresholds"
// object, latency comes from the expected_response:true sub-metric, and
// dropped_iterations is absent when zero.
func TestLoad_RealK6Output(t *testing.T) {
	b, err := os.ReadFile("testdata/k6-0.55-summary.json")
	require.NoError(t, err)
	var s k6Summary
	require.NoError(t, json.Unmarshal(b, &s))
	got, dropped, err := s.endpoints()
	require.NoError(t, err)
	assert.Zero(t, dropped)
	require.Len(t, got, 2)

	ping := got["ping"]
	assert.Equal(t, int64(20), ping.Requests)
	assert.Zero(t, ping.Failed)
	assert.InDelta(t, 2.9925785, ping.Latency["med"], 1e-9)
	assert.InDelta(t, 5.06898303, ping.Latency["p(99)"], 1e-9)

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

func TestLoadRun_RejectsInconsistentDeliveryStats(t *testing.T) {
	dir := writeResults(t, okRun(SideBase, 1, 10))
	statsPath := filepath.Join(dir, "00-base-1", "delivery.json")
	writeFile(t, statsPath, `{"finalized":true,"expected":1,"attempts":1,"verified":1,"pending":0,"valid":1,"unique":1,"duplicates":1,"invalid":0,"contentMismatch":0,"unexpected":0,"unexpectedAttempts":0,"missing":0}`)

	_, err := loadRun(filepath.Join(dir, "00-base-1"))
	require.ErrorContains(t, err, "delivery.json")
}

func TestLoadRun_RejectsUnfinalizedDeliveryStats(t *testing.T) {
	dir := writeResults(t, okRun(SideBase, 1, 10))
	statsPath := filepath.Join(dir, "00-base-1", "delivery.json")
	writeFile(t, statsPath, `{"finalized":false,"expected":0,"attempts":1,"verified":1,"pending":1,"valid":0,"unique":0,"duplicates":0,"invalid":0,"contentMismatch":0,"unexpected":0,"unexpectedAttempts":0,"missing":0}`)

	_, err := loadRun(filepath.Join(dir, "00-base-1"))
	require.ErrorContains(t, err, "not finalized")
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

func TestCompare_ReportsDeliveryCountsWithoutJudgment(t *testing.T) {
	runs := []*Run{
		{Meta: RunMeta{Side: SideBase, Round: 1, Completed: true}, Delivery: &DeliveryStats{Finalized: true, Expected: 4, Attempts: 5, Verified: 4, Valid: 2, Unique: 2, Invalid: 2, ContentMismatch: 1, Unexpected: 1, UnexpectedAttempts: 1, Missing: 2}},
		{Meta: RunMeta{Side: SideHead, Round: 1, Completed: true}, Delivery: &DeliveryStats{Finalized: true, Expected: 4, Attempts: 4, Verified: 4, Valid: 4, Unique: 4}},
	}
	c := compare(&RunSet{Rounds: 1, Runs: runs})
	require.Len(t, c.Delivery, 2)
	assert.Equal(t, SideBase, c.Delivery[0].Side)
	assert.Equal(t, uint64(1), c.Delivery[0].Unexpected)
	assert.Equal(t, uint64(2), c.Delivery[0].Missing)

	var report bytes.Buffer
	require.NoError(t, renderMarkdown(&report, c))
	assert.Contains(t, report.String(), "## 配送受信")
	assert.Contains(t, report.String(), "計測値のみ表示し、判定はしません")
	assert.Contains(t, report.String(), "content mismatch")
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
// direction for latency (lower is better) and requests (higher is better).
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

	reqs := measure(t, ping.Measures, "requests")
	assert.Equal(t, VerdictDifferent, reqs.Verdict)
	require.NotNil(t, reqs.Better)
	assert.True(t, *reqs.Better, "more requests is better")

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
	m = newMeasure("requests", false, []float64{100, 100}, []float64{105, 105})
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
	restarted.resources = "t_ms,container,cpu_usage_usec,memory_peak,heap_inuse\n0,app,500,1,\n1000,app,10,1,\n"
	clockBack := okRun(SideHead, 1, 10)
	clockBack.resources = "t_ms,container,cpu_usage_usec,memory_peak,heap_inuse\n1000,app,0,1,\n500,app,10,1,\n"
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
			want: "dropped 1 iterations",
		},
		"container restarted": {
			runs: []fakeRun{ok(SideBase, 1), ok(SideHead, 1), restarted, ok(SideHead, 2)},
			want: "base round 2 did not complete: measurement did not hold",
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
	require.Len(t, c.Resources, 2)
	app, pg := c.Resources[0], c.Resources[1]
	assert.Equal(t, "app", app.Container)
	cpu := measure(t, app.Measures, "cpu_seconds")
	assert.Equal(t, VerdictDifferent, cpu.Verdict)
	require.NotNil(t, cpu.Better)
	assert.False(t, *cpu.Better, "more CPU is worse")
	assert.InDelta(t, 80, measure(t, app.Measures, "cpu_percent").Head.Median, 1e-9, "8s over 10s is 80% of one core")
	// memory.peak の最大は最後のサンプル (150MiB) ではなく途中の 200MiB
	assert.InDelta(t, 200, measure(t, app.Measures, "memory_peak_mib").Base.Median, 1e-9)
	assert.InDelta(t, 100, measure(t, app.Measures, "heap_inuse_max_mib").Base.Median, 1e-9)

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
	got, err := parseResources(strings.NewReader("t_ms,container,cpu_usage_usec,memory_peak,heap_inuse\n0,app,0,1,300\n1000,app,10,1,\n2000,app,20,1,200\n"))
	require.NoError(t, err)
	assert.True(t, got["app"].HasHeap)
	assert.Equal(t, int64(300), got["app"].HeapInuseMaxBytes)

	got, err = parseResources(strings.NewReader("t_ms,container,cpu_usage_usec,memory_peak,heap_inuse\n0,app,0,1,\n1000,app,10,1,200\n"))
	require.NoError(t, err)
	assert.True(t, got["app"].HasHeap, "a heap in a later sample counts")

	got, err = parseResources(strings.NewReader("t_ms,container,cpu_usage_usec,memory_peak,heap_inuse\n0,app,0,1,200\n1000,app,10,1,\n"))
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
					run.resources = "t_ms,container,cpu_usage_usec,memory_peak,heap_inuse\n0,app,0,1,1\n10000,app,1,1,1\n"
				}
				return run
			})...))
			require.True(t, c.Valid)
			require.Equal(t, "postgres", c.Resources[1].Container)
			for _, m := range c.Resources[1].Measures {
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
		"| ping | リクエスト数 | 1000 [1000, 1000] | 1000 [1000, 1000] | +0.0% | 差なし |",
		"| ping | 失敗率 / 429の率 | 0.0% / 0.0% | 0.0% / 0.0% | |  |",
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
		"bad side":            {"meta.json", `{"side":"left","round":1,"completed":true}`, "side is"},
		"round 0":             {"meta.json", `{"side":"base","round":0,"completed":true}`, "round is 0"},
		"broken meta":         {"meta.json", `{`, "meta.json"},
		"no endpoint metrics": {"k6-summary.json", `{"metrics":{"http_reqs":{"count":1}}}`, "no endpoint metrics"},
		"non-numeric value":   {"k6-summary.json", `{"metrics":{"http_reqs{endpoint:ping}":{"count":"x"}}}`, "count"},
		"bad dropped":         {"k6-summary.json", strings.Replace(onlyPingSummary, `"dropped_iterations":{"count":0}`, `"dropped_iterations":{"count":"x"}`, 1), "dropped_iterations"},
		"bad header":          {"resources.csv", "t,container\n", "header is"},
		"empty csv":           {"resources.csv", "", "header"},
		"bad number":          {"resources.csv", "t_ms,container,cpu_usage_usec,memory_peak,heap_inuse\nx,app,0,0,\n", "line 2: t_ms"},
		"short row":           {"resources.csv", "t_ms,container,cpu_usage_usec,memory_peak,heap_inuse\n0,app\n", "wrong number of fields"},
		"one sample":          {"resources.csv", "t_ms,container,cpu_usage_usec,memory_peak,heap_inuse\n0,app,5,0,\n", "fewer than 2 samples"},
		"same time":           {"resources.csv", "t_ms,container,cpu_usage_usec,memory_peak,heap_inuse\n0,app,5,0,\n0,app,6,0,\n", "fewer than 2 samples"},
		"no samples":          {"resources.csv", "t_ms,container,cpu_usage_usec,memory_peak,heap_inuse\n", "no samples"},
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
"bench_timeouts{endpoint:ping}":{"count":0,"rate":0,"thresholds":{"count>=0":false}}}}`

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
	assert.Equal(t, EndpointResult{Requests: 10, Failed: 1, Latency: map[string]float64{"med": 1, "p(95)": 2, "p(99)": 3}}, got["ping"])

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

	for _, missing := range []string{"http_reqs{endpoint:ping}", "http_req_failed{endpoint:ping}", "bench_rate_limited{endpoint:ping}", "bench_timeouts{endpoint:ping}", "http_req_duration{endpoint:ping,expected_response:true}"} {
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
