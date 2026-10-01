-- #3264: 通報の通知先 (abuse_report_notification_recipient) の外部キーを本家に揃える。
--
-- 000028 は userId -> user と systemWebhookId -> system_webhook を
-- `ON DELETE SET NULL` で張っていた。本家 (1713656541000-abuse-report-notification)
-- は次の 3 本で、どれも `ON DELETE CASCADE`。
--   FK_abuse_report_notification_recipient_userId1         userId -> user(id)
--   FK_abuse_report_notification_recipient_userId2         userId -> user_profile(userId)
--   FK_abuse_report_notification_recipient_systemWebhookId systemWebhookId -> system_webhook(id)
-- SET NULL だと、利用者や System Webhook を消したときに宛先の無い通知先が残る
-- (管理画面には宛先が空の行として出続ける)。本家は通知先ごと消える。
--
-- **TS 版から引き継いだ DB には本家の 3 本が既にある** (000028 は
-- CREATE TABLE IF NOT EXISTS なので、mk-go の外部キーは作られない)。名前で
-- 有無を見て、無いものだけ足す。mk-go の名前 (PostgreSQL の既定の
-- `<table>_<column>_fkey`) のものは、どちらの DB でも落とす。
--
-- 既に SET NULL で宛先が NULL になった行は消さない。直す前に作られた行で、
-- 宛先が無いことは管理画面から見えるので、消すかは運営者に任せる。
--
-- **外部キーを張る前に、本家では作れない形の値を空にする。**
--   - user_profile の無い利用者を指す userId。userId2 を満たさないので、残すと
--     ADD CONSTRAINT の検証で失敗し、migration が dirty のまま止まって起動
--     できなくなる。本家では userId2 があるのでこの行は存在できず、通知先としても
--     使われない (fetchEMailRecipients は user_profile と inner join する)
--   - method に合わない側の参照 (webhook 方式の行の userId、email 方式の行の
--     systemWebhookId)。検証は通るが、CASCADE にすると無関係な利用者や System
--     Webhook を消しただけで通知先ごと消える。本家の create / update は method に
--     合わない側を必ず NULL にするのでこの形にならないが、mk-go の部分更新は
--     書けた
-- どれも TS 版が書く値には当たらない。

UPDATE "abuse_report_notification_recipient" SET "userId" = NULL
WHERE "userId" IS NOT NULL
  AND ("method" = 'webhook'
       OR NOT EXISTS (SELECT 1 FROM "user_profile" p WHERE p."userId" = "abuse_report_notification_recipient"."userId"));
UPDATE "abuse_report_notification_recipient" SET "systemWebhookId" = NULL
WHERE "systemWebhookId" IS NOT NULL AND "method" = 'email';

ALTER TABLE "abuse_report_notification_recipient"
    DROP CONSTRAINT IF EXISTS "abuse_report_notification_recipient_userId_fkey";
ALTER TABLE "abuse_report_notification_recipient"
    DROP CONSTRAINT IF EXISTS "abuse_report_notification_recipient_systemWebhookId_fkey";

DO $$
BEGIN
    -- conrelid で引くので、名前の検索は今の schema の表に限られる
    -- (pg_constraint は全 schema の行を返す。#2777)。
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = '"abuse_report_notification_recipient"'::regclass
          AND conname = 'FK_abuse_report_notification_recipient_userId1'
    ) THEN
        ALTER TABLE "abuse_report_notification_recipient"
            ADD CONSTRAINT "FK_abuse_report_notification_recipient_userId1"
            FOREIGN KEY ("userId") REFERENCES "user"("id") ON DELETE CASCADE ON UPDATE NO ACTION;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = '"abuse_report_notification_recipient"'::regclass
          AND conname = 'FK_abuse_report_notification_recipient_userId2'
    ) THEN
        ALTER TABLE "abuse_report_notification_recipient"
            ADD CONSTRAINT "FK_abuse_report_notification_recipient_userId2"
            FOREIGN KEY ("userId") REFERENCES "user_profile"("userId") ON DELETE CASCADE ON UPDATE NO ACTION;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = '"abuse_report_notification_recipient"'::regclass
          AND conname = 'FK_abuse_report_notification_recipient_systemWebhookId'
    ) THEN
        ALTER TABLE "abuse_report_notification_recipient"
            ADD CONSTRAINT "FK_abuse_report_notification_recipient_systemWebhookId"
            FOREIGN KEY ("systemWebhookId") REFERENCES "system_webhook"("id") ON DELETE CASCADE ON UPDATE NO ACTION;
    END IF;
END
$$;
