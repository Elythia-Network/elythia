-- instance_gone_suspension: shared inbox が 410 を返して goneSuspended になった
-- 時刻を host 単位で記録する mk-go 独自テーブル (#3067)。
--
-- 消えたインスタンスとのフォロー関係は自動では消えない。管理画面に候補として
-- 並べ、「いつから消えているか」を見て管理者が片付けるための起点になる。
-- instance には停止した時刻の列が無く、共有テーブルに列を足すと TS へ戻したときに
-- 形が変わるので、別テーブルにする (instance_signature_capability と同じ判断)。
--
-- **行があることは「今も消えている」ことを意味しない。** 管理者が戻した後も行は
-- 残る。一覧は instance."suspensionState" で絞り、再び消えたときは時刻を更新する。
-- TS が立てた goneSuspended には行が無い (時刻は不明として扱う)。
--
-- instance への FK は張らない (instance_signature_capability と同じ理由)。
CREATE TABLE IF NOT EXISTS "instance_gone_suspension" (
    "host"        varchar(128) PRIMARY KEY,
    "suspendedAt" timestamp with time zone NOT NULL
);
