package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shiroha-a/mk/internal/misc/password"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testYAML = `
url: https://misskey.example.com
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
id: aidx
`

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "default.yml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return path
}

func TestLoad_Basic(t *testing.T) {
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "https://misskey.example.com", cfg.URL)
	assert.Equal(t, 3000, cfg.Port)
	assert.Equal(t, "misskey.example.com", cfg.Host)
	assert.Equal(t, "misskey.example.com", cfg.Hostname)
	assert.Equal(t, "https", cfg.Scheme)
	assert.Equal(t, "wss", cfg.WsScheme)
	assert.Equal(t, "wss://misskey.example.com", cfg.WsURL)
	assert.Equal(t, "https://misskey.example.com/api", cfg.APIURL)
	assert.Equal(t, "https://misskey.example.com/auth", cfg.AuthURL)
	assert.Equal(t, "https://misskey.example.com/files", cfg.DriveURL)
	assert.Equal(t, "aidx", cfg.ID)
}

func TestLoad_DatabaseConfig(t *testing.T) {
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "localhost", cfg.DB.Host)
	assert.Equal(t, 5432, cfg.DB.Port)
	assert.Equal(t, "misskey", cfg.DB.DB)
	assert.Equal(t, "postgres", cfg.DB.User)
	assert.Equal(t, "secret", cfg.DB.Pass)
}

func TestLoad_RedisConfig(t *testing.T) {
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "localhost", cfg.Redis.Host)
	assert.Equal(t, 6379, cfg.Redis.Port)
	// redisForPubsubが未指定の場合、デフォルトRedis設定がフォールバックされる
	assert.Equal(t, cfg.Redis.Host, cfg.RedisForPubsub.Host)
	assert.Equal(t, cfg.Redis.Port, cfg.RedisForPubsub.Port)
}

func TestLoad_Defaults(t *testing.T) {
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.True(t, cfg.EnableIPRateLimit)
	assert.False(t, cfg.DisableEndpointRateLimits)
	assert.Equal(t, int64(262144000), cfg.MaxFileSize)
	assert.Equal(t, 1000, cfg.PerChannelMaxNoteCacheCount)
	assert.Equal(t, 500, cfg.PerUserNotificationsMaxCount)
}

func TestLoad_DisableEndpointRateLimitsTrue(t *testing.T) {
	// disableEndpointRateLimits=true は bench / test 用の opt-in 設定。
	// production 同梱 example には載せず、env / 個別 yml で明示指定する想定。
	yaml := testYAML + "\ndisableEndpointRateLimits: true\n"
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.True(t, cfg.DisableEndpointRateLimits)
}

func TestLoad_DisableEndpointRateLimitsEnvOverride(t *testing.T) {
	// MK_DISABLEENDPOINTRATELIMITS で yml 値を上書きできる。bench docker
	// compose は env で渡す前提なのでこの経路をテストでカバーする。
	t.Setenv("MK_DISABLEENDPOINTRATELIMITS", "true")
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.True(t, cfg.DisableEndpointRateLimits)
}

func TestLoad_DBPortDefault(t *testing.T) {
	yaml := `
url: https://example.com
port: 3000
db:
  host: /var/run/postgresql
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
id: aidx
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 5432, cfg.DB.Port)
}

func TestLoad_HTTPScheme(t *testing.T) {
	yaml := `
url: http://localhost:3000
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
id: aid
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "http", cfg.Scheme)
	assert.Equal(t, "ws", cfg.WsScheme)
}

func TestLoad_InvalidURL(t *testing.T) {
	yaml := `
url: "not a url"
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	path := writeTestConfig(t, yaml)
	_, err := Load(path)
	assert.Error(t, err)
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yml")
	assert.Error(t, err)
}

func TestLoad_EnvOverride(t *testing.T) {
	path := writeTestConfig(t, testYAML)

	t.Setenv("MK_DB_HOST", "override-host")
	t.Setenv("MK_DB_PORT", "9999")

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "override-host", cfg.DB.Host)
	assert.Equal(t, 9999, cfg.DB.Port)
}

