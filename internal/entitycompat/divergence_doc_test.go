package entitycompat

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// docs/divergence.md は upstream との差分の一次資料で、diff-e2e の ignore-list を
// 足すときに「対応する記述があるか」を確認する運用になっている (docs/ci.md の `diff-e2e` の節)。
// 実態より少ない表を信じると、そこに載っていない差分を差分として認識しないまま
// 調査が進む。
//
// 実際に 3 箇所が静かにずれていた (#2634)。#2313 で分割アップロードの endpoint 4 件を
// 足したときにサマリだけ更新して内訳表を更新せず、#2332 / #2340 / instance_secret でも
// 同じことが起きた。**どれも人が数え直さない限り気付けない形**だったので、機械で
// 検証できるものは以下で固定する。
//
// 検証は 3 方向ある。どれか 1 つでも欠けると #2634 の再発を許す。
//
//	1. 見出しの件数 <-> 表の行 (§1-1 は件数列の合計、§2 は実 schema)
//	2. 見出しの件数 <-> その内訳、および直後の散文 (#2634 では 14 と 15 が隣接 2 行で矛盾していた)
//	3. 見出しの件数 <-> 冒頭サマリ (**#2634 の起点はサマリだけ更新して表を更新しなかったこと**。
//	   鏡像の「表だけ更新してサマリを更新しない」も同じだけ起きうる)
//
// §1-1 は実 schema にあたるものが無いので、当初は内部整合しか見ていなかった。
// **3 つが揃って同じだけ間違っていることがある**ので、それだけでは足りない —
// develop では §1-1 が 53、生成物の docs/api-compat.md が 49、真値が 58 だった (#2640)。
//
// upstream の endpoint 一覧を tools/apicompat から直接引くことはできない
// (**どの CI job も本家のソースを checkout しない**。本家は `make upstream-fetch` で
// 取る `.cache/misskey` にしか無い、#3378 / #3379)。ただし
// **`make apicompat` の生成物は commit されている**ので、それを経由すれば
// submodule 無しでも突き合わせられる (TestDivergenceDoc_EndpointCountMatchesAPICompat)。

var (
	divergenceHeadingRe = regexp.MustCompile(`^### (\d+-\d+)\. .*?\((\d+)`)
	// §2-2 の見出しは `(23 = 実使用 20 + 未使用の残存 3)`。
	divergenceBreakdownRe = regexp.MustCompile(`\((\d+) = 実使用 (\d+) \+ 未使用の残存 (\d+)\)`)
	// §2-2 の直後の散文は `実際に読み書きするのは 20 件** (cherrypick 由来 3 + mk-go 独自 17)`。
	divergenceProseRe = regexp.MustCompile(`読み書きするのは (\d+) 件\*\* \(cherrypick 由来 (\d+) \+ mk-go 独自 (\d+)\)`)
	// サマリ表の `| DB テーブル | 10 (+ bookkeeping 2) | ...`。
	summaryTableRe = regexp.MustCompile(`^\| DB テーブル \| (\d+) \(\+ bookkeeping (\d+)\)`)
	// サマリ表の `| DB カラム | 17 (+ 未使用の残存列 3) | 3 |`。
	summaryColumnRe = regexp.MustCompile(`^\| DB カラム \| (\d+) \(\+ 未使用の残存列 (\d+)\) \| (\d+) \|`)
	// サマリ表の endpoint 行は `GET variant 23 + alias 3 + ...` という和の式。
	summaryEndpointRe = regexp.MustCompile(`^\| API endpoint \| (.*?) \| chat (\d+) \|`)
	// 和の各項は `名前 <数字>`。**項の末尾の数字だけ**を取る (行全体から数字を
	// 拾うと `(#2313)` のような注記が項として混ざる)。末尾が数字でない項があれば
	// 黙って無視せず false を返し、書式が変わったことを呼び出し側に知らせる。
	summaryTermRe = regexp.MustCompile(`(\d+)\s*$`)
)

// bookkeepingTables are the two drop-in / tooling tables counted separately from
// mk-go's own tables in the summary.
var bookkeepingTables = map[string]bool{"migrations": true, "schema_migrations": true}

// TestDivergenceDoc_EndpointCountMatchesTable asserts that §1-1's declared count
// matches both the sum of its table's count column and the summary's expression.
func TestDivergenceDoc_EndpointCountMatchesTable(t *testing.T) {
	lines := readDivergenceDoc(t)
	start, declared := findDivergenceSection(t, lines, "1-1")

	sum := 0
	var rows []string
	for _, line := range sectionLines(lines, start)[1:] {
		cells := tableRowCells(line)
		if len(cells) < 2 {
			continue
		}
		n, err := strconv.Atoi(cells[1])
		if err != nil {
			continue // ヘッダ行 / 区切り行
		}
		sum += n
		rows = append(rows, fmt.Sprintf("%s = %d", cells[0], n))
	}

	if sum != declared {
		t.Errorf(`docs/divergence.md §1-1 の見出しは %d だが、表の件数列の合計は %d:
  %s

見出しか表のどちらかが古い。endpoint を足したら**サマリだけでなく §1-1 の表にも
行を足す**こと (#2634 はこれを怠って 4 件ずれていた)。`,
			declared, sum, strings.Join(rows, "\n  "))
	}

	// 冒頭サマリの `GET variant 23 + alias 3 + ... | chat 15` も同じ総数を表す。
	mkTerms, chat, ok := summaryEndpointCounts(lines)
	if !ok {
		t.Fatal("docs/divergence.md のサマリ表の `| API endpoint |` 行を読めない " +
			"(行が無いか、和の各項が `名前 <数字>` の形になっていない)")
	}
	if got := mkTerms + chat; got != declared {
		t.Errorf(`docs/divergence.md サマリの endpoint 合計は %d (mk-go 独自 %d + chat %d) だが、§1-1 の見出しは %d。

**#2634 の起点はサマリだけを更新して §1-1 の表を更新しなかったこと。** 逆向き
(表だけ更新してサマリを据え置く) も同じだけ起きるので、両方を直すこと。`,
			got, mkTerms, chat, declared)
	}
}

