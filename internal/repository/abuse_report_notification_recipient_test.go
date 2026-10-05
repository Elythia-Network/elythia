package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupRecipient(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "abuse_report_notification_recipient" WHERE id = ?`, id)
}

func TestAbuseReportNotificationRecipientRepository_CRUD(t *testing.T) {
	repo := NewAbuseReportNotificationRecipientRepository(testDB)

	r := &model.AbuseReportNotificationRecipient{
		ID:       "arn_1",
		Name:     "ops",
		Method:   "email",
		IsActive: true,
	}
	require.NoError(t, repo.Create(r))
	defer cleanupRecipient(t, r.ID)

	found, err := repo.FindByID(r.ID)
	require.NoError(t, err)
	assert.Equal(t, "ops", found.Name)
	assert.Equal(t, "email", found.Method)

	// List は少なくとも自身を含む
	rows, err := repo.List()
	require.NoError(t, err)
	var ids []string
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	assert.Contains(t, ids, r.ID)

	// Update の partial field 適用
	require.NoError(t, repo.Update(r.ID, map[string]any{
		"name":     "ops-updated",
		"method":   "webhook",
		"isActive": false,
	}))
	after, err := repo.FindByID(r.ID)
	require.NoError(t, err)
	assert.Equal(t, "ops-updated", after.Name)
	assert.Equal(t, "webhook", after.Method)
	assert.False(t, after.IsActive)

	// fields が空なら no-op
	require.NoError(t, repo.Update(r.ID, map[string]any{}))

	// Delete
	require.NoError(t, repo.Delete(r.ID))
	_, err = repo.FindByID(r.ID)
	assert.Error(t, err)
}

func TestAbuseReportNotificationRecipientRepository_FindByIDNotFound(t *testing.T) {
	repo := NewAbuseReportNotificationRecipientRepository(testDB)
	_, err := repo.FindByID("nonexistent-id-xxx")
	assert.Error(t, err)
}
