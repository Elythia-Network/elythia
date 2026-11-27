package entitycompat

import "testing"

// 管理者がローカルの人のアイコン・バナーを外したときに伝える先 (本家 2026.11.1、#18175)。
//
// どちらも nil なら「伝えない」なので、この行を落としてもビルドもテストも通る。
// 症状は、本人の画面に外したはずの画像が残ることと、リモートのフォロワーの側で
// 画像が変わらないことだけで、エラーもログも出ない。
func TestAdminUnsetUserImageIsWired(t *testing.T) {
	assertWired(t, serverPkg, "adminHandler.SetMeUpdatedPublisher(iHandler)",
		"管理者がアイコン・バナーを外しても、本人の画面へ meUpdated が流れず、\n"+
			"開いている画面に外したはずの画像が残る。")
	assertWired(t, serverPkg, "adminHandler.SetProfileUpdateHook(profileUpdateHook)",
		"管理者がアイコン・バナーを外しても、リモートのフォロワーへ Update(Person) が\n"+
			"配られず、相手のサーバーでは画像が残る。")
}
