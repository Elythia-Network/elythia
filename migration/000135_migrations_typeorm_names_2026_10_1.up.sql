-- #3809: 本家 2026.10.1 で増えた TypeORM migration 3 件を、migrations テーブルへ
-- seed する。形は 000067 の末尾の INSERT と同じ (000067 自体は書き換えない)。
--
-- seed が漏れると、Elythia で動かした DB に本家を繋ぎ直したときに TypeORM が
-- その migration を未実行と判定して再実行し、適用済みの列への ADD COLUMN で
-- 止まる (#2244)。**seed する前に、対応する DDL が Elythia 側にも入っていることを
-- 1 件ずつ確認済み** (入っていないまま seed すると TS が skip して schema が
-- ずれたまま放置される):
--
--   AutoArchiveAnnouncements1783913268595
--       → 000134 が announcement.autoArchiveAt を同じ型・同じコメントで追加済み
--   UserProfilePasswordComment1790477603333
--       → user_profile.password の列コメントを直すだけで、schema は変わらない。
--         Elythia は user_profile.password にコメントを付けていないので、足す
--         DDL は無い
--   FollowApprovalByAccountAge1791109435844
--       → 000120 が user_profile の followApprovalLocalSeconds /
--         followApprovalRemoteSeconds を同じ列名・型 (integer、NULL 可) で追加済み
--
-- name 一致で存在確認するので、本家が既に書いた行は二重挿入しない。
INSERT INTO "migrations" ("timestamp", "name")
SELECT v.ts, v.n FROM (VALUES
    (1783913268595::bigint, 'AutoArchiveAnnouncements1783913268595'),
    (1790477603333::bigint, 'UserProfilePasswordComment1790477603333'),
    (1791109435844::bigint, 'FollowApprovalByAccountAge1791109435844')
) AS v(ts, n)
WHERE NOT EXISTS (SELECT 1 FROM "migrations" m WHERE m."name" = v.n);
