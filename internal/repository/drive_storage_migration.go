package repository

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/elythia-network/elythia/internal/model"
)

// The failure operations of the migration: copying local drive files to
// object storage (#4180), verifying the copies (#4181) and deleting the
// local copies (#4182), and moving the migrated files back (#4183).
const (
	DriveStorageMigrationOperationMigrate = "migrate"
	DriveStorageMigrationOperationVerify  = "verify"
	DriveStorageMigrationOperationPurge   = "purge"
	DriveStorageMigrationOperationRestore = "restore"
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
	var marked bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// storedInternal を条件に入れ、並行した別の処理が先に書き換えた行を上書きしない。
		res := tx.Model(&model.DriveFile{}).
			Where(`"id" = ? AND "storedInternal" = true AND "isLink" = false`, id).
			// 写し直した実体は確かめ直すまで信用しないので、確かめた日時を消す (#4181)。
			// 写したのはローカルにあった実体なので、ローカルを消した日時も消す (#4182)。
			Updates(map[string]any{"storedInternal": false, "objectStorageMigratedAt": at,
				"objectStorageVerifiedAt": nil, "objectStorageLocalDeletedAt": nil})
		if res.Error != nil {
			return res.Error
		}
		marked = res.RowsAffected == 1
		if !marked {
			return nil
		}
		// 戻した後にもう一度移したファイルは、同じキーへ写し直したので、残っていた写しが
		// 配信する実体になる。記録を残すと、写しを消す処理 (#4197) が配信中の実体を消す。
		return tx.Where(`"fileId" = ?`, id).Delete(&model.DriveObjectStorageRestoredCopy{}).Error
	})
	if err != nil {
		return false, err
	}
	return marked, nil
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

// MarkRestored moves a migrated file back to the local storage (#4183): it
// sets storedInternal and clears the marks of the migration, the
// verification and the purge. It records copies, the copies the file leaves
// in object storage, in the same transaction (#4197). It reports false when
// the file is no longer in the migrated state with the migratedAt and
// localDeletedAt read before (deleted, made a link, purged meanwhile, or
// already moved back); the copies are not recorded then.
//
// 移した日時とローカルを消した日時で突き合わせ、条件付きの 1 回の更新で倒す。倒した
// 行は storedInternal が true になり、ローカルを消す処理 (#4182) の MarkLocalDeleted
// に当たらない。逆に、読んだ後で消す処理が印を付けた (ローカルを消した) 行は倒さない。
func (r *DriveStorageMigrationRepository) MarkRestored(ctx context.Context, id string, migratedAt time.Time, localDeletedAt *time.Time, copies []model.DriveObjectStorageRestoredCopy) (bool, error) {
	if !storable(id) {
		return false, nil
	}
	var marked bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := migratedStored(tx).
			Where(`"id" = ? AND "objectStorageMigratedAt" = ? AND "objectStorageLocalDeletedAt" IS NOT DISTINCT FROM ?`, id, migratedAt, localDeletedAt).
			Updates(map[string]any{"storedInternal": true, "objectStorageMigratedAt": nil,
				"objectStorageVerifiedAt": nil, "objectStorageLocalDeletedAt": nil})
		if res.Error != nil {
			return res.Error
		}
		marked = res.RowsAffected == 1
		if !marked || len(copies) == 0 {
			return nil
		}
		// 行を倒すのと同じトランザクションで残す。別にすると、倒した後で記録に失敗したとき、
		// 戻した行の写しがどこにも記録されずバケットに残る。同じキーの古い記録 (前に戻した
		// ときのもの) は、今の場所と日時で置き換え、消せなかった記録も消す。
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{"fileId", "location", "restoredAt", "error", "failedAt"}),
		}).Create(&copies).Error
	})
	if err != nil {
		return false, err
	}
	return marked, nil
}

