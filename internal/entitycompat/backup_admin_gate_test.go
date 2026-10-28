package entitycompat

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DB のバックアップの管理画面 (#3462) は、閲覧を含む全ての操作に強い認証
// (管理者・native token・2FA の登録・操作ごとの再認証) を課す。再認証は handler の
// 中の guarded (strongauth) が見る (#3878。route の middleware だと引数の検証より先に
// 走るため)。handler で呼び忘れると、管理者の token だけでバックアップ (利用者の
// 秘密鍵・token・パスワードの hash を含む) を落とせるようになる。build もテストも
// 通るので、internal/server の route と adminbackup の handler のソースを読んで確かめる。
//
// reauth-challenge だけは、パスキーの再認証の challenge を出す前段なので
// 再認証を課さない (handler の中で、再認証以外の条件を見る)。
var backupRoutesWithoutReauth = map[string]string{
	"admin/backup/reauth-challenge": "パスキーの challenge を出すだけで、何も見せず何も変えない。再認証以外の条件は handler の BeginPasskey が見る",
	// 画面からのリストア (#3463 (6c)) は daemon へ転送し、再認証を含む全条件を daemon が
	// 照合する。本体でも再認証すると、同じ TOTP のコードを本体と daemon で
	// 二重に消費して必ず落ちる。
	"admin/backup/restore/prepare": "daemon (internal/backup/daemon の restore_api.go) が DBVerifier で再認証する",
	"admin/backup/restore/confirm": "daemon が ticket と DBVerifier の再認証を見る",
	"admin/backup/restore/abort":   "daemon が ticket と DBVerifier の再認証を見る",
	"admin/backup/restore/status":  "daemon が ticket と同じセッションかだけを見る (DB を引かない)",
}

// backupRoutesWithoutAdmin are the admin/backup/* routes that do not go
// through the server's authentication, with where they are checked instead.
var backupRoutesWithoutAdmin = map[string]string{
	// 名前を入れ替えた後、戻した DB にセッションが無いと本体の認証が 401 で断る。
	"admin/backup/restore/status": "daemon が準備のときの ticket とセッションの hash で照合する (restore_api.go の ticketRecord)",
}

// backupHandlerMethod finds the adminbackup.Handler method a route
// registration passes.
var backupHandlerMethod = regexp.MustCompile(`backupHandler\.(\w+)`)

// guardedBackupMethods returns, for each method of adminbackup.Handler
// (receiver *Handler), whether its body calls h.guarded (the
// re-authentication).
func guardedBackupMethods(t *testing.T) map[string]bool {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "internal", "api", "adminbackup")
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Body == nil {
				continue
			}
			// 同じ名前のメソッドを別の型 (Forwarder など) が持っても取り違えない。
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); !ok || id.Name != "Handler" {
				continue
			}
			guarded := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "guarded" {
						guarded = true
					}
				}
				return true
			})
			out[fn.Name.Name] = guarded
		}
	}
	return out
}

func TestBackupAdminRoutesAreGuarded(t *testing.T) {
	routes := parseRouteRegistrations(t, serverSourcePaths(t)...)
	methods := guardedBackupMethods(t)
	// 抽出が空振りすると全部緑になるので、実在するものを名指しで要求する。
	require.True(t, methods["List"], "adminbackup の Handler.List が h.guarded を呼ばない (または読めない)")
	var found []string
	for ep, raw := range routes {
		if !strings.HasPrefix(ep, "admin/backup/") {
			continue
		}
		found = append(found, ep)
		reg := stripGoComments(raw)
		// 再認証は route の middleware に付けない。付けると引数の検証より先に走り、
		// 形の誤った本文でも TOTP のコードを使ってから 400 になる (#3878)。
		assert.NotContains(t, reg, "Guard", "%s: 再認証は route でなく handler の中で行う", ep)
		m := backupHandlerMethod.FindStringSubmatch(reg)
		if !assert.NotNil(t, m, "%s: handler が backupHandler.<Method> の形でない", ep) {
			continue
		}
		guarded, known := methods[m[1]]
		require.True(t, known, "%s: adminbackup に Handler.%s が無い", ep, m[1])
		if _, ok := backupRoutesWithoutAdmin[ep]; ok {
			assert.False(t, guarded, "%s", ep)
			continue
		}
		assert.Contains(t, reg, "middleware.RequireAdmin(", "%s: 管理者だけにする", ep)
		assert.Contains(t, reg, "middleware.RequireSecure()", "%s: native token だけにする", ep)
		if _, ok := backupRoutesWithoutReauth[ep]; ok {
			assert.False(t, guarded, "%s: 再認証の前段に再認証を課すと使えなくなる", ep)
			continue
		}
		assert.True(t, guarded, "%s: Handler.%s が再認証 (h.guarded) を呼ばない", ep, m[1])
	}
	sort.Strings(found)
	// 抽出が空振りすると全部緑になるので、実在するものを名指しで要求する。
	for _, want := range []string{"admin/backup/list", "admin/backup/download", "admin/backup/delete", "admin/backup/reauth-challenge"} {
		require.Contains(t, found, want, "internal/server から admin/backup/* を読めない")
	}
	for ep := range backupRoutesWithoutReauth {
		assert.Contains(t, found, ep, "backupRoutesWithoutReauth の %s は実在しない", ep)
	}
	for ep := range backupRoutesWithoutAdmin {
		assert.Contains(t, found, ep, "backupRoutesWithoutAdmin の %s は実在しない", ep)
		assert.Contains(t, backupRoutesWithoutReauth, ep, "%s は再認証も daemon に任せる", ep)
	}
}
