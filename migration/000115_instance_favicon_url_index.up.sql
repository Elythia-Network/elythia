-- #3383: 画像プロキシの許可確認が引く instance."faviconUrl" に index を張る。理由と失敗時の
-- 回復は 000109 を参照 (000109〜000115 で 1 組。CONCURRENTLY なので 1 file 1 文)。
CREATE INDEX CONCURRENTLY IF NOT EXISTS "IDX_instance_faviconUrl" ON "instance" ("faviconUrl") WHERE "faviconUrl" IS NOT NULL;
