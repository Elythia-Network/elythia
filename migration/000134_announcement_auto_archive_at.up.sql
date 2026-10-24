-- #3809: お知らせの自動アーカイブ (本家 2026.10.1、#17704)。
--
-- 本家の migration AutoArchiveAnnouncements1783913268595 と同じ列名・型・コメント。
-- TS 2026.10.1 以降の DB には既に列があるので IF NOT EXISTS で重ねない。
-- 期限を過ぎたものは読むときにアーカイブ扱いにし、isActive を false に落とすのは
-- 定期処理 (maintenance:archiveAnnouncements) が行う。
ALTER TABLE "announcement" ADD COLUMN IF NOT EXISTS "autoArchiveAt" TIMESTAMP WITH TIME ZONE;
COMMENT ON COLUMN "announcement"."autoArchiveAt" IS 'The date after which the Announcement is automatically archived.';
