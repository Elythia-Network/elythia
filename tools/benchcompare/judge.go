package main

import (
	"fmt"
	"math"
	"sort"
)

// Verdict is the result of comparing one measure between base and head.
type Verdict string

// Verdicts. Different is reported only when the ranges of the rounds do not
// overlap and the medians differ by at least minEffectPercent and
// minAbsDifference; Incomparable means the runs did not measure the same
// thing; Reference means the measure is shown but not judged (see
// referenceOnly).
const (
	VerdictDifferent    Verdict = "different"
	VerdictSame         Verdict = "same"
	VerdictIncomparable Verdict = "incomparable"
	VerdictReference    Verdict = "reference"
)

// Comparison is the whole result.
type Comparison struct {
	Base      Commit            `json:"base"`
	Head      Commit            `json:"head"`
	Scenario  string            `json:"scenario"`
	Rounds    int               `json:"rounds"`
	Runner    map[string]string `json:"runner,omitempty"`
	Valid     bool              `json:"valid"`
	Problems  []string          `json:"problems,omitempty"`
	Endpoints []EndpointCompare `json:"endpoints"`
	Resources []ResourceCompare `json:"resources"`
}

// EndpointCompare compares one endpoint.
type EndpointCompare struct {
	Endpoint string    `json:"endpoint"`
	Verdict  Verdict   `json:"verdict"`
	Reasons  []string  `json:"reasons,omitempty"`
	Measures []Measure `json:"measures"`
	// Error rates in percent, over all rounds of each side.
	BaseErrorRate     float64 `json:"baseErrorRate"`
	HeadErrorRate     float64 `json:"headErrorRate"`
	BaseRateLimitRate float64 `json:"baseRateLimitRate"`
	HeadRateLimitRate float64 `json:"headRateLimitRate"`
}

// ResourceCompare compares one container.
type ResourceCompare struct {
	Container string    `json:"container"`
	Measures  []Measure `json:"measures"`
}

// Measure is one number compared across the rounds of both sides.
type Measure struct {
	Name string `json:"name"`
	// LowerIsBetter is true for latencies and resources, false for
	// throughput.
	LowerIsBetter bool   `json:"lowerIsBetter"`
	Base          Spread `json:"base"`
	Head          Spread `json:"head"`
	// DeltaPercent is the head median against the base median; nil when the
	// base median is 0.
	DeltaPercent *float64 `json:"deltaPercent"`
	Verdict      Verdict  `json:"verdict"`
	Better       *bool    `json:"better,omitempty"`
}

