package federation

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// #2683: 起動時の配線検査が見る述語。**false を返す側を必ず固定する。**
//
// 反転させる変異は e2e が捕まえるが、`return true` のように**弱める**変異は
// 捕まらない (未配線を検出できなくなるのに起動は成功するため)。#2682 の
// レビューで実際に素通りした形なので、同じ型のテストを置く。
//
// Resolver と DeliverService は別オブジェクトの別 field なので、片方の述語で
// もう片方を代替できない。
func TestFederation_WiringPredicates(t *testing.T) {
	assert.False(t, (&Resolver{}).HasHostBlockChecker(), "resolver: 未配線なら false")
	assert.False(t, (&DeliverService{}).HasHostBlockChecker(), "deliver: 未配線なら false")

	// **配線したら true も固定する。** false 側だけだと、述語が別の field を
	// 読む typo (copy-paste の付け替え漏れ) が単体・e2e とも素通りする
	// (#2683 review LOW-1)。
	r := &Resolver{}
	r.SetHostBlockChecker(stubWiringHostBlock{})
	assert.True(t, r.HasHostBlockChecker(), "resolver: 配線したら true")

	d := &DeliverService{}
	d.SetHostBlockChecker(stubWiringHostBlock{})
	assert.True(t, d.HasHostBlockChecker(), "deliver: 配線したら true")

}

type stubWiringHostBlock struct{}

func (stubWiringHostBlock) IsBlocked(string) bool    { return false }
func (stubWiringHostBlock) IsAllowed(string) bool    { return true }
func (stubWiringHostBlock) FederationDisabled() bool { return false }

// #2708: silenced instance の remote public note を home へ降格する gate。
// hostBlockChecker とは独立に消せるので別に固定する。
func TestResolver_HasSilencedHostChecker(t *testing.T) {
	assert.False(t, (&Resolver{}).HasSilencedHostChecker(), "未配線なら false")

	r := &Resolver{}
	r.SetSilencedHostChecker(stubWiringSilencedHost{})
	assert.True(t, r.HasSilencedHostChecker(), "配線したら true")
	assert.False(t, r.HasHostBlockChecker(), "他の述語は満たされないこと")
}

type stubWiringSilencedHost struct{}

func (stubWiringSilencedHost) IsSilenced(string) bool { return false }

type stubWiringRolePolicies struct{}

func (stubWiringRolePolicies) GetUserPolicies(string) map[string]any { return nil }

// #3330: 受信した note の mentionLimit を投稿者の role policy から引く依存。
func TestResolver_HasRolePolicyProvider(t *testing.T) {
	assert.False(t, (&Resolver{}).HasRolePolicyProvider(), "未配線なら false")

	r := &Resolver{}
	r.SetRolePolicyProvider(stubWiringRolePolicies{})
	assert.True(t, r.HasRolePolicyProvider(), "配線したら true")
	assert.False(t, r.HasSilencedHostChecker(), "他の述語は満たされないこと")
}
