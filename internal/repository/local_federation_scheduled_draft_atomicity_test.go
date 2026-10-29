package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChangeStatusOffScheduledDraftsAreUserScopedAndStayLocalAfterReenable(t *testing.T) {
	user := insertTestUser(t, "lfdrafttx1", "lfdrafttxuser1")
	other := insertTestUser(t, "lfdrafttx2", "lfdrafttxuser2")
	scheduledAt := time.Now().Add(time.Hour)
	drafts := []*model.NoteDraft{
		{ID: "lfdrafttx-scheduled", UserID: user.ID, IsActuallyScheduled: true, ScheduledAt: &scheduledAt},
		{ID: "lfdrafttx-ordinary", UserID: user.ID},
		{ID: "lfdrafttx-other", UserID: other.ID, IsActuallyScheduled: true, ScheduledAt: &scheduledAt},
	}
	for _, draft := range drafts {
		require.NoError(t, testDB.Create(draft).Error)
	}
	t.Cleanup(func() {
		for _, draft := range drafts {
			_ = testDB.Delete(&model.NoteDraft{}, "id = ?", draft.ID).Error
		}
		_ = testDB.Delete(&model.UserKeypair{}, `"userId" = ?`, user.ID).Error
		cleanupUser(t, other.ID)
		cleanupUser(t, user.ID)
	})

	repo := NewLocalFederationRepository(testDB)
	_, changed, err := repo.ChangeStatus(user.ID, model.FederationStatusOff, time.Now(), nil, nil)
	require.NoError(t, err)
	require.True(t, changed)

	var got model.NoteDraft
	require.NoError(t, testDB.First(&got, "id = ?", "lfdrafttx-scheduled").Error)
	assert.True(t, got.LocalOnly)
	got = model.NoteDraft{}
	require.NoError(t, testDB.First(&got, "id = ?", "lfdrafttx-ordinary").Error)
	assert.False(t, got.LocalOnly, "ordinary unscheduled drafts are outside the OFF conversion")
	got = model.NoteDraft{}
	require.NoError(t, testDB.First(&got, "id = ?", "lfdrafttx-other").Error)
	assert.False(t, got.LocalOnly, "another account's scheduled draft is not changed")

	key := &model.UserKeypair{UserID: user.ID, PublicKey: "reenable-public", PrivateKey: "reenable-private"}
	_, changed, err = repo.ChangeStatus(user.ID, model.FederationStatusOn, time.Now(), key, nil)
	require.NoError(t, err)
	require.True(t, changed)
	got = model.NoteDraft{}
	require.NoError(t, testDB.First(&got, "id = ?", "lfdrafttx-scheduled").Error)
	assert.True(t, got.LocalOnly, "reenabling federation must not make the scheduled draft externally visible")
}

func TestChangeStatusOffRollbackRestoresScheduledDraftAndAccountState(t *testing.T) {
	user := insertTestUser(t, "lfdrafttxrb", "lfdraftrollback")
	draftID := "lfdrafttx-rb-scheduled"
	scheduledAt := time.Now().Add(time.Hour)
	require.NoError(t, testDB.Create(&model.NoteDraft{
		ID: draftID, UserID: user.ID, IsActuallyScheduled: true, ScheduledAt: &scheduledAt,
	}).Error)
	key := &model.UserKeypair{UserID: user.ID, PublicKey: "rollback-public", PrivateKey: "rollback-private"}
	require.NoError(t, NewUserKeypairRepository(testDB).Create(key))
	const constraint = "test_lf_scheduled_draft_status_guard"
	t.Cleanup(func() {
		_ = testDB.Exec(`ALTER TABLE "user" DROP CONSTRAINT IF EXISTS ` + constraint).Error
		_ = testDB.Delete(&model.NoteDraft{}, "id = ?", draftID).Error
		_ = testDB.Delete(&model.UserKeypair{}, `"userId" = ?`, user.ID).Error
		cleanupUser(t, user.ID)
	})
	require.NoError(t, testDB.Exec(`ALTER TABLE "user" ADD CONSTRAINT `+constraint+
		` CHECK (id <> '`+user.ID+`' OR "federationStatus" <> 'off')`).Error)

	_, _, err := NewLocalFederationRepository(testDB).ChangeStatus(user.ID, model.FederationStatusOff, time.Now(), nil, nil)
	require.Error(t, err, "the status update constraint must abort the conversion transaction")

	updated, err := NewLocalFederationRepository(testDB).FindUser(user.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FederationStatusOn, updated.FederationStatus)
	var draft model.NoteDraft
	require.NoError(t, testDB.First(&draft, "id = ?", draftID).Error)
	assert.False(t, draft.LocalOnly, "draft conversion must roll back with the failed status update")
	_, err = NewUserKeypairRepository(testDB).FindByUserID(user.ID)
	require.NoError(t, err, "key deletion must roll back with the failed status update")
}
