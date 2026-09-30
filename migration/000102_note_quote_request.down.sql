-- data loss: 送った QuoteRequest と受け取った承認の記録 (#3234) がすべて失われる。
--
-- 引用する投稿から `quoteAuthorization` が消えるので、相手のサーバーはこれまでに
-- 承認された引用を未承認として扱うようになる (再検証したときに)。
DROP TABLE IF EXISTS "note_quote_request";
