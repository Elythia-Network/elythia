-- CONCURRENTLY で作った index は CONCURRENTLY で drop して書き込みを止めない。
-- TS 版から移した DB では元からあった index も消えるが、本家の index なので、
-- 戻した後に TS 版へ戻すことはしない (復路は保証しない、#3191)。
DROP INDEX CONCURRENTLY IF EXISTS "IDX_55720b33a61a7c806a8215b825";
