-- data loss: フォローの関係ごとの Follow アクティビティの id が失われる。以後に
-- リモートのフォロワーを外したときの Reject は id の無い Follow を包み、TS から
-- 引き継いだ関係の Undo は Elythia の FollowURI を使う (列を足す前と同じ)。
ALTER TABLE "following" DROP COLUMN IF EXISTS "requestId";
