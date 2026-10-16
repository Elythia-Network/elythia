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

// renderMarkdown writes the report for the Actions summary.
func renderMarkdown(w io.Writer, c *Comparison) error {
	var b strings.Builder
	b.WriteString("# 性能比較\n\n")
	fmt.Fprintf(&b, "- base: %s (%s)\n", code(c.Base.Ref), code(short(c.Base.Commit)))
	fmt.Fprintf(&b, "- head: %s (%s)\n", code(c.Head.Ref), code(short(c.Head.Commit)))
	fmt.Fprintf(&b, "- シナリオ: %s、ラウンド: %d\n", code(c.Scenario), c.Rounds)
	for _, k := range sortedRunnerKeys(c.Runner) {
		fmt.Fprintf(&b, "- %s: %s\n", cell(k), cell(c.Runner[k]))
	}
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
	b.WriteString("判定は、各側のラウンドの最小〜最大の幅が重ならず、中央値の差が5%以上のときだけ「差あり」にする。あわせて、差の絶対量が遅延は1ms以上、CPU時間は0.5秒以上、CPU (%)は2ポイント以上、メモリとGoのヒープは8MiB以上であること。差の%は中央値どうしの比。失敗率か429の率がbaseとheadで1ポイントより大きく違うエンドポイントと、成功が0件の回があったエンドポイントは「比較不能」にする。p99は同じcommitどうしでも段を飛ぶように動くので、差だけを出し、判定には使わない(「参考」)。\n\n")
	b.WriteString("## まとめ\n\n| エンドポイント | 判定 |\n|---|---|\n")
	for _, e := range c.Endpoints {
		fmt.Fprintf(&b, "| %s | %s |\n", cell(e.Endpoint), endpointSummary(e))
	}
	b.WriteString("\n## エンドポイント\n\n")
	b.WriteString("| エンドポイント | 指標 | base (中央値 [最小, 最大]) | head (中央値 [最小, 最大]) | 差 | 判定 |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, e := range c.Endpoints {
		for _, m := range e.Measures {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", cell(e.Endpoint), measureLabel(m.Name), fmtSpread(m.Base, m.Name), fmtSpread(m.Head, m.Name), fmtDelta(m), fmtVerdict(m))
		}
		rateVerdict := ""
		if len(e.Reasons) > 0 {
			rateVerdict = verdictLabel[VerdictIncomparable]
		}
		fmt.Fprintf(&b, "| %s | 失敗率 / 429の率 | %.1f%% / %.1f%% | %.1f%% / %.1f%% | | %s |\n", cell(e.Endpoint), e.BaseErrorRate, e.BaseRateLimitRate, e.HeadErrorRate, e.HeadRateLimitRate, rateVerdict)
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
	b.WriteString("CPUは1コアを100%とする。メモリはcgroupの`memory.peak`、Goのヒープはappの`HeapInuse`の最大。\n\n")
	b.WriteString("| コンテナ | 指標 | base (中央値 [最小, 最大]) | head (中央値 [最小, 最大]) | 差 | 判定 |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, r := range c.Resources {
		for _, m := range r.Measures {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", cell(r.Container), measureLabel(m.Name), fmtSpread(m.Base, m.Name), fmtSpread(m.Head, m.Name), fmtDelta(m), fmtVerdict(m))
		}
	}
	renderDeliverySection(&b, c.Delivery)
	_, err := io.WriteString(w, b.String())
	return err
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
func endpointSummary(e EndpointCompare) string {
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
	return fmt.Sprintf("%s (改善 %d / 悪化 %d)", verdictLabel[e.Verdict], better, worse)
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
func fmtVerdict(m Measure) string {
	label := verdictLabel[m.Verdict]
	if m.Better == nil {
		return label
	}
	if *m.Better {
		return label + " (改善)"
	}
	return label + " (悪化)"
}

// code writes text as inline code, with the backticks in it replaced so that
// it cannot close the span.
func code(s string) string {
	return "`" + strings.ReplaceAll(cell(s), "`", "'") + "`"
}

func short(sha string) string {
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
