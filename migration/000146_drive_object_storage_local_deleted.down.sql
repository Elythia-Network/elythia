-- data loss: ローカルの実体を消した日時が失われる。消した実体は戻らず、どのファイルの
-- ローカルを消したかが DB から分からなくなる。
ALTER TABLE "drive_file" DROP COLUMN IF EXISTS "objectStorageLocalDeletedAt";
