package main

import (
	"fmt"
	"io"
	"strings"
)

// verdictLabel is how each verdict is shown in the report.
var verdictLabel = map[Verdict]string{
	VerdictDifferent:    "差あり",
	VerdictSame:         "差なし",
	VerdictIncomparable: "比較不能",
	VerdictReference:    "参考",
}

// sideNames is how the report calls base and head, and how it says which
// one was better.
type sideNames struct {
	base, head string
	// better and worse follow a real difference: "改善" / "悪化" for two
	// commits, "<head>が良い" / "<base>が良い" for two implementations.
	better, worse string
	labeled       bool
}

func namesOf(c *Comparison) sideNames {
	if c.Labels == nil {
		return sideNames{base: "base", head: "head", better: "改善", worse: "悪化"}
	}
	return sideNames{base: c.Labels.Base, head: c.Labels.Head, better: c.Labels.Head + "が良い", worse: c.Labels.Base + "が良い", labeled: true}
}

// renderMarkdown writes the report for the Actions summary.
func renderMarkdown(w io.Writer, c *Comparison) error {
	n := namesOf(c)
	var b strings.Builder
	if n.labeled {
		fmt.Fprintf(&b, "# 性能比較 (%s と %s)\n\n", cell(n.base), cell(n.head))
	} else {
		b.WriteString("# 性能比較\n\n")
	}
	// 本家の image の digest は、artifact が消えた後も Summary から pull し直せるよう、
	// 実装の違う比較では切らずに出す
	shorten := short
	if n.labeled {
		shorten = func(s string) string { return s }
	}
	fmt.Fprintf(&b, "- %s: %s (%s)\n", cell(n.base), code(c.Base.Ref), code(shorten(c.Base.Commit)))
	fmt.Fprintf(&b, "- %s: %s (%s)\n", cell(n.head), code(c.Head.Ref), code(shorten(c.Head.Commit)))
	fmt.Fprintf(&b, "- シナリオ: %s、ラウンド: %d\n", code(c.Scenario), c.Rounds)
	renderRunner(&b, c.Runner)
	b.WriteString("\n")
	if !c.Valid {
		b.WriteString("## 比較不能\n\n測定が揃わなかったので、差を判定しない。\n\n")
		for _, p := range c.Problems {
			fmt.Fprintf(&b, "- %s\n", cell(p))
		}
		renderDeliverySection(&b, c.Delivery)
		_, err := io.WriteString(w, b.String())
		return err
	}
	if n.labeled {
		fmt.Fprintf(&b, "判定は、各側のラウンドの最小〜最大の幅が重ならず、中央値の差が5%%以上のときだけ「差あり」にする。あわせて、差の絶対量が遅延は1ms以上、CPU時間は0.5秒以上、CPU (%%)は2ポイント以上、メモリは8MiB以上であること。差の%%は%sに対する%sの中央値の増減。失敗率か429の率が両者で1ポイントより大きく違うエンドポイントと、成功が0件の回があったエンドポイント、負荷をかける側のk6が律速だった(エンドポイントの区間で、k6がCPUの上限で止められた区切りが2%%を超えた)エンドポイント、決めた到着率で送れなかった件数(dropped_iterations)が送るべき件数の0.1%%を超えたエンドポイントは「比較不能」にする。リクエスト数は到着率で決まるので、差だけを出して判定しない(「参考」)。p99は同じものどうしでも段を飛ぶように動くので、差だけを出し、判定には使わない(「参考」)。**同じ回の中の比だけを読み、回をまたいで絶対値を比べない**(runnerのCPUが回ごとに違う)。\n\n", cell(n.base), cell(n.head))
	} else {
		b.WriteString("判定は、各側のラウンドの最小〜最大の幅が重ならず、中央値の差が5%以上のときだけ「差あり」にする。あわせて、差の絶対量が遅延は1ms以上、CPU時間は0.5秒以上、CPU (%)は2ポイント以上、メモリとGoのヒープは8MiB以上であること。差の%は中央値どうしの比。失敗率か429の率がbaseとheadで1ポイントより大きく違うエンドポイントと、成功が0件の回があったエンドポイント、負荷をかける側のk6が律速だった(エンドポイントの区間で、k6がCPUの上限で止められた区切りが2%を超えた)エンドポイント、決めた到着率で送れなかった件数(dropped_iterations)が送るべき件数の0.1%を超えたエンドポイントは「比較不能」にする。リクエスト数は到着率で決まるので、差だけを出して判定しない(「参考」)。p99は同じcommitどうしでも段を飛ぶように動くので、差だけを出し、判定には使わない(「参考」)。\n\n")
	}
	b.WriteString("## まとめ\n\n| エンドポイント | 判定 |\n|---|---|\n")
	for _, e := range c.Endpoints {
		fmt.Fprintf(&b, "| %s | %s |\n", cell(e.Endpoint), endpointSummary(e, n))
	}
	if n.labeled {
		renderRatioSection(&b, c, n)
	}
	b.WriteString("\n## エンドポイント\n\n")
	fmt.Fprintf(&b, "| エンドポイント | 指標 | %s (中央値 [最小, 最大]) | %s (中央値 [最小, 最大]) | 差 | 判定 |\n", cell(n.base), cell(n.head))
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, e := range c.Endpoints {
		for _, m := range e.Measures {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", cell(e.Endpoint), measureLabel(m.Name), fmtSpread(m.Base, m.Name), fmtSpread(m.Head, m.Name), fmtDelta(m), fmtVerdict(m, n))
		}
		rateVerdict := ""
		if len(e.Reasons) > 0 {
			rateVerdict = verdictLabel[VerdictIncomparable]
		}
		fmt.Fprintf(&b, "| %s | 失敗率 / 429の率 | %.1f%% / %.1f%% | %.1f%% / %.1f%% | | %s |\n", cell(e.Endpoint), e.BaseErrorRate, e.BaseRateLimitRate, e.HeadErrorRate, e.HeadRateLimitRate, rateVerdict)
		k6Verdict := ""
		if k6Bottleneck(e) {
			k6Verdict = verdictLabel[VerdictIncomparable]
		}
		fmt.Fprintf(&b, "| %s | k6が止められた区切り (%%、最大) | %s | %s | | %s |\n", cell(e.Endpoint), fmtPercent(e.BaseK6ThrottledPercent), fmtPercent(e.HeadK6ThrottledPercent), k6Verdict)
		dropVerdict := ""
		if e.DroppedTooMany {
			dropVerdict = verdictLabel[VerdictIncomparable]
		}
		fmt.Fprintf(&b, "| %s | 送れなかった件数 (合計) | %d | %d | | %s |\n", cell(e.Endpoint), e.BaseDropped, e.HeadDropped, dropVerdict)
	}
	var reasons []string
	for _, e := range c.Endpoints {
		for _, r := range e.Reasons {
			reasons = append(reasons, fmt.Sprintf("- %s: %s", cell(e.Endpoint), cell(r)))
		}
	}
	if len(reasons) > 0 {
		b.WriteString("\n比較不能の理由:\n\n" + strings.Join(reasons, "\n") + "\n")
	}
	b.WriteString("\n## リソース\n\n")
	if n.labeled {
		b.WriteString("CPUは1コアを100%とする。メモリはcgroupの`memory.peak`(ページキャッシュを含む)。Goのヒープはappの`HeapInuse`の最大で、片側にしか無いときは参考として出す。k6は負荷をかける側なので、差だけを出して判定しない(「参考」)。\n\n")
	} else {
		b.WriteString("CPUは1コアを100%とする。メモリはcgroupの`memory.peak`、Goのヒープはappの`HeapInuse`の最大。k6は負荷をかける側なので、差だけを出して判定しない(「参考」)。\n\n")
	}
	fmt.Fprintf(&b, "| コンテナ | 指標 | %s (中央値 [最小, 最大]) | %s (中央値 [最小, 最大]) | 差 | 判定 |\n", cell(n.base), cell(n.head))
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, r := range c.Resources {
		for _, m := range r.Measures {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", cell(r.Container), measureLabel(m.Name), fmtSpread(m.Base, m.Name), fmtSpread(m.Head, m.Name), fmtDelta(m), fmtVerdict(m, n))
		}
	}
	renderDeliverySection(&b, c.Delivery)
	_, err := io.WriteString(w, b.String())
	return err
}

