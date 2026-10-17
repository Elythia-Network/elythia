package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Side is which commit a run measured.
type Side string

// The two sides of a comparison.
const (
	SideBase Side = "base"
	SideHead Side = "head"
)

// RunSet describes one comparison: the two commits, the conditions and the
// runs. It is read from run.json at the top of the results directory.
type RunSet struct {
	Base            Commit            `json:"base"`
	Head            Commit            `json:"head"`
	Scenario        string            `json:"scenario"`
	Rounds          int               `json:"rounds"`
	DeliveryEnabled bool              `json:"deliveryEnabled,omitempty"`
	Runner          map[string]string `json:"runner,omitempty"`
	Runs            []*Run            `json:"-"`
}

// Commit is one side of the comparison.
type Commit struct {
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

// RunMeta is meta.json of one run directory.
type RunMeta struct {
	Side      Side   `json:"side"`
	Round     int    `json:"round"`
	Completed bool   `json:"completed"`
	Reason    string `json:"reason,omitempty"`
}

// Run is one measurement: one side in one round.
type Run struct {
	Dir       string
	Meta      RunMeta
	Endpoints map[string]EndpointResult
	// Dropped is k6's dropped_iterations: iterations the load generator could
	// not start, so the load was lower than requested.
	Dropped   int64
	Resources map[string]ResourceUsage
	// Delivery is an optional receiver fixture summary, separate from k6
	// endpoint metrics and resource samples.
	Delivery      *DeliveryStats
	DeliveryError string
}

// DeliveryStats is the outbound delivery artifact produced from receiver stats.
// Counts are descriptive; benchcompare does not apply performance thresholds.
type DeliveryStats struct {
	Finalized               bool     `json:"finalized"`
	Expected                uint64   `json:"expected"`
	Attempts                uint64   `json:"attempts"`
	Verified                uint64   `json:"verified"`
	Pending                 uint64   `json:"pending"`
	Valid                   uint64   `json:"valid"`
	Unique                  uint64   `json:"unique"`
	Duplicates              uint64   `json:"duplicates"`
	Invalid                 uint64   `json:"invalid"`
	ContentMismatch         uint64   `json:"contentMismatch"`
	Unexpected              uint64   `json:"unexpected"`
	UnexpectedAttempts      uint64   `json:"unexpectedAttempts"`
	Missing                 uint64   `json:"missing"`
	ThroughputJobs          *uint64  `json:"throughputJobs,omitempty"`
	ThroughputJobsPerSecond *float64 `json:"throughputJobsPerSecond,omitempty"`
	DrainSeconds            *float64 `json:"drainSeconds,omitempty"`
	ArtifactStatus          string   `json:"status,omitempty"`
	ArtifactReasons         []string `json:"reasons,omitempty"`
}

// EndpointResult is what one run measured for one endpoint.
type EndpointResult struct {
	Requests int64
	// Failed counts the requests k6 marks http_req_failed: error and
	// timeout responses, and by default every status outside 200-399
	// (including 429).
	Failed      int64
	RateLimited int64
	Timeouts    int64
	// Latencies of the successful responses in milliseconds by stat name
	// ("med", "p(95)", "p(99)").
	Latency map[string]float64
}

// ResourceUsage is what one run measured for one container.
type ResourceUsage struct {
	// CPUSeconds is the CPU time used during the run.
	CPUSeconds float64
	// CPUPercent is CPUSeconds over the wall time, where one core is 100%.
	CPUPercent float64
	// MemoryPeakBytes is the largest memory.peak of the cgroup.
	MemoryPeakBytes int64
	// HeapInuseMaxBytes is the largest Go heap in use. HasHeap is false when
	// no sample had one (every container but the app).
	HeapInuseMaxBytes int64
	HasHeap           bool
}

// latencyStats are the latency stats read from the k6 summary, in the order
// they are reported. The scenario must set summaryTrendStats to them.
var latencyStats = []string{"med", "p(95)", "p(99)"}

// errMeasurement marks a run whose measurement did not hold (a container
// restarted during the run): the run is incomparable, not unreadable.
var errMeasurement = errors.New("measurement did not hold")

// loadRunSet reads run.json and every run directory below dir. A
// subdirectory without meta.json (logs, profiles) is not a run.
func loadRunSet(dir string) (*RunSet, error) {
	var rs RunSet
	if err := readJSON(filepath.Join(dir, "run.json"), &rs); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read results: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(sub, "meta.json")); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		run, err := loadRun(sub)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		rs.Runs = append(rs.Runs, run)
	}
	// 実行の順番ではなく (ラウンド, 側) で並べ、出力を入力の並びに依存させない
	sort.Slice(rs.Runs, func(i, j int) bool {
		a, b := rs.Runs[i].Meta, rs.Runs[j].Meta
		if a.Round != b.Round {
			return a.Round < b.Round
		}
		return a.Side < b.Side
	})
	if len(rs.Runs) == 0 {
		return nil, errors.New("no run directories")
	}
	return &rs, nil
}

