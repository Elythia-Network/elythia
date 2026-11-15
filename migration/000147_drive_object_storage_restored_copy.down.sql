-- data loss: ローカルへ戻したファイルの、オブジェクトストレージに残る写しの記録が失われる。
-- 写しはバケットに残り、どれを消してよいかが DB から分からなくなる。
DROP TABLE IF EXISTS "drive_object_storage_restored_copy";
