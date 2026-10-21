package entitycompat

import (
	"testing"
)

// `admin/drive/usage` (#3053) は router が集計サービスを渡さないと動かない。
// handler は未配線を 500 で返すので**黙って 0 バイトにはならない**が、
// `internal/server` は CI のカバレッジ対象外 (CLAUDE.md Section 4) で router を
// 組み立てるテストも無いため、配線を落としても build もテストも全部緑になる。
// 気付くのは管理画面を開いた人だけになるので、#2762 (`WireMetaToggles`) と同じ
// 形で router.go の呼び出しを固定する。
//
// **キャッシュと集計そのものはここでは見ない。** TTL と singleflight は
// `internal/core/driveusage` が、SQL は `internal/repository` の実 PostgreSQL の
// テストが変異検証付きで固定している。ここが見るのは「router から正しい引数で
// 呼ばれていること」だけ。引数まで照合するので、`driveusage.NewService(..., 0, 0)`
// のように定数へ書き換えてキャッシュを無効化する形も落ちる。
//
// 集計はキャッシュの一覧に登録するので (#3738)、作る行と渡す行を分けて照合する。
func TestDriveUsageProviderIsWired(t *testing.T) {
	assertWired(t, serverPkg,
		"driveUsage := driveusage.NewService("+
			"repository.NewDriveUsageRepository(s.db), driveusage.DefaultTTL, driveusage.DefaultTopN)",
		"admin/drive/usage の集計が別の引数で作られ、キャッシュが効かなくなるか DB を読まなくなる (#3053)")
	assertWired(t, serverPkg,
		"adminHandler.SetDriveUsageProvider(driveUsage)",
		"admin/drive/usage が 500 を返し、ストレージ使用量の画面が開かなくなる (#3053)")
}

// route の登録そのものと**権限**も固定する。
//
// **`perm-check` は見てくれない。** あちらは upstream の golden に載っている
// endpoint だけを突き合わせるので、mk-go 独自の endpoint は「実装されていない」
// 扱いで黙って skip される。つまりこの endpoint の認証段は、ここでしか
// 固定されていない。middleware を落として誰でも叩ける状態にすると、リモート
// ホストの一覧と利用者ごとの使用量が無認証で読めることになる。
func TestDriveUsageRouteIsRegistered(t *testing.T) {
	assertRouteRegisteredBy(t, "routes_admin.go", "registerAdminRoutes",
		`api.POST("/admin/drive/usage", adminHandler.DriveUsage, `+
			`middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:drive"))`,
		"admin/drive/usage が 404 になる、あるいは認証段が緩む (#3053)")
}