// loadRun reads one run directory. A run that did not complete keeps only its
// meta: its numbers are not used.
func loadRun(dir string) (*Run, error) {
	run := &Run{Dir: dir}
	if err := readJSON(filepath.Join(dir, "meta.json"), &run.Meta); err != nil {
		return nil, err
	}
	if run.Meta.Side != SideBase && run.Meta.Side != SideHead {
		return nil, fmt.Errorf("meta.json: side is %q, want base or head", run.Meta.Side)
	}
	if run.Meta.Round < 1 {
		return nil, fmt.Errorf("meta.json: round is %d, want 1 or more", run.Meta.Round)
	}
	var err error
	run.DeliveryError, err = readOptionalText(filepath.Join(dir, "delivery-error.txt"))
	if err != nil {
		return nil, fmt.Errorf("delivery-error.txt: %w", err)
	}
	if !run.Meta.Completed {
		return run, nil
	}
	run.Delivery, err = readDeliveryStats(filepath.Join(dir, "delivery.json"))
	if err != nil {
		return nil, fmt.Errorf("delivery.json: %w", err)
	}
	var summary k6Summary
	if err := readJSON(filepath.Join(dir, "k6-summary.json"), &summary); err != nil {
		return nil, err
	}
	run.Endpoints, run.Dropped, err = summary.endpoints()
	if err != nil {
		return nil, fmt.Errorf("k6-summary.json: %w", err)
	}
	f, err := os.Open(filepath.Join(dir, "resources.csv"))
	if err != nil {
		return nil, fmt.Errorf("open resources: %w", err)
	}
	defer f.Close()
	run.Resources, err = parseResources(f)
	if errors.Is(err, errMeasurement) {
		run.Meta.Completed, run.Meta.Reason, run.Resources = false, err.Error(), nil
		return run, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resources.csv: %w", err)
	}
	return run, nil
}

