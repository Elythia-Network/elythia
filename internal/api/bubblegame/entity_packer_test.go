package bubblegame

import (
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/stretchr/testify/assert"
)

// HasEntityPacker は mediaURL を持つ Packer を受け取ったときだけ true を返す
// (#3794)。criticalWiring がこれで入れ忘れを見る。
func TestVersusHandler_HasEntityPacker(t *testing.T) {
	h := &VersusHandler{}
	if h.HasEntityPacker() {
		t.Error("an unwired component must report false")
	}
	h.SetEntityPacker(entity.NewPacker(entity.PackerDeps{}))
	if h.HasEntityPacker() {
		t.Error("a Packer without mediaURL must report false")
	}
	h.SetEntityPacker(entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "", []byte("secret"), false, true)}))
	if !h.HasEntityPacker() {
		t.Error("a Packer with mediaURL must report true")
	}
}

// NewHandler は受け取った依存を handler に持たせる (#3817)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	packer := entity.NewPacker(entity.PackerDeps{})
	h := NewHandler(nil, nil, packer)
	assert.Same(t, packer, h.entityPacker)
}
