package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/plugin"
)

// **既定は有効。** ビルドに含めた時点で運営者は意図して選んでいるので、
// 動かすのに追加の設定を要求しない。
func TestPluginEnabled_DefaultsToTrue(t *testing.T) {
	assert.True(t, pluginEnabled(nil))
	assert.True(t, pluginEnabled(map[string]any{}))
	assert.True(t, pluginEnabled(map[string]any{"apiKey": "x"}))
	assert.True(t, pluginEnabled(map[string]any{"enabled": true}))
}

func TestPluginEnabled_FalseDisables(t *testing.T) {
	assert.False(t, pluginEnabled(map[string]any{"enabled": false}))
}

// bool 以外が書かれていたら有効として扱う。設定の書き間違いで機能が黙って
// 消える方が、無効化が効かないより厄介。
func TestPluginEnabled_NonBoolIsTreatedAsEnabled(t *testing.T) {
	for _, v := range []any{"false", 0, nil, []string{"false"}} {
		assert.Truef(t, pluginEnabled(map[string]any{"enabled": v}), "%v (%T)", v, v)
	}
}

func TestPluginConfig_Unmarshal(t *testing.T) {
	c := pluginConfig(map[string]any{
		"enabled":  true,
		"apiKey":   "secret",
		"endpoint": "https://example.com",
		"retries":  3,
	})

	var got struct {
		APIKey   string `json:"apiKey"`
		Endpoint string `json:"endpoint"`
		Retries  int    `json:"retries"`
		Enabled  bool   `json:"enabled"`
	}
	require.NoError(t, c.Unmarshal(&got))

	assert.Equal(t, "secret", got.APIKey)
	assert.Equal(t, "https://example.com", got.Endpoint)
	assert.Equal(t, 3, got.Retries)
	// enabled は mk-go が解釈する予約キーなので、プラグインには渡さない。
	assert.False(t, got.Enabled)
}

// 設定が無ければ v を変更しない。呼び出し側が入れた既定値が残るようにする。
func TestPluginConfig_EmptyKeepsDefaults(t *testing.T) {
	var got struct {
		Endpoint string `json:"endpoint"`
	}
	got.Endpoint = "既定値"

	require.NoError(t, pluginConfig(nil).Unmarshal(&got))
	assert.Equal(t, "既定値", got.Endpoint)

	// enabled だけが書かれている場合も「設定なし」と同じ扱いになる。
	require.NoError(t, pluginConfig(map[string]any{"enabled": true}).Unmarshal(&got))
	assert.Equal(t, "既定値", got.Endpoint)
}

// **Viper は設定のキーを小文字化する** (`apiKey` → `apikey`)。プラグイン側は
// camelCase の json タグを書くのが自然なので、そこが一致しないと**エラーも出ず
// に空値が入る**。encoding/json のフィールド照合が大文字小文字を無視すること
// に依存しているので、依存していること自体をテストで固定する。
func TestPluginConfig_MatchesCamelCaseDespiteViperLowercasing(t *testing.T) {
	var got struct {
		APIKey     string `json:"apiKey"`
		MaxRetries int    `json:"maxRetries"`
	}
	require.NoError(t, pluginConfig(map[string]any{
		"apikey":     "secret",
		"maxretries": 5,
	}).Unmarshal(&got))

	assert.Equal(t, "secret", got.APIKey)
	assert.Equal(t, 5, got.MaxRetries)
}

// 一方 map の値はキーの大文字小文字が復元されない。構造体で受けられない設定
// (任意のヘッダ等) を持つプラグインが踏むので、doc comment に明記してある。
func TestPluginConfig_MapKeysStayLowercased(t *testing.T) {
	var got struct {
		Headers map[string]string `json:"headers"`
	}
	require.NoError(t, pluginConfig(map[string]any{
		"headers": map[string]any{"x-api-key": "v"},
	}).Unmarshal(&got))

	assert.Equal(t, map[string]string{"x-api-key": "v"}, got.Headers)
}

func TestPluginConfig_TypeMismatchIsError(t *testing.T) {
	var got struct {
		Retries int `json:"retries"`
	}
	err := pluginConfig(map[string]any{"retries": "three"}).Unmarshal(&got)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "設定を読み込めません")
}

func TestPluginConfig_UnconvertibleValueIsError(t *testing.T) {
	var got struct{}
	err := pluginConfig(map[string]any{"ch": make(chan int)}).Unmarshal(&got)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "設定を変換できません")
}

// --- setupPlugins との組み合わせ ---

// **無効なプラグインは登録されない。** ルートも生えない。
func TestSetupPlugins_DisabledIsSkipped(t *testing.T) {
	var called bool
	def := pluginDef("off", func(_ plugin.Context, r plugin.Router) error {
		called = true
		r.GET("/x", func(plugin.Request) (any, error) { return "y", nil })
		return nil
	}, nil)

	e := echo.New()
	s := &Server{
		echo: e, role: config.RoleServer,
		config: &config.Config{
			URL:     "https://example.com",
			Plugins: map[string]map[string]any{"off": {"enabled": false}},
		},
	}
	require.NoError(t, s.setupPlugins(e.Group("/api"), []plugin.Definition{def}, noopStorage))

	assert.False(t, called, "Routes が呼ばれない")

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/plugin/off/x", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code, "ルートも生えない")
}

