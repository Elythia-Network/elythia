package entitycompat

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **値はクォートで囲んだリテラルへ差し込まず、必ず bind する。**
//
// `fmt.Sprintf` で SQL を組むこと自体は列名やテーブル名の解決で要るが、
// **書式動詞がシングルクォートで開いたリテラルの内側に在る**形 (`'%s'`) は、
// 値をリテラルとして文字列に埋めていることを意味する。値にクォートが 1 つ
// 入ればリテラルが途中で閉じ、残りが構文として解釈される。
//
// **「これは SQL か」を判定しない。** 初版はキーワードで SQL らしさを判定して
// いたが、両方向に壊れた (実測)。
//
//   - 偽陽性: 普通の英文が `values` の部分一致で SQL と判定され、
//     「プレースホルダで bind すること」という**事実と逆の診断**が出た
//     (使い捨ての probe に `"plugin '%s' returned no values"` を置いて実測。
//     この文字列自体はリポジトリに実在しない)。allowlist の理由欄は「なぜ SQL と
//     して解釈されないか」を書かせる形なので、非 SQL には書きようがない
//   - 偽陰性: `'%s'::varchar[]` のような**断片**はキーワードに 1 つも当たらず
//     収集すらされない。このリポジトリには SQL を断片ごとに組んで `strings.Join`
//     する経路が (chart / fsck / maintenance など) あり、そこが盲点になっていた
//
// 判定を「クォートで開いた区間に動詞が在るか」だけにすると、走査 180 サイト
// (ユニークキー 98、うち人工ソース 6 サイト) に対し該当は 3 サイト / 3 キーしか
// ない (実測)。本番はそのうち `internal/server/frontend.go#renderFrontendShell` の
// 1 キー (1 サイト、JS 生成) だけで、allowlist に理由付きで載せれば済む。
//
// **代わりに網は広がる。** 値をクォートで囲んだだけのメッセージも該当する。
// 現 corpus に**メッセージの形は 0 件**で、allowlist に載っている非 SQL は
// frontend.go の JS 生成だけ。将来足りたら**値の出どころを書いて載せる**。
// だから診断は「bind しろ」と決めつけず、SQL でない場合の逃げ道も示す。
// 「これは SQL か」を推測するより、「クォートの内側に値を入れたら一度見る」の
// ほうが述語として正直で、しかも断片を取りこぼさない。
//
// 押さえ方は 3 つ:
//
//   - 述語 (`verbsInsideQuotedLiteral`) を表で固定する。クォートの状態機械なので、
//     空のリテラル (クォート 2 つ) の後ろの動詞を外側と数えること、`%%` を
//     動詞と数えないことの両方が要る。素朴な正規表現はどちらも取りこぼす
//   - 収集側を**名指しの実サイト**で固定する。件数の下限は診断を壊す
//     (正当に 1 つ減らしただけで「抽出が壊れている」と事実と逆を出す、#3135)
//   - 検出の枝は人工ソース (`sqlbindfixture`) で固定する。本番に違反が無い以上、
//     本番だけを見ていると検出側の分岐が一度も実行されない
//
// **射程外** (いずれも実測で素通りを確認済み): 外部由来の文字列が SQL へ届くかの
// taint 解析はしない (#2644 と同じ理由で偽陽性が本物の signal を埋める)。
// 書式を `const` や変数に入れた `Sprintf`、書式の途中に変数を連結した形、
// `fmt.Sprintf` 以外 (`Fprintf` / `Errorf`)、`fmt` の別名 import、
// 文字列連結や `strings.Builder` で組む SQL、`Raw` / `Exec` へ渡す非リテラル、
// 別 module の `plugins/`、`internal` / `cmd` / `plugin` 以外のツリー。
// **名前で見る走査は名前で避けられる** (#3135) ので、ここは「普通に書いたときに
// 踏む形」を確実に落とすことに寄せてある。

// sqlFormatSite is one fmt.Sprintf format string found by the scanner.
type sqlFormatSite struct {
	// File is the repo-relative, slash-separated path.
	File string
	// Func is the enclosing declaration name, as eachDecl names it.
	Func string
	Line int
	// Format is the folded format string.
	Format string
}

