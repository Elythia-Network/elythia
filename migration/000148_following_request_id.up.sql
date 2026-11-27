-- #4277: フォローの関係の元になった Follow アクティビティの id (本家 2026.11.1、#18163)。
--
-- 本家の migration FollowingRequestId1794917481157 と同じ列名・型・コメント。
-- TS 2026.11.1 以降の DB には既に列があるので IF NOT EXISTS で重ねない。
-- リモートからのフォローを外すときの Reject と、TS が送った Follow の Undo が、
-- 元の Follow の id を参照できるようにする。
ALTER TABLE "following" ADD COLUMN IF NOT EXISTS "requestId" character varying(512);
COMMENT ON COLUMN "following"."requestId" IS 'id of Follow Activity.';