// ListRestoredCopies returns up to limit recorded copies whose key is greater
// than afterKey, in key order (#4197). An empty afterKey starts from the
// first copy.
func (r *DriveStorageMigrationRepository) ListRestoredCopies(ctx context.Context, afterKey string, limit int) ([]model.DriveObjectStorageRestoredCopy, error) {
	if !storable(afterKey) {
		return nil, nil
	}
	q := r.db.WithContext(ctx).Model(&model.DriveObjectStorageRestoredCopy{})
	if afterKey != "" {
		q = q.Where(`"key" > ?`, afterKey)
	}
	var out []model.DriveObjectStorageRestoredCopy
	if err := q.Order(`"key" ASC`).Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// ListRestoredCopiesOfFile returns the recorded copies of the file (#4197).
func (r *DriveStorageMigrationRepository) ListRestoredCopiesOfFile(ctx context.Context, fileID string) ([]model.DriveObjectStorageRestoredCopy, error) {
	if !storable(fileID) {
		return nil, nil
	}
	var out []model.DriveObjectStorageRestoredCopy
	if err := r.db.WithContext(ctx).Where(`"fileId" = ?`, fileID).
		Order(`"key" ASC`).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteRestoredCopy removes the record of the copy with key at location,
// once the copy was deleted (#4197). A record replaced meanwhile (the file
// was moved back again) is kept.
func (r *DriveStorageMigrationRepository) DeleteRestoredCopy(ctx context.Context, key, location string) error {
	if !storable(key) || !storable(location) {
		return nil
	}
	return r.db.WithContext(ctx).Where(`"key" = ? AND "location" = ?`, key, location).
		Delete(&model.DriveObjectStorageRestoredCopy{}).Error
}

// RecordRestoredCopyFailure records why the copy with key at location could
// not be deleted (#4197).
func (r *DriveStorageMigrationRepository) RecordRestoredCopyFailure(ctx context.Context, key, location, msg string, at time.Time) error {
	if !storable(key) || !storable(location) {
		return nil
	}
	return r.db.WithContext(ctx).Model(&model.DriveObjectStorageRestoredCopy{}).
		Where(`"key" = ? AND "location" = ?`, key, location).
		Updates(map[string]any{"error": msg, "failedAt": at}).Error
}

// ListRestoredCopyFailures returns up to limit copies whose last deletion
// failed, newest first (#4197).
func (r *DriveStorageMigrationRepository) ListRestoredCopyFailures(ctx context.Context, limit int) ([]model.DriveObjectStorageRestoredCopy, error) {
	var out []model.DriveObjectStorageRestoredCopy
	if err := r.db.WithContext(ctx).Where(`"failedAt" IS NOT NULL`).
		Order(`"failedAt" DESC, "key" ASC`).Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// FindDriveFile returns the file with id, or nil when there is none (#4197).
func (r *DriveStorageMigrationRepository) FindDriveFile(ctx context.Context, id string) (*model.DriveFile, error) {
	if !storable(id) {
		return nil, nil
	}
	var f model.DriveFile
	err := r.db.WithContext(ctx).Where(`"id" = ?`, id).Take(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// IsStoredLocally reports whether the file has a row stored locally (not a
// link).
func (r *DriveStorageMigrationRepository) IsStoredLocally(ctx context.Context, id string) (bool, error) {
	if !storable(id) {
		return false, nil
	}
	var n int64
	if err := r.db.WithContext(ctx).Model(&model.DriveFile{}).
		Where(`"id" = ? AND "storedInternal" = true AND "isLink" = false`, id).
		Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
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
	// RestoreFailed is the number of files whose last attempt to move back
	// failed.
	RestoreFailed int64
	// RestoredCopies is the number of copies left in object storage by moving
	// files back, and CleanupFailed the number of them whose last deletion
	// failed (#4197).
	RestoredCopies int64
	CleanupFailed  int64
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
	if err := failuresOfFiles(db, DriveStorageMigrationOperationRestore).Count(&c.RestoreFailed).Error; err != nil {
		return c, err
	}
	if err := db.Raw(`SELECT count(*), count(*) FILTER (WHERE "failedAt" IS NOT NULL)
		FROM "drive_object_storage_restored_copy"`).Row().Scan(&c.RestoredCopies, &c.CleanupFailed); err != nil {
		return c, err
	}
	return c, nil
}
