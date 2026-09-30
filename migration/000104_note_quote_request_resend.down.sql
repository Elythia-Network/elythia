-- data loss: QuoteRequest の送り直しの予定 (#3238) が失われる (保留中の引用は送り直されなくなる)。
DROP INDEX IF EXISTS "IDX_note_quote_request_nextResendAt";
ALTER TABLE "note_quote_request" DROP COLUMN IF EXISTS "nextResendAt";
ALTER TABLE "note_quote_request" DROP COLUMN IF EXISTS "resendCount";
