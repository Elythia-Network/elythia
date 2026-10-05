package entitycompat

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `admin/ip/accounts` (#3104) は router が検索用 repository を渡さないと動かない。
// handler は未配線を 500 で返すので**黙って「候補なし」にはならない**が、
// `internal/server` は CI のカバレッジ対象外 (CLAUDE.md Section 4) で router を
// 組み立てるテストも無いため、配線を落としても build もテストも全部緑になる。
// #2762 (`WireMetaToggles`) / #3053 と同じ形で router.go の呼び出しを固定する。
func TestIPAccountSearchRepoIsWired(t *testing.T) {
	assertWired(t, routerGo,
		"adminHandler.SetIPSearchRepo(repository.NewUserIPSearchRepository(s.db))",
		"admin/ip/accounts が 500 を返し、IP からの関連アカウント検索が使えなくなる (#3104)")
}

// route の登録と**権限の 3 段**を固定する。
//
// **`perm-check` は見てくれない。** あちらは upstream の golden に載っている
// endpoint だけを突き合わせるので、mk-go 独自の endpoint は「実装されていない」
// 扱いで黙って skip される。つまりこの endpoint の認証段は、ここでしか
// 固定されていない。
//
// **`RequireRolePolicy` の 1 行が落ちると、既定でモデレーター全員に開く。**
// upstream の `admin/get-user-ips` は `requireAdmin: true` なので、それは同じ
// 機密情報に対して既存より緩い経路を黙って新設することになる (#3104)。しかも
// 落ちても build は通り、モデレーターから見ると「使えるようになった」だけで
// 誰も異常に気付かない。引数まで照合するので、policy 名を別のものに差し替える
// 形も落ちる。
func TestIPAccountSearchRouteIsRegistered(t *testing.T) {
	assertWired(t, routerGo,
		`api.POST("/admin/ip/accounts", adminHandler.IPAccounts, `+
			`middleware.RequireModerator(roleService), `+
			`middleware.RequireRolePolicy(roleService, corerole.PolicyCanSearchIPHistory), `+
			`middleware.RequireScope("read:admin:user-ips"))`,
		"admin/ip/accounts が 404 になる、あるいは管理者限定の既定が外れて\n"+
			"モデレーター全員が IP から関連アカウントを引けるようになる (#3104)")
}

// 関連アカウント候補 (#3105) も同じ 3 段で守る。
//
// **`admin/ip/accounts` のゲートでは捕まらない。** あれは route ごとの文字列を
// 照合するので、新しい route を足しても既存のゲートは何も言わない。扱うのは同じ
// 「利用者 ↔ IP の対応」なので、認証段が片方だけ緩むと**緩いほうから同じ情報が
// 全部引ける**。
func TestIPRelatedAccountsRouteIsRegistered(t *testing.T) {
	assertWired(t, routerGo,
		`api.POST("/admin/ip/related-accounts", adminHandler.IPRelatedAccounts, `+
			`middleware.RequireModerator(roleService), `+
			`middleware.RequireRolePolicy(roleService, corerole.PolicyCanSearchIPHistory), `+
			`middleware.RequireScope("read:admin:user-ips"))`,
		"admin/ip/related-accounts が 404 になる、あるいは管理者限定の既定が外れて\n"+
			"モデレーター全員が関連アカウント候補を引けるようになる (#3105)")
}

// IP 照会の監査 (#3106) は router が配線しないと残らない。
//
// **未配線でも照会は成立する。** 記録されないだけなので、画面からも API からも
// 異常に見えない。起動時の critical wiring 検査にも入れてあるが、そちらは
// `HasIPLookupAudit()` の戻り値を見るだけなので、**配線そのものを落とす変更**は
// ここで止める。
func TestIPLookupAuditIsWired(t *testing.T) {
	assertWired(t, routerGo,
		"adminHandler.SetIPLookupAudit(iplookuplog.NewService(ipLookupLogRepo, idGen))",
		"IP 照会が監査に残らない (照会そのものは成立するので誰も気付けない、#3106)")
	assertWired(t, routerGo,
		"adminHandler.SetIPLookupLogRepo(ipLookupLogRepo)",
		"admin/ip/lookup-log が 500 を返し、監査記録を読めなくなる (#3106)")
}

// **監査記録にも保持期間を掛ける。** この行が無いと記録が永久に残り、
// `moderation_log` に IP を書くのと変わらなくなる — 専用テーブルにした意味が消える。
func TestIPLookupLogRetentionIsWired(t *testing.T) {
	assertWired(t, routerGo,
		"cleanGenericProcessor.SetIPLookupLogPruner(repository.NewIPLookupLogRepository(s.db))",
		"IP 照会の監査記録が永久に残る (照会に使った IP がそのまま入る、#3106)")
}

// 監査記録の読み出しも照会と同じ 3 段で守る。
//
// **この応答自体が機密。** 照会に使った IP がそのまま入るので、ここが緩むと
// 「IP を引く権限は無いが、誰がどの IP を引いたかは読める」状態になる。
func TestIPLookupLogRouteIsRegistered(t *testing.T) {
	assertWired(t, routerGo,
		`api.POST("/admin/ip/lookup-log", adminHandler.IPLookupLog, `+
			`middleware.RequireModerator(roleService), `+
			`middleware.RequireRolePolicy(roleService, corerole.PolicyCanSearchIPHistory), `+
			`middleware.RequireScope("read:admin:user-ips"))`,
		"admin/ip/lookup-log が 404 になる、あるいは認証段が緩んで\n"+
			"照会の権限が無い相手に「誰がどの IP を引いたか」が読めるようになる (#3106)")
}

// IP を返す endpoint の path が、レート制限表のキーと**同じ綴りで**登録されて
// いること (#3106)。
//
// **limiter は未登録のキーを素通しする。** キーは `strings.TrimPrefix(c.Path(),
// "/api/")` で引くだけなので、router.go 側で path を rename すると**上限が
// 黙って消える** — 表にキーは残るので誰も気付かない。
//
// `internal/server/middleware` 側のテストは表のキーと値を固定するが、それは
// path の文字列を自分で書いているので、router.go との対応までは見ていない。
// **2 つの一覧を突き合わせるのはここだけ。**
func TestIPLookupRoutesHaveRateLimits(t *testing.T) {
	registered := registeredAPIPostPaths(t)
	// router.go に登録されている path (`/api` は Echo の group が付ける)。
	routes := []string{
		"/admin/ip/accounts",
		"/admin/ip/related-accounts",
		"/admin/ip/lookup-log",
		// upstream の口。返すのは同じ「利用者 ↔ IP の対応」。
		"/admin/get-user-ips",
	}
	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			require.True(t, registered[route],
				"%s が router.go に登録されていない、あるいは path が変わっている。\n"+
					"rename したなら、この一覧と DefaultEndpointLimits のキーも一緒に直すこと (#3106)", route)
			key := strings.TrimPrefix(route, "/")
			limit, ok := middleware.DefaultEndpointLimits[key]
			require.True(t, ok,
				"%s に対応する上限が DefaultEndpointLimits に無い。\n"+
					"limiter は未登録のキーを素通しするので、上限そのものを迂回できる (#3106)", route)
			assert.Positive(t, limit.Max, "%s の上限が 0 で、実質無制限", route)
			// **窓も見る。** `Duration: 0` は limiter が数えないので、Max だけ
			// 見ていると実質無制限に戻せる (敵対的レビュー 2 周目で実測)。
			assert.Positive(t, limit.Duration, "%s の窓が 0 で、実質無制限", route)
			// **IP bucket を見ない設定であること。** limiter は権限検査より前に
			// 走るので、両方見ると同じ出口 IP から未認証で叩くだけで正当な
			// モデレーターを締め出せる (#3106)。
			assert.True(t, limit.UserBucketOnly, "%s が IP bucket も消費する", route)
		})
	}
}

// registeredAPIPostPaths collects the literal paths of `api.POST("...", ...)`
// calls in router.go.
//
// **コメントは数えない** (go/parser が落とす)。**1 つも拾えなかったら落とす** —
// 書式が変わって空振りすると、検査していないのに緑になる。
func registeredAPIPostPaths(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", "..", routerGo), nil, 0)
	require.NoError(t, err)
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "POST" {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "api" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		path, err := strconv.Unquote(lit.Value)
		if err == nil {
			out[path] = true
		}
		return true
	})
	require.NotEmpty(t, out, "router.go から api.POST の path を 1 つも拾えていない")
	return out
}
