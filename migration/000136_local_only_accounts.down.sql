-- この migration を戻すと、連合状態と最初の外部参照の記録が失われる。
-- 旧スキーマでは、以前 off / pending だったアカウントも連合する扱いになる。
-- local-only 化の際に削除された鍵は復元できず、既存ノートの localOnly も残るため、
-- アカウント設定とノートの状態が一致しなくなる可能性がある。

ALTER TABLE user_pending
    DROP CONSTRAINT IF EXISTS "CHK_user_pending_federationStatus",
    DROP COLUMN IF EXISTS "federationStatus";

ALTER TABLE "user"
    DROP CONSTRAINT IF EXISTS "CHK_user_federationStatus",
    DROP COLUMN IF EXISTS "firstExternalReferenceHost",
    DROP COLUMN IF EXISTS "firstExternalReferenceReason",
    DROP COLUMN IF EXISTS "firstExternalReferenceAt",
    DROP COLUMN IF EXISTS "federationEnabledAt",
    DROP COLUMN IF EXISTS "federationStatus";
