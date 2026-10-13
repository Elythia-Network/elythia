-- #3362 の戻し。
-- data loss: 受信を拒否するソフトウェアの設定が消える。
ALTER TABLE "meta" DROP COLUMN IF EXISTS "inboxSuspendedSoftware";
