package entitycompat

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **`kind` を宣言できない経路に第三者アプリのトークンを入れない (#3037)。**
//
// upstream の `ApiCallService.ts:412-413` は「`kind` が無く、かつ資格情報を
// 要する endpoint」に対し app token を**一律で拒否**する。mk-go は
// `RequireScope` を route ごとに配線する形なので、この後半の規則にあたるのが
// `RequireSecure` (native token だけ通す) と `RejectAppToken` (app token だけ
// 落とす) の 2 つ。
//
// **どちらも無い credential route は、scope を一度も見ない。** つまり
// `read:account` しか許可していないアプリのトークンでも到達できる。実際に
// `emoji-application/*` の 3 つがその状態だった。
//
// `internal/server` は CI のカバレッジ対象外で router を組み立てるテストも
// 無いので、新しい route を足しても build もテストも緑のまま抜ける。
func TestCredentialRoutesWithoutScopeRejectAppTokens(t *testing.T) {
	// **同 package の括弧対応パーサを使う (#3037 レビュー)。** パーサを 2 本
	// 持つこと自体がドリフト源。**そのパーサ自身がコメント中の括弧を数えて
	// いたのはレビュー 3 周目で直した** — 括弧入りのコメントを 1 行足すだけで
	// 後続 route を飲み込み、その route の middleware が手前の判定に混ざる。
	routes := parseRouteRegistrations(t, serverSourcePaths(t)...)
	require.Greater(t, len(routes), 400, "route の抽出が壊れている")
	require.Contains(t, routes, "i/change-password")
	assert.Contains(t, routes["i/change-password"], "RequireSecure", "middleware を拾えていない")

	var missing []string
	for ep, raw := range routes {
		// **コメントは数えない (#3037 レビュー / #2856 と同型)。**
		// `// middleware.RejectAppToken(),` と書くとコンパイルは通り、
		// app token が handler に到達するのに、文字列一致だと gate は緑のまま。
		reg := stripGoComments(raw)

		// **upstream の規則は `requireCredential || requireModerator ||
		// requireAdmin` (`ApiCallService.ts:413`)。** `RequireAuth` だけを見ると、
		// `RequireModerator` / `RequireAdmin` だけで資格情報を要求している
		// route (実測 95 本) が視界に入らない。
		if !strings.Contains(reg, "RequireAuth") &&
			!strings.Contains(reg, "RequireModerator") &&
			!strings.Contains(reg, "RequireAdmin") &&
			!strings.Contains(reg, "RequireRolePolicy(") {
			continue
		}
		if strings.Contains(reg, "RequireScope") ||
			strings.Contains(reg, "RequireSecure") ||
			strings.Contains(reg, "RejectAppToken") {
			continue
		}
		if _, ok := appTokenGateExempt["/"+ep]; ok {
			continue
		}
		missing = append(missing, "/"+ep)
	}
	sort.Strings(missing)
	assert.Empty(t, missing,
		"scope を見ない credential route がある。`RequireScope` / `RequireSecure` / `RejectAppToken` のいずれかを配線するか、"+
			"理由を添えて appTokenGateExempt に登録すること")
}

// appTokenGateExempt lists credential routes that may be reached with an app
// token, with the reason.
//
// **理由には「upstream がどう扱うか」を書く。** 「見たけど大丈夫だった」では
// 次に読む人が判断をやり直せない。
var appTokenGateExempt = map[string]string{}

// **除外の一覧が腐らないようにする。** 実在しない path が残ると、その行が
// 何を守っているのか分からなくなる。gate を後から付けた route が残っていても
// 同じなので、そちらも落とす (#3037 レビュー)。
func TestAppTokenGateExemptHasNoDeadEntries(t *testing.T) {
	routes := parseRouteRegistrations(t, serverSourcePaths(t)...)
	for path, reason := range appTokenGateExempt {
		raw, ok := routes[strings.TrimPrefix(path, "/")]
		assert.True(t, ok, "appTokenGateExempt の %q が internal/server に無い", path)
		assert.NotEmpty(t, reason, "%q に理由が無い", path)

		reg := stripGoComments(raw)
		assert.False(t,
			strings.Contains(reg, "RequireScope") || strings.Contains(reg, "RequireSecure") ||
				strings.Contains(reg, "RejectAppToken"),
			"%q は既に gate を持っている。appTokenGateExempt から外すこと", path)
	}
}

// stripGoComments removes // and /* */ comments, leaving string and rune
// literals alone.
//
// **文字列の中は触らない。** route の path には `//` を含む URL が入りうる
// (`https://…`)。素朴に `//` 以降を落とすと、その行の残りの middleware まで
// 消えて偽陽性になる。
//
// **rune literal も同じ扱いにする (#3037 レビュー 2 周目)。** シングル
// クォートを見ていなかったので、`'"'` や '`' のような rune literal が
// **文字列モードを開いたまま以降を verbatim にコピー**していた。実測で
// `middleware.Sep('"') /* middleware.RejectAppToken() */` を router.go に
// 置くと、コメントアウトされた middleware が生きていると判定される
// (= 1 周目で塞いだ形が rune literal 1 つで戻る)。
func stripGoComments(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); i++ {
		switch {
		case src[i] == '"' || src[i] == '`' || src[i] == '\'':
			quote := src[i]
			b.WriteByte(src[i])
			for i++; i < len(src); i++ {
				b.WriteByte(src[i])
				if src[i] == '\\' && quote != '`' && i+1 < len(src) {
					i++
					b.WriteByte(src[i])
					continue
				}
				if src[i] == quote {
					break
				}
			}
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i++
			b.WriteByte(' ')
		default:
			b.WriteByte(src[i])
		}
	}
	return b.String()
}

// **コメント落としが文字列を壊さないこと。**
func TestStripGoComments(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   string
		want string
	}{
		{"行コメント", "a // b\nc", "a \nc"},
		{"ブロックコメント", "a /* b */ c", "a   c"},
		{"文字列の中の //", `x("https://e.test")`, `x("https://e.test")`},
		{"文字列の中の /*", "x(`a /* b`)", "x(`a /* b`)"},
		{"エスケープされた引用符", `x("a\"// b")`, `x("a\"// b")`},
		// **rune literal (#3037 レビュー 2 周目)。** シングルクォートを
		// 見ていないと、これらが文字列モードを開いたまま以降を verbatim に
		// コピーし、**コメント落としが丸ごと無効になる**。
		{"rune の二重引用符", `x('"') // c`, `x('"') ` + "\n"},
		{"rune のバッククォート", "x('`') // c", "x('`') \n"},
		{"rune のスラッシュ", `x('/') /* c */`, `x('/')  `},
		{"エスケープされた rune", `x('\'') // c`, `x('\'') ` + "\n"},
		{"rune のバックスラッシュ", `x('\\') // c`, `x('\\') ` + "\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, stripGoComments(tt.in))
		})
	}
}
