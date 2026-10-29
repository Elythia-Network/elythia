-- インスタンスごとのチャートのリクエスト数を、smallintからintegerへ広げる (#3873)。
-- 本家は range: 'small' で32767が上限になり、大きなサーバーとの間では1日の数が
-- 頭打ちになる。上限で止める処理 (本家2026.9.1) は残るので、0にはならないが、
-- 実際より少ない数が正しい数のように見えてしまう。
-- 型を変えると表を書き換えるが、列ごとにALTERを分けると書き換えが列の数だけ走るので、
-- 1つの表の3列は1つのALTERにまとめる。
ALTER TABLE "__chart__instance"
    ALTER COLUMN "___requests_failed" TYPE integer,
    ALTER COLUMN "___requests_succeeded" TYPE integer,
    ALTER COLUMN "___requests_received" TYPE integer;

ALTER TABLE "__chart_day__instance"
    ALTER COLUMN "___requests_failed" TYPE integer,
    ALTER COLUMN "___requests_succeeded" TYPE integer,
    ALTER COLUMN "___requests_received" TYPE integer;
