package entitycompat

import (
	"go/ast"
	"testing"
)

// users/show の関係と /api/i の未読は、reader を配線したときだけ 1 文で引く
// (#3608)。配線が無くても応答は同じ (表ごとに引く経路に戻る) なので、**行を
// 消しても build もテストも通り、黙って 10 本 / 5 本に戻る**。ここで固定する。
func TestRelationSingleStatementReadersAreWired(t *testing.T) {
	assertWired(t, serverPkg, "usersHandler.SetUserRelationReader(userRelationRepo)",
		"users/show の関係が 1 文でなく 10 本の同時の問い合わせに戻る (#3608)")
	assertWired(t, serverPkg, "iHandler.SetMeUnreadFlagsReader(repository.NewMeUnreadFlagsRepository(s.db))",
		"/api/i の未読の真偽値が 1 文でなく 5 本の問い合わせに戻る (#3608)")
	assertWired(t, serverPkg, "userRelationRepo := repository.NewUserRelationRepository(s.db)",
		"users/show などの関係の reader が作られない (#3608)")
	// 上の構築の行は buildRepos の中にある (#3684)。setupRoutes が使うのは別名なので、
	// 構築から別名までの 3 段も照合する。上の行だけだと、setupRoutes の別名を nil に
	// 書き換えても緑のまま通る (#3684 のレビューで実測)。
	assertWired(t, serverPkg, "userRelation: userRelationRepo",
		"buildRepos が作った関係の reader を repos に入れていない (#3608)")
	// wiring のリテラル全体ではなく、repos のキーだけを照合する。後の段階で wiring に
	// フィールドを足す書き方 (リテラルに足す / w.core = ... と後から入れる) のどちらでも当たる。
	assertWired(t, serverPkg, "repos: s.buildRepos()",
		"setupRoutes が buildRepos の repository を使っていない (#3608)")
	assertWired(t, serverPkg, "userRelationRepo := w.repos.userRelation",
		"setupRoutes の関係の reader が buildRepos で作ったものでなくなる (#3608)")
}

// internal/server で組み立てる userrelation.Repos は、すべて Relation を持つこと。
//
// 1 箇所だけ Relation を書き忘れると、その経路 (blocking/create・ap/show など)
// だけが 10 本に戻る。文字列の照合では「どれか 1 つにある」しか言えないので、
// 複合リテラルを 1 つずつ見る。抽出が空振りして緑になるのを防ぐため、今ある
// 3 箇所を下限として要求する。
func TestEveryRouterRelationReposHasRelation(t *testing.T) {
	// internal/server の全ファイルを読む (#3672)。報告はファイル名と行の組で出る。
	fset, files := parseServerFiles(t, 0)
	found := 0
	inspectServerFiles(files, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Repos" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "userrelation" {
			return true
		}
		found++
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Relation" {
				if isNilish(kv.Value) {
					break
				}
				return true
			}
		}
		t.Errorf("%s: userrelation.Repos に Relation が無い。この経路だけ関係を 10 本で引くことになる (#3608)",
			fset.Position(lit.Pos()))
		return true
	})
	if found < 3 {
		t.Fatalf("%s の userrelation.Repos の複合リテラルが %d 個しか見つからない (3 個以上あるはず)。"+
			"書き方を変えたならこの gate も直すこと", serverPkg, found)
	}
}

// isNilish reports whether e is nil, or nil passed as the only argument of a
// call such as the conversion repository.UserRelationReader(nil). interface
// への変換で書いた nil は配線が無いのと同じで、黙って 10 本に戻る。ポインタ型
// の nil を変換したものは `r.Relation != nil` を通ってしまい、呼んだ時点で
// panic する。引数 1 つの呼び出しに nil を渡す形は、変換でも constructor でも
// 配線として意味が無いので、まとめて落とす。
func isNilish(e ast.Expr) bool {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			break
		}
		e = p.X
	}
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name == "nil"
	case *ast.CallExpr:
		return len(v.Args) == 1 && isNilish(v.Args[0])
	}
	return false
}
