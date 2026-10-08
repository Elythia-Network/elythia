package server

import (
	"log/slog"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/pluginsecrets"
	"github.com/elythia-network/elythia/internal/core/pluginsecret"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/plugin"
)

// pluginSecretsPath is the reserved path of the secret endpoints (#3470).
// `_` で始まるパスはプラグインが登録できない (reservedPluginPath)。
const pluginSecretsPath = "/_secrets"

// pluginSecretService returns the wired service, or one without a key when
// nothing is wired (tests that build a bare Server).
func (s *Server) pluginSecretService() *pluginsecret.Service {
	if s.pluginSecrets != nil {
		return s.pluginSecrets
	}
	return unconfiguredPluginSecrets()
}

// unconfiguredPluginSecrets refuses every operation with
// plugin.ErrSecretsUnavailable. repository を持たないが、鍵が無い service は
// 保存・読み出し・削除のどれも repository に触れる前に断る。
func unconfiguredPluginSecrets() *pluginsecret.Service {
	svc, _ := pluginsecret.New(nil, nil)
	return svc
}

// registerPluginSecretRoutes wires the control-panel endpoints for one plugin.
//
// **管理者で、かつブラウザでログインした token だけを通す。** 書き込みの口は
// 一覧も含めて同じ条件にする:
//
//   - RequireSecure: token が users.token と一致すること。第三者アプリや
//     MiAuth の token (access_tokens の行) は、scope に関係なく通さない。
//     group 側の RejectAppToken と重なるが、こちらは「native token だけ」を
//     積極的に要求する形で、app token の判定が漏れても落ちる
//   - RequireAdmin: モデレーターは通さない。外部サービスの鍵を差し替えられる
//     のは、インスタンスの運営者だけにする
//
// 予約パスなので、プラグインの登録より後に張る (peer の受け口と同じ)。
func (s *Server) registerPluginSecretRoutes(group *echo.Group, def plugin.Definition) {
	h := pluginsecrets.New(s.pluginSecrets, def.Name, def.Secrets)
	roles := s.pluginRoles
	if roles == nil {
		// 判定できないときに通すと、権限の穴が動いているように見える形で残る。
		roles = denyAllRoles{}
	}
	guard := []echo.MiddlewareFunc{middleware.RequireSecure(), middleware.RequireAdmin(roles)}
	group.POST(pluginSecretsPath, h.List, guard...)
	group.POST(pluginSecretsPath+"/set", h.Set, guard...)
	group.POST(pluginSecretsPath+"/delete", h.Delete, guard...)
}

// denyAllRoles answers "no" to every role question.
type denyAllRoles struct{}

func (denyAllRoles) IsAdministrator(string) bool { return false }
func (denyAllRoles) IsModerator(string) bool     { return false }

// warnPluginSecretsWithoutKey tells the operator, once at startup, that
// enabled plugins declare secrets which cannot be stored because
// pluginSecretKey is not configured.
//
// **起動時に言う。** 言わないと、運営者は管理画面を開くまで (bot なら動かない
// 理由を調べ始めるまで) 鍵が要ることに気付けない。plugins には有効なもの
// だけを渡すこと。
func warnPluginSecretsWithoutKey(plugins []plugin.Definition, keyConfigured bool) {
	if keyConfigured {
		return
	}
	var names []string
	for _, def := range plugins {
		if len(def.Secrets) > 0 {
			names = append(names, def.Name)
		}
	}
	if len(names) == 0 {
		return
	}
	slog.Warn("plugin secrets: pluginSecretKey が未設定のため、プラグインの秘密の値を保存も読み出しもできません",
		"plugins", strings.Join(names, ","),
		"対処", "設定ファイルに pluginSecretKey (openssl rand -base64 32) を書くか MK_PLUGINSECRETKEY を渡して再起動してください")
}

// secretNames lists the declared secret names for admin/server-plugins.
func secretNames(specs []plugin.SecretSpec) []string {
	out := make([]string, 0, len(specs))
	for _, sp := range specs {
		out = append(out, sp.Name)
	}
	return out
}