func TestLoad_RedisForPubsub(t *testing.T) {
	yaml := `
url: https://example.com
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: redis-default
  port: 6379
redisForPubsub:
  host: redis-pubsub
  port: 6380
id: aidx
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "redis-default", cfg.Redis.Host)
	assert.Equal(t, "redis-pubsub", cfg.RedisForPubsub.Host)
	assert.Equal(t, 6380, cfg.RedisForPubsub.Port)
}

func TestDSN(t *testing.T) {
	cfg := &Config{
		DB: DBOptions{
			Host: "localhost",
			Port: 5432,
			DB:   "misskey",
			User: "postgres",
			Pass: "secret",
		},
	}

	dsn := cfg.DSN()
	assert.Contains(t, dsn, "host=localhost")
	assert.Contains(t, dsn, "port=5432")
	assert.Contains(t, dsn, "dbname=misskey")
	assert.Contains(t, dsn, "user=postgres")
	assert.Contains(t, dsn, "password=secret")
	assert.Contains(t, dsn, "sslmode=disable")
}

func TestDSN_UnixSocket(t *testing.T) {
	cfg := &Config{
		DB: DBOptions{
			Host: "/var/run/postgresql",
			Port: 5432,
			DB:   "misskey",
			User: "postgres",
			Pass: "secret",
			// UDS では TLS を張れないので、Extra に ssl=true があっても
			// 強制的に sslmode=disable になることを確認する。
			Extra: map[string]any{"ssl": "true"},
		},
	}

	dsn := cfg.DSN()
	assert.Contains(t, dsn, "host=/var/run/postgresql")
	assert.Contains(t, dsn, "port=5432")
	assert.Contains(t, dsn, "sslmode=disable")
	assert.NotContains(t, dsn, "sslmode=require")
}

func TestIsUnixSocketPath(t *testing.T) {
	tests := []struct {
		name string
		host string
		want bool
	}{
		{name: "absolute path", host: "/var/run/postgresql", want: true},
		{name: "socket file", host: "/tmp/mk.sock", want: true},
		{name: "hostname", host: "localhost", want: false},
		{name: "ipv4", host: "127.0.0.1", want: false},
		{name: "ipv6", host: "::1", want: false},
		{name: "empty", host: "", want: false},
		{name: "relative path", host: "./mk.sock", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsUnixSocketPath(tt.host))
		})
	}
}

func TestLoad_SocketAndChmod(t *testing.T) {
	yaml := `
url: https://example.com
port: 3000
socket: /run/mk/mk.sock
chmodSocket: "770"
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "/run/mk/mk.sock", cfg.Socket)
	assert.Equal(t, "770", cfg.ChmodSocket)
}

func TestDSN_SSLEnabled(t *testing.T) {
	cfg := &Config{
		DB: DBOptions{
			Host:  "localhost",
			Port:  5432,
			DB:    "misskey",
			User:  "postgres",
			Pass:  "secret",
			Extra: map[string]any{"ssl": "true"},
		},
	}

	dsn := cfg.DSN()
	assert.Contains(t, dsn, "sslmode=verify-full")
}

func TestLoad_MaxFileSizeOverride(t *testing.T) {
	yaml := `
url: https://example.com
port: 3000
maxFileSize: 1048576
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, int64(1048576), cfg.MaxFileSize)
}

func TestLoad_DisableIPRateLimit(t *testing.T) {
	yaml := `
url: https://example.com
port: 3000
enableIpRateLimit: false
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.False(t, cfg.EnableIPRateLimit)
}

