-- バブルゲームの 1:1 対戦の記録 (#3232)。mk-go 独自のテーブルで、upstream には無い。
--
-- 1 局 1 行。id は Redis の対局 (bubbleVersus:match:<id>) と同じ値で、時刻順に並ぶ。
-- user1 が招待した側。両者の報告は届いた時点で書き足す (時間切れは両者の報告が
-- そろうまで終局しないので、先に来た側の記録を置いておく場所が要る。Redis には
-- 記録を置かない)。endedAt / winnerId / reason は終局が確定したときに書く。
--
-- どちらかが退会したら、その対局は消す (相手の履歴からも消える。#3232 の決定)。
-- 終局から 30 日たったものは定期処理が消す。
CREATE TABLE IF NOT EXISTS "bubble_game_versus_record" (
    "id" varchar(32) PRIMARY KEY,
    "user1Id" varchar(32) NOT NULL REFERENCES "user"("id") ON DELETE CASCADE,
    "user2Id" varchar(32) NOT NULL REFERENCES "user"("id") ON DELETE CASCADE,
    "gameMode" varchar(128) NOT NULL,
    "seed" varchar(64) NOT NULL,
    "startedAt" timestamp with time zone NOT NULL,
    "endedAt" timestamp with time zone,
    "winnerId" varchar(32),
    "reason" varchar(32),
    "user1Score" integer,
    "user1Frame" integer,
    "user1Reason" varchar(32),
    "user1GameVersion" integer,
    "user1Logs" jsonb,
    "user1Public" boolean NOT NULL DEFAULT false,
    "user2Score" integer,
    "user2Frame" integer,
    "user2Reason" varchar(32),
    "user2GameVersion" integer,
    "user2Logs" jsonb,
    "user2Public" boolean NOT NULL DEFAULT false
);
-- 履歴の一覧 (利用者ごとに id の降順で引く)
CREATE INDEX IF NOT EXISTS "IDX_bubble_game_versus_record_user1Id_id" ON "bubble_game_versus_record" ("user1Id", "id");
CREATE INDEX IF NOT EXISTS "IDX_bubble_game_versus_record_user2Id_id" ON "bubble_game_versus_record" ("user2Id", "id");
-- 30 日の後始末
CREATE INDEX IF NOT EXISTS "IDX_bubble_game_versus_record_endedAt" ON "bubble_game_versus_record" ("endedAt");
