-- #4180: ローカルに置いたドライブのファイルを、管理画面からオブジェクトストレージへ
-- 写す (#4179)。Elythia 独自の列と表。TS は知らない列と表を無視する。
--
-- objectStorageMigratedAt: 移す処理で写した日時。初めからオブジェクトストレージへ
-- アップロードしたファイルは NULL のまま。戻す処理 (#4183 で足す予定) は、この列の
-- ある行だけを戻す。値の無い列の追加なので、表は書き直さない。
ALTER TABLE "drive_file" ADD COLUMN IF NOT EXISTS "objectStorageMigratedAt" TIMESTAMP WITH TIME ZONE;

-- drive_object_storage_migration_failure: 写す処理などで失敗したファイルと理由。
-- 1 件の失敗で全体を止めないので、ここに残して管理画面に出し、次に流したときに
-- 取り直す。成功したら行を消す。ファイルを消せば行も消える。
CREATE TABLE IF NOT EXISTS "drive_object_storage_migration_failure" (
    "fileId"    varchar(32) NOT NULL,
    "operation" varchar(16) NOT NULL,
    "error"     text NOT NULL,
    "failedAt"  timestamp with time zone NOT NULL,
    PRIMARY KEY ("fileId", "operation"),
    CONSTRAINT "FK_drive_object_storage_migration_failure_file" FOREIGN KEY ("fileId")
        REFERENCES "drive_file" ("id") ON DELETE CASCADE
);
