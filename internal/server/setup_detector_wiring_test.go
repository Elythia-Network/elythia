package server

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/testutil"
)

// **初回セットアップの判定に使う利用者の数え方が、`requireSetup`を返す5つの
// 経路すべてへ渡ること(#3782)。**
//
// 以前は`meta.SetLocalUserCounter`でパッケージの変数を書き換えていたので、
// 1回の配線で全部に効いた。組み立てで作った値を部品ごとに渡す形にしたので、
// 1つ渡し忘れるとその経路だけ「`rootUserId`だけで判定する」に戻り、利用者が
// 居るのにセットアップ画面を出す(#3037で直した不具合の再来)。`rootUserId`を
// 外し、ローカルの利用者を1人置いた状態で、各経路の`requireSetup`がfalseに
// なることを見る。
//
// 利用者を一度見たという印は、組み立てで作った`SetupDetector`の値が持つ(#3803)。
// 5つの経路は同じ値を共有するので、先に見た経路の観測で後の経路も数えずにfalseに
// なるが、それで配線の漏れは隠れない。渡し忘れた経路はnilか別の値を持ち、その値には
// 印が無いので、`rootUserId`だけで判定してtrueを返す。
func TestNewServer_WiresTheSetupDetectorToEveryMetaEmbedding(t *testing.T) {
	db := serverIntegrationDB
	if db == nil {
		t.Skip("PostgreSQL unavailable")
	}
	var root struct{ RootUserID *string }
	require.NoError(t, db.Raw(`SELECT "rootUserId" AS root_user_id FROM "meta" LIMIT 1`).Scan(&root).Error)
	t.Cleanup(func() {
		require.NoError(t, db.Exec(`UPDATE "meta" SET "rootUserId" = ?`, root.RootUserID).Error)
	})
	require.NoError(t, db.Exec(`UPDATE "meta" SET "rootUserId" = NULL`).Error)
	cleanupPluginAccountUsers(t, db, "sd_user")
	createLocalUser(t, db, "sd-user", "sd_user", "sdusertokenx3782", "sd-pass", false)

	srv := newServerWithConfig(t, func(*config.Config) {})

	// OAuthの同意画面は、client_idのページからredirect_uriを読めたときだけ出る。
	client := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, `<html><head><link rel="redirect_uri" href="/callback"></head><body></body></html>`)
	}))
	t.Cleanup(client.Close)
	authorize := "/oauth/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {client.URL + "/"},
		"redirect_uri":          {client.URL + "/callback"},
		"scope":                 {"read:account"},
		"code_challenge":        {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"},
		"code_challenge_method": {"S256"},
	}.Encode()

	embedded := regexp.MustCompile(`(?s)id="misskey_meta"[^>]*>(.*?)</script>`)
	fromHTML := func(t *testing.T, path string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		m := embedded.FindStringSubmatch(rec.Body.String())
		require.Len(t, m, 2, "misskey_meta must be embedded: %s", rec.Body.String())
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(html.UnescapeString(m[1])), &out))
		return out
	}

	for _, tt := range []struct {
		name string
		meta func(t *testing.T) map[string]any
	}{
		{name: "api/meta", meta: func(t *testing.T) map[string]any {
			rec := postJSON(t, srv, "/api/meta", "", map[string]any{})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var out map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
			return out
		}},
		{name: "SPA shell", meta: func(t *testing.T) map[string]any { return fromHTML(t, "/") }},
		{name: "permalink SSR", meta: func(t *testing.T) map[string]any { return fromHTML(t, "/@sd_user") }},
		{name: "embed", meta: func(t *testing.T) map[string]any { return fromHTML(t, "/embed/user-timeline/sd-user") }},
		{name: "OAuth consent", meta: func(t *testing.T) map[string]any { return fromHTML(t, authorize) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.meta(t)
			require.Contains(t, got, "requireSetup")
			assert.Equal(t, false, got["requireSetup"], "rootUserIdだけで判定している(数え方が渡っていない)")
		})
	}
}

// **組み立て直した世代は、前の世代の「利用者を見た」印を持ち越さない(#3803)。**
//
// 以前は印がパッケージの変数だったので、利用者の居るDBで一度観測すると、利用者の
// 居ないDBへ戻して組み立て直した後も、数えずにセットアップ画面を出さなかった。
// 最初の管理者を作る窓(`admin/accounts/create`)は開いているのに、画面が出ない。
//
// 利用者0の状態を作るので、パッケージの共有schemaではなく専用のschemaを使う。
func TestNewServer_SetupLatchDoesNotOutliveTheGeneration(t *testing.T) {
	db, err := testutil.OpenTestDBSchema("setupgen")
	if err != nil {
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	testutil.ApplyMigrations(db)
	reset := func() {
		require.NoError(t, db.Exec(`TRUNCATE "user" CASCADE`).Error)
		require.NoError(t, db.Exec(`DELETE FROM "used_username"`).Error)
		require.NoError(t, db.Exec(`UPDATE "meta" SET "rootUserId" = NULL`).Error)
	}
	reset()
	t.Cleanup(reset)

	requireSetup := func(h *pluginAccountsHarness) bool {
		t.Helper()
		rec := h.do(http.MethodPost, "/api/meta", "", map[string]any{})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var m map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))
		v, ok := m["requireSetup"].(bool)
		require.True(t, ok, "requireSetup must be a bool: %v", m["requireSetup"])
		return v
	}

	createLocalUser(t, db, "sg-user", "sg_user", "sgusertokenx3803", "sg-pass", false)
	first := newPluginAccountsHarnessDB(t, db, nil)
	require.False(t, requireSetup(first), "利用者が居るのでセットアップ画面を出さない (前提)")

	// 利用者の居ないDBへ戻し、組み立て直した。
	reset()
	second := newPluginAccountsHarnessDB(t, db, nil)
	assert.True(t, requireSetup(second), "前の世代の印を持ち越している")
}
