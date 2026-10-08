package repository

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

func TestPluginSecretRepository_CRUD(t *testing.T) {
	ctx := context.Background()
	clean := func() {
		require.NoError(t, testDB.Exec(`DELETE FROM "plugin_secret" WHERE "pluginName" IN ('repo-a', 'repo-b')`).Error)
	}
	clean()
	t.Cleanup(clean)
	repo := NewPluginSecretRepository(testDB)
	t0 := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)

	require.NoError(t, repo.Upsert(ctx, &model.PluginSecret{PluginName: "repo-a", Name: "z", SecretCiphertext: []byte{1}, UpdatedAt: t0}))
	require.NoError(t, repo.Upsert(ctx, &model.PluginSecret{PluginName: "repo-a", Name: "a", SecretCiphertext: []byte{2}, UpdatedAt: t0}))
	require.NoError(t, repo.Upsert(ctx, &model.PluginSecret{PluginName: "repo-b", Name: "a", SecretCiphertext: []byte{3}, UpdatedAt: t0}))

	// 同じ (pluginName, name) は上書きする。
	t1 := t0.Add(time.Hour)
	require.NoError(t, repo.Upsert(ctx, &model.PluginSecret{PluginName: "repo-a", Name: "a", SecretCiphertext: []byte{4, 5}, UpdatedAt: t1}))

	got, err := repo.FindByName(ctx, "repo-a", "a")
	require.NoError(t, err)
	assert.Equal(t, []byte{4, 5}, got.SecretCiphertext)
	assert.True(t, got.UpdatedAt.Equal(t1))

	list, err := repo.ListByPlugin(ctx, "repo-a")
	require.NoError(t, err)
	require.Len(t, list, 2, "他のプラグインの行が混ざっている")
	assert.Equal(t, "a", list[0].Name)
	assert.Equal(t, "z", list[1].Name)

	require.NoError(t, repo.Delete(ctx, "repo-a", "a"))
	_, err = repo.FindByName(ctx, "repo-a", "a")
	assert.True(t, IsNotFound(err))
	require.NoError(t, repo.Delete(ctx, "repo-a", "a"), "無い行の削除")

	other, err := repo.FindByName(ctx, "repo-b", "a")
	require.NoError(t, err, "別のプラグインの同名の行まで消した")
	assert.Equal(t, []byte{3}, other.SecretCiphertext)
}

// NUL を含む値は引く前に弾く (#3025)。
func TestPluginSecretRepository_NULNeverReachesTheQuery(t *testing.T) {
	ctx := context.Background()
	repo := NewPluginSecretRepository(testDB)
	_, err := repo.FindByName(ctx, "repo\x00a", "a")
	assert.True(t, IsNotFound(err))
	_, err = repo.FindByName(ctx, "repo-a", "a\x00")
	assert.True(t, IsNotFound(err))
	list, err := repo.ListByPlugin(ctx, "repo\x00a")
	require.NoError(t, err)
	assert.Empty(t, list)
	assert.NoError(t, repo.Delete(ctx, "repo\x00a", "a"))
}
