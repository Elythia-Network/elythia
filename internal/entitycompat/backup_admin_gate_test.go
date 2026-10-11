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
	// 画面からのリストア (#3463 (6c)) は daemon へ転送し、再認証を含む全条件を daemon が
	// 照合する。本体で backupGuard を付けると、同じ TOTP のコードを本体と daemon で
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

func TestBackupAdminRoutesAreGuarded(t *testing.T) {
	routes := parseRouteRegistrations(t, filepath.Join("..", "server", "router.go"))
	var found []string
	for ep, raw := range routes {
		if !strings.HasPrefix(ep, "admin/backup/") {
			continue
		}
		found = append(found, ep)
		reg := stripGoComments(raw)
		if _, ok := backupRoutesWithoutAdmin[ep]; ok {
			assert.NotContains(t, reg, "backupGuard", "%s", ep)
			continue
		}
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
	for ep := range backupRoutesWithoutAdmin {
		assert.Contains(t, found, ep, "backupRoutesWithoutAdmin の %s は実在しない", ep)
		assert.Contains(t, backupRoutesWithoutReauth, ep, "%s は再認証も daemon に任せる", ep)
	}
	assertWired(t, routerGo, "backupGuard := backupHandler.Guard()",
		"admin/backup/* の再認証が無い guard になる")
}
