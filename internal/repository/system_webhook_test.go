package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemWebhookRepository_Full(t *testing.T) {
	repo := NewSystemWebhookRepository(testDB)

	sw := &model.SystemWebhook{
		ID: "sw_1", Name: "System Hook", URL: "https://example.com/s",
		On: model.StringArray{"userCreated"}, IsActive: true, UpdatedAt: time.Now(),
	}
	require.NoError(t, repo.Create(sw))
	defer testDB.Exec(`DELETE FROM "system_webhook" WHERE id = ?`, sw.ID)

	// FindByID
	got, err := repo.FindByID("sw_1")
	require.NoError(t, err)
	assert.Equal(t, "System Hook", got.Name)

	// FindByID not found
	_, err = repo.FindByID("ghost_sw")
	assert.Error(t, err)

	// List
	list, err := repo.List()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(list), 1)

	// ListActive
	active, err := repo.ListActive()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(active), 1)

	// Update
	got.Name = "Renamed"
	require.NoError(t, repo.Update(got))
	updated, err := repo.FindByID("sw_1")
	require.NoError(t, err)
	assert.Equal(t, "Renamed", updated.Name)

	// UpdateLatestStatus
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, repo.UpdateLatestStatus("sw_1", now, 202))
	got2, err := repo.FindByID("sw_1")
	require.NoError(t, err)
	require.NotNil(t, got2.LatestStatus)
	assert.Equal(t, 202, *got2.LatestStatus)

	// Delete
	require.NoError(t, repo.Delete("sw_1"))
	_, err = repo.FindByID("sw_1")
	assert.Error(t, err)
}

func TestSystemWebhookRepository_ListActiveFiltersInactive(t *testing.T) {
	repo := NewSystemWebhookRepository(testDB)

	active := &model.SystemWebhook{
		ID: "sw_a", Name: "A", URL: "https://a", On: model.StringArray{"userCreated"},
		IsActive: true, UpdatedAt: time.Now(),
	}
	inactive := &model.SystemWebhook{
		ID: "sw_i", Name: "I", URL: "https://i", On: model.StringArray{"userCreated"},
		IsActive: true, UpdatedAt: time.Now(),
	}
	require.NoError(t, repo.Create(active))
	require.NoError(t, repo.Create(inactive))
	defer testDB.Exec(`DELETE FROM "system_webhook" WHERE id IN (?, ?)`, "sw_a", "sw_i")
	// default:true の壁を回避するため UPDATE で flip する。
	require.NoError(t, testDB.Exec(`UPDATE "system_webhook" SET "isActive" = false WHERE id = ?`, "sw_i").Error)

	list, err := repo.ListActive()
	require.NoError(t, err)
	hasInactive := false
	for _, h := range list {
		if h.ID == "sw_i" {
			hasInactive = true
		}
	}
	assert.False(t, hasInactive, "inactive system webhooks must be filtered out")
}

func TestSystemWebhookRepository_UpdateAdminFieldsPreservesLatestStatus(t *testing.T) {
	repo := NewSystemWebhookRepository(testDB)

	sw := &model.SystemWebhook{
		ID: "sw_adm", Name: "orig", URL: "https://o", Secret: "s",
		On: model.StringArray{"userCreated"}, IsActive: true, UpdatedAt: time.Now(),
	}
	require.NoError(t, repo.Create(sw))
	defer testDB.Exec(`DELETE FROM "system_webhook" WHERE id = ?`, sw.ID)

	// 配送 processor が status を書き込んだ後の admin Update で
	// latestSentAt/latestStatus が保持されることを検証する。
	sentAt := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, repo.UpdateLatestStatus(sw.ID, sentAt, 202))

	require.NoError(t, repo.UpdateAdminFields(sw.ID, map[string]any{
		"name":     "renamed",
		"url":      "https://new",
		"isActive": false,
	}))

	got, err := repo.FindByID(sw.ID)
	require.NoError(t, err)
	assert.Equal(t, "renamed", got.Name)
	assert.Equal(t, "https://new", got.URL)
	assert.False(t, got.IsActive)
	require.NotNil(t, got.LatestStatus, "LatestStatus must not be cleared by admin update")
	assert.Equal(t, 202, *got.LatestStatus)
	require.NotNil(t, got.LatestSentAt, "LatestSentAt must not be cleared by admin update")
}

func TestSystemWebhookRepository_UpdateAdminFieldsEmptyIsNoop(t *testing.T) {
	repo := NewSystemWebhookRepository(testDB)
	// id が存在しなくても empty fields なら error にしない。
	require.NoError(t, repo.UpdateAdminFields("nonexistent", map[string]any{}))
}
