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
		require.NoError(t, db.Exec(`DELETE FROM "drive_object_storage_restored_copy"`).Error)
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

func TestDriveStorageMigrationRepository_Verify(t *testing.T) {
	ctx := context.Background()
	db := driveStorageMigrationDB(t)
	repo := NewDriveStorageMigrationRepository(db)
	for _, id := range []string{"m3", "m1", "m2", "lc"} {
		storageMigFile(t, db, id, true, false)
	}
	storageMigFile(t, db, "s3", false, false) // 初めからオブジェクトストレージ
	// マイクロ秒より下の端数は PostgreSQL が丸める。突き合わせは DB から読んだ値で行う。
	at := time.Date(2026, 11, 15, 0, 0, 0, 123456789, time.UTC)
	for _, id := range []string{"m1", "m2", "m3"} {
		ok, err := repo.MarkMigrated(ctx, id, at)
		require.NoError(t, err)
		require.True(t, ok)
	}

	// 写した行だけを id 順に返す。ローカルの行と初めからオブジェクトストレージの行は返さない。
	got, err := repo.ListMigrated(ctx, "", 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"m1", "m2"}, []string{got[0].ID, got[1].ID})
	got, err = repo.ListMigrated(ctx, "m2", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "m3", got[0].ID)
	got, err = repo.ListMigrated(ctx, "a\x00b", 10)
	require.NoError(t, err)
	assert.Empty(t, got)
	// 移した日時を持っていても、ローカルにある行 (戻した直後など) は確かめない。
	storageMigFile(t, db, "mb", true, false)
	require.NoError(t, db.Exec(`UPDATE "drive_file" SET "objectStorageMigratedAt" = ? WHERE "id" = 'mb'`, at).Error)
	got, err = repo.ListMigrated(ctx, "m3", 10)
	require.NoError(t, err)
	assert.Empty(t, got, "a row stored locally was listed for the verification")
	require.NoError(t, db.Exec(`DELETE FROM "drive_file" WHERE "id" = 'mb'`).Error)

	// DB から読んだ移した日時で突き合わせられる (精度の丸めで外れない)。
	got, err = repo.ListMigrated(ctx, "", 10)
	require.NoError(t, err)
	verifiedAt := at.Add(time.Hour)
	for _, f := range got[:2] {
		ok, err := repo.SetVerified(ctx, f.ID, *f.ObjectStorageMigratedAt, &verifiedAt)
		require.NoError(t, err)
		assert.True(t, ok, f.ID)
	}
	// 移した日時が違えば (確かめている間に写し直された) 書かない。
	ok, err := repo.SetVerified(ctx, "m3", at.Add(time.Second), &verifiedAt)
	require.NoError(t, err)
	assert.False(t, ok)
	for _, id := range []string{"lc", "s3", "missing", "a\x00b"} {
		ok, err := repo.SetVerified(ctx, id, at, &verifiedAt)
		require.NoError(t, err)
		assert.False(t, ok, id)
	}
	c, err := repo.Counts(ctx)
	require.NoError(t, err)
	assert.Equal(t, DriveStorageMigrationCounts{Local: 1, Migrated: 3, Verified: 2, Purgeable: 2}, c)

	// 一致しなかったら印を消す。
	ok, err = repo.SetVerified(ctx, "m2", *got[1].ObjectStorageMigratedAt, nil)
	require.NoError(t, err)
	assert.True(t, ok)
	var m2 model.DriveFile
	require.NoError(t, db.Where(`"id" = ?`, "m2").Take(&m2).Error)
	assert.Nil(t, m2.ObjectStorageVerifiedAt)

	// 写し直すと、確かめた印は消える。
	require.NoError(t, db.Exec(`UPDATE "drive_file" SET "storedInternal" = true WHERE "id" = 'm1'`).Error)
	ok, err = repo.MarkMigrated(ctx, "m1", at.Add(2*time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	var m1 model.DriveFile
	require.NoError(t, db.Where(`"id" = ?`, "m1").Take(&m1).Error)
	assert.Nil(t, m1.ObjectStorageVerifiedAt, "a new copy kept the mark of the old one")

	// 確かめる処理の失敗は、移す処理の失敗とは別に数える。
	require.NoError(t, repo.RecordFailure(ctx, "m2", DriveStorageMigrationOperationVerify, "md5", at))
	require.NoError(t, repo.RecordFailure(ctx, "m3", DriveStorageMigrationOperationMigrate, "x", at))
	require.NoError(t, repo.RecordFailure(ctx, "lc", DriveStorageMigrationOperationMigrate, "y", at))
	c, err = repo.Counts(ctx)
	require.NoError(t, err)
	assert.Equal(t, DriveStorageMigrationCounts{Local: 1, Migrated: 3, Failed: 2, VerifyFailed: 1}, c)
	list, err := repo.ListFailures(ctx, DriveStorageMigrationOperationVerify, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "m2", list[0].FileID)
}

func TestDriveStorageMigrationRepository_Purge(t *testing.T) {
	ctx := context.Background()
	db := driveStorageMigrationDB(t)
	repo := NewDriveStorageMigrationRepository(db)
	for _, id := range []string{"p3", "p1", "p2", "nv", "lc"} {
		storageMigFile(t, db, id, true, false)
	}
	at := time.Date(2026, 11, 15, 0, 0, 0, 123456789, time.UTC)
	for _, id := range []string{"p1", "p2", "p3", "nv"} {
		ok, err := repo.MarkMigrated(ctx, id, at)
		require.NoError(t, err)
		require.True(t, ok)
	}
	files, err := repo.ListMigrated(ctx, "", 10)
	require.NoError(t, err)
	migratedAt := map[string]time.Time{}
	for _, f := range files {
		migratedAt[f.ID] = *f.ObjectStorageMigratedAt
		if f.ID != "nv" {
			ok, err := repo.SetVerified(ctx, f.ID, *f.ObjectStorageMigratedAt, &at)
			require.NoError(t, err)
			require.True(t, ok)
		}
	}

	// 確かめた行だけを id 順に返す。確かめていない行とローカルの行は返さない。
	got, err := repo.ListPurgeable(ctx, "", 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"p1", "p2"}, []string{got[0].ID, got[1].ID})
	got, err = repo.ListPurgeable(ctx, "p2", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "p3", got[0].ID)
	got, err = repo.ListPurgeable(ctx, "a\x00b", 10)
	require.NoError(t, err)
	assert.Empty(t, got)
	c, err := repo.Counts(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(3), c.Purgeable)

	deletedAt := at.Add(time.Hour)
	ok, err := repo.MarkLocalDeleted(ctx, "p1", migratedAt["p1"], deletedAt)
	require.NoError(t, err)
	assert.True(t, ok)
	// 同じ実行のジョブが重なっても、消した日時を上書きしない。
	ok, err = repo.MarkLocalDeleted(ctx, "p1", migratedAt["p1"], deletedAt.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, ok)
	// 移した日時が違う (写し直された) 行と、移していない行には書かない。
	ok, err = repo.MarkLocalDeleted(ctx, "p2", at.Add(time.Second), deletedAt)
	require.NoError(t, err)
	assert.False(t, ok)
	for _, id := range []string{"lc", "missing", "a\x00b"} {
		ok, err := repo.MarkLocalDeleted(ctx, id, at, deletedAt)
		require.NoError(t, err)
		assert.False(t, ok, id)
	}
	got, err = repo.ListPurgeable(ctx, "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"p2", "p3"}, []string{got[0].ID, got[1].ID}, "a purged row was listed again")

	require.NoError(t, repo.RecordFailure(ctx, "p2", DriveStorageMigrationOperationPurge, "kept", at))
	require.NoError(t, repo.RecordFailure(ctx, "p3", DriveStorageMigrationOperationVerify, "x", at))
	require.NoError(t, repo.RecordFailure(ctx, "nv", DriveStorageMigrationOperationVerify, "y", at))
	c, err = repo.Counts(ctx)
	require.NoError(t, err)
	assert.Equal(t, DriveStorageMigrationCounts{Local: 1, Migrated: 4, Verified: 3, VerifyFailed: 2,
		Purgeable: 2, LocalDeleted: 1, PurgeFailed: 1}, c)

	// 戻すと、移した・確かめた・ローカルを消した日時がすべて消え、ローカルの行になる (#4183)。
	ok, err = repo.MarkRestored(ctx, "p2", at.Add(time.Second), nil, nil)
	require.NoError(t, err)
	assert.False(t, ok, "a row migrated at another time was moved back")
	for _, id := range []string{"lc", "missing", "a\x00b"} {
		ok, err := repo.MarkRestored(ctx, id, at, nil, nil)
		require.NoError(t, err)
		assert.False(t, ok, id)
	}
	// 移しただけ (ローカルを消していない) の行は、消した日時を NULL として突き合わせて戻す。
	// 一番多い経路。条件を `= ?` にすると NULL どうしが一致せず、どの行も戻らない。
	ok, err = repo.MarkRestored(ctx, "p3", migratedAt["p3"], nil, nil)
	require.NoError(t, err)
	assert.True(t, ok, "a migrated row whose local copy was not purged was not moved back")
	local, err := repo.IsStoredLocally(ctx, "p3")
	require.NoError(t, err)
	require.True(t, local)
	require.NoError(t, db.Exec(`UPDATE "drive_file" SET "storedInternal" = false, "objectStorageMigratedAt" = ? WHERE "id" = 'p3'`, migratedAt["p3"]).Error)
	// リンクに倒した行は、移した日時が残っていても戻さない。
	require.NoError(t, db.Exec(`UPDATE "drive_file" SET "isLink" = true WHERE "id" = 'p3'`).Error)
	ok, err = repo.MarkRestored(ctx, "p3", migratedAt["p3"], nil, nil)
	require.NoError(t, err)
	assert.False(t, ok, "a link row was moved back")
	require.NoError(t, db.Exec(`UPDATE "drive_file" SET "isLink" = false WHERE "id" = 'p3'`).Error)
	// ローカルを消した行も戻せ、消した日時が消える。
	ok, err = repo.MarkLocalDeleted(ctx, "p2", migratedAt["p2"], deletedAt)
	require.NoError(t, err)
	require.True(t, ok)
	// 読んだ後でローカルを消された (消した日時が読んだ値と違う) 行は倒さない。
	ok, err = repo.MarkRestored(ctx, "p2", migratedAt["p2"], nil, nil)
	require.NoError(t, err)
	assert.False(t, ok, "a row purged after it was read was moved back")
	local, err = repo.IsStoredLocally(ctx, "p2")
	require.NoError(t, err)
	assert.False(t, local)
	var read model.DriveFile
	require.NoError(t, db.Where(`"id" = ?`, "p2").Take(&read).Error)
	ok, err = repo.MarkRestored(ctx, "p2", migratedAt["p2"], read.ObjectStorageLocalDeletedAt, nil)
	require.NoError(t, err)
	assert.True(t, ok)
	local, err = repo.IsStoredLocally(ctx, "p2")
	require.NoError(t, err)
	assert.True(t, local)
	for _, id := range []string{"missing", "a\x00b"} {
		local, err := repo.IsStoredLocally(ctx, id)
		require.NoError(t, err)
		assert.False(t, local, id)
	}
	var p2 model.DriveFile
	require.NoError(t, db.Where(`"id" = ?`, "p2").Take(&p2).Error)
	assert.True(t, p2.StoredInternal)
	assert.Nil(t, p2.ObjectStorageMigratedAt)
	assert.Nil(t, p2.ObjectStorageVerifiedAt)
	assert.Nil(t, p2.ObjectStorageLocalDeletedAt)
	// 戻した行には、ローカルを消す処理の印が当たらない。
	ok, err = repo.MarkLocalDeleted(ctx, "p2", migratedAt["p2"], deletedAt)
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = repo.MarkRestored(ctx, "p2", migratedAt["p2"], nil, nil)
	require.NoError(t, err)
	assert.False(t, ok, "a row moved back twice")
	require.NoError(t, repo.RecordFailure(ctx, "p3", DriveStorageMigrationOperationRestore, "neither", at))
	c, err = repo.Counts(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), c.RestoreFailed)
	require.NoError(t, repo.ClearFailure(ctx, "p3", DriveStorageMigrationOperationRestore))

	// 写し直すと、ローカルを消した日時も消える (写したのはローカルにあった実体)。
	require.NoError(t, db.Exec(`UPDATE "drive_file" SET "storedInternal" = true WHERE "id" = 'p1'`).Error)
	ok, err = repo.MarkMigrated(ctx, "p1", at.Add(2*time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	var p1 model.DriveFile
	require.NoError(t, db.Where(`"id" = ?`, "p1").Take(&p1).Error)
	assert.Nil(t, p1.ObjectStorageLocalDeletedAt)
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

// 戻した写しの記録 (#4197): 行を倒すのと同じトランザクションで残し、写し直したら外す。
func TestDriveStorageMigrationRepository_RestoredCopies(t *testing.T) {
	ctx := context.Background()
	db := driveStorageMigrationDB(t)
	repo := NewDriveStorageMigrationRepository(db)
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"r1", "r2"} {
		storageMigFile(t, db, id, true, false)
		ok, err := repo.MarkMigrated(ctx, id, at)
		require.NoError(t, err)
		require.True(t, ok)
	}
	copies := func(id, location string) []model.DriveObjectStorageRestoredCopy {
		var out []model.DriveObjectStorageRestoredCopy
		for _, k := range []string{"ak-", "tk-"} {
			out = append(out, model.DriveObjectStorageRestoredCopy{Key: k + id, FileID: id, Location: location + "/" + k + id, RestoredAt: at})
		}
		return out
	}
	count := func() (int64, int64) {
		t.Helper()
		c, err := repo.Counts(ctx)
		require.NoError(t, err)
		return c.RestoredCopies, c.CleanupFailed
	}

	// 倒せなかった (移した日時が違う) ときは記録しない。
	ok, err := repo.MarkRestored(ctx, "r1", at.Add(time.Second), nil, copies("r1", "b"))
	require.NoError(t, err)
	require.False(t, ok)
	n, _ := count()
	assert.Zero(t, n, "copies were recorded for a row that was not moved back")

	ok, err = repo.MarkRestored(ctx, "r1", at, nil, copies("r1", "b"))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = repo.MarkRestored(ctx, "r2", at, nil, copies("r2", "b"))
	require.NoError(t, err)
	require.True(t, ok)
	n, failed := count()
	assert.EqualValues(t, 4, n)
	assert.Zero(t, failed)

	got, err := repo.ListRestoredCopies(ctx, "", 3)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, []string{"ak-r1", "ak-r2", "tk-r1"}, []string{got[0].Key, got[1].Key, got[2].Key})
	got, err = repo.ListRestoredCopies(ctx, "tk-r1", 3)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "tk-r2", got[0].Key)
	assert.Equal(t, "r2", got[0].FileID)
	assert.Equal(t, "b/tk-r2", got[0].Location)
	of, err := repo.ListRestoredCopiesOfFile(ctx, "r1")
	require.NoError(t, err)
	assert.Len(t, of, 2)
	for _, bad := range []string{"a\x00b"} {
		got, err := repo.ListRestoredCopies(ctx, bad, 3)
		require.NoError(t, err)
		assert.Empty(t, got)
		of, err := repo.ListRestoredCopiesOfFile(ctx, bad)
		require.NoError(t, err)
		assert.Empty(t, of)
	}

	// 失敗は場所が一致する記録にだけ残る。
	require.NoError(t, repo.RecordRestoredCopyFailure(ctx, "ak-r1", "elsewhere/ak-r1", "x", at))
	_, failed = count()
	assert.Zero(t, failed, "a failure was recorded on a copy at another location")
	require.NoError(t, repo.RecordRestoredCopyFailure(ctx, "ak-r1", "b/ak-r1", "s3 down", at))
	_, failed = count()
	assert.EqualValues(t, 1, failed)
	fails, err := repo.ListRestoredCopyFailures(ctx, 10)
	require.NoError(t, err)
	require.Len(t, fails, 1)
	require.NotNil(t, fails[0].Error)
	assert.Equal(t, "s3 down", *fails[0].Error)

	// 削除も場所が一致する記録だけ。
	require.NoError(t, repo.DeleteRestoredCopy(ctx, "tk-r1", "elsewhere/tk-r1"))
	n, _ = count()
	assert.EqualValues(t, 4, n, "a record at another location was deleted")
	require.NoError(t, repo.DeleteRestoredCopy(ctx, "tk-r1", "b/tk-r1"))
	n, _ = count()
	assert.EqualValues(t, 3, n)

	// もう一度移して戻すと、同じキーの記録を今の場所で置き換え、失敗も消す。まず移し直すと、
	// そのファイルの記録が外れる (写し直した写しが配信する実体になる)。
	ok, err = repo.MarkMigrated(ctx, "r1", at.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	of, err = repo.ListRestoredCopiesOfFile(ctx, "r1")
	require.NoError(t, err)
	assert.Empty(t, of, "the records of a file migrated again were kept")
	of, err = repo.ListRestoredCopiesOfFile(ctx, "r2")
	require.NoError(t, err)
	assert.Len(t, of, 2, "the records of another file were dropped")
	found, err := repo.FindDriveFile(ctx, "r1")
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.False(t, found.StoredInternal)
	// 古い記録が残っている (移し直す前の世代の DB など) ところへ戻すと、置き換える。
	require.NoError(t, db.Create(&model.DriveObjectStorageRestoredCopy{Key: "ak-r1", FileID: "r1", Location: "old/ak-r1", RestoredAt: at,
		Error: ptrString("old failure"), FailedAt: &at}).Error)
	ok, err = repo.MarkRestored(ctx, "r1", at.Add(time.Hour), nil, copies("r1", "c"))
	require.NoError(t, err)
	require.True(t, ok)
	of, err = repo.ListRestoredCopiesOfFile(ctx, "r1")
	require.NoError(t, err)
	require.Len(t, of, 2)
	assert.Equal(t, "c/ak-r1", of[0].Location)
	assert.Nil(t, of[0].Error, "the old failure was kept")
	assert.Nil(t, of[0].FailedAt)
	found, err = repo.FindDriveFile(ctx, "r1")
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.True(t, found.StoredInternal)
	for _, id := range []string{"missing", "a\x00b"} {
		found, err := repo.FindDriveFile(ctx, id)
		require.NoError(t, err)
		assert.Nil(t, found, id)
	}

	// ファイルの行を消しても、記録は残る (写しはバケットに残るので、消す手掛かりを失わない)。
	require.NoError(t, db.Exec(`DELETE FROM "drive_file" WHERE "id" = 'r2'`).Error)
	of, err = repo.ListRestoredCopiesOfFile(ctx, "r2")
	require.NoError(t, err)
	assert.Len(t, of, 2, "deleting the file dropped the records of its copies")
}