// TestDivergenceDoc_TableCountMatchesSchema asserts that §2-1's declared count
// matches the number of tables the migrations create that upstream lacks.
func TestDivergenceDoc_TableCountMatchesSchema(t *testing.T) {
	own := ownTables(t)

	lines := readDivergenceDoc(t)
	start, declared := findDivergenceSection(t, lines, "2-1")
	if len(own) != declared {
		t.Errorf(`docs/divergence.md §2-1 の見出しは %d だが、migration が作る upstream 非存在テーブルは %d 件:
  %s

表に無いテーブルがあるか、逆に消えたテーブルが残っている。`,
			declared, len(own), strings.Join(own, "\n  "))
	}

	// 見出しが合っていても表に行が無ければ意味が無いので、各テーブルの行が
	// あることも見る。**節全体の部分文字列で探さないこと** — §2-1 の行は他の
	// テーブルを backtick で相互参照している (user_keypair_extra の行が
	// `user_keypair` を挙げる等) ので、行を消して散文で触れるだけで素通りする。
	for _, table := range own {
		if !divergenceRowNames(sectionLines(lines, start), table) {
			t.Errorf("docs/divergence.md §2-1 に %q の行が無い", table)
		}
	}

	// サマリの `DB テーブル N (+ bookkeeping M)`。
	mk, bookkeeping, ok := summaryTableCounts(lines)
	if !ok {
		t.Fatal("docs/divergence.md のサマリ表に `| DB テーブル |` の行が無い")
	}
	var wantBookkeeping int
	for _, table := range own {
		if bookkeepingTables[table] {
			wantBookkeeping++
		}
	}
	if mk+bookkeeping != declared || bookkeeping != wantBookkeeping {
		t.Errorf(`docs/divergence.md サマリの「DB テーブル %d (+ bookkeeping %d)」が §2-1 の見出し %d と合わない
(bookkeeping は %v の %d 件)。`,
			mk, bookkeeping, declared, sortedKeys(bookkeepingTables), wantBookkeeping)
	}
}

// TestDivergenceDoc_ColumnCountMatchesSchema asserts that §2-2's declared count
// matches the number of mk-go-only columns on tables upstream also has.
func TestDivergenceDoc_ColumnCountMatchesSchema(t *testing.T) {
	own := ownColumnsOnSharedTables(t)

	lines := readDivergenceDoc(t)
	start, declared := findDivergenceSection(t, lines, "2-2")
	if len(own) != declared {
		t.Errorf(`docs/divergence.md §2-2 の見出しは %d だが、upstream 共有テーブルの mk-go 独自カラムは %d 件:
  %s

**見出しの内訳 (実使用 N + 未使用の残存 M) も直すこと。** #2634 では見出しと
直後の散文で 14 と 15 が食い違っていた。`,
			declared, len(own), strings.Join(own, "\n  "))
	}

	// 行の存在は table と column の両方で見る。**列名だけで探すと、同名の列
	// (createdAt が app / auth_session / note_favorite の 3 箇所にある) が他の
	// 行に残っているせいで、行が丸ごと消えても素通りする。**
	for _, qualified := range own {
		dot := strings.IndexByte(qualified, '.')
		table, col := qualified[:dot], qualified[dot+1:]
		if !divergenceRowMentions(sectionLines(lines, start), table, col) {
			t.Errorf("docs/divergence.md §2-2 に %q の行が無い", qualified)
		}
	}

	// 未使用の残存列は schema_drift_test.go の allowlist がそのまま実数になる。
	unused := 0
	for qualified := range createOnlyAllowlist {
		if !contains(own, qualified) {
			t.Errorf("createOnlyAllowlist の %q が mk-go 独自カラムに含まれない (allowlist が古い)", qualified)
			continue
		}
		unused++
	}

	// 見出しの内訳と直後の散文。#2634 で矛盾していたのはまさにここ。
	section := strings.Join(sectionLines(lines, start), "\n")
	bm := divergenceBreakdownRe.FindStringSubmatch(lines[start])
	if bm == nil {
		t.Fatalf("§2-2 の見出しが `(N = 実使用 M + 未使用の残存 K)` の形でない: %q", lines[start])
	}
	total, inUse, residual := atoi(t, bm[1]), atoi(t, bm[2]), atoi(t, bm[3])
	if total != declared || inUse+residual != total {
		t.Errorf("§2-2 の見出しの内訳が破綻している: %d = %d + %d", total, inUse, residual)
	}
	if residual != unused {
		t.Errorf("§2-2 の見出しの「未使用の残存 %d」が createOnlyAllowlist の %d 件と合わない", residual, unused)
	}

	pm := divergenceProseRe.FindStringSubmatch(section)
	if pm == nil {
		t.Fatal("§2-2 の散文が `読み書きするのは N 件** (cherrypick 由来 A + mk-go 独自 B)` の形でない")
	}
	proseInUse, cherrypick, mkOwn := atoi(t, pm[1]), atoi(t, pm[2]), atoi(t, pm[3])
	if proseInUse != inUse {
		t.Errorf(`§2-2 の見出しの「実使用 %d」と散文の「読み書きするのは %d 件」が食い違う。

#2634 で見つかったのがこの形 (14 と 15 が隣接 2 行で矛盾)。`, inUse, proseInUse)
	}
	if cherrypick+mkOwn != proseInUse {
		t.Errorf("§2-2 の散文の内訳が破綻している: %d != %d + %d", proseInUse, cherrypick, mkOwn)
	}

	// サマリの `DB カラム N (+ 未使用の残存列 M) | C`。
	sMk, sResidual, sCherrypick, ok := summaryColumnCounts(lines)
	if !ok {
		t.Fatal("docs/divergence.md のサマリ表に `| DB カラム |` の行が無い")
	}
	if sMk != mkOwn || sResidual != residual || sCherrypick != cherrypick {
		t.Errorf(`docs/divergence.md サマリの「DB カラム %d (+ 未使用の残存列 %d) | %d」が
§2-2 の内訳 (mk-go 独自 %d / 残存 %d / cherrypick %d) と合わない。`,
			sMk, sResidual, sCherrypick, mkOwn, residual, cherrypick)
	}
}

