package pgrepo

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/pluginsecret"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

func TestRepo_RoundTripAndNotFound(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)
	clean := func() {
		require.NoError(t, db.Exec(`DELETE FROM "plugin_secret" WHERE "pluginName" IN ('pg-a', 'pg-b')`).Error)
	}
	clean()
	t.Cleanup(clean)
	ctx := context.Background()
	r := New(repository.NewPluginSecretRepository(db))

	_, err := r.FindByName(ctx, "pg-a", "apiKey")
	assert.ErrorIs(t, err, pluginsecret.ErrNotFound)

	at := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	require.NoError(t, r.Upsert(ctx, pluginsecret.Row{PluginName: "pg-a", Name: "b", Ciphertext: []byte{1, 2}, UpdatedAt: at}))
	require.NoError(t, r.Upsert(ctx, pluginsecret.Row{PluginName: "pg-a", Name: "a", Ciphertext: []byte{3}, UpdatedAt: at}))
	require.NoError(t, r.Upsert(ctx, pluginsecret.Row{PluginName: "pg-b", Name: "a", Ciphertext: []byte{4}, UpdatedAt: at}))

	got, err := r.FindByName(ctx, "pg-a", "b")
	require.NoError(t, err)
	assert.Equal(t, "pg-a", got.PluginName)
	assert.Equal(t, "b", got.Name)
	assert.Equal(t, []byte{1, 2}, got.Ciphertext)
	assert.True(t, got.UpdatedAt.Equal(at))

	rows, err := r.ListByPlugin(ctx, "pg-a")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "a", rows[0].Name)
	assert.Equal(t, []byte{3}, rows[0].Ciphertext)

	require.NoError(t, r.Delete(ctx, "pg-a", "a"))
	_, err = r.FindByName(ctx, "pg-a", "a")
	assert.ErrorIs(t, err, pluginsecret.ErrNotFound)
}

// **DB の障害を ErrNotFound に化けさせない。** 化けると service が「未設定」と
// 答えて、障害が正常な状態に見える。
func TestRepo_DBErrorsAreNotNotFound(t *testing.T) {
	db := testutil.MustOpenTestDB()
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	r := New(repository.NewPluginSecretRepository(db))
	ctx := context.Background()

	_, err = r.FindByName(ctx, "pg-a", "a")
	require.Error(t, err)
	assert.NotErrorIs(t, err, pluginsecret.ErrNotFound)
	_, err = r.ListByPlugin(ctx, "pg-a")
	assert.Error(t, err)
}
