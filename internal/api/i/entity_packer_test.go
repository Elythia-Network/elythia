package i

import (
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
)

// HasEntityPacker は mediaURL を持つ Packer を受け取ったときだけ true を返す
// (#3790)。criticalWiring がこれで入れ忘れを見る。
func TestHandler_HasEntityPacker(t *testing.T) {
	h := &Handler{}
	if h.HasEntityPacker() {
		t.Error("an unwired handler must report false")
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
