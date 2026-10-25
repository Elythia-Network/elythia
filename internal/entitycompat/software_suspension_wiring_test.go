package entitycompat

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"testing"
)

// **配送と受信の queue の processor と、連合の resolver が、それぞれ自分の設定を読む判定を
// 受け取っていること (#3829、resolver は #3833)。**
//
// 配送停止 (`meta.deliverSuspendedSoftware`、#3558) の判定は
// `corefederation.NewSuspendedChecker`、受信の拒否 (`meta.inboxSuspendedSoftware`、#3362) の
// 判定は `corefederation.NewInboxSuspendedChecker` で作り、どちらも同じ型
// `*corefederation.SuspendedChecker` を返す (読む meta の列は中の関数で分かれる)。
// 構築子の必須の引数にしたので渡し忘れはコンパイルで、nil は
// `TestBuiltHandlersHaveNoNilDependencies` で落ちるが、**取り違えは型でも nil の検査でも
// 捕まらない**。取り違えると、管理画面の配送停止が受信に、受信の拒否が配送に効き、
// どちらもエラーもログも出ない。構築子の呼び出しを引数まで照合する。
func TestQueueProcessorSoftwareCheckersAreWired(t *testing.T) {
	assertWired(t, serverPkg,
		"deliverProcessor := processors.NewDeliverProcessor(apClient, instanceService, "+
			"processors.NewRepoSigningKeySource(keypairRepo, keypairExtraRepo), "+
			"corefederation.NewSuspendedChecker(metaRepo, instanceRepo), "+
			"metadataRefresher, corefollowing.MetaInstanceStatsGate(metaRepo))",
		"配送の processor が配送停止 (deliverSuspendedSoftware) の判定を受け取っていない。\n"+
			"管理画面で配送停止にしたソフトウェアへ配送し続ける (#3558)")
	// 受信側は buildFederation で 1 回だけ作る inboxSoftwareChecker
	// (NewInboxSuspendedChecker) を渡す。
	assertWired(t, serverPkg,
		"inboxProcessor := processors.NewInboxProcessor(federationProcessor, federationResolver, "+
			"instanceService, inboxSoftwareChecker, metadataRefresher, "+
			"processors.NewRedisInboxReplayGuard(s.redis.Default))",
		"受信の processor が受信の拒否 (inboxSuspendedSoftware) の判定を受け取っていない。\n"+
			"管理画面で受信を拒否したソフトウェアの activity を受け付ける (#3362)")
	// resolver も受信の拒否の判定を受ける (#3563)。#3833 で構築子の必須の引数にした。
	assertWired(t, serverPkg,
		"federationResolver := corefederation.NewResolver(userRepo, noteRepo, apURLs, apFetcher, idGen, "+
			"instanceService, inboxSoftwareChecker, instanceService, roleService, suspensionOriginRepo, s.background)",
		"resolver が受信の拒否 (inboxSuspendedSoftware) の判定を受け取っていない。\n"+
			"受信を拒否したソフトウェアのホストの投稿を、リノートや返信の連鎖、ap/show で取り込む (#3563)")
	assertInboxSoftwareCheckerAssignments(t)
}

// inboxSoftwareCheckerSources are the only right-hand sides allowed for the
// variable inboxSoftwareChecker in internal/server.
//
// buildFederation が受信の拒否の判定を 1 回だけ作り、setupRoutes と buildHandlers は
// その別名を置く。どちらの形も 1 つ以上あることを下限として要求する。
var inboxSoftwareCheckerSources = []string{
	"corefederation.NewInboxSuspendedChecker(metaRepo, instanceRepo)",
	"fedSvcs.inboxSoftwareChecker",
}

// assertInboxSoftwareCheckerAssignments fails when any assignment to
// inboxSoftwareChecker in internal/server has another right-hand side.
//
// 上の照合は、2 つの構築子の呼び出し (inbox へ渡す変数の名前まで) しか見ない。別名を
// `inboxSoftwareChecker := corefederation.NewSuspendedChecker(...)` で作り直したり、後から
// 代入し直したりすると、呼び出しは残ったまま受信が配送停止の設定を読むようになる。
// 見るのは変数への代入だけで、構造体のフィールド (`fedSvcs.inboxSoftwareChecker = …`、
// 構築子のリテラルのキー) を経由した取り違えは見ない (既知の限界)。
// **ファイルを名指ししない (#3672)。** 別名は router.go と wiring_handlers.go にあり、
// 組み立てを別のファイルへ移しても落ちないよう、パッケージ全体の代入を見る。
func assertInboxSoftwareCheckerAssignments(t *testing.T) {
	t.Helper()
	const name = "inboxSoftwareChecker"
	allowed := map[string]int{}
	for _, src := range inboxSoftwareCheckerSources {
		allowed[src] = 0
	}
	render := func(n ast.Node) string {
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, token.NewFileSet(), n); err != nil {
			t.Fatalf("print: %v", err)
		}
		return normalizeWiring(buf.String())
	}
	fset := token.NewFileSet()
	check := func(lhs []ast.Expr, names []*ast.Ident, rhs []ast.Expr) {
		idents := names
		for _, e := range lhs {
			if id, ok := e.(*ast.Ident); ok {
				idents = append(idents, id)
			}
		}
		for i, id := range idents {
			if id.Name != name {
				continue
			}
			got := "(多値の代入)"
			if len(rhs) == len(idents) {
				got = render(rhs[i])
			} else if len(rhs) == 0 {
				got = "(値なしの宣言)"
			}
			if _, ok := allowed[got]; !ok {
				t.Errorf("%s: %s の右辺が %q。受信の拒否の判定 (%v) 以外を入れると、"+
					"受信が別の設定を読む (#3362)", fset.Position(id.Pos()), name, got, inboxSoftwareCheckerSources)
				continue
			}
			allowed[got]++
		}
	}
	for _, s := range serverSources(t) {
		f, err := parser.ParseFile(fset, s.rel, s.src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", s.rel, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				check(n.Lhs, nil, n.Rhs)
			case *ast.ValueSpec:
				check(nil, n.Names, n.Values)
			}
			return true
		})
	}
	// **何も拾えなかったら落とす (CLAUDE.md Section 4)。**
	for src, n := range allowed {
		if n == 0 {
			t.Errorf("internal/server に `%s := %s` が 1 つも無い。書式が変わって拾えていないか、"+
				"組み立てが変わったなら inboxSoftwareCheckerSources を直すこと", name, src)
		}
	}
}
