-- #4277: 本家 2026.11.1 で増えた TypeORM migration 1 件を、migrations テーブルへ
-- seed する。形は 000067 の末尾の INSERT と同じ (000067 / 000135 は書き換えない)。
--
-- seed が漏れると、Elythia で動かした DB に本家を繋ぎ直したときに TypeORM が
-- その migration を未実行と判定して再実行し、適用済みの列への ADD COLUMN で
-- 止まる (#2244)。**seed する前に、対応する DDL が Elythia 側にも入っていることを
-- 確認済み** (入っていないまま seed すると TS が skip して schema がずれたまま
-- 放置される):
--
--   FollowingRequestId1794917481157
--       → 000148 が following.requestId を同じ列名・型 (character varying(512)、
--         NULL 可)・同じコメントで追加済み
--
-- name 一致で存在確認するので、本家が既に書いた行は二重挿入しない。
INSERT INTO "migrations" ("timestamp", "name")
SELECT v.ts, v.n FROM (VALUES
    (1794917481157::bigint, 'FollowingRequestId1794917481157')
) AS v(ts, n)
WHERE NOT EXISTS (SELECT 1 FROM "migrations" m WHERE m."name" = v.n);
