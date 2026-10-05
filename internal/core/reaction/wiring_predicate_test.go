package reaction

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/model"
)

// #2683: 起動時の配線検査が見る述語。**false を返す側を必ず固定する。**
//
// 反転させる変異は e2e が捕まえるが、`return true` のように**弱める**変異は
// 捕まらない (未配線を検出できなくなるのに起動は成功するため)。#2682 の
// レビューで実際に素通りした形なので、同じ型のテストを置く。
func TestService_HasUserRolesProvider(t *testing.T) {
	assert.False(t, (&Service{}).HasUserRolesProvider(),
		"未配線なら false を返すこと (常に true だと検査が無意味になる)")

	// **配線したら true も固定する。** false 側だけだと、述語が別の field を
	// 読む typo (copy-paste の付け替え漏れ) が単体・e2e とも素通りする
	// (#2683 review LOW-1)。
	s := &Service{}
	s.SetUserRolesProvider(stubWiringUserRoles{})
	assert.True(t, s.HasUserRolesProvider(), "配線したら true")

}

type stubWiringUserRoles struct{}

func (stubWiringUserRoles) GetUserRoles(string) ([]*model.Role, error) { return nil, nil }

// #2708: reaction の gate は 3 つ (userRoles / mediaSilence / blockingChecker)。
// **1 つずつ配線して独立性を固定する。**
func TestService_WiringPredicates2708(t *testing.T) {
	empty := &Service{}
	assert.False(t, empty.HasMediaSilenceChecker(), "未配線なら false")
	assert.False(t, empty.HasBlockingChecker(), "未配線なら false")

	t.Run("mediaSilence だけ配線", func(t *testing.T) {
		s := &Service{}
		s.SetMediaSilenceChecker(stubWiringMediaSilence{})
		assert.True(t, s.HasMediaSilenceChecker(), "配線したら true")
		assert.False(t, s.HasBlockingChecker(), "他の述語は満たされないこと")
		assert.False(t, s.HasUserRolesProvider(), "他の述語は満たされないこと")
	})
	t.Run("blockingChecker だけ配線", func(t *testing.T) {
		s := &Service{}
		s.SetBlockingChecker(stubWiringReactionBlocking{})
		assert.True(t, s.HasBlockingChecker(), "配線したら true")
		assert.False(t, s.HasMediaSilenceChecker(), "他の述語は満たされないこと")
		assert.False(t, s.HasUserRolesProvider(), "他の述語は満たされないこと")
	})
}

type stubWiringMediaSilence struct{}

func (stubWiringMediaSilence) IsMediaSilenced(string) bool { return false }

type stubWiringReactionBlocking struct{}

func (stubWiringReactionBlocking) IsBlocked(string, string) (bool, error) { return false, nil }
