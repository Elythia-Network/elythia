package meta

import (
	"sync/atomic"

	"github.com/elythia-network/elythia/internal/model"
)

// localUserCounter reports how many local users exist. 起動時に 1 度だけ
// 配線する (`entity.SetMediaURLContext` と同じ形)。nil なら数えない。
var localUserCounter atomic.Pointer[func() (int64, error)]

// setupLatch remembers that local users were once observed.
//
// **一度 false になったら戻らない。** 利用者は増える一方なので、観測した
// 時点で確定してよい。以後 COUNT を発行しないための掛け金。
var setupLatch struct {
	seen atomic.Bool
}

// SetLocalUserCounter wires the counter used by RequireSetup.
//
// **起動時専用。** リクエストを捌いている最中に差し替えない。
func SetLocalUserCounter(fn func() (int64, error)) {
	if fn == nil {
		localUserCounter.Store(nil)
		return
	}
	localUserCounter.Store(&fn)
}

// RequireSetup reports whether the client should show the initial setup screen.
//
// **サーバー側の受け入れ条件と揃える (#3037)。** `admin/accounts/create` の
// 初回セットアップ窓は `rootUserId` が未設定 **かつ**ローカル利用者が 0 のとき
// だけ開く。フロントの判定を `rootUserId` だけにしておくと、TS から引き継いだ
// DB などで `rootUserId` が NULL・利用者ありの状態になったとき、**セットアップ
// 画面が出続けて作成ボタンが必ず `ACCESS_DENIED` を返す**。フロントの catch は
// 「権限がありません」しか出さないので、運営者には原因も対処も分からない。
//
// **upstream は `rootUserId` だけを見る。** そちらはサーバー側の窓も同じ条件
// なので整合している。mk-go は窓を狭めた側なので、表示条件も合わせる。
//
// counter 未配線なら従来どおり `rootUserId` だけで判定する (テストや、
// 数える手段が無い経路)。
func RequireSetup(m *model.Meta) bool {
	if m == nil || m.RootUserID != nil {
		return false
	}
	if setupLatch.seen.Load() {
		return false
	}
	fn := localUserCounter.Load()
	if fn == nil {
		return true
	}
	n, err := (*fn)()
	if err != nil {
		// **読めないときはセットアップを促さない。** 促して失敗させるより、
		// 出さないほうが害が小さい (本当に初回なら次のリクエストで出る)。
		return false
	}
	if n > 0 {
		setupLatch.seen.Store(true)
		return false
	}
	return true
}

// ResetSetupLatchForTest clears the latch so tests can observe both states.
func ResetSetupLatchForTest() { setupLatch.seen.Store(false) }
