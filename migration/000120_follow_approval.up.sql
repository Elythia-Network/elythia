-- #3466: 作られてから日の浅いアカウントからのフォローを止める設定。
--
-- followApprovalLocalSeconds / followApprovalRemoteSeconds は本家の
-- PR 17998 (migration FollowApprovalByAccountAge1791109435844) と同じ列名・型。
-- 本家に追従するときは、この 2 列を足す migration を重ねて作らない。
-- NULL と 0 はどちらも「止めない」。値は秒で、i/update が 0〜2592000 に絞る。
--
-- 本家の版 (2026.10.0) にはまだ無い列なので、この DB を TS へ戻して、この
-- migration を含む本家の版へ上げると、本家の migration が already exists で
-- 落ちる。復路は保証しない (#3191)。
ALTER TABLE "user_profile" ADD COLUMN IF NOT EXISTS "followApprovalLocalSeconds" integer;
ALTER TABLE "user_profile" ADD COLUMN IF NOT EXISTS "followApprovalRemoteSeconds" integer;

-- 止め方 (Elythia 独自、追加のみ)。'request' (本家と同じ。リクエストにして
-- 通知を出す) / 'silentRequest' (リクエストにするが通知を出さない) /
-- 'silentFollow' (フォローを成立させ、通知を出さない)。既定は本家と同じ動き。
-- TS は読まない。
ALTER TABLE "user_profile" ADD COLUMN IF NOT EXISTS "followApprovalAction" varchar(16) NOT NULL DEFAULT 'request';

-- 'silentFollow' で通知を出さずに成立させたフォローの記録 (Elythia 独自)。
-- 受け手が following/silent/list で後から確かめる。同じ人から何度来ても 1 行に
-- まとめ、id (= 日時) を最後に来たときのものに書き換える。フォローが解除されても
-- 消さない (確かめたいのは「誰が来たか」なので)。どちらかの利用者が消えたら
-- 行も消える。TS は未知のテーブルを無視する。
CREATE TABLE IF NOT EXISTS "silent_follow" (
    "id"         varchar(32) NOT NULL PRIMARY KEY,
    "followerId" varchar(32) NOT NULL REFERENCES "user" ("id") ON DELETE CASCADE,
    "followeeId" varchar(32) NOT NULL REFERENCES "user" ("id") ON DELETE CASCADE,
    CONSTRAINT "UQ_silent_follow_followeeId_followerId" UNIQUE ("followeeId", "followerId")
);

-- following/silent/list は受け手ごとに id の順で引く。
CREATE INDEX IF NOT EXISTS "IDX_silent_follow_followeeId_id" ON "silent_follow" ("followeeId", "id");
-- 退会した follower の行を CASCADE で消すときの FK 側の index。
CREATE INDEX IF NOT EXISTS "IDX_silent_follow_followerId" ON "silent_follow" ("followerId");