// Key returns the stable "<file>#<func>" identifier used by the allowlist.
//
// 行番号を key にしない。上下に 1 行足しただけで allowlist が死んだ entry になり、
// 検査対象から黙って外れる。
func (s sqlFormatSite) Key() string { return s.File + "#" + s.Func }

func (s sqlFormatSite) String() string {
	return fmt.Sprintf("%s (%s:%d)", s.Key(), s.File, s.Line)
}

// sqlFormatRoots are the trees scanned for format strings.
var sqlFormatRoots = []string{"internal", "cmd", "plugin"}

// quotedVerbAllowlist maps "<file>#<func>" to the reason a format verb is
// allowed to sit inside a quoted literal there.
//
// **理由には「どの値が入るか」を書くこと。** 「安全なはず」ではなく、値の
// 出どころを書く。SQL でないものをここに足すときも同じ。
var quotedVerbAllowlist = map[string]string{
	"internal/server/frontend.go#renderFrontendShell": "SQL ではなく JS の生成。" +
		"入るのは Vite manifest のエントリ名で、サーバー側で決まる値。",
	"internal/entitycompat/sqlbindfixture/sample.go#UnsafeQuotedValue":        "人工ソース: 検出の枝を固定するための違反形。どこからも呼ばない。",
	"internal/entitycompat/sqlbindfixture/sample.go#UnsafeConcatenatedFormat": "人工ソース: 書式を定数連結で折り返した違反形。畳んでから判定していることを固定する。",
}

// sqlFormatMustDetect names real call sites the scanner has to keep finding,
// each with a substring its format must contain.
//
// 収集が縮んだことは件数では見られない。実在するサイトを名指しで要求する。
// 並びは固定する (map で回すと報告されるサイトが実行ごとに変わる)。
var sqlFormatMustDetect = []struct{ Key, Want string }{
	{"internal/core/chart/repository.go#ApplyDeltas", "array_cat"},
	{"internal/core/chart/repository.go#Insert", "INSERT INTO"},
	{"internal/core/chart/repository.go#findOne", "SELECT"},
	{"internal/core/fsck/fsck.go#repair", "UPDATE"},
	{"internal/entitycompat/sqlbindfixture/sample.go#SafeConcatenatedFormat", "WHERE"},
	{"internal/entitycompat/sqlbindfixture/sample.go#SafePlaceholder", "array_cat"},
	{"internal/entitycompat/sqlbindfixture/sample.go#UnsafeConcatenatedFormat", "WHERE"},
	{"internal/maintenance/host_backfill.go#BackfillHostColumnBatch", "SELECT"},
	{"internal/repository/poll.go#IncrementVote", "votes["},
	// **`internal/` だけを名指しにしない。** 走査の内訳は internal 175 / cmd 2 /
	// plugin 3 (実測) なので、`cmd` か `plugin` を root から外しても違反集合も
	// 名指しも一切変わらず、5 サイトが黙って検査対象から消える (#3136 と同じ型)。
	// cmd の名指しは以前 cmd/migrate の DSN 組み立てだったが、DSN は
	// config.DatabaseURL へ寄せたので healthcheck の URL に替えた。
	{"cmd/misskey/main.go#runHealthcheck", "/healthz"},
	{"plugin/api.go#Error", "plugin: API"},
}