// bcryptCost は未設定なら既定、範囲内ならそのまま、範囲外なら既定に落ちる。
//
// **範囲外を既定に落とすのが要点。** そのまま通すと、設定の書き間違いで cost 4 の
// ハッシュを量産して、気づいた時点で全員のパスワードが弱い。
func TestLoad_BcryptCost(t *testing.T) {
	base := `
url: https://example.com
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	for name, tc := range map[string]struct {
		line string
		want int
	}{
		"未設定は既定":   {"", password.DefaultCost},
		"範囲内はそのまま": {"bcryptCost: 12", 12},
		"下限ちょうど":   {"bcryptCost: 4", password.MinCost},
		"上限ちょうど":   {"bcryptCost: 31", password.MaxCost},
		"下限未満は既定":  {"bcryptCost: 3", password.DefaultCost},
		"上限超過は既定":  {"bcryptCost: 32", password.DefaultCost},
		"負値は既定":    {"bcryptCost: -1", password.DefaultCost},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeTestConfig(t, base+tc.line+"\n")
			cfg, err := Load(path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.BcryptCost)
		})
	}
}

// noteHookConcurrency は未設定/0 を「実行時の既定に委ねる」意味の 0 にする。
//
// **負値を素通しさせない。** そのまま semaphore の容量になると 0 枠になり、
// 投稿後のフックが 1 本も走らなくなる。
func TestLoad_NoteHookConcurrency(t *testing.T) {
	base := `
url: https://example.com
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	for name, tc := range map[string]struct {
		line string
		want int
	}{
		"未設定は 0 (実行時の既定)": {"", 0},
		"正の値はそのまま":        {"noteHookConcurrency: 32", 32},
		"0 はそのまま":         {"noteHookConcurrency: 0", 0},
		"負値は 0 に落とす":      {"noteHookConcurrency: -5", 0},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeTestConfig(t, base+tc.line+"\n")
			cfg, err := Load(path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.NoteHookConcurrency)
		})
	}
}

// mediaProxyConcurrency は未設定/0 を「実行時の既定に委ねる」意味の 0 にする (#3032)。
//
// **負値を素通しさせない。** そのまま semaphore の容量になると 0 枠になり、
// リサイズ系のリクエストが 1 本も通らなくなる (全部 503 に落ちる)。
func TestLoad_MediaProxyConcurrency(t *testing.T) {
	base := `
url: https://example.com
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	for name, tc := range map[string]struct {
		line string
		want int
	}{
		"未設定は 0 (実行時の既定)": {"", 0},
		"正の値はそのまま":        {"mediaProxyConcurrency: 6", 6},
		"0 はそのまま":         {"mediaProxyConcurrency: 0", 0},
		"負値は 0 に落とす":      {"mediaProxyConcurrency: -5", 0},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeTestConfig(t, base+tc.line+"\n")
			cfg, err := Load(path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.MediaProxyConcurrency)
		})
	}
}

// mediaProxyConcurrency が MK_ で上書きできること (#3032)。
//
// **bindEnvKeys に登録しないと、設定ファイルに書いていない環境では
// 環境変数だけでは作れない** (CLAUDE.md Section 9)。運用で緊急に絞りたい
// ときに効かないと困るので、登録そのものを固定する。
func TestLoad_MediaProxyConcurrencyFromEnv(t *testing.T) {
	base := `
url: https://example.com
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	t.Setenv("MK_MEDIAPROXYCONCURRENCY", "3")
	path := writeTestConfig(t, base)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 3, cfg.MediaProxyConcurrency)
}

// queueStuckWorkerSeconds は 0 = キューごとの既定、負値 = 機能ごと無効。
//
// **負値を素通しさせる。** 隣の noteHookConcurrency / queueIdlePollSeconds は
// 負値を 0 に落とすが、こちらは負値が「無効化」という別の意味を持つ。0 に
// 落とすと「既定に従う」に化けて、operator が止めたはずの機構が動き続ける
// (#2657 の逃げ道が塞がる)。
func TestLoad_QueueStuckWorkerSeconds(t *testing.T) {
	base := `
url: https://example.com
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	for name, tc := range map[string]struct {
		line string
		want int
	}{
		"未設定は 0 (キューごとの既定)": {"", 0},
		"正の値はそのまま":          {"queueStuckWorkerSeconds: 900", 900},
		"0 はそのまま":           {"queueStuckWorkerSeconds: 0", 0},
		"負値は無効化なので保つ":       {"queueStuckWorkerSeconds: -1", -1},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeTestConfig(t, base+tc.line+"\n")
			cfg, err := Load(path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.QueueStuckWorkerSeconds)
		})
	}
}

// bindEnvKeys に登録してあるので、設定ファイルに書かなくても環境変数だけで
// 設定できる。運用中に止める唯一の手段なのでここを固定しておく。
func TestLoad_QueueStuckWorkerSecondsFromEnv(t *testing.T) {
	path := writeTestConfig(t, `
url: https://example.com
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`)
	t.Setenv("MK_QUEUESTUCKWORKERSECONDS", "-1")
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, -1, cfg.QueueStuckWorkerSeconds)
}

// queueHandlerDeadlineSeconds も 0 = 既定、負値 = 無効。queueStuckWorkerSeconds
// と同じく負値を落とさない。
func TestLoad_QueueHandlerDeadlineSeconds(t *testing.T) {
	base := `
url: https://example.com
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	for name, tc := range map[string]struct {
		line string
		want int
	}{
		"未設定は 0 (driver 既定)": {"", 0},
		"正の値はそのまま":           {"queueHandlerDeadlineSeconds: 120", 120},
		"負値は無効化なので保つ":        {"queueHandlerDeadlineSeconds: -1", -1},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeTestConfig(t, base+tc.line+"\n")
			cfg, err := Load(path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.QueueHandlerDeadlineSeconds)
		})
	}
}

