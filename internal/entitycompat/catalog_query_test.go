package entitycompat

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// catalogFrom matches a FROM / JOIN / comma-separated FROM entry on a catalog
// view whose rows span schemas.
//
// `pg_indexes_size(` のような関数名を拾わないよう単語境界を要求する。
// `pg_catalog.` の修飾、`"pg_indexes"` のような引用符、JOIN、カンマで並べた FROM でも拾う。FROM の素の
// 名前だけを見ていると、普通に書ける別の綴りで検査を避けられてしまう。
//
// **`information_schema.schemata` は入れない。** あれは schema の一覧そのもので、
// 自分の schema に絞る概念が無い (`pluginstore` が plugin 用 schema を列挙するのに
// 使っている)。`pg_class` / `pg_attribute` も対象外 — join の書き方が多様で単純な
// 述語検査に乗らないため。規則は CLAUDE.md / docs/testing.md 側に書いてある。
var catalogFrom = regexp.MustCompile(tableRefLead + pgCatalogPrefix +
	`"?(?:pg_indexes\b|information_schema"?\s*\.\s*"?(?:columns|tables)\b)`)

// tableRefLead is what precedes a table reference: FROM, JOIN, or the comma of
// an implicit join (`FROM pg_class c, pg_indexes i`).
const tableRefLead = `(?i)(?:\b(?:from|join)\s+|,\s*)`

// pgCatalogPrefix is an optional `pg_catalog.` qualifier, quoted or not.
const pgCatalogPrefix = `(?:"?pg_catalog"?\s*\.\s*)?`

// scopedByPredicate matches a schema filter used as a predicate.
//
// **SELECT 句に列名があるだけでは不十分。** 単に `schemaname` を含むかで見ると
// `SELECT schemaname, indexdef FROM pg_indexes WHERE tablename = ?` が通って
// しまう (実際にこの gate の初版がその変異を見逃した)。列名は catalog によって
// 違う (`pg_indexes` は schemaname、`information_schema.*` は table_schema、
// `pg_namespace` を join する形は nspname)。
var scopedByPredicate = regexp.MustCompile(`(?i)\b(schemaname|table_schema|nspname)\s*(=|in\b)`)

// unscopedOptOut lets a query opt out when it deliberately looks across schemas.
//
// **理由を同じ行に書かせる。** `\s` は改行を含むので `\s*\S` だと次の行の
// 先頭にマッチしてしまい、理由なしのマーカーを通す (初版がそうだった)。
// 改行を除いた空白 `[^\S\n]` で受ける。
var unscopedOptOut = regexp.MustCompile(`--[^\S\n]*unscoped:[^\S\n]*\S`)

// indexdefCandidate matches a literal that can evaluate pg_get_indexdef: the
// `indexdef` column of pg_indexes, or a direct call to pg_get_indexdef.
var indexdefCandidate = regexp.MustCompile(tableRefLead + pgCatalogPrefix +
	`"?pg_indexes\b|\bpg_get_indexdef\s*\(`)

// whereKeyword marks where the predicate part of a query starts.
var whereKeyword = regexp.MustCompile(`(?i)\bwhere\b`)

// indexdefRef matches `indexdef` or `pg_get_indexdef` as a word.
var indexdefRef = regexp.MustCompile(`(?i)\b(pg_get_)?indexdef\b`)

// indexdefInWhere reports whether `indexdef` / `pg_get_indexdef` appears after
// the first WHERE of the query.
//
// #3365: `indexdef` の条件は index の pg_class だけを参照するので、plan によっては
// その scan まで押し下げられ、`schemaname = current_schema()` で絞る前に全 schema の
// index で `pg_get_indexdef` が評価される。並行する他パッケージの DDL がその index を
// 消していると `could not open relation with OID` で落ちる。schema で絞っても防げない
// ので、絞りの検査とは別に見る。`-- unscoped:` の opt-out も効かせない。
//
// SQL を自前で解析しない。最初の WHERE より後ろに語として現れたら違反にする。
// ORDER BY や副問い合わせの SELECT 句に書いた形も落ちるが、誤検知は赤になって
// 気付けるのに対し、取りこぼしは緑のまま残るので、広く取る側に倒す。
func indexdefInWhere(sql string) bool {
	loc := whereKeyword.FindStringIndex(sql)
	return loc != nil && indexdefRef.MatchString(sql[loc[1]:])
}

