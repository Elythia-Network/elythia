package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
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
	Labels    *Labels           `json:"labels,omitempty"`
	Valid     bool              `json:"valid"`
	Problems  []string          `json:"problems,omitempty"`
	Endpoints []EndpointCompare `json:"endpoints"`
	Resources []ResourceCompare `json:"resources"`
	Delivery  []DeliveryRound   `json:"delivery,omitempty"`
	DeliveryMeasures []Measure  `json:"deliveryMeasures,omitempty"`
}

// DeliveryRound carries receiver counts and a data-quality status through to
// JSON and Markdown output. A delivery problem makes the performance
// comparison invalid; the raw counts remain available for diagnosis.
type DeliveryRound struct {
	Side    Side           `json:"side"`
	Round   int            `json:"round"`
	Status  DeliveryStatus `json:"status"`
	Reasons []string       `json:"reasons,omitempty"`
	DeliveryStats
}

// DeliveryStatus describes whether a normal delivery run is suitable for a
// performance comparison. It is a correctness status, not a performance
// threshold.
type DeliveryStatus string

const (
	DeliveryStatusValid      DeliveryStatus = "valid"
	DeliveryStatusIncomplete DeliveryStatus = "incomplete"
	DeliveryStatusInvalid    DeliveryStatus = "invalid"
)

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
	// The largest share, in percent, of k6's CPU periods in which k6 was
	// throttled during the endpoint's window, over the rounds of each side.
	// Nil when a round could not be measured.
	BaseK6ThrottledPercent *float64 `json:"baseK6ThrottledPercent"`
	HeadK6ThrottledPercent *float64 `json:"headK6ThrottledPercent"`
	// Iterations the arrival rate asked for but k6 could not start, over all
	// rounds of each side.
	BaseDropped int64 `json:"baseDropped"`
	HeadDropped int64 `json:"headDropped"`
	// DroppedTooMany is true when either side dropped more than
	// maxDroppedPercent of the endpoint's iterations.
	DroppedTooMany bool `json:"droppedTooMany"`
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
	// BaseOverHead is the base median over the head median; nil when the
	// head median is 0. Above 1, head is lower (faster, for a latency).
	BaseOverHead *float64 `json:"baseOverHead,omitempty"`
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

// maxK6ThrottledPercent is the largest share, in percent, of k6's CPU periods
// during an endpoint's window in which k6 may be throttled (stopped by its CPU
// limit) before the endpoint is incomparable (#3656).
//
// k6 が CPU の上限で止められると、止まっている間に返った応答は、止まった分だけ長く
// 測られる。1 つの区切り (100ms) の中で止まるのは枠を使い切った後の残りだけなので、
// 止められた区切りの割合が r なら、止まっていた時間は区間の r 未満になる。到着が一定
// (constant-arrival-rate) なら、長く測られる応答もおおむね r 未満になる。判定に使う
// いちばん上の分位は p95 (上位 5%) で、r が 5% に近づくと p95 が k6 の待ちで決まりうる。
// 2% なら、長く測られた応答がすべて上位に寄っても、p95 はサーバーの分布の p97 (上位 5% の
// うち 2% を k6 の分が占め、残り 3% / 98%) までにとどまる。k6 が飽和すると、区切りの
// ほぼすべてで止められる (手元で CPU 0.75 の k6 を飽和させたとき、どのエンドポイントの
// 区間も 94.7〜100%) ので、飽和した回はこの線で確実に分かれる。
const maxK6ThrottledPercent = 2.0

