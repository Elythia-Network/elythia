package admin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// condExplanations maps every leaf condition to the wording that explains it in
// `selfGrantableRoleMessage`.
//
// **この文面は判定より 2 回遅れている (#3037 / #3045)。** どちらも「判定に
// 条件が足された」のに文面が古い一覧のまま残り、管理者は**自分が使っていない
// 条件の一覧**を読まされた。定数の直上のコメントが 1 回目を記録しているのに
// 2 回目が起きたので、doc コメントではなくテストで止める。
//
// **識別子で書けないものがあるので対応表を持つ。** フォロー数 / 投稿数の
// 4 + 2 種は管理画面でも日本語のラベルなので、文面に `followersLessThanOrEq`
// とは書かない。型が増えたらこの表と文面の両方を足すことになる。
var condExplanations = map[role.CondFormulaType]string{
	role.CondTypeIsLocal:         "isLocal",
	role.CondTypeIsRemote:        "isRemote",
	role.CondTypeIsSuspended:     "isSuspended",
	role.CondTypeIsLocked:        "isLocked",
	role.CondTypeIsBot:           "isBot",
	role.CondTypeIsCat:           "isCat",
	role.CondTypeIsExplorable:    "isExplorable",
	role.CondTypeCreatedLessThan: "createdLessThan",
	role.CondTypeCreatedMoreThan: "createdMoreThan",
	role.CondTypeRoleAssignedTo:  "roleAssignedTo",
	// **`followers` と `following` は別のラベル。** upstream の ja-JP は
	// `フォロワー数が～以下` / `フォロー数が～以下` で、混ぜると片方が
	// もう片方の語で「説明済み」になり、その型の検査が空虚になる。
	role.CondTypeFollowersLessThanOrEq: "フォロワー数",
	role.CondTypeFollowersMoreThanOrEq: "フォロワー数",
	role.CondTypeFollowingLessThanOrEq: "フォロー数",
	role.CondTypeFollowingMoreThanOrEq: "フォロー数",
	role.CondTypeNotesLessThanOrEq:     "投稿数",
	role.CondTypeNotesMoreThanOrEq:     "投稿数",
}

// **拒否されうる条件は全部、文面で説明すること (#3045)。**
//
// 管理画面はこの 400 の本文しか出さないので、ここに無い条件で弾かれた管理者は
// 何を直せばよいか分からない。**葉の一覧は `internal/core/role` のソースから
// 読む** — テスト側に第 2 の一覧を置くと、それ自体が同期の対象になる。
func TestSelfGrantableRoleMessageExplainsEveryLeafCondition(t *testing.T) {
	leaves := declaredLeafCondTypes(t)
	// 抽出が空振りすると「検査していないのに緑」になる。upstream の
	// `RoleService.evalCond` は葉 16 種 (+ and / or / not)。
	require.GreaterOrEqual(t, len(leaves), 16, "cond_formula.go から拾った葉の型")
	for _, typ := range leaves {
		t.Run(string(typ), func(t *testing.T) {
			phrase, ok := condExplanations[typ]
			require.True(t, ok, "対応表に %s が無い。文面と両方に足すこと", typ)
			assert.Contains(t, selfGrantableRoleMessage, phrase,
				"%s を説明する語が 400 の本文に無い", typ)
		})
	}
	// 対応表に死んだ entry が残らないようにする (型が消えたときに気付く)。
	declared := map[role.CondFormulaType]bool{}
	for _, typ := range leaves {
		declared[typ] = true
	}
	for typ := range condExplanations {
		assert.True(t, declared[typ], "対応表の %s はもう宣言されていない", typ)
	}
}

// declaredLeafCondTypes reads the CondFormulaType constants out of the role
// package source, dropping the three composites that `condSatisfiable` folds.
func declaredLeafCondTypes(t *testing.T) []role.CondFormulaType {
	t.Helper()
	const src = "../../core/role/cond_formula.go"
	file, err := parser.ParseFile(token.NewFileSet(), src, nil, 0)
	require.NoError(t, err, src)
	var out []role.CondFormulaType
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if ident, ok := vs.Type.(*ast.Ident); !ok || ident.Name != "CondFormulaType" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				switch typ := role.CondFormulaType(unquoted); typ {
				case role.CondTypeAnd, role.CondTypeOr, role.CondTypeNot:
				default:
					out = append(out, typ)
				}
			}
		}
	}
	return out
}