// verbsInsideQuotedLiteral returns the byte offsets of fmt verbs that sit
// inside a single-quoted literal within format.
//
// リテラルはクォートで開いて閉じるだけなので、状態を反転させれば足りる。
// 空のリテラル (クォート 2 つ) は開いて即閉じるため、その後ろは外側に戻る。
// `%%` は書式動詞ではなくリテラルの `%` なので数えない。
func verbsInsideQuotedLiteral(format string) []int {
	var inside, afterFirstQuote []int
	inLiteral := false
	firstQuote := -1
	for i := 0; i < len(format); i++ {
		switch format[i] {
		case '\'':
			if firstQuote < 0 {
				firstQuote = i
			}
			inLiteral = !inLiteral
		case '%':
			if i+1 < len(format) && format[i+1] == '%' {
				i++
				continue
			}
			if inLiteral {
				inside = append(inside, i)
			}
			if firstQuote >= 0 {
				afterFirstQuote = append(afterFirstQuote, i)
			}
		}
	}
	// **クォートが釣り合わない書式では、パリティだけで内外を決められない。**
	// 余ったクォート 1 つで以降の内外が全部反転するので、`-- don't touch` を
	// 先頭に置くだけで後続の本物の違反が 0 件になる (実測)。逆に
	// `can't resolve %s` のような英文は、存在しないリテラルの内側だと判定される。
	// **「内側 0 件」は「安全」ではなく「判定できない」を意味する**ので、安全側へ
	// 倒さず最初のクォートより後ろの動詞をすべて報告する。SQL なら bind し、
	// SQL でないなら値の出どころを allowlist に書く。
	if inLiteral {
		return afterFirstQuote
	}
	return inside
}

// foldConstString folds a constant string expression - a literal, or literals
// joined with + - into its value.
//
// **畳まないと、折り返した書式が丸ごと検査対象から消える。** 長い SQL を
// `"..." + "..."` で折り返すのは普通の書き方で、第 1 引数が `*ast.BasicLit`
// のときだけ拾う形はその書式を 1 件も収集しない (実測)。
func foldConstString(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.ParenExpr:
		return foldConstString(v.X)
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			// parser が作る文字列リテラルでは到達しない枝 (raw string の `\r` は
			// 仕様どおり除去され、改行やクォートも通る)。到達したら畳めないものと
			// 同じ扱いで収集しないので、**そのサイトは検査対象から外れる**。
			return "", false
		}
		return s, true
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, ok := foldConstString(v.X)
		if !ok {
			return "", false
		}
		r, ok := foldConstString(v.Y)
		if !ok {
			return "", false
		}
		return l + r, true
	}
	return "", false
}

// sprintfFormat returns the folded format string of a fmt.Sprintf call.
func sprintfFormat(n ast.Node) (string, token.Pos, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", token.NoPos, false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Sprintf" {
		return "", token.NoPos, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "fmt" {
		return "", token.NoPos, false
	}
	value, ok := foldConstString(call.Args[0])
	if !ok {
		return "", token.NoPos, false
	}
	return value, call.Args[0].Pos(), true
}

// scanSQLFormatSites collects every fmt.Sprintf format literal under the
// configured roots, skipping test files.
func scanSQLFormatSites(t *testing.T, root string) ([]sqlFormatSite, map[string]bool) {
	t.Helper()
	var sites []sqlFormatSite
	ambiguous := map[string]bool{}
	fset := token.NewFileSet()
	for _, dir := range sqlFormatRoots {
		abs := filepath.Join(root, dir)
		if _, err := os.Stat(abs); err != nil {
			continue
		}
		err := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return fmt.Errorf("parse %s: %w", path, perr)
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			rel = filepath.ToSlash(rel)
			// **同一ファイルに同名の宣言が 2 つあると key が衝突する。** Go は
			// パッケージ関数と別型のメソッドが同名を持てるので、`<file>#<func>`
			// の allowlist が無関係な宣言へ黙って広がる (実測で素通りした)。
			// **ただし同名自体は正当** — `internal/activitypub/types.go` は別々の型に
			// `UnmarshalJSON` を持つ。落とすのは**その key を allowlist が使っている
			// ときだけ**にする (曖昧な許可がそこでしか起きないため)。
			// `eachDecl` は他のゲートと共有なので owner の形は変えない。
			seenOwner := map[string]bool{}
			eachDecl(file, func(owner string, n ast.Node) {
				if seenOwner[owner] {
					ambiguous[rel+"#"+owner] = true
				}
				seenOwner[owner] = true
				ast.Inspect(n, func(x ast.Node) bool {
					value, pos, ok := sprintfFormat(x)
					if !ok {
						return true
					}
					sites = append(sites, sqlFormatSite{
						File:   rel,
						Func:   owner,
						Line:   fset.Position(pos).Line,
						Format: value,
					})
					return true
				})
			})
			return nil
		})
		require.NoError(t, err, "walk %s", abs)
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})
	return sites, ambiguous
}

