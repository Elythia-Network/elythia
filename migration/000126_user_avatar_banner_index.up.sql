-- #3590: `user."avatarId"` / `"bannerId"` に index を張る。
--
-- 000001 の `FK_user_avatarId` / `FK_user_bannerId` は `ON DELETE SET NULL` なので、
-- `drive_file` を 1 行消すたびに FK の処理が `user` を 2 回 seq scan していた
-- (検証用の利用者 22,000 人で、リモートのファイル 1,000 件の削除が 4.6 秒)。
-- リモートのキャッシュの掃除、アカウント・インスタンスの削除が `user` の大きさに
-- 比例していた。
--
-- **TS 版から移った DB では作らない。** 本家は OneToOne の一意制約の index
-- (`REL_58f5c71eaab331645112cf8cfa` / `REL_afc64b53f8db3707ceb34eb28e`) を持ち、
-- FK の検索はそれで済む。名前を本家に揃える形 (`IF NOT EXISTS`) にできないのは、
-- 本家の index が UNIQUE で、Elythia の DB で一意を保証できているか確かめていない
-- ため (重複があると作成が失敗する)。そこで「同じ列を先頭に持つ有効な index が
-- 無いときだけ作る」形にした。
--
-- 条件付きにするため DO ブロックの中で作り、CONCURRENTLY は使えない
-- (CONCURRENTLY は関数・DO ブロックの中で実行できない)。作るあいだ `user` への
-- 書き込みが止まるが、NULL でない行だけの部分 index で、利用者 22,000 人の
-- fixture では 2 本で数十 ms だった。
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_index x
        JOIN pg_class t ON t.oid = x.indrelid
        JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = x.indkey[0]
        WHERE t.relname = 'user' AND t.relnamespace = to_regnamespace(current_schema())
          AND a.attname = 'avatarId' AND x.indisvalid
    ) THEN
        CREATE INDEX IF NOT EXISTS "IDX_user_avatarId" ON "user" ("avatarId") WHERE "avatarId" IS NOT NULL;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_index x
        JOIN pg_class t ON t.oid = x.indrelid
        JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = x.indkey[0]
        WHERE t.relname = 'user' AND t.relnamespace = to_regnamespace(current_schema())
          AND a.attname = 'bannerId' AND x.indisvalid
    ) THEN
        CREATE INDEX IF NOT EXISTS "IDX_user_bannerId" ON "user" ("bannerId") WHERE "bannerId" IS NOT NULL;
    END IF;
END
$$;
