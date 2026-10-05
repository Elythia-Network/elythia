package config_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 既定 (env 未設定) は従来どおり両方を担う。ここが崩れると、env を触っていない
// 既存デプロイが片肺で起動する。
func TestResolveProcessRole_DefaultsToBoth(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "")

	role, err := config.ResolveProcessRole()
	require.NoError(t, err)
	assert.Equal(t, config.RoleBoth, role)
	assert.True(t, role.RunsServer())
	assert.True(t, role.RunsQueue())
}

func TestResolveProcessRole_OnlyServer(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "1")

	role, err := config.ResolveProcessRole()
	require.NoError(t, err)
	assert.Equal(t, config.RoleServer, role)
	assert.True(t, role.RunsServer())
	assert.False(t, role.RunsQueue(), "server ノードはジョブを処理しない")
}

func TestResolveProcessRole_OnlyQueue(t *testing.T) {
	t.Setenv(config.EnvOnlyQueue, "1")

	role, err := config.ResolveProcessRole()
	require.NoError(t, err)
	assert.Equal(t, config.RoleQueue, role)
	assert.False(t, role.RunsServer(), "queue ノードは API を生やさない")
	assert.True(t, role.RunsQueue())
}

// upstream は truthy 判定なので `=false` でも有効になる。mk-go は値を解釈する
// (意図的な差分、docs/divergence.md)。
func TestResolveProcessRole_FalsyValuesDisable(t *testing.T) {
	for _, v := range []string{"0", "false", "FALSE", "no", "off", "", "  "} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(config.EnvOnlyQueue, v)
			role, err := config.ResolveProcessRole()
			require.NoError(t, err)
			assert.Equal(t, config.RoleBoth, role)
		})
	}
}

func TestResolveProcessRole_TruthyValues(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " on "} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(config.EnvOnlyQueue, v)
			role, err := config.ResolveProcessRole()
			require.NoError(t, err)
			assert.Equal(t, config.RoleQueue, role)
		})
	}
}

// タイポを黙って偽扱いすると、配送ノードが Web ノードとして起動して
// 「ジョブが溜まり続ける」形で表面化する。起動時に落とす。
func TestResolveProcessRole_InvalidValueErrors(t *testing.T) {
	t.Setenv(config.EnvOnlyQueue, "ture")

	_, err := config.ResolveProcessRole()
	require.Error(t, err)
	assert.Contains(t, err.Error(), config.EnvOnlyQueue)
}

// 両方の変数で同じ検査が効くこと。片方だけ厳しくしても、タイポした側が
// 素通りするなら意味が無い。
func TestResolveProcessRole_InvalidOnlyServerErrors(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "yep")

	_, err := config.ResolveProcessRole()
	require.Error(t, err)
	assert.Contains(t, err.Error(), config.EnvOnlyServer)
}

// upstream は onlyServer を優先して黙って続行するが、矛盾した設定は運用ミス
// なので落とす (意図的な差分)。
func TestResolveProcessRole_BothSetErrors(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "1")

	_, err := config.ResolveProcessRole()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// 片方が偽なら矛盾ではない。
func TestResolveProcessRole_BothSetButOneFalsy(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "0")

	role, err := config.ResolveProcessRole()
	require.NoError(t, err)
	assert.Equal(t, config.RoleServer, role)
}
