-- note_quote_request の承認 URI の index (#3234 段階 4)。
--
-- 引用される作者が承認を取り消すと、承認の URI を object にした Delete が届く。
-- それを自分の引用の記録と照合するのに使う (Delete はノートの削除でも毎回届くので、
-- 全件を舐めない)。
CREATE INDEX IF NOT EXISTS "IDX_note_quote_request_approvalUri"
    ON "note_quote_request" ("approvalUri");
