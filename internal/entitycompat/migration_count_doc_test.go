package entitycompat

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// migration の本数を述べた記述が実態とずれていないか検査する (#2874)。
//
// **この gate が見るのは 8 ファイル 22 箇所** (数え方: migrationCountClaims の
// 20 + down が no-op の一覧 1 + 破壊的なマイグレーションの表 1)。1 本足したとき
// 実際に動く箇所はその一部で、#2866 (000082 の追加) では 17 箇所だった
// (total 4 + destructive 11 + 一覧 1 + 表 1。テーブルを作らず data loss 宣言も
// 持たない migration なので tables / dataloss は動かない)。**そのうち 5 箇所が
// 漏れて**レビューで見つかった。CLAUDE.md が「最多の型は片側更新」と名指し
// している型で、CI では検出されない。
//
// **truth は「機械的に一意に数えられるもの」に限る。** 対象外にしたのは 3 つ。
//
//   - 「宣言が無いまま DROP する down が 51 本」— `docs/architecture.md` は
//     「`DROP TABLE` / `DROP COLUMN`」、`docs/migration-from-ts.md` は
//     「`DROP TABLE` / `DROP COLUMN` / `DELETE`」と**定義が違うのに同じ 51** を
//     出している (実測ではどちらの定義でも 51。down で `DELETE FROM` を持つ
//     `000077` が `DROP` も持つため)。どちらを truth にするか決められない
//   - 「102」(`docs/api-compatibility.md`) — 112 から「上記 9 件」と
//     `schema_migrations` を引いた数で、9 件の定義に依存する
//   - 「データを不可逆に変えるのはこのうち 8 本」(no-op down の一覧と同じ文) —
//     「不可逆に変えるか」は機械判定できない。**no-op down を 1 本足すと
//     一覧と「7 本」は gate が要求するが、この「6 本」だけ古いまま残る**

// migrationUpFiles returns every tracked up migration.
//
// **`git ls-files` で見る** (#2857)。ディスクを走査すると、`git add` を忘れた
// 新規 migration が手元では数えられて CI で初めてずれる。
func migrationUpFiles(t *testing.T) []string {
	t.Helper()
	return trackedMigrationFiles(t, "migration/*.up.sql")
}

func trackedMigrationFiles(t *testing.T, pattern string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", pattern)
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	require.NoError(t, err, "git ls-files %s が失敗した", pattern)
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	require.NotEmpty(t, files, "%s が 1 つも見つからない (検査していないのに緑になる)", pattern)
	sort.Strings(files)
	return files
}

// noopDownMigrations returns the numeric prefix of every migration whose down
// does nothing.
//
// **「何もしない」を広く取る。** 判定を `SELECT 1;` の完全一致にすると、
// `SELECT 1; -- 意図的に no-op` や `select 1;`、コメントだけ、空ファイルが
// 素通りする (実測)。どれも「巻き戻せない」ことに変わりはないので、
// doc の一覧に載っていなければ落とすべき。
func noopDownMigrations(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var noop []string
	for _, rel := range trackedMigrationFiles(t, "migration/*.down.sql") {
		body, err := os.ReadFile(filepath.Join(root, rel))
		require.NoError(t, err)
		if !downIsNoop(string(body)) {
			continue
		}
		name := filepath.Base(rel)
		num, _, ok := strings.Cut(name, "_")
		require.True(t, ok, "migration の名前に連番の区切りが無い: %s", rel)
		noop = append(noop, num)
	}
	sort.Strings(noop)
	return noop
}

// downIsNoop reports whether the down script performs no work.
func downIsNoop(body string) bool {
	// ブロックコメントを先に落とす (行コメントだけを見る形だと素通りする)。
	body = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(body, " ")
	var stmts []string
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			stmts = append(stmts, trimmed)
		}
	}
	// **空白と連続するセミコロンも畳む。** `SELECT  1;` / `SELECT 1 ;` /
	// `SELECT 1;;` はどれも no-op だが、畳まないと「no-op でない」と判定して
	// 一覧から漏れる (実測)。1 周目で潰した大小文字・行コメントと同じ穴が
	// 別の軸で残っていた。
	joined := strings.Join(strings.Fields(strings.ToLower(strings.Join(stmts, " "))), " ")
	joined = strings.TrimRight(joined, "; ")
	return joined == "" || joined == "select 1"
}

