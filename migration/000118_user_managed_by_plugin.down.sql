-- 000118 の逆。
--
-- data loss: どのアカウントをどのプラグインが管理しているかの記録が消える。戻した
-- 後は普通のアカウントとして扱われ、ネイティブトークンを外からのリクエストでも
-- 受け付けるようになる (パスワードは持たないので、パスワードではログインできない
-- まま)。気になるなら、戻す前に該当するアカウントを凍結しておく。
DROP INDEX IF EXISTS "IDX_user_managedByPlugin";
ALTER TABLE "user" DROP COLUMN IF EXISTS "managedByPlugin";
