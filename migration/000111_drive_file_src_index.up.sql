-- #3383: 画像プロキシの許可確認が引く drive_file."src" に index を張る。hash にする理由と
-- 失敗時の回復は 000109 を参照 (000109〜000115 で 1 組。CONCURRENTLY なので 1 file 1 文)。
CREATE INDEX CONCURRENTLY IF NOT EXISTS "IDX_drive_file_src" ON "drive_file" USING hash ("src") WHERE "src" IS NOT NULL;
