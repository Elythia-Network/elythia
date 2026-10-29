package localfederation

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var integrationDB *gorm.DB

func TestMain(m *testing.M) {
	db, err := testutil.OpenTestDB()
	if err != nil {
		panic("failed to open localfederation integration DB: " + err.Error())
	}
	integrationDB = db
	testutil.ApplyMigrations(db)
	os.Exit(m.Run())
}

func TestServiceWithPostgres_StateAndKeyLifecycle(t *testing.T) {
	repo := repository.NewLocalFederationRepository(integrationDB)
	service := NewService(repo, true)
	now := time.Date(2026, 10, 27, 3, 4, 5, 0, time.UTC)
	service.SetClock(func() time.Time { return now })

	pending := createFederationTestUser(t, model.FederationStatusPending)
	plainNote := createFederationTestNote(t, pending.ID, false)

	status, err := service.GetStatus(pending.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FederationStatusPending, status.FederationStatus)
	assert.False(t, status.IsSystemAccount)
	assert.Nil(t, status.FederationEnabledAt)
	assert.Nil(t, status.FirstExternalReferenceAt)

	status, err = service.ChangeStatus(pending.ID, model.FederationStatusOn)
	require.NoError(t, err)
	assert.Equal(t, model.FederationStatusOn, status.FederationStatus)
	require.NotNil(t, status.FederationEnabledAt)
	assert.True(t, now.Equal(*status.FederationEnabledAt))

	rsa, err := repository.NewUserKeypairRepository(integrationDB).FindByUserID(pending.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, rsa.PublicKey)
	assert.NotEmpty(t, rsa.PrivateKey)
	ed, err := repository.NewUserKeypairExtraRepository(integrationDB).FindByUserID(pending.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, ed.Ed25519PublicKey)
	assert.NotEmpty(t, ed.Ed25519PrivateKey)

	note, err := repository.NewNoteRepository(integrationDB).FindByID(plainNote.ID)
	require.NoError(t, err)
	assert.False(t, note.LocalOnly, "pending-to-on must preserve pre-existing ordinary notes")

	recordedAt := now.Add(time.Minute)
	service.SetClock(func() time.Time { return recordedAt })
	recorded, err := service.ObserveExternalReference(pending.ID, ExternalReferenceReasonActor, "peer.example")
	require.NoError(t, err)
	assert.True(t, recorded)
	status, err = service.GetStatus(pending.ID)
	require.NoError(t, err)
	require.NotNil(t, status.FirstExternalReferenceAt)
	assert.True(t, recordedAt.Equal(*status.FirstExternalReferenceAt))
	require.NotNil(t, status.FirstExternalReferenceReason)
	assert.Equal(t, ExternalReferenceReasonActor, *status.FirstExternalReferenceReason)
	require.NotNil(t, status.FirstExternalReferenceHost)
	assert.Equal(t, "peer.example", *status.FirstExternalReferenceHost)

	secondRecorded, err := service.ObserveExternalReference(pending.ID, ExternalReferenceReasonDelivery, "other.example")
	require.NoError(t, err)
	assert.False(t, secondRecorded)
	status, err = service.ChangeStatus(pending.ID, model.FederationStatusOff)
	require.ErrorIs(t, err, ErrFederationAlreadyReferenced)
	status, err = service.GetStatus(pending.ID)
	require.NoError(t, err)
	assert.True(t, recordedAt.Equal(*status.FirstExternalReferenceAt))
	assert.Equal(t, ExternalReferenceReasonActor, *status.FirstExternalReferenceReason)
	assert.Equal(t, "peer.example", *status.FirstExternalReferenceHost)

	off := createFederationTestUser(t, model.FederationStatusPending)
	oldNote := createFederationTestNote(t, off.ID, false)
	status, err = service.ChangeStatus(off.ID, model.FederationStatusOff)
	require.NoError(t, err)
	assert.Equal(t, model.FederationStatusOff, status.FederationStatus)
	note, err = repository.NewNoteRepository(integrationDB).FindByID(oldNote.ID)
	require.NoError(t, err)
	assert.True(t, note.LocalOnly, "pending-to-off must make existing notes local-only")
	_, err = repository.NewUserKeypairRepository(integrationDB).FindByUserID(off.ID)
	assert.Error(t, err)

	localOnlyNote := createFederationTestNote(t, off.ID, true)
	status, err = service.ChangeStatus(off.ID, model.FederationStatusOn)
	require.NoError(t, err)
	assert.Equal(t, model.FederationStatusOn, status.FederationStatus)
	assert.True(t, recordedAt.Equal(*status.FederationEnabledAt))
	rsa, err = repository.NewUserKeypairRepository(integrationDB).FindByUserID(off.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, rsa.PublicKey)
	ed, err = repository.NewUserKeypairExtraRepository(integrationDB).FindByUserID(off.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, ed.Ed25519PublicKey)
	note, err = repository.NewNoteRepository(integrationDB).FindByID(localOnlyNote.ID)
	require.NoError(t, err)
	assert.True(t, note.LocalOnly, "enabling federation must not broaden past local-only notes")

	status, err = service.ChangeStatus(off.ID, model.FederationStatusOn)
	require.NoError(t, err)
	assert.Equal(t, model.FederationStatusOn, status.FederationStatus)
	assert.True(t, recordedAt.Equal(*status.FederationEnabledAt))
}