// ownTables returns the tables mk-go's migrations create that upstream lacks.
func ownTables(t *testing.T) []string {
	t.Helper()
	upstream := loadUpstreamColumns(t)
	createOnly, _, _ := parseMigrations(t)

	var own []string
	for table := range createOnly {
		if _, shared := upstream[table]; shared {
			continue
		}
		// chart 系は upstream でも定義位置が違うだけ (models/ ではなく
		// core/chart/charts/entities/) なので独自ではない。doc 側にもその旨の
		// 注記がある。
		if strings.HasPrefix(table, "__chart__") || strings.HasPrefix(table, "__chart_day__") {
			continue
		}
		own = append(own, table)
	}
	sort.Strings(own)
	return own
}

// ownColumnsOnSharedTables returns `table.column` for every column mk-go's
// migrations leave on a table that upstream also has.
func ownColumnsOnSharedTables(t *testing.T) []string {
	t.Helper()
	upstream := loadUpstreamColumns(t)
	_, _, current := parseMigrations(t)

	var own []string
	for table, set := range current {
		up, shared := upstream[table]
		if !shared {
			continue
		}
		for col := range set {
			if !up[col] {
				own = append(own, table+"."+col)
			}
		}
	}
	sort.Strings(own)
	return own
}

// divergenceRowMentions reports whether some table row names both the table and
// the column, allowing either to be a `a` / `b` group.
func divergenceRowMentions(section []string, table, column string) bool {
	for _, line := range section {
		cells := tableRowCells(line)
		if len(cells) < 2 {
			continue
		}
		if cellNames(cells[0])[table] && cellNames(cells[1])[column] {
			return true
		}
	}
	return false
}

// divergenceRowNames reports whether some table row's first cell names the table.
func divergenceRowNames(section []string, table string) bool {
	for _, line := range section {
		cells := tableRowCells(line)
		if len(cells) > 0 && cellNames(cells[0])[table] {
			return true
		}
	}
	return false
}

// cellNames splits an "`a` / `b`" cell into the set {a, b}.
func cellNames(cell string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(cell, "/") {
		out[strings.Trim(strings.TrimSpace(part), "`")] = true
	}
	return out
}

func tableRowCells(line string) []string {
	if !strings.HasPrefix(line, "|") {
		return nil
	}
	if trimmed := strings.NewReplacer("|", "", " ", "", "-", "", ":", "").Replace(line); trimmed == "" {
		return nil // 区切り行
	}
	cells := strings.Split(strings.Trim(line, "|"), "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

func summaryEndpointCounts(lines []string) (int, int, bool) {
	for _, line := range lines {
		m := summaryEndpointRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		mk := 0
		for _, term := range strings.Split(m[1], "+") {
			tm := summaryTermRe.FindStringSubmatch(term)
			if tm == nil {
				return 0, 0, false
			}
			v, err := strconv.Atoi(tm[1])
			if err != nil {
				return 0, 0, false
			}
			mk += v
		}
		chat, err := strconv.Atoi(m[2])
		return mk, chat, err == nil
	}
	return 0, 0, false
}

func summaryTableCounts(lines []string) (mk, bookkeeping int, ok bool) {
	for _, line := range lines {
		if m := summaryTableRe.FindStringSubmatch(line); m != nil {
			mk, err1 := strconv.Atoi(m[1])
			bk, err2 := strconv.Atoi(m[2])
			return mk, bk, err1 == nil && err2 == nil
		}
	}
	return 0, 0, false
}

func summaryColumnCounts(lines []string) (mk, residual, cherrypick int, ok bool) {
	for _, line := range lines {
		if m := summaryColumnRe.FindStringSubmatch(line); m != nil {
			a, err1 := strconv.Atoi(m[1])
			b, err2 := strconv.Atoi(m[2])
			c, err3 := strconv.Atoi(m[3])
			return a, b, c, err1 == nil && err2 == nil && err3 == nil
		}
	}
	return 0, 0, 0, false
}

func readDivergenceDoc(t *testing.T) []string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("..", "..", "docs", "divergence.md"))
	if err != nil {
		t.Fatalf("read docs/divergence.md: %v", err)
	}
	return strings.Split(string(blob), "\n")
}

// findDivergenceSection returns the heading's line index and the count declared
// in its parentheses.
func findDivergenceSection(t *testing.T, lines []string, section string) (int, int) {
	t.Helper()
	for i, line := range lines {
		m := divergenceHeadingRe.FindStringSubmatch(line)
		if m == nil || m[1] != section {
			continue
		}
		return i, atoi(t, m[2])
	}
	t.Fatalf("docs/divergence.md に §%s の見出し (件数付き) が無い", section)
	return 0, 0
}