func TestSQLBindVerbDetectionShapes(t *testing.T) {
	// 述語そのものを固定する。本番に違反が無いので、ここが唯一「内側」の枝を
	// 通す経路になる。
	cases := []struct {
		name   string
		format string
		want   int
	}{
		{"value spliced into a quoted brace literal", `WHERE j = '{"k": "%s"}'`, 1},
		{"value spliced into a quoted scalar", `WHERE "noteId" = '%s'`, 1},
		{"fragment with no SQL keyword at all", `'%s'::varchar[]`, 1},
		{"column name outside any literal", `SELECT %s FROM "t"`, 0},
		{"placeholder instead of a verb", `SET "%s" = array_cat("%s", ?::varchar[])`, 0},
		{"empty literal then a verb", `SELECT %s FROM "t" WHERE "h" <> '' ORDER BY %s ASC`, 0},
		{"escaped percent inside a literal", `SELECT %s FROM "t" WHERE "n" LIKE '%%x%%'`, 0},
		{"two literals, verb between them", `WHERE a = 'x' AND b = %s AND c = 'y'`, 0},
		{"verb inside the second literal", `WHERE a = 'x' AND b = 'p%sq'`, 1},
		{"two verbs inside one literal", `WHERE a = '%s-%s'`, 2},
		{"integer verb inside a literal", `SET votes['%d'] = 1`, 1},
		// クォートが釣り合わない形。パリティだけだと前者は 0 件に化け、
		// 後者は存在しないリテラルの内側と判定される。
		{"unbalanced quote hides a later violation", "-- don't touch\nWHERE c = '%s'", 1},
		{"unbalanced quote in plain prose", `can't resolve %s`, 1},
		{"unbalanced quote before a safe verb", `it's fine: %s`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Len(t, verbsInsideQuotedLiteral(tc.format), tc.want, "format: %s", tc.format)
		})
	}
}

func TestSQLBindFoldsConcatenatedFormats(t *testing.T) {
	// 定数連結の畳み込みを固定する。ここが効かないと、折り返した書式が
	// 収集そのものから消える (違反ではなく**不在**になるので、違反集合の
	// 比較では気付けない)。
	src := "package p\n\nimport \"fmt\"\n\n" +
		"func a(v string) string { return fmt.Sprintf(`x = '` + `%s` + `'`, v) }\n" +
		"func b(v string) string { return fmt.Sprintf((`y = ` + `'%s'`), v) }\n" +
		"func c(v, w string) string { return fmt.Sprintf(`z = ` + w + `'%s'`, v) }\n"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", src, 0)
	require.NoError(t, err)

	got := map[string]string{}
	eachDecl(file, func(owner string, n ast.Node) {
		ast.Inspect(n, func(x ast.Node) bool {
			if value, _, ok := sprintfFormat(x); ok {
				got[owner] = value
			}
			return true
		})
	})
	require.Equal(t, `x = '%s'`, got["a"], "リテラルの連結は畳む")
	require.Equal(t, `y = '%s'`, got["b"], "括弧をまたいでも畳む")
	_, hasC := got["c"]
	require.False(t, hasC, "変数を挟んだ連結は定数でないので収集しない (射程外)")
	require.Len(t, verbsInsideQuotedLiteral(got["a"]), 1, "畳んだ結果で違反と判定できること")
}

