-- note_quote_request: ローカルの利用者がリモートの投稿を引用したときに送った
-- QuoteRequest と、その答えの記録 (#3234)。mk-go 独自テーブル。
--
-- FEP-044f (Mastodon 4.5 以降が使う引用の承認)。引用される側から Accept が
-- 返ったら、その承認 URI を引用する投稿の `quoteAuthorization` として配る。
-- 第三者はそれを取得して確かめてから引用を表示する。TS へ戻すとこのテーブルは
-- 読まれず、引用する投稿から `quoteAuthorization` が消える (相手側では、これまでの
-- 引用が再検証のときに未承認に戻る)。
CREATE TABLE IF NOT EXISTS "note_quote_request" (
    -- 引用する投稿 (ローカル)。消えたら記録も消える。
    "noteId"      varchar(32) PRIMARY KEY REFERENCES "note"("id") ON DELETE CASCADE,
    -- 送った QuoteRequest の id。Accept / Reject の object と突き合わせる。
    "requestUri"  varchar(512) NOT NULL,
    -- pending / accepted / rejected
    "state"       varchar(16) NOT NULL DEFAULT 'pending',
    -- 受け取った承認 (QuoteAuthorization) の URI。accepted のときだけ入る。
    "approvalUri" varchar(512),
    -- その承認を付けた Update を配り終えたか。承認が変わると false に戻る。
    -- 配り直しに失敗しても承認の記録は消さず、これで「まだ配っていない」を表す。
    "updateSent"  boolean NOT NULL DEFAULT false
);
CREATE UNIQUE INDEX IF NOT EXISTS "IDX_note_quote_request_requestUri"
    ON "note_quote_request" ("requestUri");
