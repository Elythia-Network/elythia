-- TS 版から移った DB では、本家が作った同じ名前の index も落とす
-- (000091 の down と同じ扱い。TS 版へ戻すことは保証していない、#3191)。
DROP INDEX CONCURRENTLY IF EXISTS "IDX_e637cba4dc4410218c4251260e";