// maxDroppedPercent is the largest share, in percent, of an endpoint's
// iterations (sent and dropped, over all rounds of a side) that k6 may drop
// before the endpoint is incomparable (#3656).
//
// 送れなかった iteration は、その側が決めた率より軽い負荷で測ったことを示す。ただ、
// 毎秒 1000 件のエンドポイントでは、50ms ほどの詰まり (GC の停止や CPU の区切りの待ち) で
// 応答を待つ VU が 100 個に届き、数件が落ちうる。30000 件のうちの数件は、判定に使う p95
// (上位 5%) をほとんど動かさない。落ちた iteration は送られず測られないので、落ちた分が
// すべて遅い応答 (詰まりの最中) だったとすると、測った分布から上位が抜け、p95 はサーバーの
// 分布の p94.9 (95 × 99.9%) の方へ下がる。落とした側がわずかに速く見える向きで、0.1% まで
// ならその偏りは 0.1 分位にとどまる。それを超えたら、その側は率に追いつけていないとして
// 比較不能にする。
const maxDroppedPercent = 0.1

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
	c := &Comparison{Base: rs.Base, Head: rs.Head, Scenario: rs.Scenario, Rounds: rs.Rounds, Runner: rs.Runner, Labels: rs.Labels, Valid: true}
	validateDeliveryRuns(c, rs)
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
		// エンドポイントのシナリオで落ちた分は、そのエンドポイントだけを比較不能にする。
		// どのシナリオにも数えられない分は、どこの負荷が減ったか分からないので比較全体を止める
		var attributed int64
		for _, e := range r.Endpoints {
			attributed += e.Dropped
		}
		if n := r.Dropped - attributed; n > 0 {
			c.problem("%s round %d dropped %d iterations outside the endpoints' scenarios (the load was lower than requested)", r.Meta.Side, r.Meta.Round, n)
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
		// Delivery throughput is useful only when the whole comparison is
		// comparable. Do not leave a valid-looking measure beside a failed run.
		c.DeliveryMeasures = nil
		return c
	}
	c.Endpoints = compareEndpoints(base, head)
	c.Resources = compareResources(base, head, rs.Labels != nil)
	return c
}

func validateDeliveryRuns(c *Comparison, rs *RunSet) {
	hasDelivery := rs.DeliveryEnabled
	for _, r := range rs.Runs {
		if r.Delivery != nil || r.DeliveryError != "" {
			hasDelivery = true
			break
		}
	}
	if !hasDelivery {
		return
	}

	allValid := true
	throughput := map[Side][]float64{SideBase: {}, SideHead: {}}
	for _, r := range rs.Runs {
		if r.Delivery == nil {
			reason := "delivery stats are missing"
			if r.DeliveryError != "" {
				reason += "; " + r.DeliveryError
			}
			c.problem("%s round %d %s", r.Meta.Side, r.Meta.Round, reason)
			allValid = false
			continue
		}
		status, reasons := deliveryStatus(*r.Delivery)
		status, reasons = mergeDeliveryArtifactStatus(status, reasons, r.Delivery.ArtifactStatus, r.Delivery.ArtifactReasons, r.DeliveryError)
		c.Delivery = append(c.Delivery, DeliveryRound{
			Side: r.Meta.Side, Round: r.Meta.Round, Status: status,
			Reasons: reasons, DeliveryStats: *r.Delivery,
		})
		if status != DeliveryStatusValid {
			c.problem("%s round %d delivery %s: %s", r.Meta.Side, r.Meta.Round, status, strings.Join(reasons, ", "))
			allValid = false
			continue
		}
		if !rs.DeliveryEnabled {
			continue
		}
		rate, reason := deliveryThroughput(*r.Delivery)
		if reason != "" {
			c.problem("%s round %d delivery throughput: %s", r.Meta.Side, r.Meta.Round, reason)
			allValid = false
			continue
		}
		throughput[r.Meta.Side] = append(throughput[r.Meta.Side], rate)
	}
	if !rs.DeliveryEnabled {
		return
	}
	if allValid && len(throughput[SideBase]) == rs.Rounds && len(throughput[SideHead]) == rs.Rounds {
		c.DeliveryMeasures = append(
			c.DeliveryMeasures,
			newMeasure("delivery_unique_jobs_per_sec", false, throughput[SideBase], throughput[SideHead]),
		)
	} else if allValid {
		c.problem("delivery throughput is missing from one or more side rounds")
	}
}

