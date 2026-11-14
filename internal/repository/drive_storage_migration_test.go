package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// Counts は drive_file の全行を数えるので、shared schema の他のテストの行が
// 混ざる。専用の schema を一度だけ作って使い回す (drive_usage_test.go と同じ)。
var (
	storageMigDB     *gorm.DB
	storageMigDBOnce sync.Once
	storageMigDBErr  error
)

func driveStorageMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	storageMigDBOnce.Do(func() {
		db, err := testutil.OpenTestDBSchema("drivestoragemig")
		if err != nil {
			storageMigDBErr = err
			return
		}
		testutil.ApplyMigrations(db)
		storageMigDB = db
	})
	require.NoError(t, storageMigDBErr)
	require.NotNil(t, storageMigDB)
	db := storageMigDB
	reset := func() {
		require.NoError(t, db.Exec(`DELETE FROM "drive_object_storage_migration_failure"`).Error)
		require.NoError(t, db.Exec(`DELETE FROM "drive_file"`).Error)
	}
	reset()
	t.Cleanup(reset)
	return db
}

func storageMigFile(t *testing.T, db *gorm.DB, id string, storedInternal, isLink bool) {
	t.Helper()
	ak := "ak-" + id
	require.NoError(t, db.Create(&model.DriveFile{
		ID: id, MD5: "0123456789abcdef0123456789abcdef", Name: id, Type: "image/png", Size: 1,
		StoredInternal: storedInternal, IsLink: isLink, URL: "https://local.example/files/" + ak, AccessKey: &ak,
	}).Error)
}

func TestDriveStorageMigrationRepository_ListAndMark(t *testing.T) {
	ctx := context.Background()
	db := driveStorageMigrationDB(t)
	repo := NewDriveStorageMigrationRepository(db)
	storageMigFile(t, db, "f3", true, false)
	storageMigFile(t, db, "f1", true, false)
	storageMigFile(t, db, "f2", true, false)
	storageMigFile(t, db, "s3", false, false) // 初めからオブジェクトストレージ
	storageMigFile(t, db, "ln", false, true)  // リモートの参照
	storageMigFile(t, db, "lk", true, true)   // 参照なのに storedInternal (TS 製の DB の古い行など)

	got, err := repo.ListLocal(ctx, "", 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []string{"f1", "f2"}, []string{got[0].ID, got[1].ID})
	got, err = repo.ListLocal(ctx, "f2", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "f3", got[0].ID)
	got, err = repo.ListLocal(ctx, "a\x00b", 10)
	require.NoError(t, err)
	assert.Empty(t, got)

	at := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	ok, err := repo.MarkMigrated(ctx, "f1", at)
	require.NoError(t, err)
	assert.True(t, ok)
	// 2 回目と、ローカルに無い行は書き換えない。
	ok, err = repo.MarkMigrated(ctx, "f1", at.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, ok)
	for _, id := range []string{"s3", "ln", "lk", "missing", "a\x00b"} {
		ok, err = repo.MarkMigrated(ctx, id, at)
		require.NoError(t, err)
		assert.False(t, ok, id)
	}
	var f1 model.DriveFile
	require.NoError(t, db.Where(`"id" = ?`, "f1").Take(&f1).Error)
	assert.False(t, f1.StoredInternal)
	require.NotNil(t, f1.ObjectStorageMigratedAt)
	assert.True(t, f1.ObjectStorageMigratedAt.Equal(at))
	// url の列は書き換えない。
	assert.Equal(t, "https://local.example/files/ak-f1", f1.URL)
	var s3 model.DriveFile
	require.NoError(t, db.Where(`"id" = ?`, "s3").Take(&s3).Error)
	assert.Nil(t, s3.ObjectStorageMigratedAt)

	c, err := repo.Counts(ctx)
	require.NoError(t, err)
	assert.Equal(t, DriveStorageMigrationCounts{Local: 2, Migrated: 1}, c)

	// 写した状態の行だけが IsMigrated。
	for id, want := range map[string]bool{"f1": true, "f2": false, "s3": false, "ln": false, "lk": false, "missing": false, "a\x00b": false} {
		got, err := repo.IsMigrated(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, want, got, id)
	}
}

func TestDriveStorageMigrationRepository_Failures(t *testing.T) {
	ctx := context.Background()
	db := driveStorageMigrationDB(t)
	repo := NewDriveStorageMigrationRepository(db)
	storageMigFile(t, db, "f1", true, false)
	storageMigFile(t, db, "f2", true, false)
	t0 := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)

	require.NoError(t, repo.RecordFailure(ctx, "f1", DriveStorageMigrationOperationMigrate, "first", t0))
	require.NoError(t, repo.RecordFailure(ctx, "f2", DriveStorageMigrationOperationMigrate, "other", t0.Add(time.Minute)))
	// 同じファイルと操作は上書きする。
	require.NoError(t, repo.RecordFailure(ctx, "f1", DriveStorageMigrationOperationMigrate, "second", t0.Add(2*time.Minute)))
	require.NoError(t, repo.RecordFailure(ctx, "f1", "verify", "not counted", t0))
	require.NoError(t, repo.RecordFailure(ctx, "a\x00b", DriveStorageMigrationOperationMigrate, "x", t0))

	list, err := repo.ListFailures(ctx, DriveStorageMigrationOperationMigrate, 10)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "f1", list[0].FileID, "not newest first")
	assert.Equal(t, "second", list[0].Error)
	list, err = repo.ListFailures(ctx, DriveStorageMigrationOperationMigrate, 1)
	require.NoError(t, err)
	assert.Len(t, list, 1)
	c, err := repo.Counts(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), c.Failed)

	require.NoError(t, repo.ClearFailure(ctx, "f1", DriveStorageMigrationOperationMigrate))
	require.NoError(t, repo.ClearFailure(ctx, "a\x00b", DriveStorageMigrationOperationMigrate))
	c, err = repo.Counts(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), c.Failed)

	// 消えたファイルの失敗は、外部キーに断られても誤りにしない (記録もしない)。
	require.NoError(t, repo.RecordFailure(ctx, "gone", DriveStorageMigrationOperationMigrate, "x", t0))
	var n int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM "drive_object_storage_migration_failure" WHERE "fileId" = 'gone'`).Scan(&n).Error)
	assert.Zero(t, n)

	// 失敗を記録した後で link 行へ倒された行の記録は数えない (取り直されないため)。
	require.NoError(t, db.Exec(`UPDATE "drive_file" SET "isLink" = true, "storedInternal" = false WHERE "id" = 'f2'`).Error)
	c, err = repo.Counts(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), c.Failed)
	list, err = repo.ListFailures(ctx, DriveStorageMigrationOperationMigrate, 10)
	require.NoError(t, err)
	assert.Empty(t, list)
	require.NoError(t, db.Exec(`UPDATE "drive_file" SET "isLink" = false, "storedInternal" = true WHERE "id" = 'f2'`).Error)
	require.NoError(t, repo.RecordFailure(ctx, "f2", DriveStorageMigrationOperationMigrate, "other", t0))

	// ファイルを消せば、失敗の記録も消える。
	require.NoError(t, db.Exec(`DELETE FROM "drive_file" WHERE "id" = 'f2'`).Error)
	c, err = repo.Counts(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), c.Failed)
}
