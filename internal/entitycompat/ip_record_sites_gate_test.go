package entitycompat

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 「利用者の IP を記録する呼び出しはここだけ」という不変条件を固定する (#3135)。
//
// **これは関連アカウント検索 (#3105) の前提。** あちらの関連度は `user_ip` の観測
// だけを見るので、**失敗したサインインの IP がここに入ると第三者が他人の関連候補を
// 作れる** — 攻撃者が対象アカウントの ID で自分の IP から失敗を繰り返せば、その IP が
// 対象の「使用した IP」として記録され、攻撃者自身のアカウントが候補に並ぶ。
// #3066 が完了条件に挙げているのはこの攻撃面。
//
// **振る舞いテストでは守れない。** 守りたいのは「どこからも呼ばれていない」という
// 構造的な性質で、endpoint ごとのテストは叩いた経路しか見ない。実際、`/api/signin`
// だけを叩くテストは `SigninFlow` (同梱フロントが実際に使うほう) に記録を足す変異を
// 素通りさせた (実測)。非同期化 (`go h.ipRecorder.Record(...)`) も振る舞い側からは
// 不安定にしか捕まらない (実測で `-race` 5 回中 3 回)。
//
// **allowlist には「なぜ許されるか」を書く。** 2 引数の `.Record(` は IP 記録とは
// 限らない (配送健全性など) ので、増えたときに人が判断できるようにする。
//
// **射程**: 見るのは `internal/` だけ (`cmd/` / `tools/` / `plugin/` に 2 引数の
// `.Record(` は 0 件)。**`user_ip` に実際に書くのは `UserIPRepository.Observe`
// (3 引数) で、そちらは見ていない** — 呼ぶのは `internal/core/iplog` の 1 箇所だが、
// 引数の数で絞るこの走査には入らない。
func TestIPRecordCallSitesAreAllowlisted(t *testing.T) {
	root := filepath.Join(repoRoot(t), "internal")
	sites := scanRecordRefs(t, root)

	found := map[string]bool{}
	for _, s := range sites {
		found[s.key()] = true
	}

	// **抽出が壊れたことを、実在する call site の名指しで見る。** 「件数の下限」だと
	// (a) allowlist の長さと比べる形は dead-entry 検査より論理的に弱く、(b) call site を
	// 正当に 1 つ消しただけでも「抽出が壊れている」と**事実と逆の診断**を出す (実測)。
	// **`assert` で回す。** `require` だと最初の 1 件で止まり、下の dead-entry 検査の
	// 正確な診断 (「allowlist の %q が実在しない」) に到達しない (実測)。
	for _, want := range mustDetectRecordSites {
		assert.Truef(t, found[want], "%s を拾えていない。抽出が壊れているか、call site が移動した", want)
	}

	// **拾ってはいけない形も固定する。** 引数の数が違う呼び出しと、`Record` という
	// 名前のフィールドへのアクセスは call site ではない。
	for _, notWant := range []string{
		"internal/entitycompat/recordfixture/sample.go#CallOneArg",
		"internal/entitycompat/recordfixture/sample.go#FieldAccess",
	} {
		assert.Falsef(t, found[notWant], "%s を call site として拾っている (偽陽性)", notWant)
	}

	for _, s := range sites {
		reason, ok := ipRecordAllowlist[s.key()]
		assert.Truef(t, ok,
			"%s の %s に利用者の IP を記録しうる `Record` がある。**それが失敗したサインインの"+
				"経路から届いていないか確認すること** (#3135)。IP と無関係な Record なら "+
				"allowlist に理由付きで足す。",
			s.File, s.Func)
		if ok {
			assert.NotEmptyf(t, reason, "%s の allowlist に理由が無い", s.key())
		}
	}

	// allowlist の死んだ entry も落とす (移動・削除に気付けるように)。
	for key := range ipRecordAllowlist {
		assert.Truef(t, found[key], "allowlist の %q が実在しない。移動したか消えた", key)
	}
}

// mustDetectRecordSites は「走査が生きていれば必ず見つかるはずの call site」。
//
// allowlist と同じ中身を別に持つのは意図的で、**allowlist から entry を消すだけでは
// この要求が消えない**ようにするため (`secretfield-check` の `mustDetectSecretFields`
// と同じ形)。
var mustDetectRecordSites = []string{
	"internal/api/signin/handler.go#RecordSuccessfulSignin",
	"internal/api/signin/passkey.go#finishPasskeySignin",
	"internal/core/deliveryhealth/service.go#RecordDelivery",
	"internal/server/middleware/client_ip.go#RecordClientIP",
	"internal/entitycompat/recordfixture/sample.go#CallTwoArgs",
	"internal/entitycompat/recordfixture/sample.go#MethodValue",
	"internal/entitycompat/recordfixture/sample.go#var Closure",
}

// ipRecordAllowlist は 2 引数の `.Record(` を許す場所と、その理由。
var ipRecordAllowlist = map[string]string{
	"internal/api/signin/handler.go#RecordSuccessfulSignin": "" +
		"password / TOTP / backup code / WebAuthn(2FA) が通る成功側の共通入口。" +
		"失敗経路 `fail()` からは呼ばれない — `fail()` が触るのは `signins` テーブル (`recordSignin`) " +
		"だけで、IP recorder にも `RecordSuccessfulSignin` にも届かない。",
	"internal/api/signin/passkey.go#finishPasskeySignin": "" +
		"パスキーの成功経路。`RecordSuccessfulSignin` を経由せず直接呼ぶ — あちらと違って " +
		"レスポンスを `{signinResponse: ...}` で包み、`loginNotifier` と新規ログイン通知メールを " +
		"発火しない (この非対称は #3135 の範囲外の既存挙動)。**passwordless 無効の `fail()` より " +
		"後ろにあること**を下の順序テストが固定する。",
	"internal/server/middleware/client_ip.go#RecordClientIP": "" +
		"認証済みリクエストの IP を記録する middleware。**認証を通った後でしか動かない** " +
		"(未認証は `TestRecordClientIP_SkipsAnonymous` が固定)。",
	"internal/core/deliveryhealth/service.go#RecordDelivery": "" +
		"IP とは無関係。配送先ホストごとの成否を記録する (`RecordDelivery` の中の `Record(host, outcome)`)。",

	// --- 走査の枝を固定する人工ソース ---
	//
	// **実データには「メソッド値として持ち出す」形も「package 変数のクロージャの中で
	// 呼ぶ」形も無い**ので、そこを拾わない実装にしても実データからは何も起こらない。
	// 枝そのものをここで固定する (`secretfield-check` と同じ形)。
	"internal/entitycompat/recordfixture/sample.go#CallTwoArgs": "人工ソース: 素直な 2 引数の呼び出し。",
	"internal/entitycompat/recordfixture/sample.go#MethodValue": "人工ソース: メソッド値として持ち出す形。",
	"internal/entitycompat/recordfixture/sample.go#var Closure": "人工ソース: package 変数のクロージャの中の呼び出し。",
}

// 成功側の共通入口を呼ぶ場所も固定する。
//
// **`.Record(` の列挙だけでは塞がらない。** 失敗経路から allowlist 済みの
// `RecordSuccessfulSignin` を呼べば、記録は許可済みの関数の中で起きるので 2 引数の
// `.Record(` は増えない。実測でその変異は静的ゲートも振る舞いテストも素通りした。
func TestRecordSuccessfulSigninCallSitesAreAllowlisted(t *testing.T) {
	root := filepath.Join(repoRoot(t), "internal")
	// 宣言そのもの (`func (h *Handler) RecordSuccessfulSignin`) は呼び出しでは
	// ないので、走査は呼び出しだけを返す。
	sites := scanCallSites(t, root, "RecordSuccessfulSignin", 0)

	found := map[string]bool{}
	for _, s := range sites {
		found[s.key()] = true
	}
	for want := range recordSuccessfulSigninCallSites {
		require.Truef(t, found[want], "%s を拾えていない。抽出が壊れているか、call site が移動した", want)
	}
	for _, s := range sites {
		reason, ok := recordSuccessfulSigninCallSites[s.key()]
		assert.Truef(t, ok,
			"%s の %s が `RecordSuccessfulSignin` を呼んでいる。**サインインが本当に成功した"+
				"経路か確認すること** (#3135)。",
			s.File, s.Func)
		if ok {
			assert.NotEmptyf(t, reason, "%s の allowlist に理由が無い", s.key())
		}
	}
}

var recordSuccessfulSigninCallSites = map[string]string{
	"internal/api/signin/handler.go#ok": "" +
		"サインイン成功のレスポンスを組む関数。ここに来る時点で認証は通っている。",
	"internal/api/signup/handler.go#fireSigninSideEffects": "" +
		"新規登録直後の自動サインイン。作ったばかりの本人なので成功と同じ扱いでよい。",
}

// パスキーの記録が「失敗を返し切った後」にあることを固定する。
//
// **call site の列挙だけでは足りない。** `Record` を `fail()` より前へ動かしても
// 場所は変わらないので allowlist は通る。実際にその変異は endpoint の振る舞い
// テストも素通りした (実測)。passwordless 無効の利用者に対してパスキーの署名検証を
// 通した時点で記録してしまうと、**本人でない誰かが試した IP** が入りうる。
//
// **最初の `Record` を基準にする。** 最後のものを見る形だと、既存の呼び出しを残した
// まま `fail()` より前にもう 1 つ**足す**変異が素通りする (実測)。
func TestPasskeyIPRecordComesAfterFailures(t *testing.T) {
	path := filepath.Join(repoRoot(t), "internal/api/signin/passkey.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	require.NoError(t, err)

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "finishPasskeySignin" {
			fn = fd
			break
		}
	}
	require.NotNil(t, fn, "finishPasskeySignin が見つからない (rename した?)")

	var lastFail, firstRecord token.Pos
	noteRecord := func(pos token.Pos) {
		if firstRecord == token.NoPos || pos < firstRecord {
			firstRecord = pos
		}
	}
	called := map[ast.Node]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := unparen(call.Fun).(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "fail":
			if call.Pos() > lastFail {
				lastFail = call.Pos()
			}
		case "Record":
			called[sel] = true
			if len(call.Args) == 2 {
				noteRecord(call.Pos())
			}
		}
		return true
	})
	// **メソッド値も候補にする。** 呼び出しの形だけを見ると、`f := h.ipRecorder.Record`
	// と書いて前に置く変異が素通りする (実測)。
	skip := nonValueSelectors(fn.Body)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Record" && !called[sel] && !skip[sel] {
			noteRecord(sel.Pos())
		}
		return true
	})

	require.NotEqual(t, token.NoPos, lastFail, "`fail(` を 1 つも拾えていない (検査が空振りしている)")
	require.NotEqual(t, token.NoPos, firstRecord, "IP を記録する `Record(` を拾えていない")
	assert.Greaterf(t, int(firstRecord), int(lastFail),
		"IP の記録 (%s) と `fail(` (%s) の順序が逆。**失敗を返し切ってから記録すること** — "+
			"記録を前へ動かしたか、記録より後ろに失敗分岐を足したかのどちらか。",
		fset.Position(firstRecord), fset.Position(lastFail))
}

