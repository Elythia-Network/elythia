package config

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodePluginSecretKey(t *testing.T) {
	key := bytes.Repeat([]byte{0xfb}, PluginSecretKeySize) // URL 用と標準で表記が変わる値

	for name, raw := range map[string]string{
		"標準":                 base64.StdEncoding.EncodeToString(key),
		"標準 (padding 無し)":    base64.RawStdEncoding.EncodeToString(key),
		"URL 用":              base64.URLEncoding.EncodeToString(key),
		"URL 用 (padding 無し)": base64.RawURLEncoding.EncodeToString(key),
		"前後の空白":              "  " + base64.StdEncoding.EncodeToString(key) + "\n",
	} {
		got, err := decodePluginSecretKey(raw)
		require.NoError(t, err, name)
		assert.Equal(t, key, got, name)
	}

	got, err := decodePluginSecretKey("")
	require.NoError(t, err)
	assert.Nil(t, got, "未設定は nil (機能を使わない)")

	// **書き間違いは起動を止める。** 黙って未設定に倒さない。
	for name, raw := range map[string]string{
		"短い":         base64.StdEncoding.EncodeToString(key[:16]),
		"長い":         base64.StdEncoding.EncodeToString(append(key, 1)),
		"base64 でない": "not base64 !!",
		"平文の文字列":     "change-me",
	} {
		_, err := decodePluginSecretKey(raw)
		assert.Errorf(t, err, "%s を受け付けた", name)
	}
}

const pluginSecretKeyBase = `
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

func TestLoad_PluginSecretKey(t *testing.T) {
	key := bytes.Repeat([]byte{7}, PluginSecretKeySize)
	enc := base64.StdEncoding.EncodeToString(key)

	cfg, err := Load(writeTestConfig(t, pluginSecretKeyBase+"pluginSecretKey: "+enc+"\n"))
	require.NoError(t, err)
	assert.Equal(t, key, cfg.PluginSecretKey)

	cfg, err = Load(writeTestConfig(t, pluginSecretKeyBase))
	require.NoError(t, err)
	assert.Nil(t, cfg.PluginSecretKey)

	_, err = Load(writeTestConfig(t, pluginSecretKeyBase+"pluginSecretKey: change-me\n"))
	assert.Error(t, err, "不正な鍵で起動できてしまう")
}

// bindEnvKeys に登録してあるので、設定ファイルに書かなくても環境変数だけで
// 鍵を渡せる (鍵を設定ファイルに置きたくない運用のため)。
func TestLoad_PluginSecretKeyFromEnv(t *testing.T) {
	key := bytes.Repeat([]byte{8}, PluginSecretKeySize)
	t.Setenv("MK_PLUGINSECRETKEY", base64.StdEncoding.EncodeToString(key))
	cfg, err := Load(writeTestConfig(t, pluginSecretKeyBase))
	require.NoError(t, err)
	assert.Equal(t, key, cfg.PluginSecretKey)
}
