-- data loss: 連合のルール (#3090) がすべて失われる。
--
-- meta のホスト単位の設定 (blockedHosts など) は別の場所にあるので残る。ルールで
-- 拒否・書き換えていた投稿は、以後そのまま取り込まれる (既に取り込んだ投稿は
-- 書き換えた形のまま残る)。
DROP TABLE IF EXISTS "federation_rule";