// requiredCatalogFiles are real files whose catalog queries the gate must see.
//
// 抽出の下限。検査した件数が 0 でないことだけを見ると、抽出が一部だけ空振りしても
// 気付けない。#3365 で直した user_uri_index_test.go と、pg_get_indexdef を直接呼ぶ
// migration のテストを名指しで要求する。
var requiredCatalogFiles = map[string]string{
	"internal/repository/user_uri_index_test.go":                "pg_indexes",
	"internal/repository/index_lookup_test.go":                  "pg_indexes",
	"internal/repository/migration_note_reaction_index_test.go": "pg_get_indexdef",
	// internal/repository の外。列挙を repository に狭める変異を落とすため。
	"internal/pluginstore/pluginstore_test.go": "catalog",
}

// minListedGoFiles is a lower bound on the files git ls-files returns.
//
// 名指しのファイルが残る形に列挙を狭めても気付けるよう、件数にも下限を置く。
// 2026-10-16 時点で 2560 件 (`git ls-files -- 'internal/*.go' | wc -l`)。
const minListedGoFiles = 1000

// gateOwnFile is skipped because its fixtures are deliberately unscoped queries.
//
// **除外したファイルに実クエリを書かないこと。** ここだけは gate の外にある。
const gateOwnFile = "internal/entitycompat/catalog_query_test.go"

// #2777: `pg_indexes` や `information_schema.*` は **schema を跨いで全件返す**。
// テスト DB は #2450 でパッケージごとに schema を分けているので、絞らないと
// 2 つ壊れる:
//
//   - 他 schema の同名オブジェクトを自分のものと取り違え、migration が適用されて
//     いなくても regression guard が緑になる (実測で 3 テストが空振りしていた —
//     `internal_repository_ts` の index 定義が返っていた)
//   - 他パッケージの `ApplyMigrations` が DDL 中だと
//     `could not open relation with OID (SQLSTATE XX000)` で落ちる。**CI の
//     required check (`test`) が不定期に赤くなる** (PR #2778 で実際に踏んだ)
//
// doc に書くだけだと再発する (17-19 schema ある限り条件は揃ったまま) ので、
// 静的に検出する。
//
// **文字列の切り出しは `go/parser` で行う。** backtick を機械的にペアリングする
// 初版は、コメントや rune literal に含まれる backtick でずれ、**gate 自身の
// ファイルで既にずれていた** (backtick 41 個 = 奇数)。ずれると「どのチャンクにも
// 入らない」クエリができて黙って無検査になる。
func TestCatalogQueriesAreSchemaScoped(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	checked := 0
	// 名指しで要求したファイルごとに、実際に見た検査の種類を記録する。
	seen := map[string]map[string]bool{}

	// ディスクを走査すると git add し忘れたファイルが手元でだけ検査されるので、
	// git ls-files で列挙する。
	out, err := exec.Command("git", "-C", root, "ls-files", "--", "internal/*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files internal/: %v", err)
	}
	files := strings.Fields(string(out))
	if len(files) < minListedGoFiles {
		t.Fatalf("git ls-files が internal/ の .go を %d 件しか返さない (下限 %d)。列挙が狭まっている",
			len(files), minListedGoFiles)
	}
	for _, rel := range files {
		if rel == gateOwnFile {
			continue
		}
		kinds, findings, perr := scanCatalogSource(fset, filepath.Join(root, filepath.FromSlash(rel)), nil)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		for _, k := range kinds {
			markSeen(seen, rel, k)
			if k == "catalog" {
				checked++
			}
		}
		for _, fd := range findings {
			t.Errorf("%s:%d: %s\n  query: %s", rel, fd.line, fd.problem, fd.sql)
		}
	}
	// 列挙が空なら「検査していないのに緑」になる。
	if checked == 0 {
		t.Fatal("カタログを引くクエリが 1 つも見つからない。gate が空振りしている")
	}
	for file, kind := range requiredCatalogFiles {
		if !seen[file][kind] {
			t.Errorf("%s に %s を引くクエリが見つからない。抽出が空振りしているか、"+
				"ファイルが変わったなら requiredCatalogFiles を直すこと", file, kind)
		}
	}
}