// destructiveMigrationRows counts the rows of the 破壊的なマイグレーション table.
//
// **doc 自身の表を truth にする。** migration の中身から「共有テーブルに触るか」を
// 機械的に判定しようとすると、upstream に無いテーブル (`signup_application`) を
// 触るものまで拾って人手で外すことになる。表は 1 行 1 migration なので、
// そちらを数えれば判断が要らない。
func destructiveMigrationRows(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "docs/migration-from-ts.md"))
	require.NoError(t, err)
	// **見出しが取れなかったら落とす。** 全文へフォールバックすると
	// `#### mk-go 内での切り戻し` 配下の第 2 の表まで拾って 10 → 14 になり、
	// 「14 に直せ」と読めるメッセージが出る (実測)。
	i := strings.Index(string(body), "### 破壊的なマイグレーション")
	require.GreaterOrEqual(t, i, 0,
		"docs/migration-from-ts.md に「### 破壊的なマイグレーション」の見出しが無い。"+
			"**見出しを変えたならこの gate も直すこと**")
	rest := string(body)[i:]
	// **終端も同じく落とす。** 見出しだけ守っても、`#### ` が見つからないときに
	// 全文へ伸びれば同じことが起きる (小見出しを `###` に昇格しただけで
	// 10 → 14 になり、5 ファイル 11 箇所に「14 に直せ」と出る。実測)。
	//
	// 「次の `#` 行まで」に一般化しないこと — この doc は fence の中に
	// `# ローカルビルドの場合` のようなシェルコメントを 8 行持っているので、
	// 行頭 `#` 一般で切ると別の穴が開く。
	j := strings.Index(rest, "\n#### ")
	require.GreaterOrEqual(t, j, 0,
		"docs/migration-from-ts.md の「### 破壊的なマイグレーション」節の終端 (`#### `) が"+
			"見つからない。**節構成を変えたならこの gate も直すこと**")
	section := []byte(rest[:j])
	rows := regexp.MustCompile("(?m)^\\| `([0-9]{6})` \\|").FindAllSubmatch(section, -1)
	var out []string
	for _, r := range rows {
		out = append(out, string(r[1]))
	}
	require.NotEmpty(t, out, "破壊的なマイグレーションの表が拾えない (検査していないのに緑になる)")
	sort.Strings(out)
	return out
}

func dataLossDeclaredDowns(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	for _, rel := range trackedMigrationFiles(t, "migration/*.down.sql") {
		body, err := os.ReadFile(filepath.Join(root, rel))
		require.NoError(t, err)
		if strings.Contains(string(body), "-- data loss") {
			out = append(out, rel)
		}
	}
	return out
}

func createdTableNames(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	re := regexp.MustCompile(`CREATE TABLE(?: IF NOT EXISTS)? +"([^"]+)"`)
	seen := map[string]bool{}
	for _, rel := range migrationUpFiles(t) {
		body, err := os.ReadFile(filepath.Join(root, rel))
		require.NoError(t, err)
		for _, m := range re.FindAllSubmatch(body, -1) {
			seen[string(m[1])] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	require.NotEmpty(t, out, "CREATE TABLE が 1 つも見つからない")
	sort.Strings(out)
	return out
}

// tsAffectingDestructivePattern captures the enumerated migrations that touch
// values written by Misskey TS, and the count stated alongside them.
var tsAffectingDestructivePattern = regexp.MustCompile("残る (\\d+) 件 \\(((?:`\\d{6}`(?: / )?)+)\\) は TS が書いた値にも当たる")

// tsAffectingDestructiveMigrations returns the migrations that the 破壊的な
// マイグレーション section declares as touching values Misskey TS wrote.
//
// **doc 自身の一覧を truth にする。** 「TS が書いた値に当たるか」は SQL からは
// 機械判定できない (`000081` は条件付きの DELETE、`000084` は入っている値が列
// DEFAULT のままかどうかで意味が変わる)。一覧は 1 行に列挙されているので拾える。
//
// これがあることで「うち N 件は mk-go 側だけが作るもの」を offset ではなく
// **引き算の結果**として検証できる。#2700 で 2 件目 (`000084`) が出たときに
// offset `-1` を `-2` へ動かす誘惑があったが、それは「正しい数字を書いた人に
// 誤った数字へ直させる」方向に効くので、数え方のほうを直した。
func tsAffectingDestructiveMigrations(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "docs/migration-from-ts.md"))
	require.NoError(t, err)

	m := tsAffectingDestructivePattern.FindSubmatch(body)
	require.NotNil(t, m, "docs/migration-from-ts.md で TS が書いた値に当たる migration の一覧が拾えない。"+
		"**書式を変えたならこの gate の正規表現も直すこと** — 拾えないまま放置すると、"+
		"「うち N 件は mk-go 側だけが作るもの」が検査されないまま緑になる")

	listed := regexp.MustCompile(`\d{6}`).FindAllString(string(m[2]), -1)
	stated, err := strconv.Atoi(string(m[1]))
	require.NoError(t, err)
	require.Equal(t, stated, len(listed),
		"「残る N 件」の N と列挙された migration の数が食い違っている")

	rows := destructiveMigrationRows(t)
	for _, num := range listed {
		require.Contains(t, rows, num,
			"TS が書いた値に当たるとされている %s が破壊的なマイグレーションの表に無い", num)
	}
	sort.Strings(listed)
	return listed
}

