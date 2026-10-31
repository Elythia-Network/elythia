-- #3369 の巻き戻し。000009 と同じく、外部キーの無い列に戻す。
--
-- **TS 版から引き継いだ DB でも本家の制約を落とす。** up はそれを作らずに済ませた
-- 場合があるが、down からは区別できない。TS 版へ戻すなら、本家の migration が
-- 作り直すことはないので、巻き戻さずに戻すこと。
--
-- up が NULL にした id (どの drive file も指していなかった値と空文字) は戻らない。
-- どれも表示には使われていなかった値。

ALTER TABLE "page" DROP CONSTRAINT IF EXISTS "FK_a9ca79ad939bf06066b81c9d3aa";