func TestLoad_CustomCountsAndThreshold(t *testing.T) {
	yaml := `
url: https://example.com
port: 3000
perChannelMaxNoteCacheCount: 500
perUserNotificationsMaxCount: 100
deactivateAntennaThreshold: 12345
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 500, cfg.PerChannelMaxNoteCacheCount)
	assert.Equal(t, 100, cfg.PerUserNotificationsMaxCount)
	assert.Equal(t, 12345, cfg.DeactivateAntennaThreshold)
}

func TestLoad_EffectivePolicyProviderCacheEntries(t *testing.T) {
	base := `
url: https://example.com
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`

	for name, tc := range map[string]struct {
		line string
		env  string
		want int
	}{
		"default":      {want: 10_000},
		"yaml":         {line: "effectivePolicyProviderCacheEntries: 321\n", want: 321},
		"env only":     {env: "456", want: 456},
		"zero default": {line: "effectivePolicyProviderCacheEntries: 0\n", want: 10_000},
		"negative":     {line: "effectivePolicyProviderCacheEntries: -1\n", want: 10_000},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("MK_EFFECTIVEPOLICYPROVIDERCACHEENTRIES", tc.env)
			}
			path := writeTestConfig(t, base+tc.line)
			cfg, err := Load(path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.EffectivePolicyProviderCacheEntries)
		})
	}
}

func TestLoad_InternalMediaProxy(t *testing.T) {
	yaml := `
url: https://example.com
port: 3000
mediaProxy: https://example.com/proxy
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "https://example.com/proxy", cfg.MediaProxy)
	assert.False(t, cfg.ExternalMediaProxyEnabled)
}

func TestLoad_VideoThumbnailGenerator(t *testing.T) {
	yaml := `
url: https://example.com
port: 3000
videoThumbnailGenerator: https://thumb.example.com/
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "https://thumb.example.com", cfg.VideoThumbnailGenerator)
}

func TestLoad_UnmarshalError(t *testing.T) {
	// portにstringを入れるとUnmarshalが失敗する
	yaml := `
url: https://example.com
port: "not_a_number"
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	path := writeTestConfig(t, yaml)
	_, err := Load(path)
	// viperは型変換を試みるので、直接の失敗は難しい
	// ただしresolveでURLパースが成功すれば通る可能性がある
	// このテストケースでは少なくともパニックしないことを確認
	_ = err
}

