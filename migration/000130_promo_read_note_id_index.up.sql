-- #3590: `promo_read."noteId"` に index を張る。
--
-- `promo_read."noteId"` の FK は `ON DELETE CASCADE` なので、`note` を 1 行消すたびに
-- FK の処理が `promo_read` を seq scan していた (000029 の index は `userId` が先頭の
-- ものだけ)。000127 の `note_unread` と同じ形。
--
-- 本家にはこの列だけの index は無いので、Elythia 独自の名前にする。
-- 行数が読めないので CONCURRENTLY で書き込みを止めずに作る。失敗したときの回復は
-- docs/deployment.md の「大きな表に index を足す migration」にある。
CREATE INDEX CONCURRENTLY IF NOT EXISTS "IDX_promo_read_noteId" ON "promo_read" ("noteId");