type recordSite struct {
	File string // repo 相対
	Func string
}

func (s recordSite) key() string { return s.File + "#" + s.Func }

// scanRecordRefs は `X.Record(a, b)` の呼び出しと、**呼び出さずに値として持ち出した
// `X.Record`** を、それを含む宣言ごとに拾う。
func scanRecordRefs(t *testing.T, root string) []recordSite {
	t.Helper()
	return scanCallSites(t, root, "Record", 2)
}

// scanCallSites は `X.<name>(...)` の呼び出しと、呼び出さずに値として持ち出した
// `X.<name>` を、それを含む宣言ごとに拾う。`wantArgs` が正なら引数の数で絞る。
//
// **引数の数だけで絞らない。** 監査ログの `Record(iplookuplog.Entry{...})` は 1 引数
// なので入らないが、`rec := h.ipRecorder.Record; rec(a, b)` と書くと呼び出し側が
// `*ast.Ident` になり、引数 2 の `.Record(` としては現れない。**署名検証より前に
// 呼ばれる `resolvePasskeyUser` (今の `resolvePasskeyKey`) にその形を仕込む変異が、静的ゲートも振る舞いテストも
// 素通りした** (実測)。メソッド値として持ち出す形も call site として数える。
//
// **型の位置とフィールドアクセスは除く。** `a.Record{}` / `var r a.Record` /
// `h.Record.Val` のような正当な書き方まで call site にすると、診断が事実と無関係な
// ことを断定する。いまの `internal/` に該当は 0 件だが、`Record` は一般的な語なので
// 踏むのは時間の問題。
//
// 型は見ていないので IP 以外の 2 引数 Record も拾うが、それは allowlist に理由を
// 書いて分ける (`internal/core/deliveryhealth` が該当)。
func scanCallSites(t *testing.T, root, name string, wantArgs int) []recordSite {
	t.Helper()
	repo := repoRoot(t)
	var sites []recordSite

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// `testdata` は Go ツールチェーンが無視する場所で、コンパイルできない
			// Go を置ける。parse させると無関係な理由でこのゲートが落ちる。
			if d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, rerr := filepath.Rel(repo, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)

		eachDecl(f, func(owner string, n ast.Node) {
			skip := nonValueSelectors(n)
			called := map[ast.Node]bool{}
			ast.Inspect(n, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := unparen(call.Fun).(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != name {
					return true
				}
				called[sel] = true
				if wantArgs <= 0 || len(call.Args) == wantArgs {
					sites = append(sites, recordSite{File: rel, Func: owner})
				}
				return true
			})
			ast.Inspect(n, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != name || called[sel] || skip[sel] {
					return true
				}
				// 呼び出さずに参照している = メソッド値として持ち出している。
				sites = append(sites, recordSite{File: rel, Func: owner})
				return true
			})
		})
		return nil
	})
	require.NoError(t, err, "walk %s", root)

	sort.Slice(sites, func(i, j int) bool { return sites[i].key() < sites[j].key() })
	return sites
}