// sectionLines returns the lines from the heading up to the next heading of any
// level.
func sectionLines(lines []string, start int) []string {
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "#") {
			return lines[start:i]
		}
	}
	return lines[start:]
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("数値として読めない: %q", s)
	}
	return n
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// apiCompatOnlyRe matches the generated matrix's summary line
// `- mk-go only (TS spec 外): **58**`.
var apiCompatOnlyRe = regexp.MustCompile(`^- mk-go only \(TS spec 外\): \*\*(\d+)\*\*`)

// forkTagSummaryRe matches the summary row
// `| fork frontend の独自変更 | 23 tag (`-mk.0` ～ `-mk.22`) | — | — |`.
// **範囲は base 込みで書く。** submodule を bump すると `-mk.N` は 0 に戻るので、
// `-mk.0 ～ -mk.1` のような表記だと base をまたいだ範囲が読めない (#2879)。
var forkTagSummaryRe = regexp.MustCompile("^\\| fork frontend の独自変更 \\| (\\d+) tag \\(`([0-9.]+-mk\\.\\d+[a-z]*)` ～ `([0-9.]+-mk\\.\\d+[a-z]*)`\\)")

// forkTagRowRe matches a §4-2 table row `| `2026.7.0-mk.12` | ... |`.
var forkTagRowRe = regexp.MustCompile("^\\| `([0-9.]+)-mk\\.(\\d+[a-z]*)` \\|")

// TestDivergenceDoc_EndpointCountMatchesAPICompat ties §1-1 to the generated
// matrix. TestDivergenceDoc_EndpointCountMatchesTable only checks that the
// heading, the table and the summary agree **with each other**, so all three
// can be wrong together — and they were: on develop §1-1 said 53 while the
// (stale) matrix said 49 and the true count was 58. §1-1 の内訳表に数えられて
// いなかったのは `admin/server-plugins` / `admin/server-metrics` /
// `admin/self-check` / `admin/federation/{delivery,inbox}-health` の 5 件で、
// **うち 4 件は生成物の側には載っていた** (= 突き合わせていれば気付けた、#2640)。
//
// docs/api-compat.md は `make apicompat` が生成して commit されているので、
// **submodule を checkout しない test-shards job からでも読める**。upstream の
// endpoint 一覧を直接引けないという制約は、この生成物を経由すれば回避できる。
func TestDivergenceDoc_EndpointCountMatchesAPICompat(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join("..", "..", "docs", "api-compat.md"))
	if err != nil {
		t.Fatalf("read docs/api-compat.md: %v", err)
	}
	generated := -1
	for _, line := range strings.Split(string(blob), "\n") {
		if m := apiCompatOnlyRe.FindStringSubmatch(line); m != nil {
			generated = atoi(t, m[1])
			break
		}
	}
	if generated < 0 {
		t.Fatal("docs/api-compat.md に `- mk-go only (TS spec 外): **N**` の行が無い " +
			"(tools/apicompat の出力書式が変わった?)")
	}

	_, declared := findDivergenceSection(t, readDivergenceDoc(t), "1-1")
	if declared != generated {
		t.Errorf(`docs/divergence.md §1-1 の見出しは %d だが、docs/api-compat.md は %d と言っている。

**どちらが古いかは中身を見ないと決まらない。** api-compat.md 側が古いなら
`+"`make apicompat`"+` で再生成する (route dump に stack が要る)。divergence.md 側が
古いなら §1-1 の表・見出し・冒頭サマリの 3 箇所すべてを直す。`, declared, generated)
	}
}

