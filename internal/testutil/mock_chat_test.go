package testutil

import "github.com/elythia-network/elythia/internal/repository"

// 集約の動機を最大化する compile-time assertion (#709)。ChatRepository に
// 新メソッドが追加されたとき、各 test ファイルを介さず mock 自身が
// コンパイルエラーに落ちて気付ける。
//
// production code ファイル (mock_chat.go) で行うと testutil → repository の
// import が production 側に乗ってしまい、repository の test 経由で import
// cycle になる。`_test.go` に置けば testutil package の test build 時のみ
// repository を含み、production graph には乗らない。
var _ repository.ChatRepository = (*MockChatRepository)(nil)
