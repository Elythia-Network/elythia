package notifications

import (
	"testing"

	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NewHandler は受け取った依存を handler に持たせる (#3817)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) と wiring-check は入れ忘れを
// 見なくなった。構築子が引数を捨てても build は通るので、ここで固定する。
// 関数の依存は比べられないので、呼んで渡したものが返ることを見る。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	packer := entity.NewPacker(nil, entity.PackerDeps{})
	hider := notehide.NewHider(nil, nil)
	moderators := stubModeratorChecker{moderators: map[string]bool{"mod1": true}}
	states := func([]string) (map[string]model.AbuseReportState, error) {
		return map[string]model.AbuseReportState{"r1": {Resolved: true}}, nil
	}
	unresolved := func() (int64, error) { return 7, nil }
	applications := func(string) (entity.SignupApplicationStatus, bool) {
		return entity.SignupApplicationStatus{Status: "approved"}, true
	}

	h := NewHandler(nil, nil, packer, hider, moderators, states, unresolved, applications)

	assert.Same(t, packer, h.entityPacker)
	assert.Same(t, hider, h.noteHider)
	assert.Equal(t, moderators, h.moderatorChecker)
	require.NotNil(t, h.abuseReportStates)
	got, err := h.abuseReportStates(nil)
	require.NoError(t, err)
	assert.True(t, got["r1"].Resolved)
	require.NotNil(t, h.abuseUnresolvedCount)
	n, err := h.abuseUnresolvedCount()
	require.NoError(t, err)
	assert.EqualValues(t, 7, n)
	require.NotNil(t, h.signupApplicationLookup)
	app, ok := h.signupApplicationLookup("a1")
	assert.True(t, ok)
	assert.Equal(t, "approved", app.Status)
}