// judgeCatalogQuery returns which checks apply to sql ("pg_get_indexdef" for
// the #3365 check, "catalog" and "pg_indexes" for the #2777 check) and the
// violations found.
//
// 1 つのリテラルの判定をここにまとめ、gate の判定の配線ごと単体で固定する。
// 走査の側に判定を書くと、違反を報告する行を消す変異が、実際の違反が無い限り
// 素通りする (実測で素通りした)。
func judgeCatalogQuery(sql string) (kinds, problems []string) {
	if indexdefCandidate.MatchString(sql) {
		kinds = append(kinds, "pg_get_indexdef")
		if indexdefInWhere(sql) {
			problems = append(problems, "`indexdef` / `pg_get_indexdef` を WHERE に書いている (#3365)。\n"+
				"  schema で絞る前に全 schema の index で評価され、並行する DDL で\n"+
				"  `could not open relation with OID` になる。定義は SELECT で取り、Go の側で見ること。\n"+
				"  ORDER BY や CTE の外側など、絞った後に読む安全な形も落とす (WHERE より後ろを広く見るため)。\n"+
				"  その場合も SELECT 句で読み、並べ替えや絞り込みは Go の側で行うこと。")
		}
	}
	if !catalogFrom.MatchString(sql) {
		return kinds, problems
	}
	kinds = append(kinds, "catalog")
	if strings.Contains(strings.ToLower(sql), "pg_indexes") {
		kinds = append(kinds, "pg_indexes")
	}
	if !scopedByPredicate.MatchString(sql) && !unscopedOptOut.MatchString(sql) {
		problems = append(problems, "カタログを schema で絞っていないクエリがある (#2777)。\n"+
			"  `schemaname` / `table_schema` = current_schema() を WHERE に足すこと。\n"+
			"  意図的に schema を跨ぐ場合は SQL に `-- unscoped: <理由>` を書く。")
	}
	return kinds, problems
}

// catalogFinding is one violation found in a Go source file.
type catalogFinding struct {
	line    int
	problem string
	sql     string
}

// scanCatalogSource judges every string literal in a Go source, and every `+`
// chain whose leaves are all string literals as one concatenated query.
// src follows parser.ParseFile (nil reads filename).
//
// 長いクエリを `"..." +` で分けて書くと、1 つ目のリテラルに FROM、2 つ目に WHERE が
// 入り、リテラル単位では #3365 の形を見落とす。葉がすべて文字列の連結は、つないで
// から判定する。変数を挟む連結はつながず、リテラルごとに判定する。
func scanCatalogSource(fset *token.FileSet, filename string, src any) (kinds []string, findings []catalogFinding, err error) {
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, nil, err
	}
	judge := func(pos token.Pos, sql string) {
		k, problems := judgeCatalogQuery(sql)
		kinds = append(kinds, k...)
		for _, p := range problems {
			findings = append(findings, catalogFinding{
				line: fset.Position(pos).Line, problem: p,
				sql: strings.Join(strings.Fields(sql), " "),
			})
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BinaryExpr:
			if sql, ok := concatStringLits(n); ok {
				judge(n.Pos(), sql)
				return false
			}
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			if sql, uerr := strconv.Unquote(n.Value); uerr == nil {
				judge(n.Pos(), sql)
			}
		}
		return true
	})
	return kinds, findings, nil
}

// concatStringLits returns the concatenation when e is a `+` chain of string
// literals only.
func concatStringLits(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return concatStringLits(e.X)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		l, ok := concatStringLits(e.X)
		if !ok {
			return "", false
		}
		r, ok := concatStringLits(e.Y)
		return l + r, ok
	}
	return "", false
}

func markSeen(seen map[string]map[string]bool, file, kind string) {
	if seen[file] == nil {
		seen[file] = map[string]bool{}
	}
	seen[file][kind] = true
}