func mergeDeliveryArtifactStatus(
	status DeliveryStatus,
	reasons []string,
	artifactStatus string,
	artifactReasons []string,
	deliveryError string,
) (DeliveryStatus, []string) {
	merged := append([]string(nil), reasons...)
	addReason := func(reason string) {
		if reason == "" {
			return
		}
		for _, existing := range merged {
			if existing == reason {
				return
			}
		}
		merged = append(merged, reason)
	}

	artifact := DeliveryStatus(artifactStatus)
	switch artifact {
	case "":
	case DeliveryStatusValid, DeliveryStatusIncomplete, DeliveryStatusInvalid:
		status = worseDeliveryStatus(status, artifact)
	default:
		status = worseDeliveryStatus(status, DeliveryStatusInvalid)
		addReason(fmt.Sprintf("unrecognized delivery artifact status %q", artifactStatus))
	}
	if len(artifactReasons) > 0 && (artifact == "" || artifact == DeliveryStatusValid) {
		// A valid artifact must not carry failure reasons. Treat contradictory
		// metadata as incomplete rather than trusting its optimistic status.
		status = worseDeliveryStatus(status, DeliveryStatusIncomplete)
	}
	for _, reason := range artifactReasons {
		addReason(reason)
	}
	if deliveryError != "" {
		status = worseDeliveryStatus(status, DeliveryStatusIncomplete)
		addReason("delivery run error: " + deliveryError)
	}
	return status, merged
}

