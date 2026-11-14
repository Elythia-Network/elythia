-- data loss: オブジェクトストレージへ写した日時と、失敗の記録が失われる。写した
-- ファイルはオブジェクトストレージに残り、storedInternal=false のまま配信され続けるが、
-- どれを移す処理で写したかが分からなくなり、戻す処理 (#4183 で足す予定) で戻せなくなる。
DROP TABLE IF EXISTS "drive_object_storage_migration_failure";
ALTER TABLE "drive_file" DROP COLUMN IF EXISTS "objectStorageMigratedAt";
