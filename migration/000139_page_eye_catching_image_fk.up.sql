-- page.eyeCatchingImageId に、本家と同じ drive_file への外部キーを張る (#3369)。
--
-- 本家 (1756062689648-NonCascadingPageEyeCatching) は
--   FK_a9ca79ad939bf06066b81c9d3aa  eyeCatchingImageId -> drive_file(id)  ON DELETE SET NULL
-- を持つ。000009 は列だけを作っていたので、drive file を消すと消えた id が残って
-- いた (pack と SSR は見つからなければ null を返すので、表示の害は無かった)。
--
-- **TS 版から引き継いだ DB には同じ名前の制約が既にある** (000009 は
-- CREATE TABLE IF NOT EXISTS なので、列だけの定義は当たらない)。ただし本家の
-- NonCascadingPageEyeCatching より前の TS 版の DB では、同じ外部キーが ON DELETE
-- CASCADE のまま残っている (000029 は本家の migration を適用済みと記録するだけで、
-- 中身は流さない)。さらに古い DB (本家 1651224615271-foreign-key より前) では、
-- 名前が FK_3126dd7c502c9e4d7597ef7ef10 のまま。どちらも、残すと drive file を
-- 消したときに Page ごと消えるので、本家と同じく落としてから SET NULL で張り直す。
-- 既に SET NULL の制約があるときは何もしない。
--
-- **張る前に、どの drive file も指していない id と空文字を NULL にする。** 残すと
-- ADD CONSTRAINT の検証で失敗し、migration が dirty のまま止まって起動できなくなる。
-- 空文字は、検証を飛ばす大きな body の pages/update が保存しえた値。どちらも表示には
-- 使われていない (見つからない画像は null として返していた)。

UPDATE "page" SET "eyeCatchingImageId" = NULL
WHERE "eyeCatchingImageId" IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM "drive_file" f WHERE f."id" = "page"."eyeCatchingImageId");

ALTER TABLE "page" DROP CONSTRAINT IF EXISTS "FK_3126dd7c502c9e4d7597ef7ef10";

DO $$
BEGIN
    -- conrelid で引くので、名前の検索は今の schema の表に限られる
    -- (pg_constraint は全 schema の行を返す。#2777)。
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = '"page"'::regclass
          AND conname = 'FK_a9ca79ad939bf06066b81c9d3aa'
          AND confdeltype <> 'n'
    ) THEN
        ALTER TABLE "page" DROP CONSTRAINT "FK_a9ca79ad939bf06066b81c9d3aa";
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = '"page"'::regclass
          AND conname = 'FK_a9ca79ad939bf06066b81c9d3aa'
    ) THEN
        ALTER TABLE "page"
            ADD CONSTRAINT "FK_a9ca79ad939bf06066b81c9d3aa"
            FOREIGN KEY ("eyeCatchingImageId") REFERENCES "drive_file"("id") ON DELETE SET NULL ON UPDATE NO ACTION;
    END IF;
END
$$;
