-- CONCURRENTLY で作った index は CONCURRENTLY で drop して書き込みを止めない。
DROP INDEX CONCURRENTLY IF EXISTS "IDX_note_channelId_userId_id";
