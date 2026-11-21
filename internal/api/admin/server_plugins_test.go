package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callServerPlugins invokes the handler and decodes the response body.
func callServerPlugins(t *testing.T, h *Handler) (int, map[string]json.RawMessage) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/server-plugins", strings.NewReader("{}"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, h.ServerPlugins(c))

	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return rec.Code, body
}

// プラグイン無しは「空」であって「不明」ではないので [] を返す (null にしない)。
func TestServerPlugins_EmptyIsArrays(t *testing.T) {
	h := &Handler{}
	h.SetPluginOrphanSchemas(func(context.Context) ([]string, error) { return nil, nil })

	code, body := callServerPlugins(t, h)
	assert.Equal(t, http.StatusOK, code)
	assert.JSONEq(t, `[]`, string(body["plugins"]))
	assert.JSONEq(t, `[]`, string(body["orphanSchemas"]))
}

func TestServerPlugins_ReturnsSnapshot(t *testing.T) {
	h := &Handler{}
	h.SetServerPlugins([]ServerPluginInfo{{
		Name:       "status",
		Version:    "1.0.0",
		APIVersion: 1,
		Enabled:    true,
		Routes:     true,
		Jobs:       true,
		Migrations: 2,
		Schema:     "plugin_status",
		ConfigKeys: []string{"maxLength"},
	}})
	h.SetPluginOrphanSchemas(func(context.Context) ([]string, error) {
		return []string{"plugin_removed"}, nil
	})

	code, body := callServerPlugins(t, h)
	assert.Equal(t, http.StatusOK, code)

	var plugins []ServerPluginInfo
	require.NoError(t, json.Unmarshal(body["plugins"], &plugins))
	require.Len(t, plugins, 1)
	assert.Equal(t, "status", plugins[0].Name)
	assert.Equal(t, "plugin_status", plugins[0].Schema)
	assert.Equal(t, []string{"maxLength"}, plugins[0].ConfigKeys)
	assert.JSONEq(t, `["plugin_removed"]`, string(body["orphanSchemas"]))
}

// **検査の失敗は null で表す。** 空配列を返すと「確認できていない」のに
// 「残存なし」と読めてしまう。
func TestServerPlugins_OrphanCheckFailureIsNull(t *testing.T) {
	h := &Handler{}
	h.SetServerPlugins([]ServerPluginInfo{{Name: "status"}})
	h.SetPluginOrphanSchemas(func(context.Context) ([]string, error) {
		return nil, errors.New("db down")
	})

	code, body := callServerPlugins(t, h)
	assert.Equal(t, http.StatusOK, code)
	// 一覧そのものは返る (診断の失敗で全体を落とさない)。
	assert.Contains(t, string(body["plugins"]), "status")
	assert.Equal(t, "null", string(body["orphanSchemas"]))
}

// 未配線 (SetPluginOrphanSchemas が呼ばれない構成) でも null になる。
func TestServerPlugins_UnwiredOrphanListerIsNull(t *testing.T) {
	h := &Handler{}

	code, body := callServerPlugins(t, h)
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "null", string(body["orphanSchemas"]))
}
