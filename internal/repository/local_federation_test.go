package repository

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestLocalFederationRepository_ObserveExternalReferenceRecordsOnlyFirstObservation(t *testing.T) {
	user := insertTestUser(t, "lfobs01", "lfobserve")
	defer cleanupUser(t, user.ID)
	repo := NewLocalFederationRepository(testDB)
	firstAt := time.Date(2026, 10, 26, 1, 2, 3, 0, time.UTC)
	secondAt := firstAt.Add(time.Minute)

	first, recorded, err := repo.ObserveExternalReference(user.ID, "actor", "peer.example", firstAt)
	require.NoError(t, err)
	assert.True(t, recorded)
	require.NotNil(t, first.FirstExternalReferenceAt)
	assert.True(t, firstAt.Equal(*first.FirstExternalReferenceAt))
	assert.Equal(t, "actor", *first.FirstExternalReferenceReason)
	assert.Equal(t, "peer.example", *first.FirstExternalReferenceHost)

	second, recorded, err := repo.ObserveExternalReference(user.ID, "outbound", "other.example", secondAt)
	require.NoError(t, err)
	assert.False(t, recorded)
	require.NotNil(t, second.FirstExternalReferenceAt)
	assert.True(t, firstAt.Equal(*second.FirstExternalReferenceAt))
	assert.Equal(t, "actor", *second.FirstExternalReferenceReason)
	assert.Equal(t, "peer.example", *second.FirstExternalReferenceHost)
}

func TestLocalFederationRepository_ChangeStatusOffConvertsNotesAndDeletesKeys(t *testing.T) {
	user := insertTestUser(t, "lfoff01", "lfoffuser")
	defer cleanupUser(t, user.ID)
	note := &model.Note{ID: "lfoffn1", UserID: user.ID, Visibility: model.NoteVisibilityPublic}
	require.NoError(t, testDB.Create(note).Error)
	t.Cleanup(func() { testDB.Delete(&model.Note{}, "id = ?", note.ID) })
	require.NoError(t, NewUserKeypairRepository(testDB).Create(&model.UserKeypair{UserID: user.ID, PublicKey: "pub", PrivateKey: "priv"}))
	require.NoError(t, NewUserKeypairExtraRepository(testDB).Upsert(&model.UserKeypairExtra{UserID: user.ID, Ed25519PublicKey: "edpub", Ed25519PrivateKey: "edpriv"}))
	repo := NewLocalFederationRepository(testDB)

	updated, changed, err := repo.ChangeStatus(user.ID, model.FederationStatusOff, time.Now(), nil, nil)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, model.FederationStatusOff, updated.FederationStatus)
	gotNote, err := NewNoteRepository(testDB).FindByID(note.ID)
	require.NoError(t, err)
	assert.True(t, gotNote.LocalOnly)
	_, err = NewUserKeypairRepository(testDB).FindByUserID(user.ID)
	assert.Error(t, err)
	_, err = NewUserKeypairExtraRepository(testDB).FindByUserID(user.ID)
	assert.Error(t, err)
}

func TestLocalFederationRepository_ChangeStatusOffConvertsOnlyScheduledDrafts(t *testing.T) {
	user := insertTestUser(t, "lfdraft01", "lfdraftuser")
	defer cleanupUser(t, user.ID)
	scheduledAt := time.Now().Add(time.Hour)
	drafts := []*model.NoteDraft{
		{ID: "lfdraft-scheduled", UserID: user.ID, IsActuallyScheduled: true, ScheduledAt: &scheduledAt},
		{ID: "lfdraft-ordinary", UserID: user.ID},
		{ID: "lfdraft-already", UserID: user.ID, IsActuallyScheduled: true, ScheduledAt: &scheduledAt, LocalOnly: true},
	}
	for _, draft := range drafts {
		require.NoError(t, testDB.Create(draft).Error)
		draftID := draft.ID
		t.Cleanup(func() { testDB.Delete(&model.NoteDraft{}, "id = ?", draftID) })
	}

	_, changed, err := NewLocalFederationRepository(testDB).ChangeStatus(user.ID, model.FederationStatusOff, time.Now(), nil, nil)
	require.NoError(t, err)
	require.True(t, changed)
	var scheduled, ordinary, alreadyLocal model.NoteDraft
	require.NoError(t, testDB.First(&scheduled, "id = ?", "lfdraft-scheduled").Error)
	assert.True(t, scheduled.LocalOnly)
	require.NoError(t, testDB.First(&ordinary, "id = ?", "lfdraft-ordinary").Error)
	assert.False(t, ordinary.LocalOnly, "ordinary unscheduled drafts retain their prior behavior")
	require.NoError(t, testDB.First(&alreadyLocal, "id = ?", "lfdraft-already").Error)
	assert.True(t, alreadyLocal.LocalOnly)
}

