package systemaccount_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/systemaccount"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func TestFetch_PersistsAndRepairsFederationMetadataWithPostgres(t *testing.T) {
	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	testutil.ApplyMigrations(db)

	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	users := repository.NewUserRepository(db)
	keys := repository.NewUserKeypairRepository(db)
	extraKeys := repository.NewUserKeypairExtraRepository(db)
	accounts := repository.NewSystemAccountRepository(db)
	service := systemaccount.NewService(users, keys, accounts, idGen)
	service.SetKeypairExtraRepo(extraKeys)
	now := time.Date(2026, 10, 27, 4, 5, 6, 0, time.UTC)
	service.SetClock(func() time.Time { return now })

	kind := fmt.Sprintf("fedstate-coverage-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupSystemAccountFixture(db, kind) })
	created, err := service.Fetch(kind)
	require.NoError(t, err)
	require.Equal(t, model.FederationStatusOn, created.FederationStatus)
	require.NotNil(t, created.FederationEnabledAt)
	require.True(t, now.Equal(*created.FederationEnabledAt))
	require.NotNil(t, created.FirstExternalReferenceAt)
	require.True(t, now.Equal(*created.FirstExternalReferenceAt))
	require.NotNil(t, created.FirstExternalReferenceReason)
	assert.Equal(t, "system-account", *created.FirstExternalReferenceReason)

	persisted, err := users.FindByID(created.ID)
	require.NoError(t, err)
	require.Equal(t, model.FederationStatusOn, persisted.FederationStatus)
	require.NotNil(t, persisted.FederationEnabledAt)
	require.NotNil(t, persisted.FirstExternalReferenceAt)
	require.NotNil(t, persisted.FirstExternalReferenceReason)
	assert.Equal(t, "system-account", *persisted.FirstExternalReferenceReason)
	rsa, err := keys.FindByUserID(created.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, rsa.PrivateKey)
	ed, err := extraKeys.FindByUserID(created.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, ed.Ed25519PrivateKey)

	firstRSAPublic, firstEdPublic := rsa.PublicKey, ed.Ed25519PublicKey
	second, err := service.Fetch(kind)
	require.NoError(t, err)
	assert.Equal(t, created.ID, second.ID)
	rsa, err = keys.FindByUserID(created.ID)
	require.NoError(t, err)
	ed, err = extraKeys.FindByUserID(created.ID)
	require.NoError(t, err)
	assert.Equal(t, firstRSAPublic, rsa.PublicKey)
	assert.Equal(t, firstEdPublic, ed.Ed25519PublicKey)

	orphanKind := kind + "-orphan"
	orphan := createSystemAccountUser(t, db, idGen.Generate(now), orphanKind+".actor", model.FederationStatusOff)
	enabledAt := now.Add(-48 * time.Hour)
	referencedAt := now.Add(-24 * time.Hour)
	reason, host := "legacy-actor", "old-peer.example"
	orphan.FederationEnabledAt = &enabledAt
	orphan.FirstExternalReferenceAt = &referencedAt
	orphan.FirstExternalReferenceReason = &reason
	orphan.FirstExternalReferenceHost = &host
	require.NoError(t, users.UpdateUser(orphan.ID, map[string]any{
		"federationEnabledAt":          enabledAt,
		"firstExternalReferenceAt":     referencedAt,
		"firstExternalReferenceReason": reason,
		"firstExternalReferenceHost":   host,
	}))
	require.NoError(t, users.CreateProfile(&model.UserProfile{UserID: orphan.ID}))
	require.NoError(t, keys.Create(&model.UserKeypair{UserID: orphan.ID, PublicKey: "existing-rsa-public", PrivateKey: "existing-rsa-private"}))
	require.NoError(t, extraKeys.Upsert(&model.UserKeypairExtra{UserID: orphan.ID, Ed25519PublicKey: "existing-ed-public", Ed25519PrivateKey: "existing-ed-private"}))

	repaired, err := service.Fetch(orphanKind)
	require.NoError(t, err)
	assert.Equal(t, orphan.ID, repaired.ID)
	assert.Equal(t, model.FederationStatusOn, repaired.FederationStatus)
	assert.True(t, enabledAt.Equal(*repaired.FederationEnabledAt))
	assert.True(t, referencedAt.Equal(*repaired.FirstExternalReferenceAt))
	assert.Equal(t, reason, *repaired.FirstExternalReferenceReason)
	assert.Equal(t, host, *repaired.FirstExternalReferenceHost)
	rsa, err = keys.FindByUserID(orphan.ID)
	require.NoError(t, err)
	assert.Equal(t, "existing-rsa-public", rsa.PublicKey)
	ed, err = extraKeys.FindByUserID(orphan.ID)
	require.NoError(t, err)
	assert.Equal(t, "existing-ed-public", ed.Ed25519PublicKey)

	sparseKind := kind + "-sparse"
	sparse := createSystemAccountUser(t, db, idGen.Generate(now), sparseKind+".actor", model.FederationStatusPending)
	recovered, err := service.Fetch(sparseKind)
	require.NoError(t, err)
	assert.Equal(t, sparse.ID, recovered.ID)
	assert.Equal(t, model.FederationStatusOn, recovered.FederationStatus)
	require.NotNil(t, recovered.FederationEnabledAt)
	assert.True(t, now.Equal(*recovered.FederationEnabledAt))
	require.NotNil(t, recovered.FirstExternalReferenceAt)
	assert.True(t, now.Equal(*recovered.FirstExternalReferenceAt))
	require.NotNil(t, recovered.FirstExternalReferenceReason)
	assert.Equal(t, "system-account", *recovered.FirstExternalReferenceReason)
	_, err = users.FindProfileByUserID(sparse.ID)
	require.NoError(t, err)
	rsa, err = keys.FindByUserID(sparse.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, rsa.PublicKey)
	ed, err = extraKeys.FindByUserID(sparse.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, ed.Ed25519PublicKey)

	remoteKind := kind + "-remote"
	remoteHost := "remote.example"
	remote := createSystemAccountUser(t, db, idGen.Generate(now), remoteKind+".actor", model.FederationStatusOff)
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", remote.ID).Update("host", remoteHost).Error)
	require.NoError(t, accounts.Create(&model.SystemAccount{ID: idGen.Generate(now), UserID: remote.ID, Type: remoteKind}))
	remoteFetched, err := service.Fetch(remoteKind)
	require.NoError(t, err)
	assert.Equal(t, remote.ID, remoteFetched.ID)
	assert.Equal(t, model.FederationStatusOff, remoteFetched.FederationStatus)
	assert.Nil(t, remoteFetched.FederationEnabledAt)
	assert.Nil(t, remoteFetched.FirstExternalReferenceAt)
}