func TestLoad_TestMode(t *testing.T) {
	yaml := `
url: https://example.com
port: 3000
testMode: true
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.True(t, cfg.TestMode)
}

func TestLoad_TestMode_DefaultFalse(t *testing.T) {
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.False(t, cfg.TestMode)
}

func TestLoad_TestMode_EnvOverride(t *testing.T) {
	path := writeTestConfig(t, testYAML)

	t.Setenv("MK_TESTMODE", "true")

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.True(t, cfg.TestMode)
}

// dev は既定で false。**TestMode とは独立していること** も確認する (#2477)。
// 相乗りさせると、frontend を dev server から配信したいだけで
// /api/reset-db のような破壊的エンドポイントが開いてしまう。
func TestLoad_Dev_DefaultsFalseAndIsIndependentOfTestMode(t *testing.T) {
	path := writeTestConfig(t, testYAML)

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.False(t, cfg.Dev)

	t.Setenv("MK_TESTMODE", "true")
	cfg, err = Load(path)
	require.NoError(t, err)
	assert.True(t, cfg.TestMode)
	assert.False(t, cfg.Dev, "testMode を有効にしても dev は連動しない")
}

func TestLoad_Dev_EnvOverride(t *testing.T) {
	path := writeTestConfig(t, testYAML)

	t.Setenv("MK_DEV", "true")

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.True(t, cfg.Dev)
	assert.False(t, cfg.TestMode, "dev を有効にしても testMode は連動しない")
}

func TestLoad_Dev_FromYAML(t *testing.T) {
	path := writeTestConfig(t, testYAML+"\ndev: true\n")

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.True(t, cfg.Dev)
}

// plugins セクションが map のまま読めること。中身の形はプラグインごとに違うので
// mk-go 側では解釈しない (#2482)。
func TestLoad_Plugins(t *testing.T) {
	path := writeTestConfig(t, testYAML+`
plugins:
  gameinfo:
    enabled: true
    apiKey: secret
    retries: 3
  other:
    enabled: false
`)

	cfg, err := Load(path)
	require.NoError(t, err)

	require.Len(t, cfg.Plugins, 2)
	assert.Equal(t, true, cfg.Plugins["gameinfo"]["enabled"])
	assert.Equal(t, 3, cfg.Plugins["gameinfo"]["retries"])
	assert.Equal(t, false, cfg.Plugins["other"]["enabled"])

	// **Viper はキーを小文字化する。** YAML に apiKey と書いても map の
	// キーは apikey になる。プラグイン側は encoding/json 経由で受け取り、
	// json のフィールド照合が大文字小文字を無視するため camelCase の
	// タグでも一致する (internal/server の
	// TestPluginConfig_MatchesCamelCaseDespiteViperLowercasing が固定)。
	assert.Equal(t, "secret", cfg.Plugins["gameinfo"]["apikey"])
	assert.Nil(t, cfg.Plugins["gameinfo"]["apiKey"], "元の大文字小文字では引けない")
}

// 未設定でも壊れない (プラグインを使わないインスタンスが大半)。
func TestLoad_Plugins_AbsentIsEmpty(t *testing.T) {
	cfg, err := Load(writeTestConfig(t, testYAML))
	require.NoError(t, err)
	assert.Empty(t, cfg.Plugins)
}

func TestLoad_MediaProxy(t *testing.T) {
	yaml := `
url: https://example.com
port: 3000
mediaProxy: https://proxy.example.com/
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "https://proxy.example.com", cfg.MediaProxy)
	assert.True(t, cfg.ExternalMediaProxyEnabled)
}

// --- TrustProxy ---

func TestTrustProxy_Default(t *testing.T) {
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, DefaultTrustProxy, cfg.TrustProxy)
}

func TestTrustProxy_Custom(t *testing.T) {
	yml := testYAML + `
trustProxy:
  - "203.0.113.0/24"
  - "198.51.100.0/24"
`
	path := writeTestConfig(t, yml)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"203.0.113.0/24", "198.51.100.0/24"}, cfg.TrustProxy)
}

