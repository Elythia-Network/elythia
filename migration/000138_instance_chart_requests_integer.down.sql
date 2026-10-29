-- data loss: 32767を超えたリクエスト数は、32767に切り詰められる。
-- 戻した後のコード (Range: small) も32767で止める前提なので、上限に揃えておく。
-- 切り詰めないとsmallintへの変換が範囲外で失敗し、downが通らない。
ALTER TABLE "__chart__instance"
    ALTER COLUMN "___requests_failed" TYPE smallint USING LEAST("___requests_failed", 32767),
    ALTER COLUMN "___requests_succeeded" TYPE smallint USING LEAST("___requests_succeeded", 32767),
    ALTER COLUMN "___requests_received" TYPE smallint USING LEAST("___requests_received", 32767);

ALTER TABLE "__chart_day__instance"
    ALTER COLUMN "___requests_failed" TYPE smallint USING LEAST("___requests_failed", 32767),
    ALTER COLUMN "___requests_succeeded" TYPE smallint USING LEAST("___requests_succeeded", 32767),
    ALTER COLUMN "___requests_received" TYPE smallint USING LEAST("___requests_received", 32767);
