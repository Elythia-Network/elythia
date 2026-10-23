package drive

import (
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
)

// HasEntityPacker は mediaURL を持つ Packer を受け取ったときだけ true を返す
// (#3796)。criticalWiring がこれで入れ忘れを見る。
func TestHasEntityPacker(t *testing.T) {
	for name, h := range map[string]interface {
		SetEntityPacker(*entity.Packer)
		HasEntityPacker() bool
	}{"Service": &Service{}} {
		t.Run(name, func(t *testing.T) {
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
		})
	}
}
