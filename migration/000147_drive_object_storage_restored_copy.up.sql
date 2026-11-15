-- #4197: ローカルへ戻す処理 (#4183) で戻したファイルの、オブジェクトストレージに残る写しの
-- 記録。管理画面から写しを消す処理と、戻した後に利用者がファイルを消したときに、どの写しを
-- 消してよいかをここで決める。Elythia 独自の表。TS は知らない表を無視する。
--
-- key: 写しのキー (本体・サムネイル・webpublic のアクセスキー)。drive_file への FK は
-- 張らない。利用者がファイルを消しても写しはバケットに残るので、行を消すと消す手掛かりが
-- 無くなる。
-- location: 戻したときに写しがあった場所 (バケットと、prefix を含めたオブジェクトのキー)。
-- 消すときの設定で同じ場所にならなければ、別の場所を消さないよう、消さずに失敗として残す。
-- error / failedAt: 写しを消せなかった理由と日時。消せたら行を消す。
CREATE TABLE IF NOT EXISTS "drive_object_storage_restored_copy" (
    "key"        varchar(256) NOT NULL,
    "fileId"     varchar(32) NOT NULL,
    "location"   text NOT NULL,
    "restoredAt" timestamp with time zone NOT NULL,
    "error"      text,
    "failedAt"   timestamp with time zone,
    PRIMARY KEY ("key")
);
CREATE INDEX IF NOT EXISTS "IDX_drive_object_storage_restored_copy_fileId"
    ON "drive_object_storage_restored_copy" ("fileId");
