-- CONCURRENTLY で作った index は CONCURRENTLY で drop して write block を避ける。
DROP INDEX CONCURRENTLY IF EXISTS "IDX_instance_iconUrl";