// gate 自身の判定を固定する。**これが無いと gate のロジックを弱める変異を
// 検出できない** — 実測で「列名を含むか」だけを見る初版に戻す変異が素通りした
// (対象クエリの SELECT 句に `schemaname` があるため)。
func TestCatalogQueryGate_Predicates(t *testing.T) {
	// ok = gate が「絞れている / 意図的に絞らない」と判定すること。
	for _, tc := range []struct {
		name string
		sql  string
		ok   bool
	}{
		{"WHERE で絞る", "SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND tablename = $1", true},
		{"IN でも絞れている", "SELECT indexdef FROM pg_indexes WHERE schemaname IN (current_schema())", true},
		{"information_schema は table_schema", "SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema()", true},
		{"pg_catalog で修飾して絞らない", "SELECT indexdef FROM pg_catalog.pg_indexes WHERE tablename = $1", false},
		{"JOIN で引いて絞らない", "SELECT i.indexdef FROM pg_class c JOIN pg_indexes i ON i.indexname = c.relname WHERE i.tablename = $1", false},
		{"カンマで並べた FROM で絞らない", "SELECT i.indexdef FROM pg_class c, pg_indexes i WHERE i.indexname = c.relname", false},
		{"カンマで並べた information_schema で絞らない", "SELECT 1 FROM pg_class c, information_schema.columns col WHERE col.table_name = c.relname", false},
		{"引用符で囲んで絞らない", `SELECT indexdef FROM "pg_indexes" WHERE tablename = $1`, false},
		{"information_schema を引用符で囲んで絞らない", `SELECT 1 FROM "information_schema"."columns" WHERE table_name = $1`, false},
		{"nspname で絞る形", "SELECT 1 FROM information_schema.tables t JOIN x ON 1=1 WHERE nspname = current_schema()", true},
		{"SELECT 句にあるだけでは不足", "SELECT schemaname, indexdef FROM pg_indexes WHERE tablename = $1", false},
		{"絞りが無い", "SELECT indexdef FROM pg_indexes WHERE tablename = $1", false},
		{"information_schema の絞り無し", "SELECT 1 FROM information_schema.columns WHERE table_name = $1", false},
		{"schemata は対象外なので絞り無しでよい", "SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE $1", true},
		{"opt-out は理由が要る", "SELECT count(*) FROM pg_indexes -- unscoped:\n WHERE tablename = $1", false},
		{"理由付きの opt-out は通る", "SELECT count(*) FROM pg_indexes -- unscoped: 複数 schema を数える\n WHERE tablename = $1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 検査対象から外れているものは、それ自体が「通る」判定。
			if !catalogFrom.MatchString(tc.sql) {
				if !tc.ok {
					t.Fatalf("検査対象として拾えていない: %s", tc.sql)
				}
				return
			}
			got := scopedByPredicate.MatchString(tc.sql) || unscopedOptOut.MatchString(tc.sql)
			if got != tc.ok {
				t.Errorf("judged %v, want %v for: %s", got, tc.ok, tc.sql)
			}
			_, problems := judgeCatalogQuery(tc.sql)
			if (len(problems) == 0) != tc.ok {
				t.Errorf("judgeCatalogQuery problems = %q, want ok=%v for: %s", problems, tc.ok, tc.sql)
			}
		})
	}

	// indexdef を WHERE に書く形 (#3365)。want = 違反として落とすこと。
	for _, tc := range []struct {
		name string
		sql  string
		want bool
	}{
		// #3365 で直す前の user_uri_index_test.go のクエリそのまま。schema で絞っていても落とす。
		{"#3365 の修正前の形", `
		SELECT indexname, indexdef FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename = 'user'
		  AND indexdef LIKE '%USING btree (uri)%'`, true},
		{"#3365 の修正後の形", `
		SELECT indexname, indexdef FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename = 'user'`, false},
		{"大文字でも拾う", "SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND INDEXDEF ILIKE '%gin%'", true},
		{"pg_get_indexdef を WHERE で呼ぶ", "SELECT c.relname FROM pg_class c WHERE c.relnamespace = to_regnamespace(current_schema()) AND pg_get_indexdef(c.oid) LIKE '%uri%'", true},
		{"pg_get_indexdef を SELECT で呼ぶだけ", "SELECT pg_get_indexdef(c.oid) FROM pg_class c WHERE c.relnamespace = to_regnamespace(current_schema()) AND c.relname = ?", false},
		{"opt-out があっても落とす", "SELECT indexname FROM pg_indexes -- unscoped: 理由\n WHERE indexdef LIKE '%uri%'", true},
		{"WHERE が無い", "SELECT indexdef FROM pg_indexes", false},
		{"pg_catalog で修飾した形", "SELECT indexname FROM pg_catalog.pg_indexes WHERE schemaname = current_schema() AND indexdef LIKE '%uri%'", true},
		{"JOIN で引く形", "SELECT c.relname FROM pg_class c JOIN pg_indexes i ON i.indexname = c.relname WHERE i.schemaname = current_schema() AND i.indexdef LIKE '%uri%'", true},
		{"カンマで並べた FROM", "SELECT c.relname FROM pg_class c, pg_indexes i WHERE i.indexname = c.relname AND i.schemaname = current_schema() AND i.indexdef LIKE '%uri%'", true},
		{"引用符で囲んだ形", `SELECT indexname FROM "pg_indexes" WHERE schemaname = current_schema() AND indexdef LIKE '%uri%'`, true},
		// 外側の WHERE に indexdef があり、その後ろに副問い合わせの WHERE が続く。
		// 最後の WHERE より後ろだけを見ると見落とす。
		{"外側の WHERE の後ろに副問い合わせの WHERE", "SELECT indexname FROM pg_indexes WHERE indexdef LIKE '%x%' AND indexname IN (SELECT indexname FROM pg_indexes WHERE schemaname = current_schema())", true},
		{"小文字の where", "select indexname from pg_indexes\nwhere schemaname = current_schema() and indexdef like '%x%'", true},
		{"列名の一部は語として拾わない", "SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND indexdef_backup = 1", false},
	} {
		t.Run("indexdef/"+tc.name, func(t *testing.T) {
			if !indexdefCandidate.MatchString(tc.sql) {
				t.Fatalf("indexdef の検査対象として拾えていない: %s", tc.sql)
			}
			if got := indexdefInWhere(tc.sql); got != tc.want {
				t.Errorf("indexdefInWhere = %v, want %v for: %s", got, tc.want, tc.sql)
			}
			// 走査が使う判定の入口でも同じ結論になること。
			_, problems := judgeCatalogQuery(tc.sql)
			reported := false
			for _, p := range problems {
				reported = reported || strings.Contains(p, "#3365")
			}
			if reported != tc.want {
				t.Errorf("judgeCatalogQuery reported #3365 = %v, want %v for: %s", reported, tc.want, tc.sql)
			}
		})
	}

	// 検査対象の切り出し。`pg_indexes_size(...)` は view ではなく関数、
	// `pg_indexes_backup` は別テーブル。どちらも `\b` が無いと誤検知する。
	for _, tc := range []struct {
		sql  string
		want bool
	}{
		{"SELECT indexdef FROM pg_indexes WHERE tablename = $1", true},
		{"SELECT 1 FROM information_schema.columns WHERE table_name = $1", true},
		{"SELECT pg_indexes_size(c.oid) FROM pg_class c", false},
		{"SELECT * FROM pg_indexes_backup", false},
		{"SELECT * FROM information_schema_backup", false},
		{"SELECT indexdef FROM pg_catalog.pg_indexes", true},
		{"SELECT 1 FROM pg_class c JOIN pg_indexes i ON true", true},
		{`SELECT indexdef FROM "pg_indexes"`, true},
		{`SELECT 1 FROM "information_schema"."tables"`, true},
		{"SELECT * FROM pg_catalog.pg_indexes_backup", false},
		{"SELECT 1 FROM pg_class c, pg_indexes i", true},
		{"SELECT 1 FROM pg_class c,information_schema.tables t", true},
		{"SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE $1", false},
	} {
		if got := catalogFrom.MatchString(tc.sql); got != tc.want {
			t.Errorf("catalogFrom matched %v, want %v for: %s", got, tc.want, tc.sql)
		}
	}
}