func TestSQLBindNoVerbInsideQuotedLiteral(t *testing.T) {
	root := repoRoot(t)
	sites, ambiguous := scanSQLFormatSites(t, root)
	require.NotEmpty(t, sites, "書式を 1 つも拾えていない。走査が壊れている")

	// key が 1 ファイル内の複数宣言を指していないこと。指していると、
	// allowlist の 1 行が無関係な宣言まで黙って許す。
	var ambiguousKeys []string
	for key := range quotedVerbAllowlist {
		if ambiguous[key] {
			ambiguousKeys = append(ambiguousKeys, key)
		}
	}
	for _, want := range sqlFormatMustDetect {
		if ambiguous[want.Key] {
			ambiguousKeys = append(ambiguousKeys, want.Key)
		}
	}
	sort.Strings(ambiguousKeys)
	assert.Emptyf(t, ambiguousKeys, "同名の宣言が 2 つあるファイルを key が指している。"+
		"どちらを指すか決まらないので rename すること:\n  %s", strings.Join(ambiguousKeys, "\n  "))

	// 収集が縮んでいないこと。件数ではなく実在するサイトを名指しで要求する。
	byKey := map[string][]sqlFormatSite{}
	for _, s := range sites {
		byKey[s.Key()] = append(byKey[s.Key()], s)
	}
	var missing []string
	for _, want := range sqlFormatMustDetect {
		found := false
		for _, s := range byKey[want.Key] {
			if strings.Contains(s.Format, want.Want) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, fmt.Sprintf("%s (書式に %q を含むもの)", want.Key, want.Want))
		}
	}
	assert.Emptyf(t, missing, "拾えなくなった call site が %d 件ある。走査が壊れている可能性がある:\n  %s",
		len(missing), strings.Join(missing, "\n  "))

	// 違反の集合は allowlist と完全に一致すること。
	var violations []string
	seen := map[string]bool{}
	for _, s := range sites {
		if len(verbsInsideQuotedLiteral(s.Format)) == 0 {
			continue
		}
		seen[s.Key()] = true
		if _, ok := quotedVerbAllowlist[s.Key()]; ok {
			continue
		}
		violations = append(violations, fmt.Sprintf(
			"%s: 書式動詞がクォートで開いたリテラルの内側に在る。"+
				"SQL ならプレースホルダで bind する。SQL でないなら allowlist に値の出どころを書く\n    %s",
			s, s.Format))
	}
	// **assert で報告する。** require だと先に止まって、下の死んだ entry の
	// 診断が出ない (#3135 と同じ型)。
	assert.Emptyf(t, violations, "値をリテラルへ差し込んでいる箇所が %d 件ある:\n%s",
		len(violations), strings.Join(violations, "\n"))

	// allowlist に死んだ entry が残らないこと。残ると「検査しているつもりで
	// 何も守っていない」状態になる。
	var dead []string
	for key, reason := range quotedVerbAllowlist {
		if !seen[key] {
			dead = append(dead, fmt.Sprintf("%s (理由: %s)", key, reason))
			continue
		}
		assert.NotEmptyf(t, strings.TrimSpace(reason), "allowlist の %s に理由が無い", key)
	}
	sort.Strings(dead)
	assert.Emptyf(t, dead, "allowlist の entry が現在の走査で検出されない。entry が古い:\n  %s",
		strings.Join(dead, "\n  "))
}

// applyDeltasFormats pins every SQL format string ApplyDeltas builds.
//
// **「プレースホルダが在るか」では守れない。** ダミーの `?` を 1 つ足し、
// `pgArrayLiteral` とは別名のビルダで配列をリテラルへ畳み、要素数で分岐させる
// 変異は、静的ゲートも chart パッケージの全テストも**緑のまま通った** (実測。
// リテラルが SQL テキストに載ることは手元で確認した)。書式そのものを固定すると、
// この形も、文字列連結や
// `strings.Builder` へ移す形も、同時に落ちる。
//
// 正当に SQL を変えるときはここも直す。並びは無視して集合として比べる。
var applyDeltasFormats = []string{
	`"%s" = "%s" + ?`,
	`"%s" = LEAST("%s"::bigint + ?, ?)`,
	`"%s" = GREATEST("%s"::bigint + ?, ?)`,
	`"%s" = ?`,
	`"%s" = array_cat("%s", ?::varchar[])`,
	`UPDATE "%s" SET %s WHERE "id" = ?`,
}

