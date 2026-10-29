package localfederation

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrations136And137BackfillAndValidateLegacyAccounts(t *testing.T) {
	db, err := testutil.OpenTestDBSchema("localfederation_migration")
	require.NoError(t, err)
	tx := db.Begin()
	require.NoError(t, tx.Error)
	t.Cleanup(func() { _ = tx.Rollback().Error })

	// 既存の共有スキーマを変更せず、ロールバック専用の兄弟スキーマに
	// 旧テーブルを最小構成で作る。全 migration の再実行と列削除は行わない。
	require.NoError(t, tx.Exec(`CREATE TABLE "user" (id varchar(32) PRIMARY KEY, host varchar(255))`).Error)
	require.NoError(t, tx.Exec(`CREATE TABLE user_pending (id varchar(32) PRIMARY KEY)`).Error)
	require.NoError(t, tx.Exec(`CREATE TABLE system_account ("userId" varchar(32) PRIMARY KEY)`).Error)
	require.NoError(t, tx.Exec(`INSERT INTO "user" (id, host) VALUES (?, NULL), (?, ?)`, "legacy-local", "legacy-remote", "legacy.remote.example").Error)
	require.NoError(t, tx.Exec(`INSERT INTO "user" (id, host) VALUES (?, NULL), (?, NULL), (?, NULL), (?, NULL)`, "legacy-local-preserved", "legacy-local-enabled-at", "legacy-local-off", "legacy-local-pending").Error)
	require.NoError(t, tx.Exec(`INSERT INTO user_pending (id) VALUES (?)`, "legacy-pending").Error)

	applyMigration := func(name string) {
		t.Helper()
		sql, err := os.ReadFile(filepath.Join("..", "..", "..", "migration", name))
		require.NoError(t, err)
		require.NoError(t, tx.Exec(string(sql)).Error)
	}
	assertConstraintValidation := func(name, table string, validated bool) {
		t.Helper()
		var got struct {
			Count     int64 `gorm:"column:constraint_count"`
			Validated bool  `gorm:"column:validated"`
		}
		require.NoError(t, tx.Raw(`SELECT COUNT(*) AS constraint_count, COALESCE(bool_and(convalidated), false) AS validated FROM pg_constraint WHERE conname = ? AND conrelid = ?::regclass`, name, table).Scan(&got).Error)
		assert.Equal(t, int64(1), got.Count, "constraint %s exists on %s", name, table)
		assert.Equal(t, validated, got.Validated, "constraint %s validation state", name)
	}
	type userState struct {
		Status          model.FederationStatus `gorm:"column:federationStatus"`
		EnabledAt       *time.Time             `gorm:"column:federationEnabledAt"`
		ReferenceAt     *time.Time             `gorm:"column:firstExternalReferenceAt"`
		ReferenceReason *string                `gorm:"column:firstExternalReferenceReason"`
		ReferenceHost   *string                `gorm:"column:firstExternalReferenceHost"`
	}
	readUser := func(id string) userState {
		var got userState
		require.NoError(t, tx.Raw(`SELECT "federationStatus", "federationEnabledAt", "firstExternalReferenceAt", "firstExternalReferenceReason", "firstExternalReferenceHost" FROM "user" WHERE id = ?`, id).Scan(&got).Error)
		return got
	}

	// DDLだけの136と、既存行のbackfill・制約検査を行う137を分けて適用する。
	applyMigration("000136_local_only_accounts.up.sql")
	oldEnabledAt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	oldReferenceAt := time.Date(2021, 2, 3, 4, 5, 6, 0, time.UTC)
	require.NoError(t, tx.Exec(`UPDATE "user" SET "federationEnabledAt" = ?, "firstExternalReferenceAt" = ?, "firstExternalReferenceReason" = ?, "firstExternalReferenceHost" = ? WHERE id = ?`, oldEnabledAt, oldReferenceAt, "actor", "remote.example", "legacy-local-preserved").Error)
	require.NoError(t, tx.Exec(`UPDATE "user" SET "federationEnabledAt" = ? WHERE id = ?`, oldEnabledAt, "legacy-local-enabled-at").Error)
	require.NoError(t, tx.Exec(`UPDATE "user" SET "federationStatus" = ? WHERE id = ?`, model.FederationStatusOff, "legacy-local-off").Error)
	require.NoError(t, tx.Exec(`UPDATE "user" SET "federationStatus" = ? WHERE id = ?`, model.FederationStatusPending, "legacy-local-pending").Error)
	assertConstraintValidation("CHK_user_federationStatus", "user", false)
	assertConstraintValidation("CHK_user_pending_federationStatus", "user_pending", false)
	local := readUser("legacy-local")
	assert.Equal(t, model.FederationStatusOn, local.Status)
	assert.Nil(t, local.EnabledAt)
	assert.Nil(t, local.ReferenceAt)
	assert.Nil(t, local.ReferenceReason)
	assert.Nil(t, local.ReferenceHost)
	remote := readUser("legacy-remote")
	assert.Equal(t, model.FederationStatusOn, remote.Status)
	assert.Nil(t, remote.ReferenceAt)
	assert.Nil(t, remote.ReferenceReason)
	assert.Nil(t, remote.ReferenceHost)
	var pendingStatus model.FederationStatus
	require.NoError(t, tx.Raw(`SELECT "federationStatus" FROM user_pending WHERE id = ?`, "legacy-pending").Scan(&pendingStatus).Error)
	assert.Equal(t, model.FederationStatusOn, pendingStatus)

	applyMigration("000137_backfill_local_account_federation.up.sql")
	assertConstraintValidation("CHK_user_federationStatus", "user", true)
	assertConstraintValidation("CHK_user_pending_federationStatus", "user_pending", true)
	local = readUser("legacy-local")
	assert.Equal(t, model.FederationStatusOn, local.Status)
	assert.NotNil(t, local.EnabledAt)
	assert.NotNil(t, local.ReferenceAt)
	require.NotNil(t, local.ReferenceReason)
	assert.Equal(t, "migration", *local.ReferenceReason)
	assert.Nil(t, local.ReferenceHost, "migration does not invent an observing host")
	remote = readUser("legacy-remote")
	assert.Nil(t, remote.ReferenceAt)
	assert.Nil(t, remote.ReferenceReason)
	assert.Nil(t, remote.ReferenceHost)
	require.NoError(t, tx.Raw(`SELECT "federationStatus" FROM user_pending WHERE id = ?`, "legacy-pending").Scan(&pendingStatus).Error)
	assert.Equal(t, model.FederationStatusOn, pendingStatus)

	preserved := readUser("legacy-local-preserved")
	assert.Equal(t, model.FederationStatusOn, preserved.Status)
	require.NotNil(t, preserved.EnabledAt)
	assert.True(t, oldEnabledAt.Equal(*preserved.EnabledAt))
	require.NotNil(t, preserved.ReferenceAt)
	assert.True(t, oldReferenceAt.Equal(*preserved.ReferenceAt))
	require.NotNil(t, preserved.ReferenceReason)
	assert.Equal(t, "actor", *preserved.ReferenceReason)
	require.NotNil(t, preserved.ReferenceHost)
	assert.Equal(t, "remote.example", *preserved.ReferenceHost)

	enabledAt := readUser("legacy-local-enabled-at")
	assert.Equal(t, model.FederationStatusOn, enabledAt.Status)
	require.NotNil(t, enabledAt.EnabledAt)
	assert.True(t, oldEnabledAt.Equal(*enabledAt.EnabledAt), "backfill preserves an existing enable time")
	assert.NotNil(t, enabledAt.ReferenceAt)
	require.NotNil(t, enabledAt.ReferenceReason)
	assert.Equal(t, "migration", *enabledAt.ReferenceReason)

	for _, id := range []string{"legacy-local-off", "legacy-local-pending"} {
		skipped := readUser(id)
		assert.Nil(t, skipped.ReferenceAt, "migration does not mark %s as referenced", id)
		assert.Nil(t, skipped.ReferenceReason)
		assert.Nil(t, skipped.EnabledAt)
	}

	// 2回目の適用でも、既存の観測記録と初回backfillを上書きしない。
	applyMigration("000137_backfill_local_account_federation.up.sql")
	preserved = readUser("legacy-local-preserved")
	require.NotNil(t, preserved.EnabledAt)
	assert.True(t, oldEnabledAt.Equal(*preserved.EnabledAt))
	require.NotNil(t, preserved.ReferenceAt)
	assert.True(t, oldReferenceAt.Equal(*preserved.ReferenceAt))
	require.NotNil(t, preserved.ReferenceReason)
	assert.Equal(t, "actor", *preserved.ReferenceReason)
	require.NotNil(t, preserved.ReferenceHost)
	assert.Equal(t, "remote.example", *preserved.ReferenceHost)
	enabledAt = readUser("legacy-local-enabled-at")
	require.NotNil(t, enabledAt.EnabledAt)
	assert.True(t, oldEnabledAt.Equal(*enabledAt.EnabledAt))

	// no-op downでも安全印を保持し、不可逆なOFF切替を引き続き拒否する。
	applyMigration("000137_backfill_local_account_federation.down.sql")
	assertConstraintValidation("CHK_user_federationStatus", "user", true)
	local = readUser("legacy-local")
	assert.NotNil(t, local.ReferenceAt)
	require.NotNil(t, local.ReferenceReason)
	assert.Equal(t, "migration", *local.ReferenceReason)
	service := NewService(repository.NewLocalFederationRepository(tx), false)
	_, err = service.ChangeStatus("legacy-local", model.FederationStatusOff)
	require.ErrorIs(t, err, ErrFederationAlreadyReferenced,
		"the retained marker keeps legacy local accounts ineligible for irreversible disable")
}