func worseDeliveryStatus(a, b DeliveryStatus) DeliveryStatus {
	rank := func(s DeliveryStatus) int {
		switch s {
		case DeliveryStatusInvalid:
			return 2
		case DeliveryStatusIncomplete:
			return 1
		default:
			return 0
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func deliveryThroughput(s DeliveryStats) (float64, string) {
	if s.ThroughputJobs == nil || *s.ThroughputJobs != s.Unique {
		return 0, "throughputJobs is missing or differs from unique deliveries"
	}
	if s.DrainSeconds == nil || *s.DrainSeconds <= 0 {
		return 0, "drainSeconds is missing or not positive"
	}
	if s.ThroughputJobsPerSecond == nil || *s.ThroughputJobsPerSecond <= 0 {
		return 0, "throughputJobsPerSecond is missing or not positive"
	}
	expected := float64(s.Unique) / *s.DrainSeconds
	delta := math.Abs(*s.ThroughputJobsPerSecond - expected)
	tolerance := math.Max(1e-9, math.Abs(expected)*1e-6)
	if delta > tolerance {
		return 0, fmt.Sprintf("throughput %.9g does not match unique/drain %.9g", *s.ThroughputJobsPerSecond, expected)
	}
	return *s.ThroughputJobsPerSecond, ""
}

func deliveryStatus(s DeliveryStats) (DeliveryStatus, []string) {
	var reasons []string
	if !s.Finalized {
		reasons = append(reasons, "manifest is not finalized")
	}
	if s.Pending > 0 {
		reasons = append(reasons, fmt.Sprintf("%d arrivals are pending", s.Pending))
	}
	if s.Expected == 0 {
		reasons = append(reasons, "expected delivery count is zero")
	}
	if s.Missing > 0 {
		reasons = append(reasons, fmt.Sprintf("%d expected deliveries are missing", s.Missing))
	}
	if s.Invalid > 0 {
		reasons = append(reasons, fmt.Sprintf("%d invalid deliveries", s.Invalid))
	}
	if s.ContentMismatch > 0 {
		reasons = append(reasons, fmt.Sprintf("%d content mismatches", s.ContentMismatch))
	}
	if s.Unexpected > 0 {
		reasons = append(reasons, fmt.Sprintf("%d unexpected delivery pairs", s.Unexpected))
	}
	countReasons := deliveryCountConsistencyReasons(s)
	reasons = append(reasons, countReasons...)
	if len(reasons) == 0 {
		return DeliveryStatusValid, nil
	}
	if s.Expected == 0 || s.Invalid > 0 || s.ContentMismatch > 0 || s.Unexpected > 0 || len(countReasons) > 0 {
		return DeliveryStatusInvalid, reasons
	}
	return DeliveryStatusIncomplete, reasons
}

func deliveryCountConsistencyReasons(s DeliveryStats) []string {
	var reasons []string
	if s.Valid > s.Attempts || s.Invalid > s.Attempts-s.Valid || s.UnexpectedAttempts != s.Attempts-s.Valid-s.Invalid {
		reasons = append(reasons, fmt.Sprintf("attempts %d != valid %d + invalid %d + unexpected attempts %d", s.Attempts, s.Valid, s.Invalid, s.UnexpectedAttempts))
	}
	if s.ContentMismatch > s.Invalid {
		reasons = append(reasons, fmt.Sprintf("content mismatch %d exceeds invalid %d", s.ContentMismatch, s.Invalid))
	}
	if s.Valid > s.Verified || s.ContentMismatch > s.Verified-s.Valid || s.UnexpectedAttempts != s.Verified-s.Valid-s.ContentMismatch {
		reasons = append(reasons, fmt.Sprintf("verified %d != valid %d + content mismatch %d + unexpected attempts %d", s.Verified, s.Valid, s.ContentMismatch, s.UnexpectedAttempts))
	}
	if s.Duplicates > s.Valid || s.Unique != s.Valid-s.Duplicates {
		reasons = append(reasons, fmt.Sprintf("valid %d != unique %d + duplicates %d", s.Valid, s.Unique, s.Duplicates))
	}
	if s.Unique > s.Expected {
		reasons = append(reasons, fmt.Sprintf("unique %d exceeds expected %d", s.Unique, s.Expected))
	} else if s.Missing != s.Expected-s.Unique {
		reasons = append(reasons, fmt.Sprintf("missing %d != expected %d - unique %d", s.Missing, s.Expected, s.Unique))
	}
	if s.Unexpected > s.UnexpectedAttempts {
		reasons = append(reasons, fmt.Sprintf("unexpected pairs %d exceeds unexpected attempts %d", s.Unexpected, s.UnexpectedAttempts))
	}
	return reasons
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
		var bReq, bFail, bLim, bDrop, hReq, hFail, hLim, hDrop int64
		collect := func(runs []*Run, req, fail, lim, drop *int64) (lat map[string][]float64, thr []float64) {
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
				*drop += e.Dropped
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
		bLat, bThr := collect(base, &bReq, &bFail, &bLim, &bDrop)
		hLat, hThr := collect(head, &hReq, &hFail, &hLim, &hDrop)
		ec.BaseDropped, ec.HeadDropped = bDrop, hDrop
		ec.BaseErrorRate, ec.HeadErrorRate = percent(bFail, bReq), percent(hFail, hReq)
		ec.BaseRateLimitRate, ec.HeadRateLimitRate = percent(bLim, bReq), percent(hLim, hReq)
		var bK6OK, hK6OK bool
		ec.BaseK6ThrottledPercent, bK6OK = maxK6Throttled(base, name)
		ec.HeadK6ThrottledPercent, hK6OK = maxK6Throttled(head, name)

		if missing {
			ec.Reasons = append(ec.Reasons, "not measured in every round")
		}
		// 送れなかった分が多ければ、その側は決めた率より軽い負荷で測っている。サーバーか k6 が
		// 率に追いつかなかったことを示し、時間は同じ負荷どうしの比較にならない
		ec.DroppedTooMany = tooManyDropped(bDrop, bReq) || tooManyDropped(hDrop, hReq)
		if ec.DroppedTooMany {
			ec.Reasons = append(ec.Reasons, fmt.Sprintf("more than %.1f%% of the iterations were dropped (the server or k6 did not keep up with the arrival rate): %d / %d", maxDroppedPercent, bDrop, hDrop))
		}
		// k6 が律速なら、速さの差はサーバーではなく k6 の待ちかもしれない
		if !bK6OK || !hK6OK {
			ec.Reasons = append(ec.Reasons, "k6's CPU was not sampled in the endpoint's window in every round")
		} else if *ec.BaseK6ThrottledPercent > maxK6ThrottledPercent || *ec.HeadK6ThrottledPercent > maxK6ThrottledPercent {
			ec.Reasons = append(ec.Reasons, fmt.Sprintf("k6 was the bottleneck: throttled in up to %.1f%% / %.1f%% of its CPU periods (more than %.0f%%)", *ec.BaseK6ThrottledPercent, *ec.HeadK6ThrottledPercent, maxK6ThrottledPercent))
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

// tooManyDropped reports whether dropped iterations are more than
// maxDroppedPercent of the iterations asked for (sent plus dropped).
func tooManyDropped(dropped, sent int64) bool {
	return percent(dropped, sent+dropped) > maxDroppedPercent
}

// maxK6Throttled returns the largest k6 throttled share of the endpoint's
// window over runs, and false when a run measured the endpoint but not k6's
// CPU in its window (the result is then nil).
func maxK6Throttled(runs []*Run, endpoint string) (*float64, bool) {
	worst := 0.0
	for _, r := range runs {
		e, ok := r.Endpoints[endpoint]
		if !ok {
			continue
		}
		p, ok := throttledPercent(r.K6CPU, e.WindowStartMs, e.WindowEndMs)
		if !ok {
			return nil, false
		}
		worst = max(worst, p)
	}
	return &worst, true
}

// throttledPercent is the share, in percent, of the CPU periods between the
// first and the last sample inside [start, end] in which the container was
// throttled. It is false with fewer than two samples inside the window.
func throttledPercent(samples []ThrottleSample, start, end int64) (float64, bool) {
	var first, last *ThrottleSample
	for i := range samples {
		s := &samples[i]
		if s.TimeMs < start || s.TimeMs > end {
			continue
		}
		if first == nil {
			first = s
		}
		last = s
	}
	if first == nil || first == last {
		return 0, false
	}
	periods := last.Periods - first.Periods
	if periods == 0 {
		// 1 度も動かなかった (区切りが数えられなかった) なら、止められてもいない
		return 0, true
	}
	return float64(last.Throttled-first.Throttled) / float64(periods) * 100, true
}

// compareResources compares the containers. The load generator's container
// (k6) is shown as a reference only. With crossImpl (the sides are
// different implementations), the Go heap that one side never has (upstream
// Misskey runs on Node.js) is shown for the other side only, as a reference,
// when the other side has it in every round. Any other gap stays
// incomparable.
func compareResources(base, head []*Run, crossImpl bool) []ResourceCompare {
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
			// 本家 (Node.js) には Go のヒープが無い。測れなかったのではなく元から無いので、
			// 比較不能にせず、ある側の値だけを参考として出す。片側だけになってよいのは
			// ヒープだけで、もう片側は全回で取れていること。CPU やメモリが片側で欠けた
			// のと、途中の回だけ欠けたのは、測れなかったので比較不能のままにする
			if crossImpl && m.heap && ((len(b) == 0 && len(h) == len(head)) || (len(h) == 0 && len(b) == len(base))) {
				rc.Measures = append(rc.Measures, oneSided(m.name, b, h))
				continue
			}
			ms := newMeasure(m.name, true, b, h)
			if len(b) != len(base) || len(h) != len(head) {
				ms.Verdict, ms.Better = VerdictIncomparable, nil
			} else if name == k6Container {
				// k6 は負荷をかける側で、その CPU やメモリはサーバーの良し悪しではない。応答の
				// 形 (gzip の有無など) で k6 が読む手間も変わるので、差は出すが判定しない
				ms.Verdict, ms.Better = VerdictReference, nil
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
// requests は到着率で送るので、送れなかった分が無ければ両側とも率 × 時間でほぼ決まる。
// 送れなかった分が多ければ、そのエンドポイントは別に比較不能になる。多い少ないで良し
// 悪しを言える場面が無いので、判定しない (#3656)。
//
// p99 は 1 ラウンド数千件のうち上位 1% で決まり、GC や CPU の取り合いで段を飛ぶ
// ように動く (A/A の 5 回で home-timeline の p99 が 412〜759ms)。同じ commit
// どうしでも、幅が重ならずに +19.0% と +15.9% の「差あり」が出た (#3559)。この 2 回
// は下限 20% で防げるが、記録には約 510ms と約 700ms の段が両方あり、両側に分かれる
// と 37% になるので、下限では防げない。
var referenceOnly = map[string]bool{
	"p(99)":                        true,
	"requests":                     true,
	"delivery_unique_jobs_per_sec": true,
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
	if m.Head.Median != 0 {
		r := m.Base.Median / m.Head.Median
		m.BaseOverHead = &r
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

// oneSided is a measure that only one side has. It is a reference: there is
// nothing to compare it with.
func oneSided(name string, base, head []float64) Measure {
	m := Measure{Name: name, LowerIsBetter: true, Verdict: VerdictReference}
	if len(base) > 0 {
		m.Base = spread(base)
	}
	if len(head) > 0 {
		m.Head = spread(head)
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