func TestSQLBindChartUniqueArrayIsParameterised(t *testing.T) {
	// unique 配列は行の値そのもの (外部由来の文字列を含みうる) なので、SQL
	// テキストへ入れずバインド引数で渡す。
	const rel = "internal/core/chart/repository.go"
	path := filepath.Join(repoRoot(t), filepath.FromSlash(rel))
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	require.NoError(t, err, "parse %s", rel)

	// 1. ApplyDeltas が組む書式の集合が 1 文字も変わっていないこと。
	var got []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "ApplyDeltas" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if value, _, ok := sprintfFormat(n); ok {
				got = append(got, value)
			}
			return true
		})
	}
	require.NotEmpty(t, got, "%s に ApplyDeltas が無い。対象が移動した可能性がある", rel)
	sort.Strings(got)
	want := append([]string(nil), applyDeltasFormats...)
	sort.Strings(want)
	require.Equal(t, want, got,
		"%s: ApplyDeltas が組む SQL 書式が変わっている。値を bind したままか確かめて applyDeltasFormats を更新すること", rel)

	// 2. pgArrayLiteral の戻り値がクエリ引数の slice にしか渡らないこと。
	argSlices := queryArgSliceNames(file)
	require.NotEmpty(t, argSlices, "%s で Exec / Raw の可変長引数を解決できない。走査が壊れている", rel)
	calls, bound := pgArrayLiteralCallSites(file, argSlices)
	require.Positive(t, calls, "%s に pgArrayLiteral の呼び出しが無い。対象が移動した可能性がある", rel)
	require.Equal(t, calls, bound,
		"%s: pgArrayLiteral の戻り値がクエリ引数以外へ渡っている (%d/%d)。"+
			"配列リテラルは SQL テキストではなくバインド引数で渡すこと", rel, bound, calls)
}

// queryArgSliceNames returns the identifiers this file expands variadically
// into Exec/Raw, i.e. the slices that actually carry query arguments.
//
// **`args` という名前を決め打ちにしない。** 局所変数を `params` へ改名しただけで
// ゲートが落ち、しかも「バインド引数で渡すこと」と**既にそうしている**コードへ
// 事実と逆の指示を出す (実測)。
func queryArgSliceNames(file *ast.File) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !call.Ellipsis.IsValid() || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "Exec", "Raw":
		default:
			return true
		}
		if id, ok := call.Args[len(call.Args)-1].(*ast.Ident); ok {
			out[id.Name] = true
		}
		return true
	})
	return out
}

// pgArrayLiteralCallSites counts pgArrayLiteral calls and how many are passed
// directly to append(<query arg slice>, ...).
func pgArrayLiteralCallSites(file *ast.File, argSlices map[string]bool) (calls, bound int) {
	appended := appendedToQueryArgs(file, argSlices)
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || ident.Name != "pgArrayLiteral" {
			return true
		}
		calls++
		if len(stack) < 2 {
			return true
		}
		switch parent := stack[len(stack)-2].(type) {
		case *ast.CallExpr:
			if isAppendToQueryArgs(parent, argSlices) {
				bound++
			}
		case *ast.AssignStmt:
			// **局所変数に受けるのは正当な書き方。** `append` の直接の実引数で
			// あることまで要求すると、既に bind しているコードへ「バインド引数で
			// 渡すこと」と事実と逆の指示が出る (実測)。1 段だけ追う。
			for _, lhs := range parent.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && appended[id.Name] {
					bound++
					break
				}
			}
		}
		return true
	})
	return calls, bound
}

// appendedToQueryArgs returns the identifiers this file appends to a query
// argument slice, i.e. the locals that legitimately carry a bound value.
func appendedToQueryArgs(file *ast.File, argSlices map[string]bool) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isAppendToQueryArgs(call, argSlices) {
			return true
		}
		for _, arg := range call.Args[1:] {
			if id, ok := arg.(*ast.Ident); ok {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}

// isAppendToQueryArgs reports whether n is append(<query arg slice>, ...).
func isAppendToQueryArgs(n ast.Node, argSlices map[string]bool) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) < 2 {
		return false
	}
	fn, ok := call.Fun.(*ast.Ident)
	if !ok || fn.Name != "append" {
		return false
	}
	dst, ok := call.Args[0].(*ast.Ident)
	return ok && argSlices[dst.Name]
}
