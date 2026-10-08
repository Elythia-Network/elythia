-- data loss: 取得済みのリモートのアカウントの作成日時が失われる。もう一度 up して
-- も、アカウント情報を取り直すまでは NULL のまま。
ALTER TABLE "user" DROP COLUMN IF EXISTS "accountCreatedAt";
