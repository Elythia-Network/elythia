-- #3362: 受信を拒否するソフトウェアの設定 (Elythia 独自)。
--
-- 形は本家の meta."deliverSuspendedSoftware" と同じ [{software, versionRange}]。
-- 列を足すだけで、TS が書いた値には触れない。TS は知らない列を読まないので、
-- TS へ戻しても無視される。
ALTER TABLE "meta" ADD COLUMN IF NOT EXISTS "inboxSuspendedSoftware" jsonb NOT NULL DEFAULT '[]';