// 文字列の連結で組み立てた SQL も 1 つのクエリとして判定すること (#3369)。
func TestCatalogQueryGate_Concatenation(t *testing.T) {
	const src = `package p

var a = ` + "`SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = current_schema()`" + ` +
	" AND indexdef LIKE '%uri%'"

var b = ("SELECT indexdef FROM pg_indexes " +
	"WHERE schemaname = current_schema()") + " AND tablename = 'user'"

var c = "SELECT indexdef FROM pg_indexes WHERE tablename = " + name

var name = "'user'"

var d = ("SELECT indexname FROM pg_indexes WHERE schemaname = current_schema()") +
	" AND indexdef LIKE '%uri%'"
`
	_, findings, err := scanCatalogSource(token.NewFileSet(), "concat.go", src)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int][]string{}
	for _, fd := range findings {
		tag := "#2777"
		if strings.Contains(fd.problem, "#3365") {
			tag = "#3365"
		}
		got[fd.line] = append(got[fd.line], tag)
	}
	// a: 連結すると #3365 の形。b: 連結すると絞れている安全な形。
	// c: 変数を挟む連結は追わず、リテラル単体で #2777 として落ちる。
	// d: 括弧で囲んだ葉も辿って連結し、#3365 の形として落とす。
	want := map[int][]string{3: {"#3365"}, 9: {"#2777"}, 13: {"#3365"}}
	if len(got) != len(want) || strings.Join(got[3], ",") != "#3365" ||
		strings.Join(got[9], ",") != "#2777" || strings.Join(got[13], ",") != "#3365" {
		t.Errorf("findings by line = %v, want %v", got, want)
	}
}
