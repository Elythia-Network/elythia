-- 000136で追加した制約を先に検証し、従来のローカルアカウントを参照済みにする。
-- DDLと全行の更新・検査を別ファイルに分け、ACCESS EXCLUSIVEを長時間保持しない。

ALTER TABLE "user" VALIDATE CONSTRAINT "CHK_user_federationStatus";
ALTER TABLE "user_pending" VALIDATE CONSTRAINT "CHK_user_pending_federationStatus";

UPDATE "user"
SET "federationEnabledAt" = COALESCE("federationEnabledAt", CURRENT_TIMESTAMP),
    "firstExternalReferenceAt" = CURRENT_TIMESTAMP,
    "firstExternalReferenceReason" = 'migration'
WHERE "host" IS NULL
  AND "federationStatus" = 'on'
  AND "firstExternalReferenceAt" IS NULL;