func TestServiceWithPostgres_InvalidatesCachedUsersAfterFederationChanges(t *testing.T) {
	innerA := repository.NewUserRepository(integrationDB)
	innerB := repository.NewUserRepository(integrationDB)
	cacheA := repository.NewCachedUserRepository(innerA)
	cacheB := repository.NewCachedUserRepository(innerB)
	var invalidations atomic.Int32
	cacheA.SetInvalidationHook(func(inv repository.UserCacheInvalidation) {
		invalidations.Add(1)
		cacheB.ApplyRemoteInvalidation(inv)
	})
	service := NewService(repository.NewCachedLocalFederationRepository(
		repository.NewLocalFederationRepository(integrationDB), cacheA,
	), true)

	off := createFederationTestUser(t, model.FederationStatusOff)
	for _, cache := range []*repository.CachedUserRepository{cacheA, cacheB} {
		cached, err := cache.FindByID(off.ID)
		require.NoError(t, err)
		assert.Equal(t, model.FederationStatusOff, cached.FederationStatus)
	}
	status, err := service.ChangeStatus(off.ID, model.FederationStatusOn)
	require.NoError(t, err)
	assert.Equal(t, model.FederationStatusOn, status.FederationStatus)
	for name, cache := range map[string]*repository.CachedUserRepository{"writer": cacheA, "peer process": cacheB} {
		cached, err := cache.FindByID(off.ID)
		require.NoError(t, err, name)
		assert.Equal(t, model.FederationStatusOn, cached.FederationStatus, name)
	}
	assert.EqualValues(t, 1, invalidations.Load(), "a committed status change must publish one user invalidation")
	_, err = service.ChangeStatus(off.ID, model.FederationStatusOn)
	require.NoError(t, err)
	assert.EqualValues(t, 1, invalidations.Load(), "a status no-op must not invalidate or publish")

	on := createFederationTestUser(t, model.FederationStatusOn)
	for _, cache := range []*repository.CachedUserRepository{cacheA, cacheB} {
		_, err := cache.FindByID(on.ID)
		require.NoError(t, err)
	}
	firstAt := time.Date(2026, 10, 27, 3, 4, 5, 0, time.UTC)
	service.SetClock(func() time.Time { return firstAt })
	recorded, err := service.ObserveExternalReference(on.ID, ExternalReferenceReasonActor, "peer.example")
	require.NoError(t, err)
	assert.True(t, recorded)
	assert.EqualValues(t, 2, invalidations.Load(), "recording the first external reference must publish one invalidation")
	for name, cache := range map[string]*repository.CachedUserRepository{"writer": cacheA, "peer process": cacheB} {
		cached, err := cache.FindByID(on.ID)
		require.NoError(t, err, name)
		require.NotNil(t, cached.FirstExternalReferenceAt, name)
		assert.True(t, firstAt.Equal(*cached.FirstExternalReferenceAt), name)
		assert.Equal(t, ExternalReferenceReasonActor, *cached.FirstExternalReferenceReason, name)
		assert.Equal(t, "peer.example", *cached.FirstExternalReferenceHost, name)
	}

	recorded, err = service.ObserveExternalReference(on.ID, ExternalReferenceReasonDelivery, "other.example")
	require.NoError(t, err)
	assert.False(t, recorded, "later observations must preserve the first reference")
	assert.EqualValues(t, 2, invalidations.Load(), "a repeated observation must not invalidate or publish")
	cached, err := cacheA.FindByID(on.ID)
	require.NoError(t, err)
	assert.True(t, firstAt.Equal(*cached.FirstExternalReferenceAt))
	assert.Equal(t, ExternalReferenceReasonActor, *cached.FirstExternalReferenceReason)
	assert.Equal(t, "peer.example", *cached.FirstExternalReferenceHost)
}

