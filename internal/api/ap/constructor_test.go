package ap

import (
	"testing"

	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/stretchr/testify/assert"
)

// NewHandler は受け取った依存を handler に持たせる (#3817)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	hider := notehide.NewHider(nil, nil)
	packer := entity.NewPacker(entity.PackerDeps{})
	hosts := apShowHostBlocker{blocked: map[string]bool{"blocked.example": true}}
	software := apShowSoftwareBlocker{suspended: map[string]bool{"susp.example": true}}
	extras := &recordingExtras{}

	h := NewHandler(nil, nil, nil, nil, nil, hider, packer, hosts, "local.example", software, extras)

	assert.Same(t, hider, h.noteHider)
	assert.Same(t, packer, h.entityPacker)
	assert.Equal(t, hosts, h.hostBlocker)
	assert.Equal(t, "local.example", h.localHost)
	assert.Equal(t, software, h.softwareBlocker)
	assert.Same(t, extras, h.detailExtras)
}
