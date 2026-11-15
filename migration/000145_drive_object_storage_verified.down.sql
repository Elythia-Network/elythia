-- data loss: オブジェクトストレージの実体を確かめた日時が失われる。もう一度確かめる
-- 処理を流せば付け直せる。
ALTER TABLE "drive_file" DROP COLUMN IF EXISTS "objectStorageVerifiedAt";