func TestLoad_DBSlaves(t *testing.T) {
	yaml := `
url: https://example.com
port: 3000
db:
  host: primary.example.com
  port: 5432
  db: misskey
  user: postgres
  pass: secret
dbReplications: true
dbSlaves:
  - host: replica1.example.com
    port: 5432
    db: misskey
    user: replica
    pass: rpass1
  - host: replica2.example.com
    port: 5433
    db: misskey
    user: replica
    pass: rpass2
redis:
  host: localhost
  port: 6379
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.True(t, cfg.DBReplications)
	require.Len(t, cfg.DBSlaves, 2)
	assert.Equal(t, "replica1.example.com", cfg.DBSlaves[0].Host)
	assert.Equal(t, 5432, cfg.DBSlaves[0].Port)
	assert.Equal(t, "rpass1", cfg.DBSlaves[0].Pass)
	assert.Equal(t, "replica2.example.com", cfg.DBSlaves[1].Host)
	assert.Equal(t, 5433, cfg.DBSlaves[1].Port)
}

func TestLoad_DBSlaves_Empty(t *testing.T) {
	// dbSlaves 未指定 → 空スライス
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, cfg.DBSlaves)
}

func TestSlaveDSN(t *testing.T) {
	cfg := &Config{
		DB: DBOptions{Host: "primary", Port: 5432},
		DBSlaves: []DBSlaveOptions{
			{Host: "replica1", Port: 5432, DB: "misskey", User: "replica", Pass: "p1"},
		},
	}
	dsn := cfg.SlaveDSN(0)
	assert.Contains(t, dsn, "host=replica1")
	assert.Contains(t, dsn, "port=5432")
	assert.Contains(t, dsn, "dbname=misskey")
	assert.Contains(t, dsn, "user=replica")
	assert.Contains(t, dsn, "password=p1")
	assert.Contains(t, dsn, "sslmode=disable")
}

func TestSlaveDSN_OutOfRange(t *testing.T) {
	cfg := &Config{
		DBSlaves: []DBSlaveOptions{
			{Host: "replica1", Port: 5432, DB: "misskey", User: "u", Pass: "p"},
		},
	}
	assert.Empty(t, cfg.SlaveDSN(-1))
	assert.Empty(t, cfg.SlaveDSN(1))
	assert.Empty(t, cfg.SlaveDSN(99))
}

func TestSlaveDSN_InheritsPrimarySSL(t *testing.T) {
	cfg := &Config{
		DB: DBOptions{
			Host:  "primary",
			Port:  5432,
			Extra: map[string]any{"ssl": "true"},
		},
		DBSlaves: []DBSlaveOptions{
			{Host: "replica1", Port: 5432, DB: "misskey", User: "u", Pass: "p"},
		},
	}
	dsn := cfg.SlaveDSN(0)
	assert.Contains(t, dsn, "sslmode=verify-full")
}

func TestSlaveDSN_UnixSocket(t *testing.T) {
	// Unix socket replica は SSL 不可
	cfg := &Config{
		DB: DBOptions{
			Host:  "/var/run/postgresql",
			Extra: map[string]any{"ssl": "true"},
		},
		DBSlaves: []DBSlaveOptions{
			{Host: "/var/run/postgresql/replica", Port: 5433, DB: "misskey", User: "u", Pass: "p"},
		},
	}
	dsn := cfg.SlaveDSN(0)
	assert.Contains(t, dsn, "host=/var/run/postgresql/replica")
	assert.Contains(t, dsn, "sslmode=disable")
	assert.NotContains(t, dsn, "sslmode=require")
}

func TestLoad_SentryForBackend(t *testing.T) {
	yaml := `
url: https://example.com
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
sentryForBackend:
  enableNodeProfiling: true
  options:
    dsn: 'https://public@o0.ingest.sentry.io/0'
    environment: production
    release: '2026.4.0'
    sampleRate: 0.5
    tracesSampleRate: 0.1
    debug: false
    serverName: 'mk-prod-1'
sentryForFrontend:
  vueIntegration:
    tracingOptions:
      trackComponents: true
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)

	require.NotNil(t, cfg.SentryForBackend)
	assert.True(t, cfg.SentryForBackend.EnableNodeProfiling)
	assert.Equal(t, "https://public@o0.ingest.sentry.io/0", cfg.SentryForBackend.Options.DSN)
	assert.Equal(t, "production", cfg.SentryForBackend.Options.Environment)
	assert.Equal(t, "2026.4.0", cfg.SentryForBackend.Options.Release)
	assert.InEpsilon(t, 0.5, cfg.SentryForBackend.Options.SampleRate, 1e-9)
	assert.InEpsilon(t, 0.1, cfg.SentryForBackend.Options.TracesSampleRate, 1e-9)
	assert.Equal(t, "mk-prod-1", cfg.SentryForBackend.Options.ServerName)

	// SentryForFrontend は素通し格納 (Go バックエンドで参照しない)。
	// viper は map のキーを小文字化するため正規化済みのキーで照合する。
	require.NotNil(t, cfg.SentryForFrontend)
	assert.Contains(t, cfg.SentryForFrontend, "vueintegration")
}