// Spread is the median and range of one side's rounds. Rounds is how many
// values it is made of; 0 means the side measured nothing.
type Spread struct {
	Median float64 `json:"median"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Rounds int     `json:"rounds"`
}

// maxRateGapPoints is how far, in percentage points, the error rate or the
// rate-limited rate of base and head may differ before the endpoint is
// incomparable. A side that fails more answers faster, so its latency is not
// an improvement.
const maxRateGapPoints = 1.0

// minRounds is the fewest rounds per side that give a range to compare.
// With one round any difference would look like a real one.
const minRounds = 2

// minEffectPercent is the smallest difference of the medians reported as
// different.
//
// 幅が重ならないだけで判定すると、同じ分布から取っても、各側 3 ラウンドで約 10%
// (2/C(6,3))、2 ラウンドで約 33% の確率で「差あり」になる。指標は 1 回の比較で
// 数十あるので、小さな差は数で切る。
const minEffectPercent = 5.0

// compare judges a run set.
func compare(rs *RunSet) *Comparison {
	c := &Comparison{Base: rs.Base, Head: rs.Head, Scenario: rs.Scenario, Rounds: rs.Rounds, Runner: rs.Runner, Valid: true}
	bySide := map[Side][]*Run{}
	for _, r := range rs.Runs {
		if !r.Meta.Completed {
			reason := r.Meta.Reason
			if reason == "" {
				reason = "no reason recorded"
			}
			c.problem("%s round %d did not complete: %s", r.Meta.Side, r.Meta.Round, reason)
			continue
		}
		if r.Dropped > 0 {
			c.problem("%s round %d dropped %d iterations (the load was lower than requested)", r.Meta.Side, r.Meta.Round, r.Dropped)
		}
		bySide[r.Meta.Side] = append(bySide[r.Meta.Side], r)
	}
	checkRounds(c, rs)
	base, head := bySide[SideBase], bySide[SideHead]
	if len(base) != len(head) {
		c.problem("base has %d completed rounds and head has %d", len(base), len(head))
	}
	if len(base) < minRounds || len(head) < minRounds {
		c.problem("fewer than %d completed rounds per side", minRounds)
	}
	if !c.Valid {
		return c
	}
	c.Endpoints = compareEndpoints(base, head)
	c.Resources = compareResources(base, head)
	return c
}

// checkRounds requires every side to have run each round 1..rs.Rounds
// exactly once. A round lost before its meta.json was written leaves no
// directory, and a leftover from an earlier run leaves two.
func checkRounds(c *Comparison, rs *RunSet) {
	if rs.Rounds < minRounds {
		c.problem("run.json asks for %d rounds, fewer than %d", rs.Rounds, minRounds)
	}
	seen := map[Side]map[int]int{SideBase: {}, SideHead: {}}
	for _, r := range rs.Runs {
		seen[r.Meta.Side][r.Meta.Round]++
	}
	for _, side := range []Side{SideBase, SideHead} {
		for round := 1; round <= rs.Rounds; round++ {
			switch n := seen[side][round]; {
			case n == 0:
				c.problem("%s round %d is missing", side, round)
			case n > 1:
				c.problem("%s round %d was recorded %d times", side, round, n)
			}
		}
		for round := range seen[side] {
			if round > rs.Rounds {
				c.problem("%s round %d is beyond the %d rounds of run.json", side, round, rs.Rounds)
			}
		}
	}
}

func (c *Comparison) problem(format string, args ...any) {
	c.Valid = false
	c.Problems = append(c.Problems, fmt.Sprintf(format, args...))
}

func compareEndpoints(base, head []*Run) []EndpointCompare {
	names := map[string]bool{}
	for _, r := range append(append([]*Run{}, base...), head...) {
		for n := range r.Endpoints {
			names[n] = true
		}
	}
	var out []EndpointCompare
	for _, name := range sortedKeys(names) {
		ec := EndpointCompare{Endpoint: name}
		var missing bool
		var bReq, bFail, bLim, hReq, hFail, hLim int64
		collect := func(runs []*Run, req, fail, lim *int64) (lat map[string][]float64, thr []float64) {
			lat = map[string][]float64{}
			for _, r := range runs {
				e, ok := r.Endpoints[name]
				if !ok {
					missing = true
					continue
				}
				*req += e.Requests
				// timeout も http_req_failed に数えられているので、足すと二重になる
				*fail += e.Failed
				*lim += e.RateLimited
				// 成功が 0 件の回は、k6 が遅延を 0 で書き出す。測っていない値なので使わない
				if e.Requests > e.Failed {
					for _, st := range latencyStats {
						lat[st] = append(lat[st], e.Latency[st])
					}
				}
				thr = append(thr, float64(e.Requests))
			}
			return lat, thr
		}
		var noSuccess bool
		bLat, bThr := collect(base, &bReq, &bFail, &bLim)
		hLat, hThr := collect(head, &hReq, &hFail, &hLim)
		ec.BaseErrorRate, ec.HeadErrorRate = percent(bFail, bReq), percent(hFail, hReq)
		ec.BaseRateLimitRate, ec.HeadRateLimitRate = percent(bLim, bReq), percent(hLim, hReq)

		if missing {
			ec.Reasons = append(ec.Reasons, "not measured in every round")
		}
		if math.Abs(ec.BaseErrorRate-ec.HeadErrorRate) > maxRateGapPoints {
			ec.Reasons = append(ec.Reasons, fmt.Sprintf("error rate differs: %.1f%% vs %.1f%%", ec.BaseErrorRate, ec.HeadErrorRate))
		}
		if math.Abs(ec.BaseRateLimitRate-ec.HeadRateLimitRate) > maxRateGapPoints {
			ec.Reasons = append(ec.Reasons, fmt.Sprintf("rate-limited rate differs: %.1f%% vs %.1f%%", ec.BaseRateLimitRate, ec.HeadRateLimitRate))
		}
		for _, st := range latencyStats {
			ms := newMeasure(st, true, bLat[st], hLat[st])
			// 成功の無い回があった側は、残りの回だけで幅を作ることになるので判定しない
			if len(bLat[st]) != len(base) || len(hLat[st]) != len(head) {
				ms.Verdict, ms.Better = VerdictIncomparable, nil
				noSuccess = true
			}
			ec.Measures = append(ec.Measures, ms)
		}
		ec.Measures = append(ec.Measures, newMeasure("requests", false, bThr, hThr))
		if noSuccess {
			ec.Reasons = append(ec.Reasons, "a round had no successful response, so its latency was not measured")
		}

		// 失敗の割合が違えば、速さの差は失敗の差かもしれないので、どの指標も判定しない
		if len(ec.Reasons) > 0 {
			ec.Verdict = VerdictIncomparable
			for i := range ec.Measures {
				ec.Measures[i].Verdict = VerdictIncomparable
				ec.Measures[i].Better = nil
			}
		} else {
			ec.Verdict = worst(ec.Measures)
		}
		out = append(out, ec)
	}
	return out
}

func compareResources(base, head []*Run) []ResourceCompare {
	names := map[string]bool{}
	for _, r := range append(append([]*Run{}, base...), head...) {
		for n := range r.Resources {
			names[n] = true
		}
	}
	var out []ResourceCompare
	for _, name := range sortedKeys(names) {
		pick := func(runs []*Run, heap bool, f func(ResourceUsage) float64) []float64 {
			var vs []float64
			for _, r := range runs {
				if u, ok := r.Resources[name]; ok && (!heap || u.HasHeap) {
					vs = append(vs, f(u))
				}
			}
			return vs
		}
		rc := ResourceCompare{Container: name}
		for _, m := range []struct {
			name string
			heap bool
			f    func(ResourceUsage) float64
		}{
			{"cpu_seconds", false, func(u ResourceUsage) float64 { return u.CPUSeconds }},
			{"cpu_percent", false, func(u ResourceUsage) float64 { return u.CPUPercent }},
			{"memory_peak_mib", false, func(u ResourceUsage) float64 { return float64(u.MemoryPeakBytes) / (1 << 20) }},
			{"heap_inuse_max_mib", true, func(u ResourceUsage) float64 { return float64(u.HeapInuseMaxBytes) / (1 << 20) }},
		} {
			b, h := pick(base, m.heap, m.f), pick(head, m.heap, m.f)
			// Go のヒープは app にしか無い。どの回にも無ければ行を出さない
			if len(b) == 0 && len(h) == 0 {
				continue
			}
			ms := newMeasure(m.name, true, b, h)
			if len(b) != len(base) || len(h) != len(head) {
				ms.Verdict, ms.Better = VerdictIncomparable, nil
			}
			rc.Measures = append(rc.Measures, ms)
		}
		out = append(out, rc)
	}
	return out
}

// minAbsDifference is the smallest difference of the medians, in the unit of
// each measure, reported as different. Measures not listed have none.
//
// 割合だけで切ると、ほとんど動いていないコンテナの CPU 時間 (0.001 秒と
// 0.003 秒) や、1ms を切る遅延の揺れが大きな % になって「差あり」になる。
var minAbsDifference = map[string]float64{
	"med":                1,
	"p(95)":              1,
	"cpu_seconds":        0.5,
	"cpu_percent":        2,
	"memory_peak_mib":    8,
	"heap_inuse_max_mib": 8,
}

// referenceOnly lists the measures that are shown with their difference but
// never judged, so they do not make an endpoint different.
//
// p99 は 1 ラウンド数千件のうち上位 1% で決まり、GC や CPU の取り合いで段を飛ぶ
// ように動く (A/A の 5 回で home-timeline の p99 が 412〜759ms)。同じ commit
// どうしでも、幅が重ならずに +19.0% と +15.9% の「差あり」が出た (#3559)。この 2 回
// は下限 20% で防げるが、記録には約 510ms と約 700ms の段が両方あり、両側に分かれる
// と 37% になるので、下限では防げない。
var referenceOnly = map[string]bool{
	"p(99)": true,
}

// newMeasure compares the per-round values of base and head.
func newMeasure(name string, lowerIsBetter bool, base, head []float64) Measure {
	m := Measure{Name: name, LowerIsBetter: lowerIsBetter}
	if len(base) == 0 || len(head) == 0 {
		m.Verdict = VerdictIncomparable
		return m
	}
	m.Base, m.Head = spread(base), spread(head)
	if m.Base.Median != 0 {
		d := (m.Head.Median - m.Base.Median) / m.Base.Median * 100
		m.DeltaPercent = &d
	}
	if referenceOnly[name] {
		m.Verdict = VerdictReference
		return m
	}
	// 幅が重なれば、差はラウンドごとのばらつきと区別できない。重ならなくても、
	// 差が小さければ偶然と区別できない (minEffectPercent)
	separated := m.Head.Max < m.Base.Min || m.Head.Min > m.Base.Max
	large := (m.DeltaPercent == nil || math.Abs(*m.DeltaPercent) >= minEffectPercent) &&
		math.Abs(m.Head.Median-m.Base.Median) >= minAbsDifference[name]
	if separated && large {
		m.Verdict = VerdictDifferent
		better := (m.Head.Median < m.Base.Median) == lowerIsBetter
		m.Better = &better
	} else {
		m.Verdict = VerdictSame
	}
	return m
}

func spread(vs []float64) Spread {
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	n := len(s)
	med := s[n/2]
	if n%2 == 0 {
		med = (s[n/2-1] + s[n/2]) / 2
	}
	return Spread{Median: med, Min: s[0], Max: s[n-1], Rounds: n}
}

// worst returns Incomparable if any measure is, else Different if any is,
// else Same. Reference measures count as Same.
func worst(ms []Measure) Verdict {
	v := VerdictSame
	for _, m := range ms {
		switch m.Verdict {
		case VerdictIncomparable:
			return VerdictIncomparable
		case VerdictDifferent:
			v = VerdictDifferent
		}
	}
	return v
}

func percent(n, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
