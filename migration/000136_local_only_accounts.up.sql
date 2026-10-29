ALTER TABLE "user"
    ADD COLUMN IF NOT EXISTS "federationStatus" varchar(16) NOT NULL DEFAULT 'on',
    ADD COLUMN IF NOT EXISTS "federationEnabledAt" timestamp with time zone,
    ADD COLUMN IF NOT EXISTS "firstExternalReferenceAt" timestamp with time zone,
    ADD COLUMN IF NOT EXISTS "firstExternalReferenceReason" varchar(32),
    ADD COLUMN IF NOT EXISTS "firstExternalReferenceHost" varchar(255);

-- 既存行の検査と初回参照記録は137で行い、列追加を行うDDL中に全行を走査・更新しない。
ALTER TABLE "user" DROP CONSTRAINT IF EXISTS "CHK_user_federationStatus";
ALTER TABLE "user" ADD CONSTRAINT "CHK_user_federationStatus" CHECK ("federationStatus" IN ('pending', 'off', 'on')) NOT VALID;

-- メール確認待ちの状態列も既定値onで追加し、既存行の検査は137へ分ける。
ALTER TABLE "user_pending" ADD COLUMN IF NOT EXISTS "federationStatus" varchar(16) NOT NULL DEFAULT 'on';
ALTER TABLE "user_pending" DROP CONSTRAINT IF EXISTS "CHK_user_pending_federationStatus";
ALTER TABLE "user_pending" ADD CONSTRAINT "CHK_user_pending_federationStatus" CHECK ("federationStatus" IN ('pending', 'off', 'on')) NOT VALID;