func TestFetch_FailsClosedWhenFederationMetadataRepairFailsWithPostgres(t *testing.T) {
	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	testutil.ApplyMigrations(db)

	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	users := repository.NewCachedUserRepository(repository.NewUserRepository(db))
	kind := fmt.Sprintf("fedstate-fail-%d", time.Now().UnixNano())
	user := createSystemAccountUser(t, db, idGen.Generate(time.Now()), kind+".actor", model.FederationStatusOff)
	orphanKind := kind + "-orphan"
	orphan := createSystemAccountUser(t, db, idGen.Generate(time.Now()), orphanKind+".actor", model.FederationStatusOff)
	require.NoError(t, repository.NewSystemAccountRepository(db).Create(&model.SystemAccount{
		ID: idGen.Generate(time.Now()), UserID: user.ID, Type: kind,
	}))
	t.Cleanup(func() {
		cleanupSystemAccountFixture(db, kind)
		cleanupSystemAccountFixture(db, orphanKind)
	})

	functionName := "systemaccount_reject_metadata_update"
	triggerName := "systemaccount_reject_metadata_update_trigger"
	createFunction := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id IN ('%s', '%s') AND NEW."federationStatus" = 'on' THEN
				RAISE EXCEPTION 'test metadata repair rejected';
			END IF;
			RETURN NEW;
		END $$`, functionName, user.ID, orphan.ID)
	require.NoError(t, db.Exec(createFunction).Error)
	require.NoError(t, db.Exec(`CREATE TRIGGER `+triggerName+` BEFORE UPDATE ON "user" FOR EACH ROW EXECUTE FUNCTION `+functionName+`()`).Error)
	t.Cleanup(func() {
		db.Exec(`DROP TRIGGER IF EXISTS ` + triggerName + ` ON "user"`)
		db.Exec(`DROP FUNCTION IF EXISTS ` + functionName + `()`)
	})

	service := systemaccount.NewService(
		users,
		repository.NewUserKeypairRepository(db),
		repository.NewSystemAccountRepository(db),
		idGen,
	)
	cachedBefore, err := users.FindByID(user.ID)
	require.NoError(t, err)
	cachedOrphanBefore, err := users.FindByID(orphan.ID)
	require.NoError(t, err)
	fetched, err := service.Fetch(kind)
	require.Error(t, err)
	assert.Nil(t, fetched)
	assert.Contains(t, err.Error(), "set federation state")

	persisted, err := users.UserRepository.FindByID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FederationStatusOff, persisted.FederationStatus)
	assert.Nil(t, persisted.FederationEnabledAt)
	assert.Nil(t, persisted.FirstExternalReferenceAt)
	cachedAfter, err := users.FindByID(user.ID)
	require.NoError(t, err)
	assert.Same(t, cachedBefore, cachedAfter)
	assert.Equal(t, model.FederationStatusOff, cachedAfter.FederationStatus)
	assert.Nil(t, cachedAfter.FederationEnabledAt)
	assert.Nil(t, cachedAfter.FirstExternalReferenceAt)
	assert.Nil(t, cachedAfter.FirstExternalReferenceReason)

	orphanFetched, err := service.Fetch(orphanKind)
	require.Error(t, err)
	assert.Nil(t, orphanFetched)
	assert.Contains(t, err.Error(), "set federation state")
	orphanPersisted, err := users.UserRepository.FindByID(orphan.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FederationStatusOff, orphanPersisted.FederationStatus)
	assert.Nil(t, orphanPersisted.FederationEnabledAt)
	assert.Nil(t, orphanPersisted.FirstExternalReferenceAt)
	cachedOrphanAfter, err := users.FindByID(orphan.ID)
	require.NoError(t, err)
	assert.Same(t, cachedOrphanBefore, cachedOrphanAfter)
	assert.Equal(t, model.FederationStatusOff, cachedOrphanAfter.FederationStatus)
	assert.Nil(t, cachedOrphanAfter.FederationEnabledAt)
	assert.Nil(t, cachedOrphanAfter.FirstExternalReferenceAt)
	assert.Nil(t, cachedOrphanAfter.FirstExternalReferenceReason)
}

func TestFetch_FailsClosedWhenNewSystemUserInsertFailsWithPostgres(t *testing.T) {
	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	testutil.ApplyMigrations(db)

	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	kind := fmt.Sprintf("fedstate-insert-fail-%d", time.Now().UnixNano())
	functionName := "systemaccount_reject_user_insert"
	triggerName := "systemaccount_reject_user_insert_trigger"
	createFunction := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW."usernameLower" = '%s.actor' THEN
				RAISE EXCEPTION 'test user insert rejected';
			END IF;
			RETURN NEW;
		END $$`, functionName, kind)
	require.NoError(t, db.Exec(createFunction).Error)
	require.NoError(t, db.Exec(`CREATE TRIGGER `+triggerName+` BEFORE INSERT ON "user" FOR EACH ROW EXECUTE FUNCTION `+functionName+`()`).Error)
	t.Cleanup(func() {
		db.Exec(`DROP TRIGGER IF EXISTS ` + triggerName + ` ON "user"`)
		db.Exec(`DROP FUNCTION IF EXISTS ` + functionName + `()`)
	})

	service := systemaccount.NewService(
		repository.NewUserRepository(db),
		repository.NewUserKeypairRepository(db),
		repository.NewSystemAccountRepository(db),
		idGen,
	)
	fetched, err := service.Fetch(kind)
	require.Error(t, err)
	assert.Nil(t, fetched)
	assert.Contains(t, err.Error(), "create user")
	_, err = repository.NewUserRepository(db).FindByUsernameLower(kind+".actor", nil)
	assert.Error(t, err, "failed user insert must not leave a partial user row")
}

