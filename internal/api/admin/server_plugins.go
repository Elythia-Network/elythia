package admin

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
)

// ServerPluginInfo describes one compiled-in server plugin (#2497).
//
// mk-go 独自の additive エンドポイント用。upstream の「プラグイン」(AiScript
// クライアントプラグイン) とは別物なので、名前空間を server-plugins に分けて
// 衝突の芽を残さない。
type ServerPluginInfo struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	APIVersion int    `json:"apiVersion"`
	// Enabled reflects the runtime config (plugins.<name>.enabled), not the
	// build-time state. ビルドから外れたプラグインはそもそもこの一覧に出ない。
	Enabled bool `json:"enabled"`
	// Routes / Jobs / EffectivePolicies reflect what the plugin declares, not
	// what this process wired. ロール分割 (#2459) で片方しか動かないプロセスでも、
	// プラグインの構成としては同じものを見せる。
	Routes            bool   `json:"routes"`
	Jobs              bool   `json:"jobs"`
	EffectivePolicies bool   `json:"effectivePolicies"`
	Migrations        int    `json:"migrations"`
	Schema            string `json:"schema"`
	// ConfigKeys lists the setting keys only. **値は返さない** — どのキーが
	// 秘密かを mk-go は判別できないので、`elythia config-dump` と同じく既定で全部
	// マスクする方針に合わせる。
	ConfigKeys []string `json:"configKeys"`
	// Secrets lists the secret names the plugin declares (#3470). **値も
	// 設定済みかどうかも返さない** — この一覧はモデレーターにも見えるので、
	// 状態は管理者だけが叩ける `plugin/<name>/_secrets` で返す。
	Secrets []string `json:"secrets"`
}

// PluginOrphanSchemaLister returns plugin schemas that no compiled-in plugin
// owns. 起動時 WARN と同じ情報を管理画面からも見られるようにする。
type PluginOrphanSchemaLister func(ctx context.Context) ([]string, error)

// SetServerPlugins injects the compiled-in plugin snapshot.
func (h *Handler) SetServerPlugins(infos []ServerPluginInfo) {
	h.serverPlugins = infos
}

// SetPluginOrphanSchemas injects the orphan schema lister.
func (h *Handler) SetPluginOrphanSchemas(fn PluginOrphanSchemaLister) {
	h.pluginOrphanSchemas = fn
}

// serverPluginsResponse is the admin/server-plugins response body.
type serverPluginsResponse struct {
	Plugins []ServerPluginInfo `json:"plugins"`
	// OrphanSchemas is null when the check could not run (未配線または DB
	// エラー)。**空配列と区別する**: null を「残存なし」と見せると、確認に
	// 失敗しているのに問題なしと読めてしまう。
	OrphanSchemas []string `json:"orphanSchemas"`
}

// ServerPlugins handles POST /api/admin/server-plugins.
func (h *Handler) ServerPlugins(c echo.Context) error {
	res := serverPluginsResponse{Plugins: h.serverPlugins}
	if res.Plugins == nil {
		// プラグイン無しは「空」であって「不明」ではないので [] を返す。
		res.Plugins = []ServerPluginInfo{}
	}

	if h.pluginOrphanSchemas != nil {
		orphans, err := h.pluginOrphanSchemas(c.Request().Context())
		if err != nil {
			// 診断情報なので一覧そのものは返す。失敗は null で表しログに残す。
			slog.Warn("admin/server-plugins: 残存データを確認できませんでした", "err", err)
		} else if orphans == nil {
			res.OrphanSchemas = []string{}
		} else {
			res.OrphanSchemas = orphans
		}
	}

	return c.JSON(http.StatusOK, res)
}
