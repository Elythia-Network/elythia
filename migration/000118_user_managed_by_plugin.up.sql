-- #3468: プラグインが管理するアカウント (ログインできない bot) を記録する。
--
-- 値は管理しているプラグインの名前 (plugin.Definition.Name。32 文字以下)。NULL は
-- 普通のアカウント。本体はこの列が埋まっているアカウントへのログインとトークン
-- 発行を全経路で拒否し、ネイティブトークンをプロセス内の呼び出しでだけ受け付ける。
--
-- Elythia 独自の列で、TS 版は読まない (追加のみ)。プラグインを外しても値は残す
-- (アカウントを自動で消さないため。#3468 の決定)。
ALTER TABLE "user" ADD COLUMN IF NOT EXISTS "managedByPlugin" varchar(32);

-- プラグインごとの一覧 (plugin.Accounts.List) を引くための部分 index。管理する
-- アカウントは利用者全体のごく一部なので、NULL の行は載せない。
CREATE INDEX IF NOT EXISTS "IDX_user_managedByPlugin" ON "user" ("managedByPlugin") WHERE "managedByPlugin" IS NOT NULL;