func TestFetch_FailsClosedForStaleSystemAccountReferenceWithPostgres(t *testing.T) {
	db, err := testutil.OpenTestDBSchema("staleaccount")
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

	var foreignKeys []struct {
		Name string `gorm:"column:conname"`
	}
	require.NoError(t, db.Raw(`SELECT conname FROM pg_constraint WHERE conrelid = '"system_account"'::regclass AND contype = 'f'`).Scan(&foreignKeys).Error)
	for _, constraint := range foreignKeys {
		quotedName := strings.ReplaceAll(constraint.Name, `"`, `""`)
		require.NoError(t, db.Exec(`ALTER TABLE "system_account" DROP CONSTRAINT "`+quotedName+`"`).Error)
	}

	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	kind := fmt.Sprintf("fedstate-stale-%d", time.Now().UnixNano())
	user := createSystemAccountUser(t, db, idGen.Generate(time.Now()), kind+".actor", model.FederationStatusOn)
	accounts := repository.NewSystemAccountRepository(db)
	require.NoError(t, accounts.Create(&model.SystemAccount{ID: idGen.Generate(time.Now()), UserID: user.ID, Type: kind}))
	assert.NoError(t, db.Delete(&model.User{}, "id = ?", user.ID).Error)
	t.Cleanup(func() { db.Exec(`DELETE FROM "system_account" WHERE type = ?`, kind) })

	service := systemaccount.NewService(
		repository.NewUserRepository(db),
		repository.NewUserKeypairRepository(db),
		accounts,
		idGen,
	)
	fetched, err := service.Fetch(kind)
	require.Error(t, err)
	assert.Nil(t, fetched)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func createSystemAccountUser(t *testing.T, db *gorm.DB, userID, username string, status model.FederationStatus) *model.User {
	t.Helper()
	user := &model.User{
		ID:                userID,
		Username:          username,
		UsernameLower:     username,
		FederationStatus:  status,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, db.Create(user).Error)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "system_account" WHERE "userId" = ?`, userID)
		db.Exec(`DELETE FROM "user_keypair_extra" WHERE "userId" = ?`, userID)
		db.Exec(`DELETE FROM "user_keypair" WHERE "userId" = ?`, userID)
		db.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, userID)
		db.Exec(`DELETE FROM "user" WHERE id = ?`, userID)
	})
	return user
}

func cleanupSystemAccountFixture(db *gorm.DB, kind string) {
	username := kind + ".actor"
	db.Exec(`DELETE FROM "system_account" WHERE type IN (?, ?, ?, ?)`, kind, kind+"-orphan", kind+"-sparse", kind+"-remote")
	db.Exec(`DELETE FROM "user_keypair_extra" WHERE "userId" IN (SELECT id FROM "user" WHERE "usernameLower" LIKE ?)`, username+"%")
	db.Exec(`DELETE FROM "user_keypair" WHERE "userId" IN (SELECT id FROM "user" WHERE "usernameLower" LIKE ?)`, username+"%")
	db.Exec(`DELETE FROM "user_profile" WHERE "userId" IN (SELECT id FROM "user" WHERE "usernameLower" LIKE ?)`, username+"%")
	db.Exec(`DELETE FROM "user" WHERE "usernameLower" LIKE ?`, username+"%")
}
