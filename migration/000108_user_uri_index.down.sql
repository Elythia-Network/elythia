-- CONCURRENTLY で作った index は CONCURRENTLY で drop して write block を避ける
-- (DROP INDEX CONCURRENTLY も transaction 外・単一文でのみ実行可能)。
--
-- TS 製 DB では up が何もしていない (upstream の Init が作った同名の index が
-- 既にある) が、down は名前でしか判定できないのでそれも落とす (000091 と同じ)。
-- TS 版へ戻すなら、up の CREATE INDEX を流し直すこと。
DROP INDEX CONCURRENTLY IF EXISTS "IDX_be623adaa4c566baf5d29ce0c8";
