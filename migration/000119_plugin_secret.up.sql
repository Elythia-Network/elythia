-- plugin_secret: サーバープラグインの秘密の値 (API キーなど、#3470)。
-- Elythia 独自テーブル。
--
-- 値は設定ファイルの pluginSecretKey で暗号化 (AES-256-GCM) した形でだけ置く。
-- 平文の列は持たない — DB のバックアップと一緒に値が漏れないようにするため。
-- 暗号文はプラグイン名と値の名前に結び付けてある (AAD) ので、行の名前を
-- 書き換えても別のプラグイン・別の名前の値としては復号できない。
--
-- プラグインの schema (plugin_<name>) ではなく public に置く。プラグインの
-- schema はプラグイン自身が自由に読み書きする場所で、そこに置くと「鍵を
-- 持たないと読めない」以外の守りが無くなる。TS へ戻すとこのテーブルは
-- 読まれない。
CREATE TABLE IF NOT EXISTS "plugin_secret" (
    "pluginName" varchar(32) NOT NULL,
    "name"       varchar(64) NOT NULL,
    -- 版 (1 バイト) + nonce (12 バイト) + 暗号文 + 認証タグ。
    "ciphertext" bytea NOT NULL,
    "updatedAt"  timestamp with time zone NOT NULL,
    PRIMARY KEY ("pluginName", "name")
);
