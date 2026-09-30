-- note_quote_request に QuoteRequest の送り直しの予定を足す (#3238)。
--
-- Mastodon は、引用する投稿を別の経路で取り込んでいる最中に QuoteRequest が届くと
-- 黙って捨て、再試行もしない。保留のまま残ったものを、毎分の定期処理が確かめてから
-- 送り直す。既存の行は nextResendAt が NULL (= 送り直さない)。
ALTER TABLE "note_quote_request" ADD COLUMN IF NOT EXISTS "resendCount" smallint NOT NULL DEFAULT 0;
ALTER TABLE "note_quote_request" ADD COLUMN IF NOT EXISTS "nextResendAt" timestamptz;
CREATE INDEX IF NOT EXISTS "IDX_note_quote_request_nextResendAt"
    ON "note_quote_request" ("nextResendAt") WHERE "state" = 'pending' AND "nextResendAt" IS NOT NULL;
