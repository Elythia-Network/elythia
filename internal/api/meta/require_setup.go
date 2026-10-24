package meta

import (
	"sync/atomic"

	"github.com/elythia-network/elythia/internal/model"
)

// SetupDetector decides whether the client should show the initial setup
// screen. It carries the local-user counter that the server assembly wires in.
//
// 数え方はパッケージのグローバルではなく、組み立てで作った値として `/api/meta`
// と frontend の shell の両方へ渡す (#3782)。グローバルのときは 1 回の配線で
// 両方に効いたが、今は片方だけ渡し忘れることが起こり得るので、配線のテスト
// (`internal/server/setup_detector_wiring_test.go`) が両方を見る。
//
// nil の `*SetupDetector` と、数え方を持たない値は「数えない」として扱う。
//
// 利用者を一度でも観測した印 (seen) も、この値が持つ (#3803)。以前はパッケージの
// 変数だったので、DB を戻してメンテナンスから抜け、組み立て直した後の世代にも
// 前の DB で観測した印が残った。組み立てるたびに NewSetupDetector で作り直すので、
// 新しい世代は最初のリクエストで 1 回だけ数え直す。
type SetupDetector struct {
	count func() (int64, error)
	// seen remembers that local users were once observed.
	//
	// **一度 true になったら戻らない。** 利用者は増える一方なので、観測した
	// 時点で確定してよい。以後 COUNT を発行しないための掛け金。
	seen atomic.Bool
}

// NewSetupDetector returns a detector that counts local users with fn.
// A nil fn yields a detector that judges by rootUserId alone.
func NewSetupDetector(fn func() (int64, error)) *SetupDetector {
	return &SetupDetector{count: fn}
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
// 数え方が無ければ従来どおり `rootUserId` だけで判定する (テストや、
// 数える手段が無い経路)。
func (d *SetupDetector) RequireSetup(m *model.Meta) bool {
	if m == nil || m.RootUserID != nil {
		return false
	}
	// nil の値には印を置く場所が無いが、数えないので印も要らない。
	if d == nil || d.count == nil {
		return true
	}
	if d.seen.Load() {
		return false
	}
	n, err := d.count()
	if err != nil {
		// **読めないときはセットアップを促さない。** 促して失敗させるより、
		// 出さないほうが害が小さい (本当に初回なら次のリクエストで出る)。
		return false
	}
	if n > 0 {
		d.seen.Store(true)
		return false
	}
	return true
}
