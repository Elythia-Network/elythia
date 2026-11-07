package entitycompat

import "testing"

// ドライブのアップロードがプロフィールの alwaysMarkNsfw / autoSensitive を読むには、
// 組み立てでプロフィールの参照を渡す必要がある (#4057)。渡し忘れても build も
// `internal/core/drive` のテストも緑のまま (テストは自分で渡す) で、設定が
// 「保存はされるが効かない」状態に戻る。#4057 の不具合そのものの形なので、
// 組み立ての呼び出しを固定する。
func TestDriveUploaderProfilesAreWired(t *testing.T) {
	assertWired(t, serverPkg,
		"driveService.SetUploaderProfiles(userRepo)",
		"設定の「常にセンシティブにする」と「自動でセンシティブにする」が、アップロードに効かなくなる (#4057)")
}