// migrationCountClaims lists every place that restates a mechanically countable
// migration fact.
//
// **名指しで持つ** — 「N 本」を機械的に拾うと無関係な数まで掛かる。
// **拾えなかったら落とす**ので、書式を変えたときに「検査していないのに緑」には
// ならない。
//
// offset は truth からの差分。**「うち mk-go 由来のもの」に offset は使わない** —
// かつては `-1` (TS が書いた行に触りうる `000081` の 1 件を引く) だったが、#2700 で
// 2 件目 (`000084`) が出た。定数を動かすと、正しい数字を書いた人に誤った数字へ
// 直させる方向に効くので、`destructive_mkgo` という fact を作って
// 「表の行数 - doc が列挙する TS 由来の件数」で導くようにしてある。
var migrationCountClaims = []struct {
	file    string
	pattern string
	fact    string
	offset  int
	symptom string
}{
	{"docs/architecture.md", `golang-migrate、現在 (\d+) 本`, "total", 0, "migration の総数"},
	{"internal/testutil/testdb.go", `再適用でエラーになるのは実測で \*\*(\d+) 本中`, "total", 0, "再適用の実測値の分母"},
	{"internal/testutil/testdb_schema_test.go", `して流し直すと、実測 (\d+) 本中`, "total", 0, "再適用の実測値の分母 (テスト側)"},
	{"docs/deployment.md", `接続 ok / migration version (\d+)`, "total", 0, "self-check の出力例 (小さいと selfcheck は FAIL を返すので、例として成立しない)"},

	{"docs/migration-from-ts.md", `共有テーブルにも触るものが (\d+) 件あるので`, "destructive", 0, "破壊的なマイグレーションの件数 (導入部)"},
	{"docs/migration-from-ts.md", `共有テーブルに触るものが (\d+) 件ある`, "destructive", 0, "破壊的なマイグレーションの件数 (本文)"},
	{"docs/migration-from-ts.md", `\*\*うち (\d+) 件は mk-go 側だけが作るもの`, "destructive_mkgo", 0, "破壊的なうち mk-go 由来のもの"},
	{"docs/migration-from-ts.md", `破壊的なマイグレーション\]\(#破壊的なマイグレーション\) の (\d+) 件は戻らない`, "destructive", 0, "破壊的なマイグレーションの件数 (切り戻し節)"},
	{"docs/migration-from-ts.md", `うち (\d+) 件は mk-go が自分で作ったものの除去`, "destructive_mkgo", 0, "破壊的なうち mk-go 由来のもの"},
	{"docs/architecture.md", `例外が (\d+) 件あり`, "destructive", 0, "破壊的なマイグレーションの件数"},
	{"docs/architecture.md", `うち (\d+) 件は mk-go が自分で作ったものの除去`, "destructive_mkgo", 0, "破壊的なうち mk-go 由来のもの"},
	{"docs/deployment.md", `原則追加のみだが、例外が (\d+) 件ある`, "destructive", 0, "破壊的なマイグレーションの件数"},
	{"docs/api-compatibility.md", `原則追加のみだが、例外が (\d+) 件ある`, "destructive", 0, "破壊的なマイグレーションの件数"},
	{"docker-compose.dropin.mk.yml", `原則追加のみ。例外は (\d+) 件`, "destructive", 0, "破壊的なマイグレーションの件数"},
	{"docker-compose.dropin.mk.yml", `うち (\d+) 件は mk-go が自分で作ったものの除去`, "destructive_mkgo", 0, "破壊的なうち mk-go 由来のもの"},

	{"docs/architecture.md", `宣言があるのは (\d+) 本だけ`, "dataloss", 0, "`-- data loss:` 宣言のある down"},
	{"docs/migration-from-ts.md", `あるのは (\d+) 本だけで`, "dataloss", 0, "`-- data loss:` 宣言のある down"},

	{"docs/api-compatibility.md", `migration が作るテーブルは (\d+)`, "tables", 0, "migration が作るテーブル数"},
	{"internal/testutil/testdb.go", `migration が作る (\d+) テーブル`, "tables", 0, "migration が作るテーブル数"},
	// **旧 CLAUDE.md の更新記録も見る。** #2756 の entry が同じ主張を持っており、
	// #3248 で docs/testing.md の「変更の経緯」へ移した。ここが漏れると、gate が
	// 落とした箇所だけ直して経緯の側が古いまま緑になる — この gate が塞ごうとして
	// いる形そのもの。
	{"docs/testing.md", `migration が作る (\d+) テーブル`, "tables", 0, "migration が作るテーブル数 (変更の経緯)"},
}

