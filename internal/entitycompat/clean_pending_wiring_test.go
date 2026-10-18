package entitycompat

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pendingPrunerParamType is the parameter type the pruner is passed as.
const pendingPrunerParamType = "PendingSignupPruner"

// pendingPrunerConstructor is the only expression that may fill it.
const pendingPrunerConstructor = "repository.NewUserPendingRepository"

// **期限切れ `user_pending` の掃除が配線されていること (#3037 レビュー)。**
//
// `NewCleanProcessor` の該当引数に `nil` を渡しても**コンパイルは通り**、
// `if p.pending != nil` で掃除が黙って止まる。`internal/server` は CI の
// カバレッジ対象外で router を組み立てるテストも無いので、build もテストも
// 緑のまま巻き戻せる。同じ PR が `captchaSvc` に AST gate を足したのと
// 完全に同じ理由。
//
// **「最後の引数がリテラルの `nil` でない」では足りない (レビュー 2 周目)。**
// 実測で 3 形が素通りした:
//
//   - `var p processors.PendingSignupPruner` を宣言して渡す (nil interface)
//   - `repository.UserPendingRepository(nil)` と型付き nil へ変換する
//   - **`NewCleanProcessor` に引数を 1 つ足す** — 位置で最後を見ているので、
//     gate は黙って**別の引数**を検査するようになる。これは普通の進化なので、
//     いちばん起きやすい
//
// そこで (a) 引数の位置を**定義側の signature から引き**、(b) そこに
// `repository.NewUserPendingRepository(...)` の呼び出しがあることまで見る。
func TestCleanProcessorReceivesThePendingPruner(t *testing.T) {
	fset := token.NewFileSet()

	// (a) 定義側から「何番目の引数か」と「引数の総数」を引く。
	def, err := parser.ParseFile(fset,
		filepath.Join("..", "queue", "processors", "clean.go"), nil, parser.ParseComments)
	require.NoError(t, err)

	idx, total := pendingPrunerParamIndex(def)
	require.NotEqual(t, -2, idx,
		"NewCleanProcessor に %s の引数が 2 つ以上ある。どれが掃除に使われるか"+
			"この gate では判定できないので、型を分けること", pendingPrunerParamType)
	require.GreaterOrEqual(t, idx, 0,
		"NewCleanProcessor に %s の引数が無い (rename した?)", pendingPrunerParamType)

	// (b) 呼び出し側がその位置に constructor を渡していること。
	// internal/server の全ファイルを読む (#3672)。
	_, files := parseServerFiles(t, parser.ParseComments)

	found := false
	wired := false
	inspectServerFiles(files, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel || sel.Sel.Name != "NewCleanProcessor" {
			return true
		}
		found = true

		// **引数の数が signature と合っていること。** 合っていなければ
		// 位置の対応が崩れているので、検査したことにしない。
		if len(call.Args) != total || idx >= len(call.Args) {
			return true
		}
		if exprString(call.Args[idx]) == pendingPrunerConstructor {
			wired = true
		}
		return true
	})

	require.True(t, found, "internal/server に NewCleanProcessor の呼び出しが無い (rename した?)")
	assert.True(t, wired,
		"NewCleanProcessor の %d 番目の引数に %s(...) を渡していない。"+
			"渡さないと期限切れの登録 (メールアドレスとパスワードハッシュ) が無期限に貯まる",
		idx, pendingPrunerConstructor)
}

// pendingPrunerParamIndex returns the flattened index of the pruner parameter
// and the total parameter count of NewCleanProcessor.
//
// **グループ化された引数 (`a, b T`) を展開する。** 展開しないと位置がずれ、
// 呼び出し側の別の引数を検査してしまう。
func pendingPrunerParamIndex(f *ast.File) (idx int, total int) {
	idx = -1
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "NewCleanProcessor" || fn.Type.Params == nil {
			return true
		}
		pos := 0
		for _, field := range fn.Type.Params.List {
			names := len(field.Names)
			if names == 0 {
				names = 1
			}
			if exprString(field.Type) == pendingPrunerParamType {
				// **同じ型の引数が 2 つあったら判定できない
				// (#3037 レビュー 3 周目)。** 上書きすると末尾が勝つので、
				// `(…, pending PendingSignupPruner, extra PendingSignupPruner)`
				// にして `pending` に nil を渡す形が素通りした (実測)。
				if idx >= 0 {
					idx = -2
				} else {
					idx = pos
				}
			}
			pos += names
		}
		total = pos
		return false
	})
	return idx, total
}

// exprString renders an identifier or a selector as source text.
//
// 呼び出し式なら**呼ばれている関数の名前**を返す
// (`repository.NewUserPendingRepository(s.db)` -> `repository.NewUserPendingRepository`)。
func exprString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok {
			return x.Name + "." + t.Sel.Name
		}
		return t.Sel.Name
	case *ast.CallExpr:
		return exprString(t.Fun)
	case *ast.StarExpr:
		return exprString(t.X)
	}
	return ""
}