func TestServiceWithPostgres_RestrictionsAndObservationPaths(t *testing.T) {
	repo := repository.NewLocalFederationRepository(integrationDB)
	service := NewService(repo, false)
	service.SetClock(nil)

	pending := createFederationTestUser(t, model.FederationStatusPending)
	recorded, err := service.ObserveExternalReference(pending.ID, ExternalReferenceReasonActor, "peer.example")
	assert.ErrorIs(t, err, ErrFederationDisabled)
	assert.False(t, recorded)

	off := createFederationTestUser(t, model.FederationStatusOff)
	recorded, err = service.ObserveExternalReference(off.ID, ExternalReferenceReasonActor, "peer.example")
	assert.ErrorIs(t, err, ErrFederationDisabled)
	assert.False(t, recorded)
	status, err := service.ChangeStatus(off.ID, model.FederationStatusOn)
	require.NoError(t, err)
	assert.Equal(t, model.FederationStatusOn, status.FederationStatus)
	_, err = repository.NewUserKeypairRepository(integrationDB).FindByUserID(off.ID)
	require.NoError(t, err)
	_, err = repository.NewUserKeypairExtraRepository(integrationDB).FindByUserID(off.ID)
	assert.Error(t, err, "Ed25519 generation remains disabled by service configuration")

	on := createFederationTestUser(t, model.FederationStatusOn)
	recorded, err = service.ObserveExternalReferenceForUser(&model.User{ID: on.ID, FederationStatus: model.FederationStatusOn}, ExternalReferenceReasonNote, "peer.example")
	require.NoError(t, err)
	assert.True(t, recorded)
	known, err := repo.FindUser(on.ID)
	require.NoError(t, err)
	recorded, err = service.ObserveExternalReferenceForUser(known, ExternalReferenceReasonDelivery, "other.example")
	require.NoError(t, err)
	assert.False(t, recorded)
	assert.Equal(t, "peer.example", *known.FirstExternalReferenceHost)

	recorded, err = service.ObserveExternalReferenceForUser(nil, ExternalReferenceReasonActor, "peer.example")
	assert.ErrorIs(t, err, repository.ErrNotFound)
	assert.False(t, recorded)
	recorded, err = service.ObserveExternalReferenceForUser(&model.User{ID: off.ID, FederationStatus: model.FederationStatusOff}, ExternalReferenceReasonActor, "peer.example")
	assert.ErrorIs(t, err, ErrFederationDisabled)
	assert.False(t, recorded)
	remoteHost := "remote.example"
	recorded, err = service.ObserveExternalReferenceForUser(&model.User{ID: "remote-user", Host: &remoteHost, FederationStatus: model.FederationStatusOff}, ExternalReferenceReasonActor, "peer.example")
	require.NoError(t, err)
	assert.False(t, recorded)

	remote := createFederationTestUser(t, model.FederationStatusOn)
	require.NoError(t, integrationDB.Model(&model.User{}).Where("id = ?", remote.ID).Update("host", remoteHost).Error)
	_, err = service.GetStatus(remote.ID)
	assert.ErrorIs(t, err, ErrNotLocalAccount)
	_, err = service.ChangeStatus(remote.ID, model.FederationStatusOff)
	assert.ErrorIs(t, err, ErrNotLocalAccount)
	recorded, err = service.ObserveExternalReference(remote.ID, ExternalReferenceReasonActor, "peer.example")
	require.NoError(t, err)
	assert.False(t, recorded)

	system := createFederationTestUser(t, model.FederationStatusOff)
	require.NoError(t, integrationDB.Create(&model.SystemAccount{ID: testFixtureID(), UserID: system.ID, Type: "localfederation-test-" + system.ID}).Error)
	status, err = service.GetStatus(system.ID)
	require.NoError(t, err)
	assert.True(t, status.IsSystemAccount)
	assert.Equal(t, model.FederationStatusOn, status.FederationStatus)
	_, err = service.ChangeStatus(system.ID, model.FederationStatusOn)
	assert.ErrorIs(t, err, ErrSystemAccount)
	recorded, err = service.ObserveExternalReference(system.ID, ExternalReferenceReasonActor, "peer.example")
	require.NoError(t, err)
	assert.False(t, recorded)

	_, err = service.ChangeStatus(testFixtureID(), model.FederationStatusOn)
	assert.Error(t, err, "missing users must fail")
	_, err = service.ChangeStatus(pending.ID, model.FederationStatusPending)
	assert.ErrorIs(t, err, ErrInvalidFederationStatus)
	_, err = service.GetStatus(testFixtureID())
	assert.Error(t, err)
	recorded, err = service.ObserveExternalReference(testFixtureID(), ExternalReferenceReasonActor, "peer.example")
	assert.Error(t, err)
	assert.False(t, recorded)

	lockUser := createFederationTestUser(t, model.FederationStatusOn)
	err = service.WithLockedUser(lockUser.ID, func(tx *gorm.DB, user *model.User) error {
		require.Equal(t, model.FederationStatusOn, user.FederationStatus)
		return tx.Model(&model.User{}).Where("id = ?", user.ID).Update("federationStatus", model.FederationStatusOff).Error
	})
	require.NoError(t, err)
	locked, err := repo.FindUser(lockUser.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FederationStatusOff, locked.FederationStatus)

	rollbackErr := fmt.Errorf("force callback rollback")
	err = service.WithLockedUser(lockUser.ID, func(tx *gorm.DB, user *model.User) error {
		require.Equal(t, model.FederationStatusOff, user.FederationStatus)
		if updateErr := tx.Model(&model.User{}).Where("id = ?", user.ID).Update("federationStatus", model.FederationStatusOn).Error; updateErr != nil {
			return updateErr
		}
		return rollbackErr
	})
	require.ErrorIs(t, err, rollbackErr)
	locked, err = repo.FindUser(lockUser.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FederationStatusOff, locked.FederationStatus)

	err = service.WithLockedUser(lockUser.ID, nil)
	assert.Error(t, err)
	err = service.WithLockedUser(testFixtureID(), func(*gorm.DB, *model.User) error { return nil })
	assert.Error(t, err)
}

