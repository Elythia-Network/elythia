-- data loss: 期間と止め方の設定と、通知を出さずに成立させたフォローの記録
-- (silent_follow) が失われる。成立したフォローそのものは following に残る。
DROP TABLE IF EXISTS "silent_follow";
ALTER TABLE "user_profile" DROP COLUMN IF EXISTS "followApprovalAction";
ALTER TABLE "user_profile" DROP COLUMN IF EXISTS "followApprovalRemoteSeconds";
ALTER TABLE "user_profile" DROP COLUMN IF EXISTS "followApprovalLocalSeconds";