// writeJitterCPUModel is the runner CPU on which PostgreSQL's writes were
// seen to stall (#3659). It is matched case-insensitively as a substring of
// cpu_model, which is the "model name" line of /proc/cpuinfo
// ("AMD EPYC 9V74 80-Core Processor").
const writeJitterCPUModel = "AMD EPYC 9V74"

// renderRunner writes the runner's CPU model first, with a note when it is
// the model of #3659, and then the other runner fields.
func renderRunner(b *strings.Builder, runner map[string]string) {
	// 機種は回ごとに違い、数字の読み方が変わる (#3659) ので、他の項目より先に、
	// 日本語の見出しで出す
	// 機種の書き方はハイパーバイザで大文字小文字が変わる例がある (Intelで
	// `INTEL(R) XEON(R) PLATINUM 8573C`) ので、比べるときは大文字に揃える。
	// /proc/cpuinfo に model name が無い環境では空になるので「(不明)」と出す
	if model, ok := runner["cpu_model"]; ok {
		shown := strings.TrimSpace(model)
		if shown == "" {
			shown = "(不明)"
		}
		fmt.Fprintf(b, "- runnerのCPU: %s\n", cell(shown))
		if strings.Contains(strings.ToUpper(model), strings.ToUpper(writeJitterCPUModel)) {
			fmt.Fprintf(b, "- **注意: この機種(%s)では書き込みが揺れうる (#3659)。** PostgreSQLはfsync・full page write・COMMITの待ちを外して動かしているが、それで揺れが消えるかは確かめていない。`notes-create`の差は流し直して確かめる\n", writeJitterCPUModel)
		}
	}
	for _, k := range sortedRunnerKeys(runner) {
		if k == "cpu_model" {
			continue
		}
		fmt.Fprintf(b, "- %s: %s\n", cell(k), cell(runner[k]))
	}
}

