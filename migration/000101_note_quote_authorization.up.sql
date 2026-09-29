-- note_quote_authorization: ローカルの投稿を引用してよいと承認した記録 (#3234)。
-- mk-go 独自テーブル。
--
-- FEP-044f (Mastodon 4.5 以降が使う引用の承認)。リモートの利用者から
-- QuoteRequest を受けて承認したら、ここに 1 行残して承認の実体
-- (QuoteAuthorization、`/notes/<noteId>/quote-authorizations/<id>`) として配る。
-- 第三者は引用を表示する前にこれを取得して確かめるので、行が消えると引用は
-- 未承認に戻る。TS へ戻すとこのテーブルは読まれず、承認の実体が 404 になる
-- (相手側では、これまでの引用が未承認に戻る)。
CREATE TABLE IF NOT EXISTS "note_quote_authorization" (
    "id"         varchar(32) PRIMARY KEY,
    -- 引用される投稿 (ローカル)。消えたら承認も消える。
    "noteId"     varchar(32) NOT NULL REFERENCES "note"("id") ON DELETE CASCADE,
    -- 引用したリモートの利用者。
    "quoterId"   varchar(32) NOT NULL REFERENCES "user"("id") ON DELETE CASCADE,
    -- 引用する投稿の URI (承認の interactingObject)。
    "quotingUri" varchar(512) NOT NULL,
    -- 受け取った QuoteRequest の id。Accept の object に返す (再送されたときも同じ値)。
    "requestId"  varchar(512)
);
CREATE UNIQUE INDEX IF NOT EXISTS "IDX_note_quote_authorization_note_quoting"
    ON "note_quote_authorization" ("noteId", "quotingUri");
CREATE INDEX IF NOT EXISTS "IDX_note_quote_authorization_quoterId"
    ON "note_quote_authorization" ("quoterId");
