package entitycompat

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DB のバックアップの管理画面 (#3462) は、閲覧を含む全ての操作に強い認証
// (管理者・native token・2FA の登録・操作ごとの再認証) を課す。再認証は
// backupGuard (strongauth) が見るので、route に付け忘れると、管理者の token
// だけでバックアップ (利用者の秘密鍵・token・パスワードの hash を含む) を
// 落とせるようになる。build もテストも通るので、router.go を読んで確かめる。
//
// reauth-challenge だけは、パスキーの再認証の challenge を出す前段なので
// backupGuard を付けない (handler の中で、再認証以外の条件を見る)。
var backupRoutesWithoutReauth = map[string]string{
	"admin/backup/reauth-challenge": "パスキーの challenge を出すだけで、何も見せず何も変えない。再認証以外の条件は handler の BeginPasskey が見る",
}

func TestBackupAdminRoutesAreGuarded(t *testing.T) {
	routes := parseRouteRegistrations(t, filepath.Join("..", "server", "router.go"))
	var found []string
	for ep, raw := range routes {
		if !strings.HasPrefix(ep, "admin/backup/") {
			continue
		}
		found = append(found, ep)
		reg := stripGoComments(raw)
		assert.Contains(t, reg, "middleware.RequireAdmin(", "%s: 管理者だけにする", ep)
		assert.Contains(t, reg, "middleware.RequireSecure()", "%s: native token だけにする", ep)
		if _, ok := backupRoutesWithoutReauth[ep]; ok {
			assert.NotContains(t, reg, "backupGuard", "%s: 再認証の前段に再認証を課すと使えなくなる", ep)
			continue
		}
		assert.Contains(t, reg, "backupGuard", "%s: 再認証 (backupGuard) が無い", ep)
	}
	sort.Strings(found)
	// 抽出が空振りすると全部緑になるので、実在するものを名指しで要求する。
	for _, want := range []string{"admin/backup/list", "admin/backup/download", "admin/backup/delete", "admin/backup/reauth-challenge"} {
		require.Contains(t, found, want, "router.go から admin/backup/* を読めない")
	}
	for ep := range backupRoutesWithoutReauth {
		assert.Contains(t, found, ep, "backupRoutesWithoutReauth の %s は実在しない", ep)
	}
	assertWired(t, routerGo, "backupGuard := backupHandler.Guard()",
		"admin/backup/* の再認証が無い guard になる")
}
