package note

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/testutil"
)

// #2683: 起動時の配線検査が見る述語。**false を返す側を必ず固定する。**
//
// 反転させる変異は e2e が捕まえるが、`return true` のように**弱める**変異は
// 捕まらない (未配線を検出できなくなるのに起動は成功するため)。#2682 の
// レビューで実際に素通りした形なので、同じ型のテストを置く。
//
// 4 つは独立に消せるので、それぞれ別に固定する。
func TestCreateService_WiringPredicates(t *testing.T) {
	empty := &CreateService{}
	assert.False(t, empty.HasBlockingRepo(), "未配線なら false")
	assert.False(t, empty.HasMetaRepo(), "未配線なら false")
	assert.False(t, empty.HasSilencingProvider(), "未配線なら false")
	assert.False(t, empty.HasDriveFileRepo(), "未配線なら false")

	// **配線したら true も固定する。** false 側だけだと、述語が別の field を
	// 読む typo (copy-paste の付け替え漏れ) が単体・e2e とも素通りする
	// (#2683 review LOW-1)。
	// **1 つずつ配線して独立性を固定する。** まとめて配線すると、述語が別の
	// field を読む typo でも全部 true になって素通りする (#2683 review LOW-1)。
	t.Run("blockingRepo だけ配線", func(t *testing.T) {
		s := &CreateService{}
		s.SetBlockingRepo(testutil.NewMockBlockingRepository())
		assert.True(t, s.HasBlockingRepo(), "配線したら true")
		assert.False(t, s.HasMetaRepo(), "他の述語は満たされないこと")
		assert.False(t, s.HasSilencingProvider(), "他の述語は満たされないこと")
		assert.False(t, s.HasDriveFileRepo(), "他の述語は満たされないこと")
	})
	t.Run("metaRepo だけ配線", func(t *testing.T) {
		s := &CreateService{}
		s.SetMetaRepo(testutil.NewMockMetaRepository())
		assert.True(t, s.HasMetaRepo(), "配線したら true")
		assert.False(t, s.HasBlockingRepo(), "他の述語は満たされないこと")
		assert.False(t, s.HasSilencingProvider(), "他の述語は満たされないこと")
		assert.False(t, s.HasDriveFileRepo(), "他の述語は満たされないこと")
	})
	t.Run("silencingProvider だけ配線", func(t *testing.T) {
		s := &CreateService{}
		s.SetSilencingProvider(stubWiringSilencing{})
		assert.True(t, s.HasSilencingProvider(), "配線したら true")
		assert.False(t, s.HasBlockingRepo(), "他の述語は満たされないこと")
		assert.False(t, s.HasMetaRepo(), "他の述語は満たされないこと")
		assert.False(t, s.HasDriveFileRepo(), "他の述語は満たされないこと")
	})
	t.Run("driveFileRepo だけ配線", func(t *testing.T) {
		s := &CreateService{}
		s.SetDriveFileRepo(testutil.NewMockDriveFileRepository())
		assert.True(t, s.HasDriveFileRepo(), "配線したら true")
		assert.False(t, s.HasBlockingRepo(), "他の述語は満たされないこと")
		assert.False(t, s.HasMetaRepo(), "他の述語は満たされないこと")
		assert.False(t, s.HasSilencingProvider(), "他の述語は満たされないこと")
	})

}

type stubWiringSilencing struct{}

func (stubWiringSilencing) IsSilenced(string) bool { return false }
