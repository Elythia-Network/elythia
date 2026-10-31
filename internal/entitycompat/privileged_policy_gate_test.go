package entitycompat

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **`/admin/*` を policy だけで開ける key の一覧が router と一致すること (#3037)。**
//
// `internal/core/role` の `privilegedPolicyKeys` は「条件つきロールがこの
// policy を配るなら、自己付与できる条件と組み合わせてはいけない」という判定に
// 使う。router 側で `RequireRolePolicy` だけで `/admin/*` を開ける policy が
// 増えたのに一覧へ足し忘れると、**その policy 経由の自己付与が無警告で通る**。
//
// `internal/server` はカバレッジ対象外で router を組み立てるテストも無いので、
// この片側更新は build もテストも緑のまま起きる。
func TestPrivilegedPolicyKeysMatchAdminRoutes(t *testing.T) {
	routes := parseRouteRegistrations(t, serverSourcePaths(t)...)
	require.Greater(t, len(routes), 400, "route の抽出が壊れている")

	// **`RequireRolePolicy` の引数そのものを読む (#3037 レビュー 2 周目)。**
	// 1 周目は `corerole.PolicyXxx` という書き方だけを正規表現で拾っていたが、
	// あの引数は `policyKey string` なので**リテラルを直接書いてもコンパイル
	// は通る**。実測で `RequireRolePolicy(roleService, "canManageZzDanger")`
	// と `zzPolicy := corerole.PolicyCanManageZzDanger` 経由の 2 形が素通りした。
	fromRouter := map[string]struct{}{}
	var unresolved []string
	for ep, raw := range routes {
		if !strings.HasPrefix(ep, "admin/") {
			continue
		}
		reg := stripGoComments(raw)
		// **moderator / admin を併用している route は対象外。** そちらは
		// policy が無くてもロールで守られるので、policy 単独では開かない。
		if strings.Contains(reg, "RequireModerator") || strings.Contains(reg, "RequireAdmin") {
			continue
		}
		for _, arg := range requireRolePolicyArgs(reg) {
			switch {
			case policyConstRe.MatchString(arg):
				fromRouter[policyConstValue(t, policyConstRe.FindStringSubmatch(arg)[1])] = struct{}{}
			case policyLiteralRe.MatchString(arg):
				fromRouter[policyLiteralRe.FindStringSubmatch(arg)[1]] = struct{}{}
			default:
				// **読めない形は落とす。** 変数を経由されると、この gate は
				// その route を黙って視界から外す = 検査していないのに緑になる。
				unresolved = append(unresolved, ep+": "+arg)
			}
		}
	}
	sort.Strings(unresolved)
	require.Empty(t, unresolved,
		"`RequireRolePolicy` の policy を定数かリテラルで書いていない。"+
			"変数を経由するとこの gate がその route を検査できない")
	require.NotEmpty(t, fromRouter, "policy だけで開く admin route を 1 つも拾えていない")

	declared := privilegedPolicyKeysFromSource(t)
	var missing, stale []string
	for value := range fromRouter {
		if _, ok := declared[value]; !ok {
			missing = append(missing, value)
		}
	}
	for value := range declared {
		if _, ok := fromRouter[value]; !ok {
			stale = append(stale, value)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)

	assert.Empty(t, missing,
		"policy だけで `/admin/*` を開ける key が `privilegedPolicyKeys` に無い。"+
			"足さないと、その policy を配る条件つきロールで自己付与できる")
	assert.Empty(t, stale,
		"`privilegedPolicyKeys` に、もう admin を開けない key が残っている")
}

var (
	policyConstRe   = regexp.MustCompile(`^\s*corerole\.(Policy\w+)\s*$`)
	policyLiteralRe = regexp.MustCompile(`^\s*"([^"]+)"\s*$`)
	// RequireRolePolicyPublic (匿名も base の policy で評価する形。users/search) も
	// policy で守る route なので一緒に読む。
	requireRolePolicyRe = regexp.MustCompile(`RequireRolePolicy(Public)?\(`)
)

// requireRolePolicyArgs returns the policy argument of every
// RequireRolePolicy / RequireRolePolicyPublic call in the registration text.
func requireRolePolicyArgs(reg string) []string {
	var out []string
	for _, c := range requireRolePolicyCalls(reg) {
		out = append(out, c.arg)
	}
	return out
}

// rolePolicyCall is one RequireRolePolicy / RequireRolePolicyPublic call.
type rolePolicyCall struct {
	// public is true for RequireRolePolicyPublic, which admits anonymous
	// callers.
	public bool
	arg    string
}

// requireRolePolicyCalls returns every RequireRolePolicy /
// RequireRolePolicyPublic call in the registration text with its policy
// argument.
//
// 引数は `(checker, policyKey)` の 2 つなので、括弧の深さを見て最後の 1 つを取る。
func requireRolePolicyCalls(reg string) []rolePolicyCall {
	var out []rolePolicyCall
	for _, loc := range requireRolePolicyRe.FindAllStringSubmatchIndex(reg, -1) {
		public := loc[2] >= 0
		depth := 1
		start := loc[1]
		last := start
		for i := start; i < len(reg); i++ {
			switch reg[i] {
			case '(', '[', '{':
				depth++
			case ')', ']', '}':
				depth--
				if depth == 0 {
					out = append(out, rolePolicyCall{public: public, arg: reg[last:i]})
					i = len(reg)
				}
			case ',':
				if depth == 1 {
					last = i + 1
				}
			}
			if depth == 0 {
				break
			}
		}
	}
	return out
}

// privilegedPolicyKeysFromSource reads the map literal from role_service.go.
//
// **ソースとして読む。** `internal/core/role` を import すると、あちらが
// `internal/entitycompat` のテスト専用依存を引き込む形になるため。
func privilegedPolicyKeysFromSource(t *testing.T) map[string]struct{} {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "core", "role", "role_service.go"))
	require.NoError(t, err)

	// **終端は行頭の `}`。** 素の `}` だと最初の entry の `{}` で切れて、
	// 1 件しか読めないまま「足りない」と誤報する。
	body := betweenMarkers(t, string(src), "var privilegedPolicyKeys = map[string]struct{}{", "\n}")
	out := map[string]struct{}{}
	for _, m := range regexp.MustCompile(`"([^"]+)":`).FindAllStringSubmatch(body, -1) {
		out[m[1]] = struct{}{}
	}
	require.NotEmpty(t, out, "privilegedPolicyKeys を読めていない")
	return out
}

// policyConstValue resolves `PolicyFoo` to its string literal.
func policyConstValue(t *testing.T, constName string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "core", "role", "role_service.go"))
	require.NoError(t, err)
	m := regexp.MustCompile(constName + `\s*=\s*"([^"]+)"`).FindStringSubmatch(string(src))
	require.NotNil(t, m, "%s の値を読めない", constName)
	return m[1]
}

func betweenMarkers(t *testing.T, src, start, end string) string {
	t.Helper()
	i := strings.Index(src, start)
	require.GreaterOrEqual(t, i, 0, "開始マーカー %q が無い", start)
	rest := src[i+len(start):]
	j := strings.Index(rest, end)
	require.GreaterOrEqual(t, j, 0, "終了マーカーが無い")
	return rest[:j]
}