// TestDivergenceDoc_ForkFrontendTagsMatchTable asserts that the summary's tag
// count and range agree with §4-2's rows. サマリは 10 tag と言い、表には 11 行
// あり、実際の submodule には 23 個の tag があった (#2640)。
//
// **捕まえるのは前 2 つの食い違いだけ。** submodule 側が進んだことは検出できない
// (test-shards job は submodule を checkout しない) ので、サマリと表を両方据え置けば
// すり抜ける。submodule bump の PR で表を足すのは人の仕事。
func TestDivergenceDoc_ForkFrontendTagsMatchTable(t *testing.T) {
	lines := readDivergenceDoc(t)

	var declared int
	var lo, hi string
	found := false
	for _, line := range lines {
		if m := forkTagSummaryRe.FindStringSubmatch(line); m != nil {
			declared, lo, hi = atoi(t, m[1]), m[2], m[3]
			found = true
			break
		}
	}
	if !found {
		t.Fatal("docs/divergence.md のサマリ表に " +
			"`| fork frontend の独自変更 | N tag (`X.Y.Z-mk.A` ～ `X.Y.Z-mk.B`) |` の行が無い " +
			"(**範囲は base 込みで書く** — bump で `-mk.N` は 0 に戻るため)")
	}

	start, _ := findDivergenceHeading(t, lines, "4-2")
	type forkTag struct{ base, n string }
	var rows []forkTag
	var tags []string
	for _, line := range sectionLines(lines, start) {
		if m := forkTagRowRe.FindStringSubmatch(line); m != nil {
			rows = append(rows, forkTag{base: m[1], n: m[2]})
			tags = append(tags, m[1]+"-mk."+m[2])
		}
	}
	if len(tags) == 0 {
		t.Fatal("docs/divergence.md §4-2 に `| `X.Y.Z-mk.N` |` 形式の行が無い")
	}

	if len(tags) != declared {
		t.Errorf(`docs/divergence.md のサマリは fork frontend の変更を %d tag と言っているが、§4-2 の表は %d 行。

tag を足したら**サマリの件数と範囲、§4-2 の表の両方**を直すこと。`, declared, len(tags))
	}
	if tags[0] != lo || tags[len(tags)-1] != hi {
		t.Errorf(`docs/divergence.md のサマリの範囲は %s ～ %s だが、§4-2 の表は %s ～ %s。`,
			lo, hi, tags[0], tags[len(tags)-1])
	}
	// **連番は base ごとに見る。** submodule を bump すると `-mk.N` は 0 に戻るので
	// (`2026.7.0-mk.22j` の次が `2026.9.0-mk.0`)、base をまたいで通し番号を期待すると
	// 必ず落ちる。#2879 の取り込みで実際に踏んだ。
	//
	// **ただし base 同士の関係も見ること。** グループ化だけにすると、base の順序が
	// 逆でも / 古い base を末尾に足しても / 同じ base が飛び飛びに現れても通ってしまう
	// (旧実装の flat な連番検査はこれらを捕まえていた)。base をまたぐ検査を丸ごと
	// 落とすと**検出力が下がる**ので、昇順と一意性をここで見る。
	seen := make(map[string]bool, len(rows))
	prevBase := ""
	for i := 0; i < len(rows); {
		j := i
		for j < len(rows) && rows[j].base == rows[i].base {
			j++
		}
		base := rows[i].base
		if seen[base] {
			t.Errorf(`docs/divergence.md §4-2 の base %s の行が飛び飛びに現れている。
同じ base の行はまとめて並べること。`, base)
		}
		seen[base] = true
		if prevBase != "" && compareForkBase(prevBase, base) >= 0 {
			t.Errorf(`docs/divergence.md §4-2 の base が昇順でない (%s の次が %s)。
古い順に並べること。`, prevBase, base)
		}
		prevBase = base

		group := make([]string, 0, j-i)
		for _, r := range rows[i:j] {
			group = append(group, r.n)
		}
		assertForkTagSequence(t, group)
		i = j
	}
}

// compareForkBase compares two `X.Y.Z` version strings numerically.
// 文字列比較だと `2026.10.0` < `2026.9.0` になるので使えない。
func compareForkBase(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// assertForkTagSequence checks that §4-2's rows step through the tag scheme
// without gaps.
//
// **機能追加は N を進め、直前の数字タグの後追い修正はその N に英字を足す**
// (`-mk.22` の修正が `-mk.22a`、次が `-mk.22b`)。**世代をまたぐ修正は新しい数字を
// 取る** — 英字は列の順序を保つためのものなので、`-mk.24` の後に `-mk.12a` を打つと
// `git describe --tags` が後戻りして見える (規則の全文は docs/divergence.md の
// 「fork frontend の変更」。先例は `-mk.23` / `-mk.25` / `-mk.28`)。
// **英字を足す形があるので数字は重複しうる。** 単純な連番検査は使えないので、
// 数字は 1 ずつ、同じ数字の中の英字は a から 1 文字ずつ進むこと
// (#2689)。
func assertForkTagSequence(t *testing.T, tags []string) {
	t.Helper()
	prevNum, prevLetter := -1, ""
	for _, tag := range tags {
		num, letter := splitForkTag(t, tag)
		switch {
		case num == prevNum+1 && letter == "":
			// 次の機能追加。
		case num == prevNum && letter == nextForkLetter(prevLetter):
			// 同じ数字へのバグ修正。
		default:
			want := fmt.Sprintf("-mk.%d", prevNum+1)
			if letter != "" || prevLetter != "" {
				want += fmt.Sprintf(" か -mk.%d%s", prevNum, nextForkLetter(prevLetter))
			}
			t.Errorf(`docs/divergence.md §4-2 の tag が順に並んでいない (-mk.%d%s の次が -mk.%s、期待は %s)。
抜けているなら「載せない」判断の根拠を書くか、行を足すこと。`, prevNum, prevLetter, tag, want)
			return
		}
		prevNum, prevLetter = num, letter
	}
}

// splitForkTag splits "22a" into (22, "a").
func splitForkTag(t *testing.T, tag string) (int, string) {
	t.Helper()
	i := strings.IndexFunc(tag, func(r rune) bool { return r < '0' || r > '9' })
	if i < 0 {
		return atoi(t, tag), ""
	}
	return atoi(t, tag[:i]), tag[i:]
}

// nextForkLetter returns the letter that follows prev ("" → "a", "a" → "b").
// 2 文字以上は想定しない (1 つの機能 tag に 26 回もバグ修正を積む前に、
// 機能側を進めるべき)。
func nextForkLetter(prev string) string {
	if prev == "" {
		return "a"
	}
	if len(prev) != 1 || prev[0] >= 'z' {
		return "?"
	}
	return string(prev[0] + 1)
}

// findDivergenceHeading is findDivergenceSection without the count requirement:
// §4-2 の見出しには件数が付かない。
func findDivergenceHeading(t *testing.T, lines []string, section string) (int, bool) {
	t.Helper()
	prefix := "## " + section + "."
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return i, true
		}
	}
	t.Fatalf("docs/divergence.md に §%s の見出しが無い", section)
	return 0, false
}

// apiCompatRowRe matches a route row `| POST | `/api/notes/create` |` in the
// generated matrix.
var apiCompatRowRe = regexp.MustCompile("^\\| (GET|POST) \\| `(/api/[^`]*)` \\|")

