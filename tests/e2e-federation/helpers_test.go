package e2e_federation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// userToken は signup で得られるユーザー情報。
type userToken struct {
	ID       string
	Username string
	Token    string
}

// signup は指定サーバーに /api/admin/accounts/create を呼んでユーザーを作成する。
// 初回呼び出しは初期セットアップ (rootUser) として扱われ認証不要。
// 2回目以降は admin のトークンが必要。
func signup(t *testing.T, srv *testServer, username string, admin *userToken) *userToken {
	t.Helper()
	params := map[string]any{
		"username": username,
		"password": "test",
	}
	if admin != nil {
		params["i"] = admin.Token
	}
	resp := srvAPIPost(t, srv, "admin/accounts/create", params)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"signup %s on %s failed: status %d", username, srv.Host, resp.StatusCode)

	var result struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Token    string `json:"token"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	return &userToken{
		ID:       result.ID,
		Username: result.Username,
		Token:    result.Token,
	}
}

// resetDB は指定サーバーの /api/reset-db を呼んでDBをリセットする。
//
// reset-db は meta テーブルを TRUNCATE して default 値で再 INSERT するので
// federation = 'none' (全 host deny) に戻ってしまう。e2e_federation は
// localhost↔localhost で federation を駆動するので 'all' (制限なし) に
// 上書きしてから返す (#780)。
func resetDB(t *testing.T, srv *testServer) {
	t.Helper()
	resp := srvAPIPost(t, srv, "reset-db", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		require.Failf(t, "reset-db failed", "server=%s status=%d body=%s",
			srv.Host, resp.StatusCode, string(body))
	}
	// reset-db 後に federation を relax する。CachedMetaRepository は
	// EnsureInitial で cache invalidate されるので、次の Fetch で 'all' を
	// 拾える。
	db := dbAGlobal
	if srv == serverB {
		db = dbBGlobal
	}
	require.NoError(t, db.Exec(`UPDATE meta SET "federation" = 'all'`).Error)
}

// srvAPIPost は POST /api/<path> を指定サーバーに送る。
func srvAPIPost(t *testing.T, srv *testServer, path string, params map[string]any) *http.Response {
	t.Helper()
	// nil の map は `null` に marshal される。本家の endpoint は body が object で
	// ないと ajv の #/type で 400 にするので (mk-go も同じ、#3330)、本家の
	// test/utils.ts の api() と同じく「引数なし」は `{}` で送る。
	if params == nil {
		params = map[string]any{}
	}
	body, err := json.Marshal(params)
	require.NoError(t, err)

	url := srv.BaseURL + "/api/" + path
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// srvAPICall は srvAPIPost の結果をJSONで返す便利ヘルパー。
func srvAPICall(t *testing.T, srv *testServer, path string, params map[string]any) map[string]any {
	t.Helper()
	resp := srvAPIPost(t, srv, path, params)
	defer resp.Body.Close()
	require.True(t, resp.StatusCode >= 200 && resp.StatusCode < 300,
		"expected 2xx from %s /api/%s, got %d", srv.Host, path, resp.StatusCode)
	return readJSON(t, resp)
}

// readJSON はレスポンスボディをmap[string]anyとして読み取る。
func readJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal(data, &result), "body: %s", string(data))
	return result
}

// resolveRemoteUser は fromUser のトークンで onServer に POST /api/ap/show を呼び、
// リモートユーザーを解決する。TS版 resolveRemoteUser(host, id, from) に対応。
func resolveRemoteUser(t *testing.T, onServer *testServer, remoteUserURI string, fromUser *userToken) map[string]any {
	t.Helper()
	result := srvAPICall(t, onServer, "ap/show", map[string]any{
		"i":   fromUser.Token,
		"uri": remoteUserURI,
	})
	require.Equal(t, "User", result["type"],
		"expected User type, got %v for URI %s", result["type"], remoteUserURI)
	obj, ok := result["object"].(map[string]any)
	require.True(t, ok, "object should be a map")
	return obj
}

// resolveRemoteNote は fromUser のトークンで onServer に POST /api/ap/show を呼び、
// リモートノートを解決する。TS版 resolveRemoteNote(host, id, from) に対応。
func resolveRemoteNote(t *testing.T, onServer *testServer, remoteNoteURI string, fromUser *userToken) map[string]any {
	t.Helper()
	result := srvAPICall(t, onServer, "ap/show", map[string]any{
		"i":   fromUser.Token,
		"uri": remoteNoteURI,
	})
	require.Equal(t, "Note", result["type"],
		"expected Note type, got %v for URI %s", result["type"], remoteNoteURI)
	obj, ok := result["object"].(map[string]any)
	require.True(t, ok, "object should be a map")
	return obj
}

// userURI はユーザーのActivityPub URIを構築する。
func userURI(srv *testServer, userID string) string {
	return fmt.Sprintf("%s/users/%s", srv.BaseURL, userID)
}

// noteURI はノートのActivityPub URIを構築する。
func noteURI(srv *testServer, noteID string) string {
	return fmt.Sprintf("%s/notes/%s", srv.BaseURL, noteID)
}