// renderRatioSection writes p50 / p95 of each endpoint and the CPU time and
// memory of each container with the ratio base / head, for a comparison of
// two implementations (#3654).
func renderRatioSection(b *strings.Builder, c *Comparison, n sideNames) {
	fmt.Fprintf(b, "\n## 比 (%s / %s)\n\n", cell(n.base), cell(n.head))
	fmt.Fprintf(b, "比は、%sの中央値を%sの中央値で割ったもの。1より大きければ%sのほうが速い(リソースなら少ない)。判定は下の表と同じ基準で、差なしのときはどちらが速いとも言わない。\n\n", cell(n.base), cell(n.head), cell(n.head))
	fmt.Fprintf(b, "| エンドポイント | p50 %s | p50 %s | p50の比 | p95 %s | p95 %s | p95の比 | 判定 (p50 / p95) |\n", cell(n.base), cell(n.head), cell(n.base), cell(n.head))
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, e := range c.Endpoints {
		p50, p95 := findMeasure(e.Measures, "med"), findMeasure(e.Measures, "p(95)")
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s | %s | %s / %s |\n", cell(e.Endpoint),
			fmtMedian(p50.Base), fmtMedian(p50.Head), fmtRatio(p50),
			fmtMedian(p95.Base), fmtMedian(p95.Head), fmtRatio(p95),
			lowerSide(p50, n, "が速い"), lowerSide(p95, n, "が速い"))
	}
	fmt.Fprintf(b, "\n| コンテナ | 指標 | %s | %s | 比 | 判定 |\n", cell(n.base), cell(n.head))
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, r := range c.Resources {
		for _, name := range []string{"cpu_seconds", "memory_peak_mib"} {
			m := findMeasure(r.Measures, name)
			if m.Name == "" {
				continue
			}
			fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s |\n", cell(r.Container), measureLabel(name), fmtMedian(m.Base), fmtMedian(m.Head), fmtRatio(m), lowerSide(m, n, "が少ない"))
		}
	}
}

// k6Bottleneck reports whether k6 was throttled beyond the limit, or not
// measured, on either side of the endpoint.
func k6Bottleneck(e EndpointCompare) bool {
	for _, p := range []*float64{e.BaseK6ThrottledPercent, e.HeadK6ThrottledPercent} {
		if p == nil || *p > maxK6ThrottledPercent {
			return true
		}
	}
	return false
}

func fmtPercent(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", *p)
}

func findMeasure(ms []Measure, name string) Measure {
	for _, m := range ms {
		if m.Name == name {
			return m
		}
	}
	return Measure{}
}

func fmtMedian(s Spread) string {
	if s.Rounds == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", s.Median)
}

func fmtRatio(m Measure) string {
	if m.Verdict == VerdictIncomparable || m.BaseOverHead == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *m.BaseOverHead)
}

