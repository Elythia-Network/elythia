package repository

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/elythia-network/elythia/internal/model"
)

// The failure operations of the migration: copying local drive files to
// object storage (#4180), verifying the copies (#4181) and deleting the
// local copies (#4182).
const (
	DriveStorageMigrationOperationMigrate = "migrate"
	DriveStorageMigrationOperationVerify  = "verify"
	DriveStorageMigrationOperationPurge   = "purge"
)

// DriveStorageMigrationRepository reads and writes what the
// local-to-object-storage migration of drive files needs (#4180): the files
// still stored locally, the mark of migrated files, and the failures.
type DriveStorageMigrationRepository struct {
	db *gorm.DB
}

// NewDriveStorageMigrationRepository constructs a
// DriveStorageMigrationRepository.
func NewDriveStorageMigrationRepository(db *gorm.DB) *DriveStorageMigrationRepository {
	return &DriveStorageMigrationRepository{db: db}
}

// localStored selects the files whose bytes are on the local storage: the
// rows the migration copies.
//
// isLink の行はリモートのファイルの参照で、ローカルにもオブジェクトストレージにも
// 実体を持たない (storedInternal は false だが、念のため両方を見る)。
func localStored(db *gorm.DB) *gorm.DB {
	return db.Model(&model.DriveFile{}).Where(`"storedInternal" = true AND "isLink" = false`)
}