// nonValueSelectors marks selector expressions that are not values: 型の位置、
// 別のセレクタの土台、代入の左辺。
func nonValueSelectors(root ast.Node) map[ast.Node]bool {
	skip := map[ast.Node]bool{}
	mark := func(e ast.Expr) {
		for {
			switch x := e.(type) {
			case *ast.StarExpr:
				e = x.X
			case *ast.ArrayType:
				e = x.Elt
			case *ast.Ellipsis:
				e = x.Elt
			default:
				if sel, ok := e.(*ast.SelectorExpr); ok {
					skip[sel] = true
				}
				return
			}
		}
	}
	ast.Inspect(root, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			// `h.Record.Val` の土台側 (`h.Record`) は最終的なフィールド名ではない。
			mark(x.X)
		case *ast.CompositeLit:
			mark(x.Type)
		case *ast.TypeAssertExpr:
			mark(x.Type)
		case *ast.ValueSpec:
			mark(x.Type)
		case *ast.Field:
			mark(x.Type)
		case *ast.MapType:
			mark(x.Key)
			mark(x.Value)
		case *ast.ChanType:
			mark(x.Value)
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				mark(lhs)
			}
		}
		return true
	})
	return skip
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// eachDecl calls fn for every top-level declaration, naming it.
//
// **`FuncDecl` だけを見ない。** package 変数に入れたクロージャの中の呼び出しが
// 見えなくなる (実測でその形が静的ゲートを素通りした)。
func eachDecl(f *ast.File, fn func(owner string, n ast.Node)) {
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				fn(d.Name.Name, d.Body)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				name := "var"
				if len(vs.Names) > 0 {
					name = "var " + vs.Names[0].Name
				}
				fn(name, vs)
			}
		}
	}
}