// lowerSide says which side was lower for a lower-is-better measure, as
// "<name><suffix>" ("が速い" for latency, "が少ない" for resources).
func lowerSide(m Measure, n sideNames, suffix string) string {
	if m.Verdict != VerdictDifferent || m.Better == nil {
		return verdictLabel[m.Verdict]
	}
	if *m.Better {
		return cell(n.head) + suffix
	}
	return cell(n.base) + suffix
}

func renderDeliverySection(b *strings.Builder, rounds []DeliveryRound) {
	if len(rounds) == 0 {
		return
	}
	b.WriteString("\n## 配送受信\n\n`status` は配送統計の完了状態と件数の整合性を表します。`incomplete` / `invalid` の round がある比較は性能判定を行わず、比較不能にします。生の件数は診断用に残します。未確定・未着は`incomplete`、期待数0・件数の不整合・`invalid` / `contentMismatch` / `unexpected` は`invalid`です。`valid` は期待 Activity ID・Inbox・本文 digest が一致した試行、`unique` はその一意 tuple 数です。故意の不正・未到着 correctness ケースは別のテストとして期待結果を判定し、性能 run に混ぜません。\n\n")
	b.WriteString("| side | round | status | expected | attempts | verified | pending | valid | unique | duplicates | invalid | content mismatch | unexpected pairs | unexpected attempts | missing |\n")
	b.WriteString("|---|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range rounds {
		fmt.Fprintf(b, "| %s | %d | %s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d |\n",
			cell(string(r.Side)), r.Round, cell(string(r.Status)), r.Expected, r.Attempts, r.Verified, r.Pending, r.Valid, r.Unique, r.Duplicates, r.Invalid, r.ContentMismatch, r.Unexpected, r.UnexpectedAttempts, r.Missing)
	}
}

// endpointSummary is the verdict of an endpoint, with how many of its
// measures got better or worse when it differs.
func endpointSummary(e EndpointCompare, n sideNames) string {
	if e.Verdict != VerdictDifferent {
		return verdictLabel[e.Verdict]
	}
	var better, worse int
	for _, m := range e.Measures {
		if m.Better == nil {
			continue
		}
		if *m.Better {
			better++
		} else {
			worse++
		}
	}
	return fmt.Sprintf("%s (%s %d / %s %d)", verdictLabel[e.Verdict], cell(n.better), better, cell(n.worse), worse)
}

func measureLabel(name string) string {
	switch name {
	case "med":
		return "p50 (ms)"
	case "p(95)":
		return "p95 (ms)"
	case "p(99)":
		return "p99 (ms)"
	case "requests":
		return "リクエスト数"
	case "cpu_seconds":
		return "CPU時間 (秒)"
	case "cpu_percent":
		return "CPU (%)"
	case "memory_peak_mib":
		return "メモリの最大 (MiB)"
	case "heap_inuse_max_mib":
		return "Goのヒープの最大 (MiB)"
	}
	return name
}

func fmtSpread(s Spread, name string) string {
	if s.Rounds == 0 {
		return "-"
	}
	if name == "requests" {
		return fmt.Sprintf("%.0f [%.0f, %.0f]", s.Median, s.Min, s.Max)
	}
	return fmt.Sprintf("%.1f [%.1f, %.1f]", s.Median, s.Min, s.Max)
}

func fmtDelta(m Measure) string {
	if m.Verdict == VerdictIncomparable {
		return ""
	}
	if m.DeltaPercent == nil {
		return "n/a"
	}
	return fmt.Sprintf("%+.1f%%", *m.DeltaPercent)
}

// cell escapes text for a Markdown table cell (and a list item): a pipe would
// split the cell and a newline would end the table.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
}

// fmtVerdict adds whether a real difference is better or worse for head.
func fmtVerdict(m Measure, n sideNames) string {
	label := verdictLabel[m.Verdict]
	if m.Better == nil {
		return label
	}
	if *m.Better {
		return label + " (" + cell(n.better) + ")"
	}
	return label + " (" + cell(n.worse) + ")"
}

// code writes text as inline code, with the backticks in it replaced so that
// it cannot close the span.
func code(s string) string {
	return "`" + strings.ReplaceAll(cell(s), "`", "'") + "`"
}

func short(sha string) string {
	// image の digest は sha256: の後ろを 12 桁にする
	if d, ok := strings.CutPrefix(sha, "sha256:"); ok {
		return "sha256:" + short(d)
	}
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func sortedRunnerKeys(m map[string]string) []string {
	keys := map[string]bool{}
	for k := range m {
		keys[k] = true
	}
	return sortedKeys(keys)
}
