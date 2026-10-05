package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebhookRepository_Full(t *testing.T) {
	repo := NewWebhookRepository(testDB)
	user := insertTestUser(t, "u_wh_1", "webhookuser")
	defer cleanupUser(t, user.ID)

	// Create
	w := &model.Webhook{
		ID:     "wh_1",
		UserID: user.ID,
		Name:   "Test Hook",
		On:     model.StringArray{"note", "follow"},
		URL:    "https://example.com/hook",
		Secret: "secret123",
		Active: true,
	}
	require.NoError(t, repo.Create(w))
	defer testDB.Exec(`DELETE FROM "webhook" WHERE id = ?`, w.ID)

	// FindByID
	found, err := repo.FindByID("wh_1")
	require.NoError(t, err)
	assert.Equal(t, "Test Hook", found.Name)

	// FindByID - not found
	_, err = repo.FindByID("ghost")
	assert.Error(t, err)

	// FindByIDAndUserID
	found2, err := repo.FindByIDAndUserID("wh_1", user.ID)
	require.NoError(t, err)
	assert.Equal(t, "Test Hook", found2.Name)

	// FindByIDAndUserID - wrong user
	_, err = repo.FindByIDAndUserID("wh_1", "wrong")
	assert.Error(t, err)

	// ListByUserID
	list, err := repo.ListByUserID(user.ID)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	// CountByUserID
	count, err := repo.CountByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	// Update
	found.Name = "Updated Hook"
	require.NoError(t, repo.Update(found))
	updated, err := repo.FindByID("wh_1")
	require.NoError(t, err)
	assert.Equal(t, "Updated Hook", updated.Name)

	// Delete
	require.NoError(t, repo.Delete("wh_1", user.ID))
	_, err = repo.FindByID("wh_1")
	assert.Error(t, err)
}

func TestWebhookRepository_ListActiveByUserID(t *testing.T) {
	repo := NewWebhookRepository(testDB)
	user := insertTestUser(t, "u_wh_2", "activeuser")
	defer cleanupUser(t, user.ID)

	active := &model.Webhook{
		ID: "wh_act_1", UserID: user.ID, Name: "Active", URL: "https://a",
		On: model.StringArray{"note"}, Active: true,
	}
	inactive := &model.Webhook{
		ID: "wh_act_2", UserID: user.ID, Name: "Inactive", URL: "https://b",
		On: model.StringArray{"note"}, Active: true,
	}
	require.NoError(t, repo.Create(active))
	require.NoError(t, repo.Create(inactive))
	defer testDB.Exec(`DELETE FROM "webhook" WHERE "userId" = ?`, user.ID)
	// GORM のdefault:trueがzero値をそのまま落とすため、Createで false を
	// 書き込めない。明示的に UPDATE で flip する。
	require.NoError(t, testDB.Exec(`UPDATE "webhook" SET active = false WHERE id = ?`, "wh_act_2").Error)

	list, err := repo.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "wh_act_1", list[0].ID)
}

func TestWebhookRepository_UpdateLatestStatus(t *testing.T) {
	repo := NewWebhookRepository(testDB)
	user := insertTestUser(t, "u_wh_3", "statususer")
	defer cleanupUser(t, user.ID)

	w := &model.Webhook{
		ID: "wh_st_1", UserID: user.ID, Name: "Status Hook", URL: "https://a",
		On: model.StringArray{"note"}, Active: true,
	}
	require.NoError(t, repo.Create(w))
	defer testDB.Exec(`DELETE FROM "webhook" WHERE id = ?`, w.ID)

	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, repo.UpdateLatestStatus("wh_st_1", now, 200))

	got, err := repo.FindByID("wh_st_1")
	require.NoError(t, err)
	require.NotNil(t, got.LatestStatus)
	assert.Equal(t, 200, *got.LatestStatus)
	require.NotNil(t, got.LatestSentAt)
	// Name etc. unchanged
	assert.Equal(t, "Status Hook", got.Name)
}