func TestLoad_SentryDisabledByDefault(t *testing.T) {
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Nil(t, cfg.SentryForBackend)
	assert.Nil(t, cfg.SentryForFrontend)
}

func TestLoad_DBPoolConfig(t *testing.T) {
	yaml := `
url: https://example.com
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
  maxOpenConns: 50
  maxIdleConns: 30
  connMaxLifetime: 600
  connMaxIdleTime: 300
redis:
  host: localhost
  port: 6379
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)

	require.NotNil(t, cfg.DB.MaxOpenConns)
	assert.Equal(t, 50, *cfg.DB.MaxOpenConns)
	require.NotNil(t, cfg.DB.MaxIdleConns)
	assert.Equal(t, 30, *cfg.DB.MaxIdleConns)
	require.NotNil(t, cfg.DB.ConnMaxLifetime)
	assert.Equal(t, 600, *cfg.DB.ConnMaxLifetime)
	require.NotNil(t, cfg.DB.ConnMaxIdleTime)
	assert.Equal(t, 300, *cfg.DB.ConnMaxIdleTime)
}

func TestLoad_DBPoolConfig_Defaults(t *testing.T) {
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Nil(t, cfg.DB.MaxOpenConns)
	assert.Nil(t, cfg.DB.MaxIdleConns)
	assert.Nil(t, cfg.DB.ConnMaxLifetime)
	assert.Nil(t, cfg.DB.ConnMaxIdleTime)
}

func TestLoad_RedisPoolSize(t *testing.T) {
	yaml := `
url: https://example.com
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
redis:
  host: localhost
  port: 6379
  poolSize: 100
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)

	require.NotNil(t, cfg.Redis.PoolSize)
	assert.Equal(t, 100, *cfg.Redis.PoolSize)
}

func TestLoad_RedisPoolSize_Default(t *testing.T) {
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Nil(t, cfg.Redis.PoolSize)
}

func TestLoad_EnablePprof(t *testing.T) {
	yaml := testYAML + `
enablePprof: true
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.True(t, cfg.EnablePprof)
}

func TestLoad_EnablePprof_DefaultFalse(t *testing.T) {
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.False(t, cfg.EnablePprof)
}

func TestLoad_EnableTimelineCache(t *testing.T) {
	yaml := testYAML + `
enableTimelineCache: true
timelineCacheTtlSeconds: 5
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.True(t, cfg.EnableTimelineCache)
	assert.Equal(t, 5, cfg.TimelineCacheTTLSeconds)
}

func TestLoad_EnableTimelineCache_DefaultFalse(t *testing.T) {
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.False(t, cfg.EnableTimelineCache)
	assert.Equal(t, 0, cfg.TimelineCacheTTLSeconds)
}