func TestLocalFederationRepository_ChangeStatusOffRejectsReferencedAccount(t *testing.T) {
	user := insertTestUser(t, "lfref01", "lfrefuser")
	defer cleanupUser(t, user.ID)
	repo := NewLocalFederationRepository(testDB)
	observedAt := time.Date(2026, 10, 26, 1, 2, 3, 0, time.UTC)
	_, recorded, err := repo.ObserveExternalReference(user.ID, "actor", "peer.example", observedAt)
	require.NoError(t, err)
	assert.True(t, recorded)

	updated, changed, err := repo.ChangeStatus(user.ID, model.FederationStatusOff, observedAt.Add(time.Minute), nil, nil)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, model.FederationStatusOn, updated.FederationStatus)
	assert.True(t, observedAt.Equal(*updated.FirstExternalReferenceAt))
}

func TestLocalFederationRepository_ChangeStatusOnCreatesKeys(t *testing.T) {
	user := insertTestUser(t, "lfon01", "lfonuser")
	defer cleanupUser(t, user.ID)
	require.NoError(t, testDB.Model(&model.User{}).Where("id = ?", user.ID).Update("federationStatus", model.FederationStatusOff).Error)
	repo := NewLocalFederationRepository(testDB)
	at := time.Date(2026, 10, 26, 1, 2, 3, 0, time.UTC)
	rsa := &model.UserKeypair{UserID: user.ID, PublicKey: "rsa-public", PrivateKey: "rsa-private"}
	ed := &model.UserKeypairExtra{UserID: user.ID, Ed25519PublicKey: "ed-public", Ed25519PrivateKey: "ed-private"}

	updated, changed, err := repo.ChangeStatus(user.ID, model.FederationStatusOn, at, rsa, ed)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, model.FederationStatusOn, updated.FederationStatus)
	require.NotNil(t, updated.FederationEnabledAt)
	assert.True(t, at.Equal(*updated.FederationEnabledAt))
	gotRSA, err := NewUserKeypairRepository(testDB).FindByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, "rsa-public", gotRSA.PublicKey)
	gotEd, err := NewUserKeypairExtraRepository(testDB).FindByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, "ed-public", gotEd.Ed25519PublicKey)
}

