package repository

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func cleanupRegistry(t *testing.T, userID string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "registry_item" WHERE "userId" = ?`, userID)
}

func TestRegistryRepository_SetAndGet(t *testing.T) {
	repo := NewRegistryRepository(testDB)
	createTestUser(t, "reg_u1")
	defer cleanupRegistry(t, "reg_u1")

	item := &model.RegistryItem{
		ID:     "ri_1",
		UserID: "reg_u1",
		Key:    "theme",
		Value:  datatypes.JSON([]byte(`"dark"`)),
		Scope:  []string{},
	}
	require.NoError(t, repo.Set(item))

	found, err := repo.Get("reg_u1", "theme", []string{}, nil)
	require.NoError(t, err)
	assert.Equal(t, "theme", found.Key)
	assert.Contains(t, string(found.Value), "dark")
}

func TestRegistryRepository_SetUpdate(t *testing.T) {
	repo := NewRegistryRepository(testDB)
	createTestUser(t, "reg_u2")
	defer cleanupRegistry(t, "reg_u2")

	item := &model.RegistryItem{
		ID: "ri_2", UserID: "reg_u2", Key: "lang",
		Value: datatypes.JSON([]byte(`"en"`)), Scope: []string{},
	}
	require.NoError(t, repo.Set(item))

	// 同じキーで更新
	item2 := &model.RegistryItem{
		ID: "ri_2b", UserID: "reg_u2", Key: "lang",
		Value: datatypes.JSON([]byte(`"ja"`)), Scope: []string{},
	}
	require.NoError(t, repo.Set(item2))

	found, err := repo.Get("reg_u2", "lang", []string{}, nil)
	require.NoError(t, err)
	assert.Contains(t, string(found.Value), "ja")
}

func TestRegistryRepository_Get_NotFound(t *testing.T) {
	repo := NewRegistryRepository(testDB)
	_, err := repo.Get("ghost", "key", []string{}, nil)
	assert.Error(t, err)
}

func TestRegistryRepository_GetAll(t *testing.T) {
	repo := NewRegistryRepository(testDB)
	createTestUser(t, "reg_u3")
	defer cleanupRegistry(t, "reg_u3")

	require.NoError(t, repo.Set(&model.RegistryItem{ID: "ri_3a", UserID: "reg_u3", Key: "k1", Value: []byte(`1`), Scope: []string{}}))
	require.NoError(t, repo.Set(&model.RegistryItem{ID: "ri_3b", UserID: "reg_u3", Key: "k2", Value: []byte(`2`), Scope: []string{}}))

	items, err := repo.GetAll("reg_u3", []string{}, nil)
	require.NoError(t, err)
	assert.Len(t, items, 2)
}

func TestRegistryRepository_KeysWithType(t *testing.T) {
	repo := NewRegistryRepository(testDB)
	createTestUser(t, "reg_u4")
	defer cleanupRegistry(t, "reg_u4")

	require.NoError(t, repo.Set(&model.RegistryItem{ID: "ri_4a", UserID: "reg_u4", Key: "str", Value: []byte(`"hello"`), Scope: []string{}}))
	require.NoError(t, repo.Set(&model.RegistryItem{ID: "ri_4b", UserID: "reg_u4", Key: "num", Value: []byte(`42`), Scope: []string{}}))
	require.NoError(t, repo.Set(&model.RegistryItem{ID: "ri_4c", UserID: "reg_u4", Key: "bool", Value: []byte(`true`), Scope: []string{}}))
	require.NoError(t, repo.Set(&model.RegistryItem{ID: "ri_4d", UserID: "reg_u4", Key: "arr", Value: []byte(`[]`), Scope: []string{}}))
	require.NoError(t, repo.Set(&model.RegistryItem{ID: "ri_4e", UserID: "reg_u4", Key: "obj", Value: []byte(`{}`), Scope: []string{}}))
	require.NoError(t, repo.Set(&model.RegistryItem{ID: "ri_4f", UserID: "reg_u4", Key: "nil", Value: []byte(`null`), Scope: []string{}}))
	require.NoError(t, repo.Set(&model.RegistryItem{ID: "ri_4g", UserID: "reg_u4", Key: "empty", Value: []byte(``), Scope: []string{}}))
	require.NoError(t, repo.Set(&model.RegistryItem{ID: "ri_4h", UserID: "reg_u4", Key: "false", Value: []byte(`false`), Scope: []string{}}))

	keys, err := repo.KeysWithType("reg_u4", []string{}, nil)
	require.NoError(t, err)
	assert.Equal(t, "string", keys["str"])
	assert.Equal(t, "number", keys["num"])
	assert.Equal(t, "boolean", keys["bool"])
	assert.Equal(t, "array", keys["arr"])
	assert.Equal(t, "object", keys["obj"])
	assert.Equal(t, "null", keys["nil"])
	assert.Equal(t, "null", keys["empty"])
	assert.Equal(t, "boolean", keys["false"])
}

func TestRegistryRepository_Remove(t *testing.T) {
	repo := NewRegistryRepository(testDB)
	createTestUser(t, "reg_u5")
	defer cleanupRegistry(t, "reg_u5")

	require.NoError(t, repo.Set(&model.RegistryItem{ID: "ri_5", UserID: "reg_u5", Key: "temp", Value: []byte(`1`), Scope: []string{}}))

	require.NoError(t, repo.Remove("reg_u5", "temp", []string{}, nil))
	_, err := repo.Get("reg_u5", "temp", []string{}, nil)
	assert.Error(t, err)
}

func TestRegistryRepository_ScopeQuery_WithDomain(t *testing.T) {
	repo := NewRegistryRepository(testDB)
	createTestUser(t, "reg_u6")
	defer cleanupRegistry(t, "reg_u6")

	domain := "app1"
	require.NoError(t, repo.Set(&model.RegistryItem{ID: "ri_6", UserID: "reg_u6", Key: "k", Value: []byte(`1`), Scope: []string{"client"}, Domain: &domain}))

	found, err := repo.Get("reg_u6", "k", []string{"client"}, &domain)
	require.NoError(t, err)
	assert.Equal(t, "k", found.Key)

	// domain なしでは見つからない
	_, err = repo.Get("reg_u6", "k", []string{"client"}, nil)
	assert.Error(t, err)
}

func TestRegistryRepository_KeysWithType_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewRegistryRepository(testDB.WithContext(ctx))
	_, err := repo.KeysWithType("x", []string{}, nil)
	assert.Error(t, err)
}

func TestRegistryRepository_GetAll_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewRegistryRepository(testDB.WithContext(ctx))
	_, err := repo.GetAll("x", []string{}, nil)
	assert.Error(t, err)
}

func TestRegistryRepository_Remove_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewRegistryRepository(testDB.WithContext(ctx))
	err := repo.Remove("x", "k", []string{}, nil)
	assert.Error(t, err)
}
