-- data loss: goneSuspended になった時刻の記録が失われる。
--
-- 停止そのもの (instance."suspensionState") と配送の止まり方は変わらない。管理画面の
-- 「消えたサーバー」で経過日数が不明になるだけで、次に同じホストが 410 を返しても
-- 既に goneSuspended なので記録し直されない (戻してから再び消えたときに記録される)。
DROP TABLE IF EXISTS "instance_gone_suspension";
