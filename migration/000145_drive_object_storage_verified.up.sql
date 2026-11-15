-- #4181: オブジェクトストレージへ写したドライブのファイルを確かめる (#4179 の (3))。
-- Elythia 独自の列。TS は知らない列を無視する。
--
-- objectStorageVerifiedAt: 確かめる処理で、オブジェクトストレージの実体が読め、
-- 大きさと md5 が記録と一致した日時。一致しなかったときと、もう一度写したときは
-- NULL に戻す。ローカルを消す処理 (#4182 で足す予定) は、この列のある行だけの
-- ローカルの実体を消す。値の無い列の追加なので、表は書き直さない。
ALTER TABLE "drive_file" ADD COLUMN IF NOT EXISTS "objectStorageVerifiedAt" TIMESTAMP WITH TIME ZONE;