func TestServiceWithPostgres_RepositoryErrorPaths(t *testing.T) {
	db, err := testutil.OpenTestDBSchema("serviceerrors")
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	testutil.ApplyMigrations(db)
	require.NoError(t, db.Exec(`CREATE TABLE IF NOT EXISTS "system_account" (
		id varchar(32) PRIMARY KEY,
		"userId" varchar(32) NOT NULL,
		type varchar(256) NOT NULL UNIQUE
	)`).Error)
	repo := repository.NewLocalFederationRepository(db)
	service := NewService(repo, false)

	broken := createFederationTestUserOn(t, db, model.FederationStatusOn)
	// Simulate a legacy/corrupt state in this isolated schema only; current
	// migrations constrain ordinary writes to the supported statuses.
	require.NoError(t, db.Exec(`ALTER TABLE "user" DROP CONSTRAINT IF EXISTS "CHK_user_federationStatus"`).Error)
	unsupported := createFederationTestUserOn(t, db, model.FederationStatus("unknown"))
	_, err = service.ChangeStatus(unsupported.ID, model.FederationStatusOff)
	assert.Error(t, err, "repository must reject an unsupported persisted state")
	persisted, err := repo.FindUser(unsupported.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FederationStatus("unknown"), persisted.FederationStatus)

	triggerName := "localfederation_reject_observation"
	functionName := "localfederation_reject_observation_fn"
	createFunction := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = '%s' AND NEW."firstExternalReferenceAt" IS NOT NULL THEN
				RAISE EXCEPTION 'test observation rejected';
			END IF;
			RETURN NEW;
		END $$`, functionName, broken.ID)
	require.NoError(t, db.Exec(createFunction).Error)
	require.NoError(t, db.Exec(`CREATE TRIGGER `+triggerName+` BEFORE UPDATE ON "user" FOR EACH ROW EXECUTE FUNCTION `+functionName+`()`).Error)
	t.Cleanup(func() {
		db.Exec(`DROP TRIGGER IF EXISTS ` + triggerName + ` ON "user"`)
		db.Exec(`DROP FUNCTION IF EXISTS ` + functionName + `()`)
	})
	recorded, err := service.ObserveExternalReference(broken.ID, ExternalReferenceReasonActor, "peer.example")
	assert.Error(t, err, "a rejected DB update must fail observation closed")
	assert.False(t, recorded)

	// A dedicated schema missing the system_account table exercises repository
	// query errors without closing or mutating the package's shared test DB.
	require.NoError(t, db.Exec(`DROP TABLE "system_account" CASCADE`).Error)
	_, err = service.GetStatus(broken.ID)
	assert.Error(t, err)
	_, err = service.ChangeStatus(broken.ID, model.FederationStatusOff)
	assert.Error(t, err)
	recorded, err = service.ObserveExternalReference(broken.ID, ExternalReferenceReasonActor, "peer.example")
	assert.Error(t, err)
	assert.False(t, recorded)
	require.NoError(t, db.Exec(`CREATE TABLE IF NOT EXISTS "system_account" (
		id varchar(32) PRIMARY KEY,
		"userId" varchar(32) NOT NULL,
		type varchar(256) NOT NULL UNIQUE
	)`).Error)
}

func TestServiceWithPostgres_ReferenceRacingDisableWins(t *testing.T) {
	user := createFederationTestUser(t, model.FederationStatusOn)
	repo := repository.NewLocalFederationRepository(integrationDB)
	service := NewService(observeBeforeDisableRepository{LocalFederationRepository: repo}, false)

	_, err := service.ChangeStatus(user.ID, model.FederationStatusOff)
	assert.ErrorIs(t, err, ErrFederationAlreadyReferenced)
	current, err := repo.FindUser(user.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FederationStatusOn, current.FederationStatus)
	require.NotNil(t, current.FirstExternalReferenceAt)
	assert.Equal(t, "race.example", *current.FirstExternalReferenceHost)
}

// observeBeforeDisableRepository creates the real interleaving between the
// service's initial read and the repository's locked state change. All writes
// still go through the real Postgres repository; this is not a DB mock.
type observeBeforeDisableRepository struct {
	repository.LocalFederationRepository
}

func (r observeBeforeDisableRepository) ChangeStatus(userID string, next model.FederationStatus, at time.Time, rsa *model.UserKeypair, ed25519 *model.UserKeypairExtra) (*model.User, bool, error) {
	if next == model.FederationStatusOff {
		if _, _, err := r.LocalFederationRepository.ObserveExternalReference(userID, "actor", "race.example", at); err != nil {
			return nil, false, err
		}
	}
	return r.LocalFederationRepository.ChangeStatus(userID, next, at, rsa, ed25519)
}

var fixtureSequence int64

func testFixtureID() string {
	fixtureSequence++
	return fmt.Sprintf("lfsvc%012x", time.Now().UnixNano()+fixtureSequence)
}

func createFederationTestUser(t *testing.T, status model.FederationStatus) *model.User {
	t.Helper()
	return createFederationTestUserOn(t, integrationDB, status)
}

func createFederationTestUserOn(t *testing.T, db *gorm.DB, status model.FederationStatus) *model.User {
	t.Helper()
	id := testFixtureID()
	token := fmt.Sprintf("%016x", time.Now().UnixNano()+fixtureSequence)
	user := &model.User{
		ID:                id,
		Username:          "lf-" + id,
		UsernameLower:     "lf-" + id,
		Token:             &token,
		FederationStatus:  status,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, db.Create(user).Error)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "system_account" WHERE "userId" = ?`, id)
		db.Exec(`DELETE FROM "user_keypair_extra" WHERE "userId" = ?`, id)
		db.Exec(`DELETE FROM "user_keypair" WHERE "userId" = ?`, id)
		db.Exec(`DELETE FROM "note" WHERE "userId" = ?`, id)
		db.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, id)
		db.Exec(`DELETE FROM "user" WHERE id = ?`, id)
	})
	return user
}

func createFederationTestNote(t *testing.T, userID string, localOnly bool) *model.Note {
	t.Helper()
	note := &model.Note{
		ID:         "n" + testFixtureID(),
		UserID:     userID,
		LocalOnly:  localOnly,
		Visibility: model.NoteVisibilityPublic,
	}
	require.NoError(t, integrationDB.Create(note).Error)
	return note
}
