package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupAvatarDeco(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "avatar_decoration" WHERE id = ?`, id)
}

func TestAvatarDecorationRepository_CRUD(t *testing.T) {
	repo := NewAvatarDecorationRepository(testDB)

	d := &model.AvatarDecoration{
		ID:      "ad_1",
		Name:    "deco",
		URL:     "https://example.com/d.png",
		RoleIDs: model.StringArray{"role1"},
	}
	require.NoError(t, repo.Create(d))
	defer cleanupAvatarDeco(t, d.ID)

	got, err := repo.FindByID(d.ID)
	require.NoError(t, err)
	assert.Equal(t, "deco", got.Name)

	_, err = repo.FindByID("ghost")
	assert.Error(t, err)

	rows, err := repo.List()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(rows), 1)

	now := time.Now()
	require.NoError(t, repo.UpdateFields(d.ID, map[string]any{
		"name":      "renamed",
		"updatedAt": now,
	}))
	got, err = repo.FindByID(d.ID)
	require.NoError(t, err)
	assert.Equal(t, "renamed", got.Name)
	require.NotNil(t, got.UpdatedAt)

	// empty fields は no-op
	require.NoError(t, repo.UpdateFields(d.ID, map[string]any{}))

	require.NoError(t, repo.Delete(d.ID))
	_, err = repo.FindByID(d.ID)
	assert.Error(t, err)
}