// routerPostRe / routerMatchRe extract the statically registered `/api/*`
// routes from router.go. `api` is the echo group mounted at `/api`.
var (
	routerPostRe  = regexp.MustCompile(`\bapi\.POST\("([^"]+)"`)
	routerGetRe   = regexp.MustCompile(`\bapi\.GET\("([^"]+)"`)
	routerMatchRe = regexp.MustCompile(`\bapi\.Match\(chartMethods, "([^"]+)"`)

	// 総数を数える側。抽出できた数と突き合わせて取りこぼしを検出する。
	routerPostCallRe  = regexp.MustCompile(`\bapi\.POST\(`)
	routerGetCallRe   = regexp.MustCompile(`\bapi\.GET\(`)
	routerMatchCallRe = regexp.MustCompile(`\bapi\.Match\(`)
)

// TestAPICompatDoc_MatchesRouter asserts that the committed
// docs/api-compat.md still describes the routes router.go registers.
//
// **生成物そのものが腐ると、それを錨にしている gate も一緒に無力化する。**
// TestDivergenceDoc_EndpointCountMatchesAPICompat は divergence.md と
// api-compat.md の一致しか見ないので、endpoint を足して**どちらも更新しない**と
// 両方が古いまま緑になる。実際 develop では `mk-go version: 1.1.2` /
// `mk-go only: 49` のまま腐っていた (#2640)。
//
// `make apicompat` の route dump には stack (DB / Redis) が要るのでテストからは
// 呼べない。代わりに router.go の**静的な登録**と突き合わせる。
//
// **POST と GET の両方を見る。** 「endpoint を足す操作は必ず POST を伴う」は
// 成り立たない — `/api/v1/instance/peers` は upstream 側も fastify の
// `get()` 直登録で、mk-go も `api.GET` 一本しか持たない。POST だけを母集団に
// すると同種の GET-only endpoint がこの gate から見えなくなる。
//
// 同梱プラグインのルート (`/api/plugin/*`) は router.go に literal で現れず、
// 生成物にも (bundled のうち enabled なものだけが) 載る。母集団から外す。
func TestAPICompatDoc_MatchesRouter(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join("..", "..", "docs", "api-compat.md"))
	if err != nil {
		t.Fatalf("read docs/api-compat.md: %v", err)
	}
	// **セクションを見る。** 生成物は「TS 側に存在するが mk-go で未実装」も同じ
	// 行書式で出す。全行を拾うと、upstream が endpoint を足した直後 (= 生成物が
	// 正しい状態) に「api-compat.md にあって router.go に無い」で落ち、しかも
	// 診断が逆を指す。submodule bump のたびに現実的に起きる。
	doc := map[string]bool{}
	inScope := false
	for _, line := range strings.Split(string(blob), "\n") {
		if strings.HasPrefix(line, "## ") {
			inScope = strings.Contains(line, "mk-go 側にしかない") ||
				strings.Contains(line, "両方に存在する")
			continue
		}
		if !inScope {
			continue
		}
		m := apiCompatRowRe.FindStringSubmatch(line)
		if m == nil || strings.HasPrefix(m[2], "/api/plugin/") {
			continue
		}
		doc[m[1]+" "+strings.TrimPrefix(m[2], "/api")] = true
	}

	joined, err := serverPackageSource()
	if err != nil {
		t.Fatalf("read internal/server/*.go: %v", err)
	}
	router := map[string]bool{}
	for _, m := range routerPostRe.FindAllStringSubmatch(joined, -1) {
		router["POST "+m[1]] = true
	}
	for _, m := range routerGetRe.FindAllStringSubmatch(joined, -1) {
		router["GET "+m[1]] = true
	}
	// charts は GET / POST の両方を 1 回の Match で登録する。
	for _, m := range routerMatchRe.FindAllStringSubmatch(joined, -1) {
		router["POST "+m[1]] = true
		router["GET "+m[1]] = true
	}

	// **0 件で通さない。** どちらかの書式が変わったら黙って空集合同士が一致する。
	if len(doc) == 0 {
		t.Fatal("docs/api-compat.md の endpoint セクションから行を 1 件も読めない (tools/apicompat の出力書式か見出しが変わった?)")
	}
	if len(router) == 0 {
		t.Fatal("internal/server/router.go から api.POST/GET(...) を 1 件も読めない (登録の書き方が変わった?)")
	}

	// **0 件チェックだけでは足りない。** 部分的な取りこぼしは router 側の集合が
	// 小さくなるだけなので、生成物との差が出ずに黙って一致する。呼び出しの総数と
	// 抽出できた数が合うことまで見る (path が literal でない / 複数行に分かれている
	// 形を塞ぐ)。
	//
	// 総数も**正規表現で数える**。`strings.Count` だと `metaapi.GET(` のような
	// 無関係な識別子まで数えて偽陽性になり、しかも診断が「正規表現を直せ」と
	// 誤誘導する。
	for _, c := range []struct {
		call   string
		loose  *regexp.Regexp
		strict *regexp.Regexp
	}{
		{"api.POST(", routerPostCallRe, routerPostRe},
		{"api.GET(", routerGetCallRe, routerGetRe},
		{"api.Match(", routerMatchCallRe, routerMatchRe},
	} {
		got, matched := len(c.loose.FindAllString(joined, -1)), len(c.strict.FindAllStringSubmatch(joined, -1))
		if got != matched {
			t.Fatalf(`router.go の %s は %d 件あるが、path を抽出できたのは %d 件。

**取りこぼした分だけ gate が緩くなる。** path が literal でない (定数 / 変数経由) か、
呼び出しが複数行に分かれている可能性がある。このテストの正規表現を実態に
合わせるか、登録を 1 行の literal に戻すこと。`, c.call, got, matched)
		}
	}

	// **`/api` 配下を生やす経路は `api.<METHOD>(` だけではない。** 抜け道を
	// 個別に固定する。fail-closed 側に倒すので、正当な理由で下記の形を使う
	// ことになったら、このテストの抽出をそちらへ拡張してから解除すること。
	//
	// **塞ぎきれてはいない。** `*echo.Group` を helper に渡してその中で登録する
	// 形 (`func(g *echo.Group){ g.POST(…) }(api)`) は静的には追えない。
	for _, g := range []struct {
		name string
		re   *regexp.Regexp
		want int
		why  string
	}{
		{"api.Group(", regexp.MustCompile(`\bapi\.Group\(`), 1,
			"サブグループ配下の登録は api.<METHOD>( として現れず、このテストから見えない (1 件は plugin_wiring.go のプラグイン用)"},
		{`s.echo.Group("/api"`, regexp.MustCompile(`\bs\.echo\.Group\("/api`), 1,
			"別名の group 変数を経由すると api.<METHOD>( に一致しない"},
		{"api.PUT/DELETE/PATCH/HEAD/CONNECT/TRACE(", regexp.MustCompile(`\bapi\.(PUT|DELETE|PATCH|HEAD|CONNECT|TRACE)\(`), 0,
			"POST / GET 以外のメソッドは母集団に入れていない"},
		{"api.Add(", regexp.MustCompile(`\bapi\.Add\(`), 0,
			"メソッドを文字列で渡す登録は api.<METHOD>( に一致しない"},
		{"api.Any(", regexp.MustCompile(`\bapi\.Any\(`), 1,
			"catchall (\"/*\") 1 件だけを想定している。増えたら実 endpoint かを確認する"},
		{`s.echo.<METHOD>("/api`, regexp.MustCompile(`\bs\.echo\.(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS|CONNECT|TRACE|Any|Match|Add|Static|File|RouteNotFound)\("/api(/|")`), 0,
			"group を通さず /api 配下へ直接登録すると、このテストから見えない"},
	} {
		if n := len(g.re.FindAllString(joined, -1)); n != g.want {
			t.Fatalf(`router.go の %s が %d 件 (想定 %d)。

理由: %s。**そこに足した endpoint は生成物と照合されない**ので、抽出をその形に
対応させてからこの固定を解除すること。`, g.name, n, g.want, g.why)
		}
	}

	missing := diffKeys(router, doc) // router にあって doc に無い
	stale := diffKeys(doc, router)   // doc にあって router に無い
	if len(missing) == 0 && len(stale) == 0 {
		return
	}
	t.Errorf(`docs/api-compat.md が internal/server/router.go と食い違っている。

**再生成が要る**: `+"`make apicompat`"+` (route dump に stack 起動が必要)。生成物が腐ると
TestDivergenceDoc_EndpointCountMatchesAPICompat の錨も一緒に無力化する。

router.go にあって api-compat.md に無い (%d 件、= endpoint を足したが再生成していない):
  %s

api-compat.md にあって router.go に無い (%d 件、= endpoint を消したか、登録の書き方が
regexp から外れた。後者ならこのテストの routerPostRe を直す):
  %s`,
		len(missing), strings.Join(missing, "\n  "),
		len(stale), strings.Join(stale, "\n  "))
}