// readDeliveryStats reports file and JSON decoding errors. Semantic issues are
// retained for deliveryStatus so the comparison can report their raw counts.
func readDeliveryStats(path string) (*DeliveryStats, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s DeliveryStats
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

func readOptionalText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// k6Summary is k6's --summary-export JSON. A metric's values are numbers,
// except "thresholds" (an object) on the metrics that have thresholds.
type k6Summary struct {
	Metrics map[string]map[string]json.RawMessage `json:"metrics"`
}

// endpointMetrics are the sub-metrics every endpoint must have. k6 exports a
// sub-metric only when the scenario puts a threshold on it, so a missing one
// means the scenario forgot it; reading it as 0 would quietly turn the error
// rate guard off.
//
// Latency is read from the successful responses only, so that a side whose
// slow requests turned into fast errors does not look faster.
var endpointMetrics = []string{
	"http_req_duration{expected_response:true}",
	"http_reqs",
	"http_req_failed",
	"bench_rate_limited",
	"bench_timeouts",
}

// endpoints returns the per-endpoint results and the dropped_iterations
// count. It fails when no endpoint, or an endpoint without one of
// endpointMetrics, is found.
func (s k6Summary) endpoints() (map[string]EndpointResult, int64, error) {
	found := map[string]map[string]map[string]float64{}
	for key, raw := range s.Metrics {
		metric, tags, ok := parseMetricKey(key)
		if !ok {
			continue
		}
		endpoint := tags["endpoint"]
		delete(tags, "endpoint")
		name := metric
		switch {
		case len(tags) == 0:
		case len(tags) == 1 && tags["expected_response"] == "true":
			name = metric + "{expected_response:true}"
		default:
			continue
		}
		vals, err := numbers(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", key, err)
		}
		if found[endpoint] == nil {
			found[endpoint] = map[string]map[string]float64{}
		}
		found[endpoint][name] = vals
	}
	if len(found) == 0 {
		return nil, 0, errors.New("no endpoint metrics")
	}
	out := make(map[string]EndpointResult, len(found))
	for endpoint, m := range found {
		for _, want := range endpointMetrics {
			if _, ok := m[want]; !ok {
				return nil, 0, fmt.Errorf("endpoint %s: %s{endpoint:%s} is missing (put a threshold on it)", endpoint, strings.SplitN(want, "{", 2)[0], endpoint)
			}
		}
		r := EndpointResult{
			Requests:    int64(m["http_reqs"]["count"]),
			Failed:      int64(m["http_req_failed"]["passes"]),
			RateLimited: int64(m["bench_rate_limited"]["count"]),
			Timeouts:    int64(m["bench_timeouts"]["count"]),
			Latency:     map[string]float64{},
		}
		for _, st := range latencyStats {
			v, ok := m["http_req_duration{expected_response:true}"][st]
			if !ok {
				return nil, 0, fmt.Errorf("endpoint %s: latency %s is missing (set summaryTrendStats)", endpoint, st)
			}
			r.Latency[st] = v
		}
		if r.Requests == 0 {
			return nil, 0, fmt.Errorf("endpoint %s: no requests", endpoint)
		}
		out[endpoint] = r
	}
	dropped, err := numbers(s.Metrics["dropped_iterations"])
	if err != nil {
		return nil, 0, fmt.Errorf("dropped_iterations: %w", err)
	}
	return out, int64(dropped["count"]), nil
}

// numbers returns the numeric values of one metric, skipping "thresholds".
// A missing metric (k6 leaves out dropped_iterations when it is zero) has
// no values.
func numbers(raw map[string]json.RawMessage) (map[string]float64, error) {
	out := make(map[string]float64, len(raw))
	for k, v := range raw {
		if k == "thresholds" {
			continue
		}
		var f float64
		if err := json.Unmarshal(v, &f); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = f
	}
	return out, nil
}

// parseMetricKey splits "metric{tag:value,...}" into the metric and its
// tags. Only keys with an endpoint tag are accepted.
func parseMetricKey(key string) (metric string, tags map[string]string, ok bool) {
	metric, rest, found := strings.Cut(key, "{")
	if !found || metric == "" || !strings.HasSuffix(rest, "}") {
		return "", nil, false
	}
	tags = map[string]string{}
	for _, kv := range strings.Split(strings.TrimSuffix(rest, "}"), ",") {
		k, v, found := strings.Cut(kv, ":")
		if !found || k == "" || v == "" || strings.ContainsAny(v, "{}") {
			return "", nil, false
		}
		tags[k] = v
	}
	if tags["endpoint"] == "" {
		return "", nil, false
	}
	return metric, tags, true
}

// resourceColumns are the columns of resources.csv, written by the sampler
// once a second per container. cpu_usage_usec is cpu.stat's usage_usec
// (cumulative); memory_peak is memory.peak; heap_inuse is empty except for
// the app.
//
// memory.peak is the peak over the cgroup's lifetime, and it cannot be reset
// on the kernels the runners use, so every run must start from freshly
// created containers (the runner does down -v before each run).
var resourceColumns = []string{"t_ms", "container", "cpu_usage_usec", "memory_peak", "heap_inuse"}

// parseResources turns the sampler's time series into one usage per
// container. It returns errMeasurement when a container's CPU time or the
// clock goes backwards, which happens when the container restarted.
func parseResources(r io.Reader) (map[string]ResourceUsage, error) {
	cr := csv.NewReader(r)
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	if strings.Join(header, ",") != strings.Join(resourceColumns, ",") {
		return nil, fmt.Errorf("header is %q, want %q", strings.Join(header, ","), strings.Join(resourceColumns, ","))
	}
	type span struct {
		firstT, lastT     int64
		firstCPU, lastCPU int64
		peak, heap        int64
		hasHeap           bool
	}
	spans := map[string]*span{}
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		// heap_inuse は app 以外では空。空は 0 ではなく「測っていない」として扱う
		hasHeap := rec[4] != ""
		nums := make([]int64, 0, 4)
		for _, col := range []int{0, 2, 3, 4} {
			if col == 4 && !hasHeap {
				nums = append(nums, 0)
				continue
			}
			n, err := strconv.ParseInt(rec[col], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("line %d: %s: %w", line, resourceColumns[col], err)
			}
			nums = append(nums, n)
		}
		t, cpu, peak, heap := nums[0], nums[1], nums[2], nums[3]
		s := spans[rec[1]]
		if s == nil {
			s = &span{firstT: t, lastT: t, firstCPU: cpu, lastCPU: cpu}
			spans[rec[1]] = s
		}
		if t < s.lastT || cpu < s.lastCPU {
			return nil, fmt.Errorf("%w: line %d: %s: time or cpu went backwards (restarted?)", errMeasurement, line, rec[1])
		}
		s.lastT, s.lastCPU = t, cpu
		s.peak = max(s.peak, peak)
		s.heap = max(s.heap, heap)
		s.hasHeap = s.hasHeap || hasHeap
	}
	out := make(map[string]ResourceUsage, len(spans))
	for name, s := range spans {
		// 時刻の違う 2 点が無いと区間が無く、CPU 使用率が出せない
		if s.lastT == s.firstT {
			return nil, fmt.Errorf("%s: fewer than 2 samples", name)
		}
		cpuSec := float64(s.lastCPU-s.firstCPU) / 1e6
		wallSec := float64(s.lastT-s.firstT) / 1e3
		out[name] = ResourceUsage{
			CPUSeconds:        cpuSec,
			CPUPercent:        cpuSec / wallSec * 100,
			MemoryPeakBytes:   s.peak,
			HeapInuseMaxBytes: s.heap,
			HasHeap:           s.hasHeap,
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no samples")
	}
	return out, nil
}
