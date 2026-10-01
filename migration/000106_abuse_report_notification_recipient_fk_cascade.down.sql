-- #3264 の巻き戻し。000028 と同じ SET NULL の外部キーに戻す。
--
-- **TS 版から引き継いだ DB でも本家の 3 本を落とす。** up はそれらを作らずに
-- 済ませた場合があるが、down からは区別できない。巻き戻した後は 000028 の
-- 状態 (mk-go の SET NULL) になる。TS 版へ戻すなら、本家の migration が
-- 作り直すことはないので、巻き戻さずに戻すこと。
--
-- up が空にした値 (プロフィールの無い利用者を指していた userId と、method に
-- 合わない側の userId / systemWebhookId) は戻らない。どれも通知には使われて
-- いなかった値。

ALTER TABLE "abuse_report_notification_recipient"
    DROP CONSTRAINT IF EXISTS "FK_abuse_report_notification_recipient_userId1";
ALTER TABLE "abuse_report_notification_recipient"
    DROP CONSTRAINT IF EXISTS "FK_abuse_report_notification_recipient_userId2";
ALTER TABLE "abuse_report_notification_recipient"
    DROP CONSTRAINT IF EXISTS "FK_abuse_report_notification_recipient_systemWebhookId";

ALTER TABLE "abuse_report_notification_recipient"
    ADD CONSTRAINT "abuse_report_notification_recipient_userId_fkey"
    FOREIGN KEY ("userId") REFERENCES "user"("id") ON DELETE SET NULL;
ALTER TABLE "abuse_report_notification_recipient"
    ADD CONSTRAINT "abuse_report_notification_recipient_systemWebhookId_fkey"
    FOREIGN KEY ("systemWebhookId") REFERENCES "system_webhook"("id") ON DELETE SET NULL;