// diffKeys returns the keys of a that b lacks, sorted.
func diffKeys(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// streamRegistryRe matches `streamRegistry.Register("name", …)` and its
// Credentialed variant. streamRegistryCallRe counts the calls so a name the
// strict form cannot read is detected instead of silently dropped.
var (
	streamRegistryRe     = regexp.MustCompile(`\bstreamRegistry\.Register(?:Credentialed)?\("([a-zA-Z]+)"`)
	streamRegistryCallRe = regexp.MustCompile(`\bstreamRegistry\.Register(?:Credentialed)?\(`)
)

// serverPackageSource concatenates internal/server's non-test sources with
// comments removed. **package 全体を読む** — router.go だけを見ると別
// ファイルからの登録を取りこぼす。実際 plugin_wiring.go は `api.Group(...)` で
// `/api` 配下を生やしている。
//
// **コメントが落ちるのは `parser.ParseFile` の mode が 0 だから。**
// `parser.ParseComments` に変えると `printer.Fprint` が `*ast.File` の
// `Comments` を印字するので、`/* */` で囲んだ登録が黙って復活する。
// `TestServerPackageSourceDropsBlockComments` がそれを固定している。
func serverPackageSource() (string, error) {
	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "server", "*.go"))
	if err != nil {
		return "", err
	}
	var body []string
	read := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		read++
		stripped, perr := goSourceWithoutComments(f, src)
		if perr != nil {
			return "", perr
		}
		body = append(body, stripped)
	}
	if read == 0 {
		return "", fmt.Errorf("internal/server/*.go を 1 ファイルも読めない")
	}
	return strings.Join(body, "\n"), nil
}

// goSourceWithoutComments re-prints Go source with every comment dropped.
//
// **コメントは AST で落とす (#2856)。** 行頭 `//` だけを除いても `/* */` で
// 囲んだ登録が生きたものとして通る (実測)。パーサはコメントを構文木に載せない。
//
// **mode は 0 でなければならない。** `parser.ParseComments` にすると
// `printer.Fprint` が `*ast.File` の `Comments` を印字するので、囲んだ登録が
// 黙って復活する。`TestGoSourceWithoutComments` がそれを固定している。
func goSourceWithoutComments(path string, src []byte) (string, error) {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, parsed); err != nil {
		return "", fmt.Errorf("print %s: %w", path, err)
	}
	return buf.String(), nil
}

