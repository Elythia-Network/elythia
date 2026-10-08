-- data loss: プラグインの秘密の値 (#3470) がすべて失われる。
--
-- 値は暗号文でしか持っていないので、戻した後に up し直しても復元できない。
-- 管理画面から入れ直すことになる。
DROP TABLE IF EXISTS "plugin_secret";
