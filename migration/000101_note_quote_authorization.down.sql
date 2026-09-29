-- data loss: 引用の承認の記録 (#3234) がすべて失われる。
--
-- 承認の実体 (QuoteAuthorization) が 404 になるので、相手のサーバーはこれまでに
-- 承認した引用を未承認として扱うようになる (再検証したときに)。
DROP TABLE IF EXISTS "note_quote_authorization";
