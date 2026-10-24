-- data loss: お知らせの自動アーカイブの日時が失われる。期限を過ぎて定期処理が
-- isActive = false に落としたものはアーカイブのまま残るが、まだ落としていないものは
-- 期限の無いアクティブなお知らせに戻る。
ALTER TABLE "announcement" DROP COLUMN IF EXISTS "autoArchiveAt";