// TestMigrationCountsInDocsMatchReality fails when a stated count drifted.
func TestMigrationCountsInDocsMatchReality(t *testing.T) {
	facts := map[string]int{
		"total":       len(migrationUpFiles(t)),
		"destructive": len(destructiveMigrationRows(t)),
		// 表の行数から、doc 自身が「TS が書いた値にも当たる」と列挙している分を引く。
		"destructive_mkgo": len(destructiveMigrationRows(t)) - len(tsAffectingDestructiveMigrations(t)),
		"dataloss":         len(dataLossDeclaredDowns(t)),
		"tables":           len(createdTableNames(t)),
	}
	root := repoRoot(t)
	bodies := map[string][]byte{}
	for _, claim := range migrationCountClaims {
		if _, ok := bodies[claim.file]; !ok {
			body, err := os.ReadFile(filepath.Join(root, claim.file))
			require.NoError(t, err, "%s を読めない", claim.file)
			bodies[claim.file] = body
		}
		want, ok := facts[claim.fact]
		require.True(t, ok, "未知の fact %q", claim.fact)
		want += claim.offset

		m := regexp.MustCompile(claim.pattern).FindSubmatch(bodies[claim.file])
		if m == nil {
			// **落とすが、他の claim も見る。** 1 つ拾えないだけで打ち切ると、
			// 残りが未検査のまま「1 件の失敗」に見える。
			t.Errorf("%s で %q が拾えない。**書式を変えたならこの gate の正規表現も直すこと** — "+
				"拾えないまま放置すると、検査していないのに緑になる (%s)",
				claim.file, claim.pattern, claim.symptom)
			continue
		}
		got, err := strconv.Atoi(string(m[1]))
		require.NoError(t, err)
		if got != want {
			t.Errorf("%s の %s が %d になっているが、実際は %d。\n"+
				"**migration を足したら件数を書いた箇所すべてを直すこと** — "+
				"#2866 では 5 箇所が漏れた", claim.file, claim.symptom, got, want)
		}
	}
}

// noopDownListPattern captures the enumerated migrations and their count.
var noopDownListPattern = regexp.MustCompile("((?:`\\d{6}`(?: / )?)+) の (\\d+) 本は down が")

// TestNoopDownMigrationListMatchesReality fails when the enumerated list of
// no-op downs drifted.
//
// **件数だけでなく一覧を突き合わせる** (#2857)。件数だけだと「1 本足して
// 1 本消す」で素通りする。
func TestNoopDownMigrationListMatchesReality(t *testing.T) {
	const docFile = "docs/migration-from-ts.md"
	body, err := os.ReadFile(filepath.Join(repoRoot(t), docFile))
	require.NoError(t, err)

	m := noopDownListPattern.FindSubmatch(body)
	require.NotNil(t, m,
		"%s で down が no-op の一覧を拾えない。**書式を変えたならこの gate も直すこと** — "+
			"区切りを変えたり行を折ったりすると空振りする", docFile)

	var documented []string
	for _, tok := range strings.Split(string(m[1]), "/") {
		if v := strings.Trim(strings.TrimSpace(tok), "`"); v != "" {
			documented = append(documented, v)
		}
	}
	sort.Strings(documented)

	actual := noopDownMigrations(t)
	require.Equal(t, actual, documented,
		"%s の「down が `SELECT 1;`」の一覧が実態とずれている。\n"+
			"**足した migration の down を no-op にしたら、ここにも足すこと**", docFile)

	stated, err := strconv.Atoi(string(m[2]))
	require.NoError(t, err)
	require.Equal(t, len(actual), stated,
		"%s の本数 (%d) が一覧の長さ (%d) と合っていない", docFile, stated, len(actual))
}

// TestDestructiveMigrationTableRowsAreUnique keeps the table usable as the
// source of truth for the counts above.
func TestDestructiveMigrationTableRowsAreUnique(t *testing.T) {
	rows := destructiveMigrationRows(t)
	seen := map[string]bool{}
	for _, r := range rows {
		require.False(t, seen[r], "破壊的なマイグレーションの表に %s が重複している (件数が水増しになる)", r)
		seen[r] = true
	}
}
