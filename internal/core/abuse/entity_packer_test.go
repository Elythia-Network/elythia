package abuse

import (
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
)

type entityPackerHolder interface {
	SetEntityPacker(*entity.Packer)
	HasEntityPacker() bool
}

// HasEntityPacker は mediaURL を持つ Packer を受け取ったときだけ true を返す
// (#3794)。criticalWiring がこれで入れ忘れを見る。
func TestHasEntityPacker(t *testing.T) {
	for name, h := range map[string]entityPackerHolder{"CreatedNotifier": &CreatedNotifier{}} {
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
