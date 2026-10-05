package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/testutil"
)

// #2708: 起動時の配線検査が見る述語。**両側を固定する。**
//
// `assert.False` だけだと述語を `return true` に弱める変異が e2e でも捕まらず、
// `assert.True` を足しても**まとめて配線すると別の field を読む typo が
// 素通りする**。1 つずつ配線して他の述語が false のままであることも見る
// (#2683 review)。
func TestHandler_HasDriveFileRepo(t *testing.T) {
	assert.False(t, (&Handler{}).HasDriveFileRepo(), "未配線なら false")

	h := &Handler{}
	h.SetDriveFileRepo(testutil.NewMockDriveFileRepository())
	assert.True(t, h.HasDriveFileRepo(), "配線したら true")

	// **別の依存だけを配線しても false のままであること。** 述語を
	// `A != nil || B != nil` のように**広げる**変異は、上の 2 つでは捕まらない
	// (未配線なら false / 配線したら true をどちらも満たしてしまう)。述語が
	// 1 つしか無いパッケージには独立性の検査が無いので、ここで代わりに置く
	// (#2709 review L-5)。
	other := &Handler{}
	other.SetUserRepo(testutil.NewMockUserRepository())
	assert.False(t, other.HasDriveFileRepo(),
		"driveFileRepo 以外を配線しても false のままであること")
}