func TestCachedLocalFederationRepository_InvalidatesUserCaches(t *testing.T) {
	user := insertTestUser(t, "lfcache01", "lfcacheuser")
	defer cleanupUser(t, user.ID)
	require.NoError(t, testDB.Model(&model.User{}).Where("id = ?", user.ID).
		Update("federationStatus", model.FederationStatusOff).Error)
	cacheA := NewCachedUserRepository(NewUserRepository(testDB))
	cacheB := NewCachedUserRepository(NewUserRepository(testDB))
	invalidations := 0
	cacheA.SetInvalidationHook(func(inv UserCacheInvalidation) {
		invalidations++
		cacheB.ApplyRemoteInvalidation(inv)
	})
	for _, cache := range []*CachedUserRepository{cacheA, cacheB} {
		cached, err := cache.FindByID(user.ID)
		require.NoError(t, err)
		assert.Equal(t, model.FederationStatusOff, cached.FederationStatus)
	}

	repo := NewCachedLocalFederationRepository(NewLocalFederationRepository(testDB), cacheA)
	rsa := &model.UserKeypair{UserID: user.ID, PublicKey: "cache-rsa-public", PrivateKey: "cache-rsa-private"}
	_, changed, err := repo.ChangeStatus(user.ID, model.FederationStatusOn, time.Now(), rsa, nil)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 1, invalidations)
	for _, cache := range []*CachedUserRepository{cacheA, cacheB} {
		cached, err := cache.FindByID(user.ID)
		require.NoError(t, err)
		assert.Equal(t, model.FederationStatusOn, cached.FederationStatus)
	}
	_, changed, err = repo.ChangeStatus(user.ID, model.FederationStatusOn, time.Now(), nil, nil)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, 1, invalidations, "a status no-op must not invalidate or publish")

	firstAt := time.Date(2026, 10, 27, 3, 4, 5, 0, time.UTC)
	_, recorded, err := repo.ObserveExternalReference(user.ID, "actor", "peer.example", firstAt)
	require.NoError(t, err)
	assert.True(t, recorded)
	assert.Equal(t, 2, invalidations)
	for _, cache := range []*CachedUserRepository{cacheA, cacheB} {
		cached, err := cache.FindByID(user.ID)
		require.NoError(t, err)
		require.NotNil(t, cached.FirstExternalReferenceAt)
		assert.True(t, firstAt.Equal(*cached.FirstExternalReferenceAt))
	}
	_, recorded, err = repo.ObserveExternalReference(user.ID, "delivery", "other.example", firstAt.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, recorded)
	assert.Equal(t, 2, invalidations, "a repeated observation must not invalidate or publish")
}

func TestLocalFederationRepository_WithLockedUserRollsBackCallbackError(t *testing.T) {
	user := insertTestUser(t, "lflock1", "lflockuser")
	defer cleanupUser(t, user.ID)
	repo := NewLocalFederationRepository(testDB)
	err := repo.WithLockedUser(user.ID, func(tx *gorm.DB, locked *model.User) error {
		assert.Equal(t, user.ID, locked.ID)
		if err := tx.Model(&model.User{}).Where("id = ?", user.ID).Update("federationStatus", model.FederationStatusOff).Error; err != nil {
			return err
		}
		return gorm.ErrInvalidTransaction
	})
	require.ErrorIs(t, err, gorm.ErrInvalidTransaction)
	updated, err := repo.FindUser(user.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FederationStatusOn, updated.FederationStatus)
}

func TestLocalFederationRepository_ObserveAndDisableSerialize(t *testing.T) {
	repo := NewLocalFederationRepository(testDB)
	for i := 0; i < 5; i++ {
		userID := fmt.Sprintf("lfrace%d", i)
		user := insertTestUser(t, userID, "lfraceuser"+fmt.Sprint(i))
		start := make(chan struct{})
		var wg sync.WaitGroup
		var recorded, disabled bool
		var observeErr error
		var changeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, recorded, observeErr = repo.ObserveExternalReference(user.ID, "actor", "peer.example", time.Now())
		}()
		go func() {
			defer wg.Done()
			<-start
			_, disabled, changeErr = repo.ChangeStatus(user.ID, model.FederationStatusOff, time.Now(), nil, nil)
		}()
		close(start)
		wg.Wait()
		require.NoError(t, observeErr)
		require.NoError(t, changeErr)
		updated, err := repo.FindUser(user.ID)
		require.NoError(t, err)
		if updated.FederationStatus == model.FederationStatusOff {
			assert.True(t, disabled)
			assert.False(t, recorded)
			assert.Nil(t, updated.FirstExternalReferenceAt)
		} else {
			assert.Equal(t, model.FederationStatusOn, updated.FederationStatus)
			assert.False(t, disabled)
			assert.True(t, recorded)
			assert.NotNil(t, updated.FirstExternalReferenceAt)
		}
		cleanupUser(t, user.ID)
	}
}
