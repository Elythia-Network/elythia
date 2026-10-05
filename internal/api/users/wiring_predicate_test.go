package users

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
func TestHandler_HasRolePolicyProvider(t *testing.T) {
	assert.False(t, (&Handler{}).HasRolePolicyProvider(),
		"未配線なら false を返すこと (常に true だと検査が無意味になる)")

	// **配線したら true も固定する。** false 側だけだと、述語が別の field を
	// 読む typo (copy-paste の付け替え漏れ) が単体・e2e とも素通りする
	// (#2683 review LOW-1)。
	x := &Handler{}
	x.SetRolePolicyProvider(stubWiringPolicyProvider{})
	assert.True(t, x.HasRolePolicyProvider(), "配線したら true")
}

type stubWiringPolicyProvider struct{}

func (stubWiringPolicyProvider) GetUserPolicies(string) map[string]any { return nil }

// #2708: 匿名 visitor への remote profile 露出 gate。**空文字は gate 無効**と
// 同義なので、未設定を false として扱う。
func TestHandler_HasUGCVisibility(t *testing.T) {
	assert.False(t, (&Handler{}).HasUGCVisibility(), "未設定なら false")

	h := &Handler{}
	h.SetUGCVisibility("local")
	assert.True(t, h.HasUGCVisibility(), "設定したら true")

	h2 := &Handler{}
	h2.SetUGCVisibility("")
	assert.False(t, h2.HasUGCVisibility(), "空文字は gate 無効なので false")
}

// #2709 review: users handler の可視性 gate は 5 つ。**1 つずつ配線して
// 独立性を固定する** (まとめて配線すると別の field を読む typo が素通りし、
// 述語を `A != nil || B != nil` に広げる変異も捕まらない)。
func TestHandler_WiringPredicates2709(t *testing.T) {
	wire := map[string]func(*Handler){
		"rolePolicyProvider": func(h *Handler) { h.SetRolePolicyProvider(stubWiringPolicyProvider{}) },
		"ugcVisibility":      func(h *Handler) { h.SetUGCVisibility("local") },
		"channelMutingRepo":  func(h *Handler) { h.SetChannelMutingRepo(testutil.NewMockChannelMutingRepository()) },
		"mutingRepo":         func(h *Handler) { h.SetMutingRepo(testutil.NewMockMutingRepository()) },
		"blockingRepo":       func(h *Handler) { h.SetBlockingRepo(testutil.NewMockBlockingRepository()) },
	}
	check := map[string]func(*Handler) bool{
		"rolePolicyProvider": (*Handler).HasRolePolicyProvider,
		"ugcVisibility":      (*Handler).HasUGCVisibility,
		"channelMutingRepo":  (*Handler).HasChannelMutingRepo,
		"mutingRepo":         (*Handler).HasMutingRepo,
		"blockingRepo":       (*Handler).HasBlockingRepo,
	}
	for name, has := range check {
		assert.False(t, has(&Handler{}), "未配線なら false: "+name)
	}
	for name, set := range wire {
		t.Run(name+" だけ配線", func(t *testing.T) {
			h := &Handler{}
			set(h)
			for other, has := range check {
				if other == name {
					assert.True(t, has(h), "配線したら true")
				} else {
					assert.False(t, has(h), "他の述語は満たされないこと: "+other)
				}
			}
		})
	}
}
