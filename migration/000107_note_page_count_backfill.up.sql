-- #3293: mk-go はページの作成・更新・削除で note.pageCount を増減していなかった。
-- リモートノートの掃除は `pageCount = 0` を「消してよい」条件の 1 つにしているので、
-- ページに埋め込んだリモートのノートが期限を過ぎると消えていた。増減は #3293 で
-- 入れたが、それより前に作ったページが参照するノートは 0 のまま残るので、
-- ページの content から数え直して埋める。
--
-- 参照の数え方は本家 PageService.collectReferencedNotes と同じ (`type: note` の
-- 文字列の `note` を、`type: section` の `children` を再帰的にたどって集め、
-- ページごとに重複を除く)。fsck の pageCount の検査と同じ式。
--
-- **増やす向きにしか直さない。** 参照されているノートだけを触るので、note 全体を
-- 走査しない。参照が無いのに値が残っている行 (TS 由来で古いもの) は、掃除の保護を
-- 外す向きなので fsck の `-fix` に任せる。値は smallint の範囲で頭打ちにする。
-- TS が維持していれば同じ値になっているので、TS 製 DB では何も変わらない。
WITH RECURSIVE blocks AS (
  SELECT p.id AS pid, b.value AS blk
  FROM "page" p,
       jsonb_array_elements(CASE WHEN jsonb_typeof(p.content) = 'array' THEN p.content ELSE '[]'::jsonb END) b
  UNION ALL
  SELECT blocks.pid, c.value
  FROM blocks,
       jsonb_array_elements(CASE WHEN blocks.blk->>'type' = 'section' AND jsonb_typeof(blocks.blk->'children') = 'array'
                                 THEN blocks.blk->'children' ELSE '[]'::jsonb END) c
), refs AS (
  SELECT DISTINCT pid, blk->>'note' AS nid FROM blocks
  WHERE blk->>'type' = 'note' AND jsonb_typeof(blk->'note') = 'string'
), counts AS (
  SELECT nid, LEAST(COUNT(*), 32767) AS n FROM refs GROUP BY nid
)
UPDATE "note" t
   SET "pageCount" = counts.n
  FROM counts
 WHERE t.id = counts.nid
   AND t."pageCount" < counts.n;
