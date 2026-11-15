package entitycompat

import (
	"testing"
)

// ドライブのファイルをオブジェクトストレージへ写す処理 (#4180) は、router が
// 作って管理 API の handler と削除の経路に渡さないと動かない。handler は未配線を
// 500 で返すが、`internal/server` の組み立てを通すテストは無いので、配線を落としても
// 全部緑になる。TestDriveUsageProviderIsWired と同じ形で呼び出しを固定する。
//
// lock の key prefix も固定する。prefix を落とすと、同じ Redis を使う別の
// インスタンスと lock を取り合う。
func TestDriveStorageMigratorIsWired(t *testing.T) {
	assertWired(t, serverPkg,
		"driveStorageMigrator := coredrive.NewStorageMigrator(repository.NewDriveStorageMigrationRepository(s.db), "+
			"localDriveStorage, driveStorage, driveStorageMigrationEnq, "+
			"coredrive.NewRedisStorageMigrationLock(s.redis.Default, s.config.Redis.KeyPrefix()))",
		"移す処理がローカルとオブジェクトストレージを取り違えるか、lock が他のインスタンスと衝突する (#4180)")
	assertWired(t, serverPkg,
		"adminHandler.SetDriveStorageMigrator(driveStorageMigrator)",
		"admin/drive/object-storage-migration/* が 500 を返す (#4180)")
	assertWired(t, serverPkg,
		"driveService.SetStorageMigrationRunning(func() bool { "+
			"running, err := driveStorageMigrator.Running(context.Background()) "+
			"return err != nil || running })",
		"移す処理の実行中にローカルの行を消すと、並行して写した分がオブジェクトストレージに孤児として残る (#4180)")
	// 配線し忘れると、ローカルへ戻したファイルを消しても、バケットの写しが残る (#4197)。
	assertWired(t, serverPkg,
		"driveService.SetRestoredCopyReleaser(driveStorageMigrator)",
		"ローカルへ戻したファイルを消しても、バケットの写しが残り、public-read なら取れ続ける (#4197)")
	assertWired(t, serverPkg,
		"objectStorageProcessor.SetRestoredCopyReleaser(driveStorageMigrator)",
		"リモートのファイルの掃除で消したファイルの、バケットの写しが残る (#4197)")
	assertWired(t, serverPkg,
		"adminHandler.SetRestoredCopyReleaser(driveStorageMigrator)",
		"管理画面の掃除で消したファイルの、バケットの写しが残る (#4197)")
	// 再認証を配線し忘れると、purge-local は 500 で閉じる (#4184)。
	assertWired(t, serverPkg,
		"adminHandler.SetDriveStorageMigrationVerifier(backupVerifier)",
		"admin/drive/object-storage-migration/purge-local が 500 を返す (#4184)")
}

// route の登録と権限も固定する。perm-check は本家の golden にある endpoint しか
// 見ないので、Elythia 独自の endpoint の認証段はここでしか固定されていない。
func TestDriveStorageMigrationRoutesAreRegistered(t *testing.T) {
	assertRouteRegisteredBy(t, "routes_admin.go", "registerAdminRoutes",
		`api.POST("/admin/drive/object-storage-migration/start", adminHandler.DriveStorageMigrationStart, `+
			`middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:drive"))`,
		"移す処理を管理者以外が始められる、あるいは 404 になる (#4180)")
	assertRouteRegisteredBy(t, "routes_admin.go", "registerAdminRoutes",
		`api.POST("/admin/drive/object-storage-migration/status", adminHandler.DriveStorageMigrationStatus, `+
			`middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:drive"))`,
		"移す処理の状態を管理者以外が読める、あるいは 404 になる (#4180)")
	assertRouteRegisteredBy(t, "routes_admin.go", "registerAdminRoutes",
		`api.POST("/admin/drive/object-storage-migration/verify", adminHandler.DriveStorageMigrationVerify, `+
			`middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:drive"))`,
		"確かめる処理を管理者以外が始められる、あるいは 404 になる (#4181)")
	assertRouteRegisteredBy(t, "routes_admin.go", "registerAdminRoutes",
		`api.POST("/admin/drive/object-storage-migration/purge-local", adminHandler.DriveStorageMigrationPurgeLocal, `+
			`middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:drive"), middleware.RequireSecure())`,
		"ローカルの実体を消す処理を管理者以外が始められる、あるいは 404 になる (#4182)")
	assertRouteRegisteredBy(t, "routes_admin.go", "registerAdminRoutes",
		`api.POST("/admin/drive/object-storage-migration/restore", adminHandler.DriveStorageMigrationRestore, `+
			`middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:drive"))`,
		"戻す処理を管理者以外が始められる、あるいは 404 になる (#4183)")
	assertRouteRegisteredBy(t, "routes_admin.go", "registerAdminRoutes",
		`api.POST("/admin/drive/object-storage-migration/cleanup-copies", adminHandler.DriveStorageMigrationCleanupCopies, `+
			`middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:drive"))`,
		"写しを消す処理を管理者以外が始められる、あるいは 404 になる (#4197)")
}