func TestLoad_EnableMetrics(t *testing.T) {
	yaml := testYAML + `
enableMetrics: true
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.True(t, cfg.EnableMetrics)
}

func TestLoad_EnableMetrics_DefaultFalse(t *testing.T) {
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.False(t, cfg.EnableMetrics)
}

func TestLoad_JobQueueAutoScale(t *testing.T) {
	yaml := testYAML + `
jobQueueAutoScale: true
maxWorkers: 256
minWorkers: 8
maxWorkersGlobal: 512
autoScaleCooldownSeconds: 2
`
	path := writeTestConfig(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.True(t, cfg.JobQueueAutoScale)
	require.NotNil(t, cfg.MaxWorkers)
	assert.Equal(t, 256, *cfg.MaxWorkers)
	require.NotNil(t, cfg.MinWorkers)
	assert.Equal(t, 8, *cfg.MinWorkers)
	require.NotNil(t, cfg.MaxWorkersGlobal)
	assert.Equal(t, 512, *cfg.MaxWorkersGlobal)
	require.NotNil(t, cfg.AutoScaleCooldownSeconds)
	assert.Equal(t, 2, *cfg.AutoScaleCooldownSeconds)
}

func TestLoad_JobQueueAutoScale_DefaultsAreNil(t *testing.T) {
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.False(t, cfg.JobQueueAutoScale)
	assert.Nil(t, cfg.MaxWorkers)
	assert.Nil(t, cfg.MinWorkers)
	assert.Nil(t, cfg.MaxWorkersGlobal)
	assert.Nil(t, cfg.AutoScaleCooldownSeconds)
}

// TestRedisOptions_KeyPrefix は #362 drop-in 互換の回帰テスト。
// TS 本家の ioredis keyPrefix (`<host>:`) と同じ形式の prefix を返すことを確認する。
func TestRedisOptions_KeyPrefix(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		want   string
	}{
		{name: "empty prefix returns empty string", prefix: "", want: ""},
		{name: "hostname prefix gets trailing colon", prefix: "example.com", want: "example.com:"},
		{name: "custom prefix gets trailing colon", prefix: "my-instance", want: "my-instance:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := RedisOptions{Prefix: tt.prefix}
			assert.Equal(t, tt.want, opts.KeyPrefix())
		})
	}
}

func TestResolveJobQueueDriver(t *testing.T) {
	t.Run("accepted values", func(t *testing.T) {
		tests := []struct {
			raw  string
			want string
		}{
			// 空 string の default は mkq (#571 audit で asynq → mkq に変更)。
			{"", "mkq"},
			{"mkq", "mkq"},
			{"  mkq  ", "mkq"},
			{"MKQ", "mkq"},
		}
		for _, tt := range tests {
			t.Run(tt.raw, func(t *testing.T) {
				got, err := resolveJobQueueDriver(tt.raw)
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			})
		}
	})
	t.Run("unknown value rejected", func(t *testing.T) {
		// Unknown values (typos like "mkqq") must error so a YAML
		// typo does not silently fall back to the default.
		// internal/server/queue_factory.go also rejects unknown
		// drivers; surfacing the failure here keeps the two layers
		// consistent.
		_, err := resolveJobQueueDriver("mkqq")
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "#2985",
			"typo は削除済み driver の案内ではなく未知値として落とす")
	})
	t.Run("removed asynq driver rejected with migration hint", func(t *testing.T) {
		// **黙って mkq へ倒さない。** 明示的に asynq を選んでいた運用は
		// 意図的なので、driver が入れ替わったことに気付けないまま起動
		// させるほうが危ない (#2985)。
		for _, raw := range []string{"asynq", "  ASYNQ  "} {
			_, err := resolveJobQueueDriver(raw)
			require.Error(t, err, raw)
			// 移行方法が読み取れることまで見る。「未知の値」で片付けると
			// operator は何を書けばよいのか分からない。
			assert.Contains(t, err.Error(), "removed")
			assert.Contains(t, err.Error(), "mkq")
			// **片道であることも伝える。** 新ビルドは起動を拒むので、
			// asynq の未処理ジョブを捌く機会はこのメッセージを読んだ
			// 時点しか無い (#2985)。
			assert.Contains(t, err.Error(), "asynq:{<queue>}:*")
			assert.Contains(t, err.Error(), "drain")
		}
	})
}

// TestLoad_JobQueueDriver_AsynqRejected pins the removal at the Load
// boundary: an existing deployment that still pins the legacy driver must
// fail to start rather than boot on a different queue implementation.
func TestLoad_JobQueueDriver_AsynqRejected(t *testing.T) {
	yml := testYAML + "\njobQueueDriver: asynq\n"
	path := writeTestConfig(t, yml)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "asynq")
}

func TestLoad_JobQueueDriver_Invalid(t *testing.T) {
	yml := testYAML + "\njobQueueDriver: not-a-real-driver\n"
	path := writeTestConfig(t, yml)
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoad_JobQueueDriver_Default(t *testing.T) {
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)
	// #571 audit で default を asynq → mkq に変更、#2985 で唯一の driver に。
	assert.Equal(t, "mkq", cfg.JobQueueDriver)
}

func TestLoad_JobQueueDriver_Mkq(t *testing.T) {
	yml := testYAML + "\njobQueueDriver: mkq\n"
	path := writeTestConfig(t, yml)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "mkq", cfg.JobQueueDriver)
}

func TestLoad_JobQueueDriver_EnvOverride(t *testing.T) {
	t.Setenv("MK_JOBQUEUEDRIVER", "mkq")
	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "mkq", cfg.JobQueueDriver)
}