// docChannelRowRe matches a §4-1 table row `| `notifications` | … |`.
var docChannelRowRe = regexp.MustCompile("^\\| `([a-zA-Z]+)` \\|")

// TestDivergenceDoc_StreamChannelsMatchRegistry asserts that §4-1 lists exactly
// the stream channels internal/server registers.
//
// **チャンネル名はファイル名と違う。** upstream のソースは `chat-room.ts` だが
// wire 上の名前 (`chName`) は `chatRoom`。#2640 の初稿はファイル名をそのまま
// 「チャンネル名も upstream に揃えてある」として並べており、18 件中 11 件が
// 実在しない名前だった。人が目で照合すると通る類の誤り。
//
// **固定できるのは「doc の一覧 == mk-go の登録」だけ。** §4-1 のもう半分の主張
// 「upstream は 18」「名前も upstream に揃えてある」は検証していない — test-shards は
// submodule を checkout しないため。doc と実装を同時に間違えれば通る。upstream 側の
// 増減も検出できないので、submodule bump の PR で人が見る。
func TestDivergenceDoc_StreamChannelsMatchRegistry(t *testing.T) {
	// 走査は `TestAPICompatDoc_MatchesRouter` と揃えて package 全体。router.go
	// だけを見ると別ファイルからの登録を取りこぼす。
	joined, err := serverPackageSource()
	if err != nil {
		t.Fatalf("read internal/server/*.go: %v", err)
	}
	registry := map[string]bool{}
	for _, m := range streamRegistryRe.FindAllStringSubmatch(joined, -1) {
		registry[m[1]] = true
	}
	if len(registry) == 0 {
		t.Fatal("internal/server から streamRegistry.Register(...) を 1 件も読めない (登録の書き方が変わった?)")
	}
	// **抽出の取りこぼしを検出する。** 名前が定数 / 変数経由だったり、`[a-zA-Z]+`
	// に収まらない文字 (数字・`_`・`-`) を含むと registry から丸ごと消え、doc に
	// 書かなくても通ってしまう。
	if got, matched := len(streamRegistryCallRe.FindAllString(joined, -1)),
		len(streamRegistryRe.FindAllStringSubmatch(joined, -1)); got != matched {
		t.Fatalf(`streamRegistry.Register(...) は %d 件あるが、名前を抽出できたのは %d 件。

**取りこぼした分だけ gate が緩くなる。** 名前が literal でない (定数 / 変数経由) か、
英字以外の文字 (数字・アンダースコア・ハイフン) を含む可能性がある。このテストの
正規表現を実態に合わせること。`, got, matched)
	}

	lines := readDivergenceDoc(t)
	start, _ := findDivergenceHeading(t, lines, "4-1")
	// **収集するのは ```text フェンスだけ。** 無条件にトグルすると、§4-1 に
	// `connect` の JSON 例を足しただけで `{` や `"type":` をチャンネル名として
	// 拾い、診断が意味不明になる。
	doc := map[string]bool{}
	inFence, collect := false, false
	for _, line := range sectionLines(lines, start) {
		if strings.HasPrefix(line, "```") {
			if inFence {
				inFence, collect = false, false
			} else {
				inFence = true
				collect = strings.TrimSpace(line) == "```text"
			}
			continue
		}
		if collect {
			for _, f := range strings.Fields(line) {
				doc[f] = true
			}
			continue
		}
		if m := docChannelRowRe.FindStringSubmatch(line); m != nil {
			doc[m[1]] = true
		}
	}
	if len(doc) == 0 {
		t.Fatal("docs/divergence.md §4-1 からチャンネル名を 1 件も読めない " +
			"(表の書式か ```text フェンスが変わった?)")
	}

	missing := diffKeys(registry, doc)
	stale := diffKeys(doc, registry)
	if len(missing) == 0 && len(stale) == 0 {
		return
	}
	t.Errorf(`docs/divergence.md §4-1 のチャンネル一覧が internal/server の登録と食い違っている。

実装にあって doc に無い (%d 件):
  %s

doc にあって実装に無い (%d 件、= 名前の綴り違いか、消えたチャンネル):
  %s

**ファイル名ではなくチャンネル名 (chName) を書くこと。** upstream のソースは
kebab-case (chat-room.ts) だが wire 上の名前は camelCase (chatRoom)。`,
		len(missing), strings.Join(missing, "\n  "),
		len(stale), strings.Join(stale, "\n  "))
}

// goSourceWithoutComments がブロックコメントを落とすことを固定する (#2856)。
//
// **`serverPackageSource` が使う関数をそのまま通す。** 同じ処理をテスト内に
// 書き写すと、本体の parser mode を変えても落ちない (実測。最初そう書いていた)。
func TestGoSourceWithoutComments(t *testing.T) {
	const src = `package p

func f() {
	/* streamRegistry.Register("ghost", nil) */
	// streamRegistry.Register("ghost2", nil)
	streamRegistry.Register("real", nil)
}
`
	body, err := goSourceWithoutComments("x.go", []byte(src))
	if err != nil {
		t.Fatalf("goSourceWithoutComments: %v", err)
	}
	for _, ghost := range []string{`"ghost"`, `"ghost2"`} {
		if strings.Contains(body, ghost) {
			t.Errorf("コメントアウトされた登録 %s が出力に残っている。"+
				"parser mode が 0 のままか確認すること", ghost)
		}
	}
	if !strings.Contains(body, `"real"`) {
		t.Error("生きた登録が出力から消えている")
	}
}