// ListLocal returns up to limit files stored locally whose id is greater
// than afterID, in id order. An empty afterID starts from the first file.
func (r *DriveStorageMigrationRepository) ListLocal(ctx context.Context, afterID string, limit int) ([]model.DriveFile, error) {
	if !storable(afterID) {
		return nil, nil
	}
	q := localStored(r.db.WithContext(ctx))
	if afterID != "" {
		q = q.Where(`"id" > ?`, afterID)
	}
	var out []model.DriveFile
	if err := q.Order(`"id" ASC`).Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// MarkMigrated marks a locally stored file as stored in object storage and
// copied by the migration at at. It reports false when the file is no longer
// stored locally (already migrated, or deleted).
func (r *DriveStorageMigrationRepository) MarkMigrated(ctx context.Context, id string, at time.Time) (bool, error) {
	if !storable(id) {
		return false, nil
	}
	// storedInternal を条件に入れ、並行した別の処理が先に書き換えた行を上書きしない。
	res := r.db.WithContext(ctx).Model(&model.DriveFile{}).
		Where(`"id" = ? AND "storedInternal" = true AND "isLink" = false`, id).
		// 写し直した実体は確かめ直すまで信用しないので、確かめた日時を消す (#4181)。
		// 写したのはローカルにあった実体なので、ローカルを消した日時も消す (#4182)。
		Updates(map[string]any{"storedInternal": false, "objectStorageMigratedAt": at,
			"objectStorageVerifiedAt": nil, "objectStorageLocalDeletedAt": nil})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// migratedStored selects the files the migration copied whose bytes are in
// object storage: the rows the verification reads.
func migratedStored(db *gorm.DB) *gorm.DB {
	return db.Model(&model.DriveFile{}).
		Where(`"storedInternal" = false AND "isLink" = false AND "objectStorageMigratedAt" IS NOT NULL`)
}

// ListMigrated returns up to limit migrated files whose id is greater than
// afterID, in id order. An empty afterID starts from the first file.
func (r *DriveStorageMigrationRepository) ListMigrated(ctx context.Context, afterID string, limit int) ([]model.DriveFile, error) {
	if !storable(afterID) {
		return nil, nil
	}
	q := migratedStored(r.db.WithContext(ctx))
	if afterID != "" {
		q = q.Where(`"id" > ?`, afterID)
	}
	var out []model.DriveFile
	if err := q.Order(`"id" ASC`).Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// SetVerified records the result of verifying a migrated file: at when the
// copy matched, nil when it did not. It reports false when the file is no
// longer in the migrated state (deleted, made a link, or migrated back).
//
// migratedAt は、確かめ始めたときに読んだ移した日時。確かめている間に写し直された
// 行 (移した日時が変わる) には、古い実体を確かめた結果を書かない。
func (r *DriveStorageMigrationRepository) SetVerified(ctx context.Context, id string, migratedAt time.Time, at *time.Time) (bool, error) {
	if !storable(id) {
		return false, nil
	}
	res := migratedStored(r.db.WithContext(ctx)).
		Where(`"id" = ? AND "objectStorageMigratedAt" = ?`, id, migratedAt).
		Update("objectStorageVerifiedAt", at)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// ListPurgeable returns up to limit verified files whose local copy has not
// been deleted, with id greater than afterID, in id order (#4182).
func (r *DriveStorageMigrationRepository) ListPurgeable(ctx context.Context, afterID string, limit int) ([]model.DriveFile, error) {
	if !storable(afterID) {
		return nil, nil
	}
	q := purgeable(r.db.WithContext(ctx))
	if afterID != "" {
		q = q.Where(`"id" > ?`, afterID)
	}
	var out []model.DriveFile
	if err := q.Order(`"id" ASC`).Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// purgeable selects the migrated files that were verified and whose local
// copy has not been deleted: the rows the purge handles.
func purgeable(db *gorm.DB) *gorm.DB {
	return migratedStored(db).
		Where(`"objectStorageVerifiedAt" IS NOT NULL AND "objectStorageLocalDeletedAt" IS NULL`)
}

// MarkLocalDeleted records that the local copy of a migrated file was deleted
// at at. It reports false when the file is no longer in the migrated state
// with the migratedAt read before (deleted, made a link, migrated again or
// migrated back), or its local copy was already marked as deleted.
func (r *DriveStorageMigrationRepository) MarkLocalDeleted(ctx context.Context, id string, migratedAt, at time.Time) (bool, error) {
	if !storable(id) {
		return false, nil
	}
	res := migratedStored(r.db.WithContext(ctx)).
		Where(`"id" = ? AND "objectStorageMigratedAt" = ? AND "objectStorageLocalDeletedAt" IS NULL`, id, migratedAt).
		Update("objectStorageLocalDeletedAt", at)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// IsMigrated reports whether the file has a row in the migrated state:
// stored in object storage (not a link) and marked as migrated.
func (r *DriveStorageMigrationRepository) IsMigrated(ctx context.Context, id string) (bool, error) {
	if !storable(id) {
		return false, nil
	}
	var n int64
	if err := r.db.WithContext(ctx).Model(&model.DriveFile{}).
		Where(`"id" = ? AND "storedInternal" = false AND "isLink" = false AND "objectStorageMigratedAt" IS NOT NULL`, id).
		Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

// RecordFailure records or replaces the failure of operation on the file.
// A file deleted meanwhile is not recorded (the foreign key refuses it).
func (r *DriveStorageMigrationRepository) RecordFailure(ctx context.Context, fileID, operation, msg string, at time.Time) error {
	if !storable(fileID) {
		return nil
	}
	f := model.DriveObjectStorageMigrationFailure{FileID: fileID, Operation: operation, Error: msg, FailedAt: at}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "fileId"}, {Name: "operation"}},
		DoUpdates: clause.AssignmentColumns([]string{"error", "failedAt"}),
	}).Create(&f).Error
	// 一覧を取った後にファイルが消されると、外部キーが記録を断る。消えたファイルの
	// 失敗は記録しても意味が無いので、成功として扱う (バッチ全体を止めない)。
	if isForeignKeyViolation(err) {
		return nil
	}
	return err
}

// ClearFailure removes the failure of operation on the file, if any.
func (r *DriveStorageMigrationRepository) ClearFailure(ctx context.Context, fileID, operation string) error {
	if !storable(fileID) {
		return nil
	}
	return r.db.WithContext(ctx).
		Where(`"fileId" = ? AND "operation" = ?`, fileID, operation).
		Delete(&model.DriveObjectStorageMigrationFailure{}).Error
}

// failuresOfFiles selects the failures of operation on files that are not
// links.
//
// 失敗を記録した後で link 行へ倒された行 (リモートのキャッシュの片付けなど) は、
// 二度と移す対象に出てこないので取り直されず、記録が残り続ける。数えない。
func failuresOfFiles(db *gorm.DB, operation string) *gorm.DB {
	return db.Model(&model.DriveObjectStorageMigrationFailure{}).
		Joins(`JOIN "drive_file" ON "drive_file"."id" = "drive_object_storage_migration_failure"."fileId"`).
		Where(`"drive_object_storage_migration_failure"."operation" = ? AND "drive_file"."isLink" = false`, operation)
}

// ListFailures returns up to limit failures of operation, newest first.
func (r *DriveStorageMigrationRepository) ListFailures(ctx context.Context, operation string, limit int) ([]model.DriveObjectStorageMigrationFailure, error) {
	var out []model.DriveObjectStorageMigrationFailure
	if err := failuresOfFiles(r.db.WithContext(ctx), operation).
		Select(`"drive_object_storage_migration_failure".*`).
		Order(`"failedAt" DESC, "fileId" ASC`).
		Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// DriveStorageMigrationCounts are the numbers the migration status shows.
type DriveStorageMigrationCounts struct {
	// Local is the number of files still stored locally.
	Local int64
	// Migrated is the number of files the migration copied.
	Migrated int64
	// Failed is the number of files whose last migration attempt failed.
	Failed int64
	// Verified is the number of migrated files whose copy was verified.
	Verified int64
	// VerifyFailed is the number of files whose last verification failed.
	VerifyFailed int64
	// Purgeable is the number of verified files whose local copy has not been
	// deleted.
	Purgeable int64
	// LocalDeleted is the number of migrated files whose local copy was
	// deleted.
	LocalDeleted int64
	// PurgeFailed is the number of files whose last purge attempt failed.
	PurgeFailed int64
}

// Counts returns the numbers of local, migrated, verified and failed files.
//
// drive_file を 1 回の走査で数える。どちらの条件にも索引は無いので、2 回に分けると
// 管理画面が状態を取り直すたびに全件を 2 回読む。
func (r *DriveStorageMigrationRepository) Counts(ctx context.Context) (DriveStorageMigrationCounts, error) {
	var c DriveStorageMigrationCounts
	db := r.db.WithContext(ctx)
	if err := db.Raw(`SELECT
		count(*) FILTER (WHERE "storedInternal" = true AND "isLink" = false) AS "local",
		count(*) FILTER (WHERE "objectStorageMigratedAt" IS NOT NULL) AS "migrated",
		count(*) FILTER (WHERE "objectStorageVerifiedAt" IS NOT NULL) AS "verified",
		count(*) FILTER (WHERE "storedInternal" = false AND "isLink" = false AND "objectStorageMigratedAt" IS NOT NULL
			AND "objectStorageVerifiedAt" IS NOT NULL AND "objectStorageLocalDeletedAt" IS NULL) AS "purgeable",
		count(*) FILTER (WHERE "objectStorageLocalDeletedAt" IS NOT NULL) AS "localDeleted"
		FROM "drive_file"`).Row().Scan(&c.Local, &c.Migrated, &c.Verified, &c.Purgeable, &c.LocalDeleted); err != nil {
		return c, err
	}
	if err := failuresOfFiles(db, DriveStorageMigrationOperationMigrate).Count(&c.Failed).Error; err != nil {
		return c, err
	}
	if err := failuresOfFiles(db, DriveStorageMigrationOperationVerify).Count(&c.VerifyFailed).Error; err != nil {
		return c, err
	}
	if err := failuresOfFiles(db, DriveStorageMigrationOperationPurge).Count(&c.PurgeFailed).Error; err != nil {
		return c, err
	}
	return c, nil
}
