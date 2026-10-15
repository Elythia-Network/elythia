-- #3590: `antenna_note_unread."noteId"` に index を張る。
--
-- `antenna_note_unread."noteId"` の FK は `ON DELETE CASCADE` なので、`note` を 1 行消すたびに
-- FK の処理が `antenna_note_unread` を seq scan していた (000037 の index は `userId` が先頭の
-- ものだけ)。000127 の `note_unread` と同じ形。
--
-- 本家にはこの列だけの index は無いので、Elythia 独自の名前にする。
-- 行数が読めないので CONCURRENTLY で書き込みを止めずに作る。失敗したときの回復は
-- docs/deployment.md の「大きな表に index を足す migration」にある。
CREATE INDEX CONCURRENTLY IF NOT EXISTS "IDX_antenna_note_unread_noteId" ON "antenna_note_unread" ("noteId");
