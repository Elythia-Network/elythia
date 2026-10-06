package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mk-go には /tarball/ を配信するルートが無いので、設定が true でも
// providesTarball は false のまま返す (#2700)。true を返すと frontend が
// /tarball/misskey-<version>.tar.gz へのリンクを表示するが、そこは SPA catchall
// (`GET /*`) が拾うので **404 にすらならず HTML が 200 で返る** (実測)。
func TestProvidesTarball_AlwaysFalse(t *testing.T) {
	cfg := &Config{PublishTarballInsteadOfProvideRepositoryURL: true}
	assert.False(t, cfg.ProvidesTarball(), "設定が true でも /tarball/ は無いので false")

	cfg.PublishTarballInsteadOfProvideRepositoryURL = false
	assert.False(t, cfg.ProvidesTarball())
}

// 設定は読むが効かないので、黙って無視せず起動時に警告する。これが唯一
// operator に伝わる経路なので、消えたら気付けるようテストで固定する。
func TestLoad_WarnsWhenTarballFlagIsEnabled(t *testing.T) {
	// slog の default はプロセス共有なので、必ず元へ戻す (#2795)。
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	path := writeTestConfig(t, testYAML+"\npublishTarballInsteadOfProvideRepositoryUrl: true\n")
	cfg, err := Load(path)
	require.NoError(t, err)

	// 設定値そのものは保持する (読み取りは忠実に行い、公開時に落とす)。
	assert.True(t, cfg.PublishTarballInsteadOfProvideRepositoryURL)
	assert.False(t, cfg.ProvidesTarball())
	assert.Contains(t, buf.String(), "publishTarballInsteadOfProvideRepositoryUrl", "無視している旨の警告が出ていない")
}

func TestLoad_NoTarballWarnWhenDisabled(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	path := writeTestConfig(t, testYAML)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.False(t, cfg.PublishTarballInsteadOfProvideRepositoryURL)
	assert.False(t, strings.Contains(buf.String(), "publishTarballInsteadOfProvideRepositoryUrl"), "無効なのに警告が出ている")
}

// nodeinfo の software.repository と meta.repositoryUrl の既定値は同じ値なので
// config 側の定数に一本化してある (#2700)。
func TestMkGoRepositoryURL(t *testing.T) {
	assert.Equal(t, "https://github.com/Elythia-Network/elythia", MkGoRepositoryURL)
}