// 無効なプラグインはストレージも開かない (接続を無駄に張らない)。
func TestSetupPlugins_DisabledDoesNotOpenStorage(t *testing.T) {
	def := pluginDef("off", func(plugin.Context, plugin.Router) error { return nil }, nil)

	e := echo.New()
	s := &Server{
		echo: e, role: config.RoleServer,
		config: &config.Config{
			URL:     "https://example.com",
			Plugins: map[string]map[string]any{"off": {"enabled": false}},
		},
	}
	err := s.setupPlugins(e.Group("/api"), []plugin.Definition{def},
		func(string) (plugin.Storage, error) { return nil, errors.New("開いてはいけない") })

	require.NoError(t, err)
}

func TestSetupPlugins_DisabledEffectivePolicyInvokesNoCallbacks(t *testing.T) {
	var opened, factoryCalled, routesCalled, jobsCalled bool
	def := plugin.Definition{
		Name:       "off",
		APIVersion: plugin.APIVersion,
		Migrations: []plugin.Migration{{Version: 1, SQL: "SELECT 1"}},
		EffectivePolicies: func(plugin.Context, plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
			factoryCalled = true
			return plugin.EffectivePolicyRegistration{}, errors.New("must not run")
		},
		Routes: func(plugin.Context, plugin.Router) error {
			routesCalled = true
			return nil
		},
		Jobs: func(plugin.Context, plugin.Jobs) error {
			jobsCalled = true
			return nil
		},
	}
	e := echo.New()
	s := &Server{
		echo: e, role: config.RoleBoth,
		config: &config.Config{
			URL:     "https://example.com",
			Plugins: map[string]map[string]any{"off": {"enabled": false}},
		},
	}

	err := s.setupPlugins(e.Group("/api"), []plugin.Definition{def}, func(string) (plugin.Storage, error) {
		opened = true
		return nil, nil
	})
	require.NoError(t, err)
	assert.False(t, opened)
	assert.False(t, factoryCalled)
	assert.False(t, routesCalled)
	assert.False(t, jobsCalled)
}

// プラグインが自分の設定を受け取れること。
func TestSetupPlugins_PassesConfigToPlugin(t *testing.T) {
	var got string
	def := pluginDef("cfg", func(c plugin.Context, _ plugin.Router) error {
		var v struct {
			APIKey string `json:"apiKey"`
		}
		if err := c.Config().Unmarshal(&v); err != nil {
			return err
		}
		got = v.APIKey
		return nil
	}, nil)

	e := echo.New()
	s := &Server{
		echo: e, role: config.RoleServer,
		config: &config.Config{
			URL:     "https://example.com",
			Plugins: map[string]map[string]any{"cfg": {"apiKey": "k123"}},
		},
	}
	require.NoError(t, s.setupPlugins(e.Group("/api"), []plugin.Definition{def}, noopStorage))
	assert.Equal(t, "k123", got)
}

// 設定が 1 つも無くても Config() は使える (nil を返さない)。
func TestSetupPlugins_ConfigIsUsableWithoutSettings(t *testing.T) {
	var err2 error
	def := pluginDef("nocfg", func(c plugin.Context, _ plugin.Router) error {
		var v struct{}
		err2 = c.Config().Unmarshal(&v)
		return nil
	}, nil)

	s, api := newPluginTestServer(config.RoleServer)
	require.NoError(t, s.setupPlugins(api, []plugin.Definition{def}, noopStorage))
	assert.NoError(t, err2)
}

// --- 設定ダンプ ---

// **プラグインの設定は既定でマスクする。** どのキーが秘密かを mk-go は
// 判別できないので、既定を「出す」にすると漏洩の機会が増える。
func TestBuildConfigDump_MasksPluginSettings(t *testing.T) {
	cfg := secretBearingConfig()
	cfg.Plugins = map[string]map[string]any{
		"gameinfo": {"enabled": true, "apiKey": "LEAK-plugin-api-key"},
		"other":    {"enabled": false},
	}

	d := BuildConfigDump(cfg, config.RoleBoth)
	rendered := RenderConfigDump(d)

	assert.NotContains(t, rendered, "LEAK-plugin-api-key", "値は出さない")
	assert.Contains(t, rendered, "gameinfo.apiKey", "キー名は診断に要るので出す")
	assert.Contains(t, rendered, "有効")
	assert.Contains(t, rendered, "無効 (enabled: false)")
}

func TestBuildConfigDump_NoPluginsSectionWhenUnset(t *testing.T) {
	d := BuildConfigDump(secretBearingConfig(), config.RoleBoth)
	for _, e := range d.Effective {
		assert.NotContains(t, e.Key, "plugin: ")
	}
}
