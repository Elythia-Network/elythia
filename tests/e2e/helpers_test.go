package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// userToken は signup で得られるユーザー情報。
// TS版 utils.ts の signup() 戻り値に対応する。
type userToken struct {
	ID       string
	Username string
	Token    string
}

// signup は /api/admin/accounts/create を呼んでユーザーを作成する。
// 初回呼び出しは初期セットアップ (rootUser) として扱われ認証不要。
// 2回目以降は最初に作ったユーザー (admin) のトークンが必要。
func signup(t *testing.T, username string, admin *userToken) *userToken {
	t.Helper()
	params := map[string]any{
		"username": username,
		"password": "test",
	}
	if admin != nil {
		params["i"] = admin.Token
	}
	resp := apiPost(t, "admin/accounts/create", params)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"signup %s failed: status %d", username, resp.StatusCode)

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

// apiPost は POST /api/<path> を呼ぶ。TS版 api(endpoint, params, user?) に対応。
func apiPost(t *testing.T, path string, params map[string]any) *http.Response {
	t.Helper()
	// nil の map は `null` に marshal される。本家の endpoint は body が object で
	// ないと ajv の #/type で 400 にするので (mk-go も同じ、#3330)、本家の
	// test/utils.ts の api() と同じく「引数なし」は `{}` で送る。
	if params == nil {
		params = map[string]any{}
	}
	body, err := json.Marshal(params)
	require.NoError(t, err)

	url := baseURL + "/api/" + path
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// httpGet は GET リクエストを送る。TS版 relativeFetch(path) に対応。
func httpGet(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := http.Get(baseURL + "/" + path)
	require.NoError(t, err)
	return resp
}

// httpFetch は任意のメソッド・ヘッダーでリクエストを送る。
// TS版 relativeFetch(path, {method, headers}) に対応。
func httpFetch(t *testing.T, method, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, baseURL+"/"+path, nil)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// readJSON はレスポンスボディをmap[string]anyとして読み取る。
func readJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal(data, &result), "body: %s", string(data))
	return result
}

// resetDB は /api/reset-db を呼んでDBをリセットする。
func resetDB(t *testing.T) {
	t.Helper()
	resp := apiPost(t, "reset-db", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		require.Failf(t, "reset-db failed", "status=%d body=%s", resp.StatusCode, string(body))
	}
}
